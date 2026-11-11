package middleware

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/retrycontrol"
	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRelayBodyBudgetEarly413RetryHeaders(t *testing.T) {
	previous := common.GetDiskCacheConfig()
	common.SetDiskCacheConfig(common.DiskCacheConfig{Path: t.TempDir()})
	t.Cleanup(func() { common.SetDiskCacheConfig(previous) })
	for _, stage := range []string{"budget_receive", "gzip_initialize"} {
		t.Run(stage, func(t *testing.T) {
			response := httptest.NewRecorder()
			router := gin.New()
			var body io.ReadCloser
			var ingress gin.HandlerFunc
			if stage == "budget_receive" {
				// 使用真实的小额计数reader，不为413反例构造256MiB正文。
				body = http.MaxBytesReader(response, io.NopCloser(strings.NewReader("123456789")), 8)
				ingress = AuthenticatedRelayBodyBudget()
			} else {
				var compressed bytes.Buffer
				writer := gzip.NewWriter(&compressed)
				writer.Header.Extra = make([]byte, 32)
				require.NoError(t, writer.Close())
				body = io.NopCloser(&compressed)
				router.Use(func(c *gin.Context) { c.Set(common.RelayBodyBudgetKey, int64(8)) })
				ingress = DecompressRequestMiddleware()
			}
			hits := 0
			router.POST("/body", BodyStorageCleanup(), ingress, func(c *gin.Context) { hits++ })
			request := httptest.NewRequest(http.MethodPost, "/body", body)
			request.Header.Set("Content-Type", "application/json")
			if stage == "gzip_initialize" {
				request.Header.Set("Content-Encoding", "gzip")
			}
			router.ServeHTTP(response, request)
			result := response.Result()
			defer result.Body.Close()
			assert.Equal(t, http.StatusRequestEntityTooLarge, result.StatusCode)
			assert.Equal(t, "request_invalid", result.Header.Get(retrycontrol.HeaderRetryReason))
			assert.Equal(t, "not_dispatched", result.Header.Get(retrycontrol.HeaderDispatchState))
			assert.Equal(t, "false", result.Header.Get(retrycontrol.HeaderRetryable))
			assert.Equal(t, "0", result.Header.Get(retrycontrol.HeaderRetryAfterMs))
			assert.Zero(t, hits, "提前413不能进入业务或建立上游连接")
		})
	}
}

func TestRelayBodyBudgetMIMEClassification(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		contentType string
		status      int
		limit       int64
	}{
		{"application/json", 200, 256 << 20},
		{"multipart/form-data; boundary=b", 200, 1 << 30},
		{"multipart/form-data", 400, 0},
		{"multipart/form-data; boundary=", 400, 0},
		{"multipart/form-data; boundary=\"bad\r\nvalue\"", 400, 0},
		{"application/json; broken", 400, 0},
	} {
		t.Run(tc.contentType, func(t *testing.T) {
			router := gin.New()
			router.POST("/body", BodyStorageCleanup(), AuthenticatedRelayBodyBudget(), func(c *gin.Context) {
				assert.Equal(t, tc.limit, common.RequestBodyLimitBytes(c))
				c.Status(http.StatusOK)
			})
			request := httptest.NewRequest(http.MethodPost, "/body", bytes.NewReader([]byte("{}")))
			request.Header.Set("Content-Type", tc.contentType)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			assert.Equal(t, tc.status, response.Code)
		})
	}
}

