package common

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRelayBodyBudgetUnknownLengthRequiresDiskEvenWhenCacheDisabled(t *testing.T) {
	previous := GetDiskCacheConfig()
	SetDiskCacheConfig(DiskCacheConfig{Enabled: false, ThresholdMB: 10, MaxSizeMB: 1024, Path: t.TempDir()})
	t.Cleanup(func() { SetDiskCacheConfig(previous) })
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.ContentLength = -1
	c.Set("relay_body_budget", int64(256<<20))
	storage, err := GetBodyStorage(c)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, CleanupBodyStorage(c)); ReleaseRelayLargeBody(c) })
	assert.True(t, storage.IsDisk(), "未知长度LLM请求必须磁盘暂存，不能受可关闭缓存开关控制")
	actual, err := io.ReadAll(storage)
	require.NoError(t, err)
	assert.Equal(t, []byte(`{"model":"m"}`), actual)
}

func TestRelayBodyBudgetUnknownLengthDoesNotFallbackWhenDiskFails(t *testing.T) {
	previous := GetDiskCacheConfig()
	SetDiskCacheConfig(DiskCacheConfig{Enabled: false, Path: "/dev/null/不能创建"})
	t.Cleanup(func() { SetDiskCacheConfig(previous) })
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.ContentLength = -1
	c.Set("relay_body_budget", int64(256<<20))
	storage, err := GetBodyStorage(c)
	require.Error(t, err)
	assert.Nil(t, storage)
	ReleaseRelayLargeBody(c)
}

func TestRelayBodyBudgetSmallDeclaredLengthCannotBypassDiskOrBusy(t *testing.T) {
	previous := GetDiskCacheConfig()
	SetDiskCacheConfig(DiskCacheConfig{Enabled: false, Path: t.TempDir()})
	t.Cleanup(func() { SetDiskCacheConfig(previous) })
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/body", io.NopCloser(io.LimitReader(repeatedBodyByte{}, RelayMemoryBodyBytes+1)))
	c.Request.ContentLength = 1
	c.Set(RelayBodyBudgetKey, int64(256<<20))
	storage, err := GetBodyStorage(c)
	require.NoError(t, err)
	defer ReleaseRelayLargeBody(c)
	require.True(t, storage.IsDisk())
	assert.Equal(t, RelayMemoryBodyBytes+1, storage.Size())
	require.NoError(t, storage.Close())
	other, _ := gin.CreateTestContext(httptest.NewRecorder())
	assert.ErrorIs(t, AcquireRelayLargeBody(other), ErrLargeRequestBusy, "中间存储关闭不能释放请求占用")
	require.NoError(t, CleanupBodyStorage(c))
	ReleaseRelayLargeBody(c)
	require.NoError(t, AcquireRelayLargeBody(other))
	ReleaseRelayLargeBody(other)
}

type repeatedBodyByte struct{}

func (repeatedBodyByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestRelayBodyBudgetCountingBoundaryUsesSmallLimits(t *testing.T) {
	previous := GetDiskCacheConfig()
	SetDiskCacheConfig(DiskCacheConfig{Path: t.TempDir()})
	t.Cleanup(func() { SetDiskCacheConfig(previous) })
	for _, size := range []int64{7, 8, 9} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/body", io.NopCloser(io.LimitReader(repeatedBodyByte{}, size)))
		c.Request.ContentLength = -1
		c.Set(RelayBodyBudgetKey, int64(8))
		storage, err := GetBodyStorage(c)
		if size <= 8 {
			require.NoError(t, err)
			assert.Equal(t, size, storage.Size())
		} else {
			assert.ErrorIs(t, err, ErrRequestBodyTooLarge)
		}
		require.NoError(t, CleanupBodyStorage(c))
		ReleaseRelayLargeBody(c)
	}
}

// multipart解析必须通过reader；测试替身拒绝整包Bytes回读。
type multipartReaderOnlyStorage struct{ BodyStorage }

