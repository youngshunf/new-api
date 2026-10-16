package commontests

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func memoryBodyStorage(t *testing.T, payload []byte) common.BodyStorage {
	t.Helper()
	config := common.GetDiskCacheConfig()
	if config.Enabled {
		disabled := config
		disabled.Enabled = false
		common.SetDiskCacheConfig(disabled)
		t.Cleanup(func() { common.SetDiskCacheConfig(config) })
	}
	storage, err := common.CreateBodyStorage(payload)
	require.NoError(t, err)
	require.False(t, storage.IsDisk(), "本专项只走真实内存 owner，不访问磁盘缓存")
	t.Cleanup(func() { assert.NoError(t, storage.Close()) })
	return storage
}

// 只读观察原 owner 的两条缓冲引用；公共 Closed API 和统计不足以证明已断引用。
// backing 指向关闭前的原 bytes.Reader，即使实现换掉 reader 指针也不能蒙混通过。
func memoryBodyReferences(t *testing.T, storage common.BodyStorage) (data, reader, backing reflect.Value) {
	t.Helper()
	owner := reflect.ValueOf(storage)
	require.Equal(t, reflect.Pointer, owner.Kind())
	require.False(t, owner.IsNil())
	owner = owner.Elem()
	require.Equal(t, "memoryStorage", owner.Type().Name())
	data = owner.FieldByName("data")
	reader = owner.FieldByName("reader")
	require.True(t, data.IsValid())
	require.True(t, reader.IsValid())
	require.Equal(t, reflect.Slice, data.Kind())
	require.Equal(t, reflect.Pointer, reader.Kind())
	require.False(t, data.IsNil(), "非空输入必须先由真实 owner 持有")
	require.False(t, reader.IsNil())
	backing = reader.Elem().FieldByName("s")
	require.True(t, backing.IsValid(), "必须读到原 bytes.Reader 的底层切片")
	require.Equal(t, reflect.Slice, backing.Kind())
	require.False(t, backing.IsNil(), "原 reader 必须先真实引用输入缓冲")
	require.Equal(t, data.Len(), backing.Len())
	return data, reader, backing
}

func TestMemoryBodyStorageCloseCutsOwnerReferencesWithoutMutatingAliases(t *testing.T) {
	payload := []byte("body-storage-owner-payload")
	expected := append([]byte(nil), payload...)
	before := common.GetDiskCacheStats()
	storage := memoryBodyStorage(t, payload)
	data, reader, backing := memoryBodyReferences(t, storage)
	originalReader := reader.Pointer()
	alias, err := storage.Bytes()
	require.NoError(t, err)
	replay, err := storage.NewReader()
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, replay.Close()) })

	require.NoError(t, storage.Close())
	assert.True(t, data.IsNil(), "Close 必须切断 owner.data，不只是减计数")
	assert.Equal(t, originalReader, reader.Pointer(), "须重置原 reader，而不是只换一个新指针")
	assert.True(t, backing.IsNil(), "关闭前原 reader 的底层引用也必须切断")
	assert.Equal(t, expected, payload, "不能清零或改写用户切片")
	assert.Equal(t, expected, alias, "已经交出的 Bytes 别名仍归调用者持有")
	readBack, err := io.ReadAll(replay)
	require.NoError(t, err)
	assert.Equal(t, expected, readBack, "关闭前取得的独立 reader 仍须可读")
	assert.EqualValues(t, len(payload), storage.Size(), "关闭不改变原 Size 合同")

	n, err := storage.Read(make([]byte, 1))
	assert.Zero(t, n)
	require.ErrorIs(t, err, common.ErrStorageClosed)
	offset, err := storage.Seek(0, io.SeekStart)
	assert.Zero(t, offset)
	require.ErrorIs(t, err, common.ErrStorageClosed)
	closedBytes, err := storage.Bytes()
	assert.Nil(t, closedBytes)
	require.ErrorIs(t, err, common.ErrStorageClosed)
	closedReader, err := storage.NewReader()
	assert.Nil(t, closedReader)
	require.ErrorIs(t, err, common.ErrStorageClosed)

	after := common.GetDiskCacheStats()
	assert.Equal(t, before.ActiveMemoryBuffers, after.ActiveMemoryBuffers)
	assert.Equal(t, before.CurrentMemoryUsageBytes, after.CurrentMemoryUsageBytes)
	require.NoError(t, storage.Close())
	repeated := common.GetDiskCacheStats()
	assert.Equal(t, after.ActiveMemoryBuffers, repeated.ActiveMemoryBuffers, "重复关闭不得再减计数")
	assert.Equal(t, after.CurrentMemoryUsageBytes, repeated.CurrentMemoryUsageBytes)
}

func TestCleanupBodyStorageCutsOwnerReferencesWithRequestWrapperStillAlive(t *testing.T) {
	payload := []byte("gin-body-storage-owner-payload")
	expected := append([]byte(nil), payload...)
	before := common.GetDiskCacheStats()
	storage := memoryBodyStorage(t, payload)
	data, reader, backing := memoryBodyReferences(t, storage)
	originalReader := reader.Pointer()
	alias, err := storage.Bytes()
	require.NoError(t, err)
	replay, err := storage.NewReader()
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, replay.Close()) })
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/body-storage-cleanup", common.NewReplayableBodyReader(storage))
	c.Set(common.KeyBodyStorage, storage)
	requestBody := c.Request.Body
	t.Cleanup(func() { assert.NoError(t, requestBody.Close()) })

	common.CleanupBodyStorage(c)
	cached, exists := c.Get(common.KeyBodyStorage)
	require.True(t, exists)
	assert.Nil(t, cached, "原 Gin 清理必须断开 Context 对 storage 的引用")
	assert.True(t, data.IsNil(), "外部仍持有 storage 时，owner.data 也必须已释放")
	assert.Equal(t, originalReader, reader.Pointer())
	assert.True(t, backing.IsNil(), "原请求包装器仍可达不能让 owner.reader 保留缓冲")
	assert.Equal(t, expected, payload)
	assert.Equal(t, expected, alias)
	readBack, err := io.ReadAll(replay)
	require.NoError(t, err)
	assert.Equal(t, expected, readBack, "独立 reader 的既有所有权不被 Gin 清理破坏")
	n, err := requestBody.Read(make([]byte, 1))
	assert.Zero(t, n)
	require.ErrorIs(t, err, common.ErrStorageClosed)
	_, err = storage.NewReader()
	require.ErrorIs(t, err, common.ErrStorageClosed)

	after := common.GetDiskCacheStats()
	assert.Equal(t, before.ActiveMemoryBuffers, after.ActiveMemoryBuffers)
	assert.Equal(t, before.CurrentMemoryUsageBytes, after.CurrentMemoryUsageBytes)
	common.CleanupBodyStorage(c)
	repeated := common.GetDiskCacheStats()
	assert.Equal(t, after.ActiveMemoryBuffers, repeated.ActiveMemoryBuffers, "重复 Gin 清理不得再次减数")
	assert.Equal(t, after.CurrentMemoryUsageBytes, repeated.CurrentMemoryUsageBytes)
	assert.True(t, data.IsNil())
	assert.True(t, backing.IsNil())
	assert.Equal(t, expected, payload)
}