func TestRelayBodyBudgetBusyRejectsBeforeCompressedRead(t *testing.T) {
	owner, _ := gin.CreateTestContext(httptest.NewRecorder())
	require.NoError(t, common.AcquireRelayLargeBody(owner))
	defer common.ReleaseRelayLargeBody(owner)
	for _, encoding := range []string{"gzip", "br", "zstd"} {
		body := &zstdBodyReadCounter{}
		router := gin.New()
		router.POST("/body", BodyStorageCleanup(), AuthenticatedRelayBodyBudget())
		request := httptest.NewRequest(http.MethodPost, "/body", io.NopCloser(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Content-Encoding", encoding)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		assert.Equal(t, http.StatusTooManyRequests, response.Code)
		assert.Contains(t, response.Body.String(), "large_request_busy")
		assert.Zero(t, body.reads.Load())
	}
}

func TestRelayBodyBudgetAuthenticationRejectionDoesNotReadGzip(t *testing.T) {
	body := &zstdBodyReadCounter{}
	router := gin.New()
	router.POST("/body", BodyStorageCleanup(), func(c *gin.Context) { c.AbortWithStatus(http.StatusUnauthorized) }, AuthenticatedRelayBodyBudget())
	request := httptest.NewRequest(http.MethodPost, "/body", io.NopCloser(body))
	request.Header.Set("Content-Encoding", "gzip")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	assert.Equal(t, http.StatusUnauthorized, response.Code)
	assert.Zero(t, body.reads.Load())
}

func TestRelayBodyBudgetCompressedInputAndOutputLimits(t *testing.T) {
	payload := bytes.Repeat([]byte("a"), 4096)
	for _, encoding := range []string{"gzip", "br", "zstd"} {
		var compressed bytes.Buffer
		switch encoding {
		case "gzip":
			writer := gzip.NewWriter(&compressed)
			_, err := writer.Write(payload)
			require.NoError(t, err)
			require.NoError(t, writer.Close())
		case "br":
			writer := brotli.NewWriter(&compressed)
			_, err := writer.Write(payload)
			require.NoError(t, err)
			require.NoError(t, writer.Close())
		case "zstd":
			writer, err := zstd.NewWriter(&compressed, zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(1024), zstd.WithSingleSegment(false))
			require.NoError(t, err)
			_, err = writer.Write(payload)
			require.NoError(t, err)
			require.NoError(t, writer.Close())
		}
		for _, limit := range []int64{int64(compressed.Len() - 1), 2048, 4096} {
			t.Run(encoding+"/"+fmt.Sprint(limit), func(t *testing.T) {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/body", bytes.NewReader(compressed.Bytes()))
				c.Request.Header.Set("Content-Encoding", encoding)
				c.Set(common.RelayBodyBudgetKey, limit)
				if !decompressRequest(c) {
					assert.Less(t, limit, int64(compressed.Len()))
					return
				}
				defer c.Request.Body.Close()
				data, err := io.ReadAll(c.Request.Body)
				assert.EqualValues(t, -1, c.Request.ContentLength)
				assert.Empty(t, c.GetHeader("Content-Encoding"))
				if limit < 4096 {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
					assert.Equal(t, payload, data)
				}
			})
		}
	}
}

func TestRelayBodyBudgetCompressedInputLimitCountsEncodingOverhead(t *testing.T) {
	for _, encoding := range []string{"gzip", "br", "zstd"} {
		t.Run(encoding, func(t *testing.T) {
			var compressed bytes.Buffer
			var limit int64
			switch encoding {
			case "gzip":
				writer := gzip.NewWriter(&compressed)
				writer.Header.Extra = make([]byte, 4096)
				require.NoError(t, writer.Close())
				limit = 1024
			case "br":
				writer := brotli.NewWriter(&compressed)
				_, err := writer.Write([]byte("a"))
				require.NoError(t, err)
				require.NoError(t, writer.Close())
				limit = int64(compressed.Len() - 1)
				require.Greater(t, limit, int64(1), "解码输出必须小于输入限额")
			case "zstd":
				// 合法skippable帧没有解码输出，拒绝只能来自压缩输入计数。
				_, err := compressed.Write(binary.LittleEndian.AppendUint32([]byte{0x50, 0x2a, 0x4d, 0x18}, 4096))
				require.NoError(t, err)
				_, err = compressed.Write(make([]byte, 4096))
				require.NoError(t, err)
				limit = 1024
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/body", bytes.NewReader(compressed.Bytes()))
			c.Request.Header.Set("Content-Encoding", encoding)
			c.Set(common.RelayBodyBudgetKey, limit)
			if !decompressRequest(c) {
				assert.Equal(t, http.StatusRequestEntityTooLarge, c.Writer.Status())
				return
			}
			defer c.Request.Body.Close()
			_, err := io.ReadAll(c.Request.Body)
			var sizeErr *http.MaxBytesError
			require.ErrorAs(t, err, &sizeErr)
			assert.Equal(t, limit, sizeErr.Limit)
		})
	}
}

type relayBudgetCanceledBody struct{}

func (relayBudgetCanceledBody) Read([]byte) (int, error) { return 0, context.Canceled }

func TestRelayBodyBudgetCanceledReadReleasesDiskAndOccupancy(t *testing.T) {
	previous := common.GetDiskCacheConfig()
	common.SetDiskCacheConfig(common.DiskCacheConfig{Path: t.TempDir()})
	t.Cleanup(func() { common.SetDiskCacheConfig(previous) })
	router := gin.New()
	router.POST("/body", BodyStorageCleanup(), AuthenticatedRelayBodyBudget(), func(c *gin.Context) { t.Error("取消读体不能进入业务") })
	request := httptest.NewRequest(http.MethodPost, "/body", io.NopCloser(relayBudgetCanceledBody{}))
	request.ContentLength = -1
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	assert.Equal(t, http.StatusBadRequest, response.Code)
	files, _, err := common.GetDiskCacheInfo()
	require.NoError(t, err)
	assert.Zero(t, files)
	other, _ := gin.CreateTestContext(httptest.NewRecorder())
	require.NoError(t, common.AcquireRelayLargeBody(other))
	common.ReleaseRelayLargeBody(other)
}

type relayBudgetReadCounter struct{ bytes int64 }

func (r *relayBudgetReadCounter) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	r.bytes += int64(len(p))
	return len(p), nil
}

func TestRelayBodyBudgetSmallDeclaredLengthBusyStopsAtBoundedPrefix(t *testing.T) {
	owner, _ := gin.CreateTestContext(httptest.NewRecorder())
	require.NoError(t, common.AcquireRelayLargeBody(owner))
	defer common.ReleaseRelayLargeBody(owner)
	body := &relayBudgetReadCounter{}
	hits := 0
	router := gin.New()
	router.POST("/body", BodyStorageCleanup(), AuthenticatedRelayBodyBudget(), func(c *gin.Context) { hits++ })
	request := httptest.NewRequest(http.MethodPost, "/body", io.NopCloser(body))
	request.ContentLength = 1
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	assert.Equal(t, http.StatusTooManyRequests, response.Code)
	assert.Contains(t, response.Body.String(), "large_request_busy")
	assert.Equal(t, common.RelayMemoryBodyBytes+1, body.bytes)
	assert.Zero(t, hits, "超限或占用失败必须在上游调用前终止")
}

func TestRelayBodyBudgetFailureReleasesOccupancyAndFiles(t *testing.T) {
	previous := common.GetDiskCacheConfig()
	common.SetDiskCacheConfig(common.DiskCacheConfig{Path: t.TempDir()})
	t.Cleanup(func() { common.SetDiskCacheConfig(previous) })
	for _, encoding := range []string{"gzip", "br", "zstd"} {
		t.Run(encoding, func(t *testing.T) {
			router := gin.New()
			router.POST("/body", BodyStorageCleanup(), AuthenticatedRelayBodyBudget(), func(c *gin.Context) { t.Error("坏压缩不能进入业务") })
			request := httptest.NewRequest(http.MethodPost, "/body", strings.NewReader("not compressed"))
			request.Header.Set("Content-Encoding", encoding)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			assert.Equal(t, http.StatusBadRequest, response.Code)
			other, _ := gin.CreateTestContext(httptest.NewRecorder())
			require.NoError(t, common.AcquireRelayLargeBody(other), "失败后的请求必须能获得占用")
			common.ReleaseRelayLargeBody(other)
			files, _, err := common.GetDiskCacheInfo()
			require.NoError(t, err)
			assert.Zero(t, files)
		})
	}
}

func TestRelayBodyBudgetCleanupRemovesMultipartAndHoldsUntilResponseEnd(t *testing.T) {
	previous := common.GetDiskCacheConfig()
	common.SetDiskCacheConfig(common.DiskCacheConfig{Path: t.TempDir()})
	t.Cleanup(func() { common.SetDiskCacheConfig(previous) })
	var tempPath string
	router := gin.New()
	router.POST("/body", BodyStorageCleanup(), AuthenticatedRelayBodyBudget(), func(c *gin.Context) {
		storage, err := common.GetBodyStorage(c)
		require.NoError(t, err)
		require.True(t, storage.IsDisk())
		require.NoError(t, storage.Close())
		other, _ := gin.CreateTestContext(httptest.NewRecorder())
		assert.ErrorIs(t, common.AcquireRelayLargeBody(other), common.ErrLargeRequestBusy)
		var formBody bytes.Buffer
		writer := multipart.NewWriter(&formBody)
		part, err := writer.CreateFormFile("file", "clip.wav")
		require.NoError(t, err)
		_, err = io.Copy(part, io.LimitReader(&relayBudgetReadCounter{}, 1024))
		require.NoError(t, err)
		require.NoError(t, writer.Close())
		form, err := multipart.NewReader(&formBody, writer.Boundary()).ReadForm(0)
		require.NoError(t, err)
		c.Request.MultipartForm = form
		file, err := form.File["file"][0].Open()
		require.NoError(t, err)
		temp, ok := file.(*os.File)
		require.True(t, ok)
		tempPath = temp.Name()
		require.NoError(t, file.Close())
		c.Status(http.StatusOK)
	})
	request := httptest.NewRequest(http.MethodPost, "/body", strings.NewReader("{}"))
	request.ContentLength = -1
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	assert.Equal(t, http.StatusOK, response.Code)
	require.NotEmpty(t, tempPath)
	_, err := os.Stat(tempPath)
	assert.ErrorIs(t, err, os.ErrNotExist)
	other, _ := gin.CreateTestContext(httptest.NewRecorder())
	require.NoError(t, common.AcquireRelayLargeBody(other))
	common.ReleaseRelayLargeBody(other)
	files, _, err := common.GetDiskCacheInfo()
	require.NoError(t, err)
	assert.Zero(t, files)
}

func TestRelayBodyBudgetDoesNotExpandAnonymousOrManagement(t *testing.T) {
	previous := constant.MaxRequestBodyMB
	previousAnonymous := constant.AnonymousRequestBodyLimitKB
	constant.MaxRequestBodyMB = 1
	constant.AnonymousRequestBodyLimitKB = -1
	t.Cleanup(func() { constant.MaxRequestBodyMB = previous; constant.AnonymousRequestBodyLimitKB = previousAnonymous })
	assert.Equal(t, int64(512<<10), common.GetAnonymousRequestBodyLimitBytes())
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	assert.Equal(t, int64(1<<20), common.RequestBodyLimitBytes(c))
	for _, size := range []int64{(512 << 10) - 1, 512 << 10, (512 << 10) + 1} {
		router := gin.New()
		router.POST("/anonymous", AnonymousRequestBodyLimit(), func(c *gin.Context) { c.Status(http.StatusOK) })
		request := httptest.NewRequest(http.MethodPost, "/anonymous", io.NopCloser(io.LimitReader(&relayBudgetReadCounter{}, size)))
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if size > 512<<10 {
			assert.Equal(t, http.StatusRequestEntityTooLarge, response.Code)
		} else {
			assert.Equal(t, http.StatusOK, response.Code)
		}
	}
}

var zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

// zstdRLEBlock is a last block that repeats 'a' once.
var zstdRLEBlock = []byte{0x0b, 0x00, 0x00, 'a'}

func readZstdBody(t *testing.T, body []byte) ([]byte, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	var data []byte
	var readErr error
	router.POST("/v1/chat/completions", DecompressRequestMiddleware(), func(c *gin.Context) {
		data, readErr = io.ReadAll(c.Request.Body)
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	request.Header.Set("Content-Encoding", "zstd")
	t.Cleanup(func() {
		assert.NoError(t, request.Body.Close())
	})
	router.ServeHTTP(httptest.NewRecorder(), request)
	return data, readErr
}

func TestDecompressRequestZstd(t *testing.T) {
	previousMaxBodyMB := constant.MaxRequestBodyMB
	constant.MaxRequestBodyMB = 16
	t.Cleanup(func() {
		constant.MaxRequestBodyMB = previousMaxBodyMB
	})

	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	require.NoError(t, err)
	payload := bytes.Repeat([]byte(`{"role":"user","content":"hello"},`), 1<<15)
	compressed := encoder.EncodeAll(payload, nil)
	require.NoError(t, encoder.Close())

	// Window descriptors 0x50, 0x68, 0x69, and 0x98 declare 1MiB, 8MiB, 9MiB, and 512MiB windows.
	smallWindowFrame := append(append(append([]byte{}, zstdMagic...), 0x00, 0x50), zstdRLEBlock...)
	boundaryWindowFrame := append(append(append([]byte{}, zstdMagic...), 0x00, 0x68), zstdRLEBlock...)
	aboveWindowFrame := append(append(append([]byte{}, zstdMagic...), 0x00, 0x69), zstdRLEBlock...)
	largeWindowFrame := append(append(append([]byte{}, zstdMagic...), 0x00, 0x98), zstdRLEBlock...)
	// A single-segment frame uses its declared content size, here 100MiB, as the window.
	largeContentFrame := binary.LittleEndian.AppendUint64(append(append([]byte{}, zstdMagic...), 0xe0), 100<<20)
	largeContentFrame = append(largeContentFrame, zstdRLEBlock...)

	largePayload := bytes.Repeat([]byte("a"), 9<<20)
	boundedEncoder, err := zstd.NewWriter(nil, zstd.WithWindowSize(1<<20), zstd.WithSingleSegment(false), zstd.WithEncoderConcurrency(1))
	require.NoError(t, err)
	largeBody := boundedEncoder.EncodeAll(largePayload, nil)
	require.NoError(t, boundedEncoder.Close())
	var header zstd.Header
	require.NoError(t, header.Decode(largeBody))
	require.False(t, header.SingleSegment)
	require.LessOrEqual(t, header.WindowSize, uint64(8<<20))

	singleEncoder, err := zstd.NewWriter(nil, zstd.WithSingleSegment(true), zstd.WithEncoderConcurrency(1))
	require.NoError(t, err)
	boundaryContentFrame := singleEncoder.EncodeAll(largePayload[:8<<20], nil)
	aboveContentFrame := singleEncoder.EncodeAll(largePayload[:(8<<20)+1], nil)
	require.NoError(t, singleEncoder.Close())
	for _, frame := range [][]byte{boundaryContentFrame, aboveContentFrame} {
		require.NoError(t, header.Decode(frame))
		require.True(t, header.SingleSegment)
	}

	cases := []struct {
		name           string
		body           []byte
		wantBody       []byte
		wantErr        error
		maxBodyMB      int
		wantLimitError bool
	}{
		{name: "regular body is decompressed", body: compressed, wantBody: payload},
		{name: "512MiB window is rejected", body: largeWindowFrame, wantErr: zstd.ErrWindowSizeExceeded},
		{name: "100MiB single-segment window is rejected", body: largeContentFrame, wantErr: zstd.ErrDecoderSizeExceeded},
		{name: "8MiB window is accepted", body: boundaryWindowFrame, wantBody: []byte("a")},
		{name: "9MiB window is rejected", body: aboveWindowFrame, wantErr: zstd.ErrWindowSizeExceeded},
		{name: "8MiB single-segment body is accepted", body: boundaryContentFrame, wantBody: largePayload[:8<<20]},
		{name: "single-segment body one byte above 8MiB is rejected", body: aboveContentFrame, wantErr: zstd.ErrDecoderSizeExceeded},
		{name: "9MiB body with a bounded window is decompressed", body: largeBody, wantBody: largePayload},
		{name: "bounded window does not bypass the decoded body limit", body: largeBody, wantBody: largePayload[:1<<20], maxBodyMB: 1, wantLimitError: true},
		{name: "window equal to a smaller request limit is accepted", body: smallWindowFrame, wantBody: []byte("a"), maxBodyMB: 1},
		{name: "window above a smaller request limit is rejected", body: boundaryWindowFrame, wantErr: zstd.ErrWindowSizeExceeded, maxBodyMB: 1},
		{name: "single-segment window above a smaller request limit is rejected", body: boundaryContentFrame, wantErr: zstd.ErrDecoderSizeExceeded, maxBodyMB: 1},
		{name: "oversized window in a later frame is rejected", body: append(append([]byte{}, compressed...), largeWindowFrame...), wantBody: payload, wantErr: zstd.ErrWindowSizeExceeded},
		{name: "oversized single-segment window in a later frame is rejected", body: append(append([]byte{}, compressed...), aboveContentFrame...), wantBody: payload, wantErr: zstd.ErrDecoderSizeExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.maxBodyMB > 0 {
				constant.MaxRequestBodyMB = tc.maxBodyMB
				t.Cleanup(func() {
					constant.MaxRequestBodyMB = 16
				})
			}
			data, err := readZstdBody(t, tc.body)

			switch {
			case tc.wantLimitError:
				var sizeErr *http.MaxBytesError
				require.ErrorAs(t, err, &sizeErr)
				assert.Equal(t, int64(tc.maxBodyMB)<<20, sizeErr.Limit)
			case tc.wantErr != nil:
				require.ErrorIs(t, err, tc.wantErr)
			default:
				require.NoError(t, err)
			}
			if tc.wantBody == nil {
				assert.Empty(t, data)
				return
			}
			assert.Equal(t, tc.wantBody, data)
		})
	}
}

type zstdBodyReadCounter struct {
	reads atomic.Int32
}

func (b *zstdBodyReadCounter) Read([]byte) (int, error) {
	b.reads.Add(1)
	return 0, io.EOF
}

func TestDecompressRequestZstdEarlyRejectionDoesNotReadBody(t *testing.T) {
	// Keep this regression meaningful even on hosts where the default decoder
	// would otherwise be synchronous because GOMAXPROCS is 1.
	previousProcs := runtime.GOMAXPROCS(2)
	t.Cleanup(func() {
		runtime.GOMAXPROCS(previousProcs)
	})

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/v1/chat/completions", DecompressRequestMiddleware(), func(c *gin.Context) {
		c.AbortWithStatus(http.StatusUnauthorized)
	})
	body := &zstdBodyReadCounter{}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", io.NopCloser(body))
	request.Header.Set("Content-Encoding", "zstd")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	// Closing also joins any asynchronous decoder before checking the read count.
	require.NoError(t, request.Body.Close())
	assert.Equal(t, http.StatusUnauthorized, response.Code)
	assert.Zero(t, body.reads.Load(), "an early rejection must not consume the request body")
}