func (multipartReaderOnlyStorage) Bytes() ([]byte, error) {
	return nil, errors.New("multipart不能整包回读")
}

func TestRelayBodyBudgetMultipartParsesWithoutBytesAndRemovesFiles(t *testing.T) {
	previous := constant.MaxFileDownloadMB
	constant.MaxFileDownloadMB = 1
	t.Cleanup(func() { constant.MaxFileDownloadMB = previous })
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("model", "test-model"))
	part, err := writer.CreateFormFile("file", "clip.wav")
	require.NoError(t, err)
	_, err = io.Copy(part, io.LimitReader(repeatedBodyByte{}, (1<<20)+1))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	storage := multipartReaderOnlyStorage{newMemoryStorage(body.Bytes())}
	defer storage.Close()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/body", http.NoBody)
	c.Request.Header.Set("Content-Type", writer.FormDataContentType())
	c.Set(KeyBodyStorage, storage)
	form, err := ParseMultipartFormReusable(c)
	require.NoError(t, err)
	defer form.RemoveAll()
	assert.Equal(t, []string{"test-model"}, form.Value["model"])
	require.Len(t, form.File["file"], 1)
	file, err := form.File["file"][0].Open()
	require.NoError(t, err)
	temp, ok := file.(*os.File)
	require.True(t, ok, "超过multipart内存额度的文件必须真实落盘")
	path := temp.Name()
	assert.Equal(t, int64((1<<20)+1), form.File["file"][0].Size)
	require.NoError(t, file.Close())
	var decoded struct {
		Model string `json:"model"`
	}
	require.NoError(t, UnmarshalBodyReusable(c, &decoded))
	assert.Equal(t, "test-model", decoded.Model)
	require.NoError(t, CleanupBodyStorage(c))
	_, err = os.Stat(path)
	assert.ErrorIs(t, err, os.ErrNotExist, "返回给不同调用者的multipart表单也必须随请求清理")
}

func TestRelayBodyBudgetRebuiltLargeBodyRequiresDisk(t *testing.T) {
	previous := GetDiskCacheConfig()
	SetDiskCacheConfig(DiskCacheConfig{Enabled: false, Path: t.TempDir()})
	t.Cleanup(func() { SetDiskCacheConfig(previous) })
	data := make([]byte, RelayMemoryBodyBytes+1)
	storage, err := CreateBodyStorage(data)
	require.NoError(t, err)
	assert.True(t, storage.IsDisk())
	require.NoError(t, storage.Close())
	SetDiskCacheConfig(DiskCacheConfig{Enabled: false, Path: "/dev/null/不能创建"})
	storage, err = CreateBodyStorage(data)
	require.Error(t, err)
	assert.Nil(t, storage, "磁盘失败不能回退整包内存")
}

func TestNewReplayableBodyReaderKeepsStorageLifecycleWithCaller(t *testing.T) {
	payload := []byte(`{"model":"test-model","input":"hello"}`)
	storage, err := CreateBodyStorage(payload)
	require.NoError(t, err)
	defer storage.Close()

	body := NewReplayableBodyReader(storage)
	assert.EqualValues(t, len(payload), body.Size())
	_, exposesCloser := any(body).(io.Closer)
	assert.False(t, exposesCloser, "the request body must not expose the storage closer")

	req, err := http.NewRequest(http.MethodPost, "https://example.com", body)
	require.NoError(t, err)
	require.NoError(t, req.Body.Close())

	replayBody, err := body.NewReader()
	require.NoError(t, err, "closing the HTTP request body must not close the storage")
	replay, err := io.ReadAll(replayBody)
	require.NoError(t, err)
	require.NoError(t, replayBody.Close())
	assert.Equal(t, payload, replay)

	require.NoError(t, storage.Close())
	_, err = body.NewReader()
	require.ErrorIs(t, err, ErrStorageClosed)
}
