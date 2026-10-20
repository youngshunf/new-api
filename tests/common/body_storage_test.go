package commontests

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
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

	require.NoError(t, common.CleanupBodyStorage(c))
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
	require.NoError(t, common.CleanupBodyStorage(c))
	repeated := common.GetDiskCacheStats()
	assert.Equal(t, after.ActiveMemoryBuffers, repeated.ActiveMemoryBuffers, "重复 Gin 清理不得再次减数")
	assert.Equal(t, after.CurrentMemoryUsageBytes, repeated.CurrentMemoryUsageBytes)
	assert.True(t, data.IsNil())
	assert.True(t, backing.IsNil())
	assert.Equal(t, expected, payload)
}

// Windows独立reader真实阻止删除；失败仍归原owner负责，后续显式Close才能完成收口。
func TestDiskBodyStorageCloseRetainsRemovalFailureUntilReaderCloses(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("本反例依赖Windows真实文件共享删除语义，不声明其它平台通过")
	}
	root := t.TempDir()
	config := common.GetDiskCacheConfig()
	t.Cleanup(func() { common.SetDiskCacheConfig(config) })
	before := common.GetDiskCacheStats()
	common.SetDiskCacheConfig(common.DiskCacheConfig{
		Enabled: true, ThresholdMB: 0, MaxSizeMB: int(before.CurrentDiskUsageBytes>>20) + 2, Path: root,
	})
	// 额外持有真实磁盘owner，防DecrementDiskFiles的钳零掩盖重复减计数。
	payloads := [][]byte{[]byte("disk-body-statistics-guard-payload"), []byte("disk-body-locked-payload")}
	var storages []common.BodyStorage
	var readers []*os.File
	var paths []string
	var readerClosed []bool
	for _, payload := range payloads {
		storage, err := common.CreateBodyStorage(payload)
		require.NoError(t, err)
		index := len(storages)
		storages = append(storages, storage)
		readerClosed = append(readerClosed, false)
		var replay io.ReadCloser
		ownedPath := ""
		t.Cleanup(func() {
			if replay != nil && !readerClosed[index] {
				assert.NoError(t, replay.Close())
			}
			assert.NoError(t, storage.Close())
			// 旧实现首红时可能遗留文件；仅兜底删除已经证明属于本临时目录的路径。
			if ownedPath != "" {
				if err := os.Remove(ownedPath); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Errorf("清理本测试磁盘文件失败: %v", err)
				}
			}
		})
		require.True(t, storage.IsDisk(), "反例必须创建真实磁盘owner，内存fallback不能算通过")
		replay, err = storage.NewReader()
		require.NoError(t, err)
		file, ok := replay.(*os.File)
		require.True(t, ok, "文件路径必须由原公开NewReader返回的真实os.File取得")
		path := file.Name()
		relative, err := filepath.Rel(root, path)
		require.NoError(t, err)
		require.True(t, filepath.IsLocal(relative), "仅允许观察和清理本t.TempDir内的路径")
		require.NotEqual(t, ".", relative)
		ownedPath = path
		readers, paths = append(readers, file), append(paths, path)
	}
	created := common.GetDiskCacheStats()
	assert.Equal(t, before.ActiveDiskFiles+2, created.ActiveDiskFiles)
	assert.Equal(t, before.CurrentDiskUsageBytes+int64(len(payloads[0])+len(payloads[1])), created.CurrentDiskUsageBytes)

	storage, replay, path := storages[1], readers[1], paths[1]
	for attempt := 0; attempt < 2; attempt++ {
		err := storage.Close()
		require.Error(t, err, "独立reader尚在持有文件，第%d次Close不得谎报删除成功", attempt+1)
		var pathError *os.PathError
		require.True(t, errors.As(err, &pathError), "必须返回真实文件删除错误，不是测试制造的error")
		assert.Equal(t, "remove", pathError.Op)
		assert.Equal(t, path, pathError.Path)
		assert.False(t, errors.Is(err, os.ErrClosed), "重复Close应只重试未完成的移除，不能重复关闭原句柄")
		stat, statErr := os.Stat(path)
		require.NoError(t, statErr)
		assert.EqualValues(t, len(payloads[1]), stat.Size())
		_, seekErr := replay.Seek(0, io.SeekStart)
		require.NoError(t, seekErr)
		actual, readErr := io.ReadAll(replay)
		require.NoError(t, readErr)
		assert.Equal(t, payloads[1], actual, "owner关闭失败不得破坏已交出的独立reader")
		pending := common.GetDiskCacheStats()
		assert.Equal(t, created.ActiveDiskFiles, pending.ActiveDiskFiles, "文件未移除不得先减磁盘计数")
		assert.Equal(t, created.CurrentDiskUsageBytes, pending.CurrentDiskUsageBytes)
	}
	require.NoError(t, replay.Close())
	readerClosed[1] = true
	require.NoError(t, storage.Close(), "独立reader关闭后显式Close应完成真正的删除")
	_, err := os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
	after := common.GetDiskCacheStats()
	assert.Equal(t, before.ActiveDiskFiles+1, after.ActiveDiskFiles)
	assert.Equal(t, before.CurrentDiskUsageBytes+int64(len(payloads[0])), after.CurrentDiskUsageBytes)
	require.NoError(t, storage.Close(), "成功后的再次Close必须幂等")
	repeated := common.GetDiskCacheStats()
	assert.Equal(t, after.ActiveDiskFiles, repeated.ActiveDiskFiles, "仍有guard文件，重复减数不能被钳零掩盖")
	assert.Equal(t, after.CurrentDiskUsageBytes, repeated.CurrentDiskUsageBytes)
}

func TestCleanupBodyStorageRetainsDiskOwnerOnFailure(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("本反例依赖Windows真实文件共享删除语义，不声明其它平台通过")
	}
	root := t.TempDir()
	config := common.GetDiskCacheConfig()
	t.Cleanup(func() { common.SetDiskCacheConfig(config) })
	before := common.GetDiskCacheStats()
	common.SetDiskCacheConfig(common.DiskCacheConfig{
		Enabled: true, ThresholdMB: 0, MaxSizeMB: int(before.CurrentDiskUsageBytes>>20) + 2, Path: root,
	})
	// 保留另一真实磁盘owner，避免统计钳零掩盖重复清理造成的多减。
	payloads := [][]byte{[]byte("gin-disk-statistics-guard"), []byte("gin-disk-locked-owner")}
	var storages []common.BodyStorage
	for _, payload := range payloads {
		storage, err := common.CreateBodyStorage(payload)
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, storage.Close()) })
		require.True(t, storage.IsDisk(), "必须创建真实磁盘owner，内存fallback不能作为证据")
		storages = append(storages, storage)
	}
	storage := storages[1]
	replay, err := storage.NewReader()
	require.NoError(t, err)
	readerClosed := false
	t.Cleanup(func() {
		if !readerClosed {
			assert.NoError(t, replay.Close())
		}
	})
	file, ok := replay.(*os.File)
	require.True(t, ok, "锁删除必须来自原NewReader的真实文件句柄")
	path := file.Name()
	relative, err := filepath.Rel(root, path)
	require.NoError(t, err)
	require.True(t, filepath.IsLocal(relative), "仅观察本测试临时目录内的文件")
	require.NotEqual(t, ".", relative)
	created := common.GetDiskCacheStats()
	assert.Equal(t, before.ActiveDiskFiles+2, created.ActiveDiskFiles)
	assert.Equal(t, before.CurrentDiskUsageBytes+int64(len(payloads[0])+len(payloads[1])), created.CurrentDiskUsageBytes)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/body-storage-disk-cleanup", http.NoBody)
	c.Set(common.KeyBodyStorage, storage)

	for attempt := 0; attempt < 2; attempt++ {
		// 原公开清理必须运输真实删除错误，并保留Context中的原owner。
		err := common.CleanupBodyStorage(c)
		require.Error(t, err)
		var pathError *os.PathError
		require.ErrorAs(t, err, &pathError)
		assert.Equal(t, "remove", pathError.Op)
		assert.Equal(t, path, pathError.Path)
		cached, exists := c.Get(common.KeyBodyStorage)
		require.True(t, exists)
		require.Same(t, storage, cached, "第%d次真实删除失败必须保留原owner供后续重试", attempt+1)
		stat, statErr := os.Stat(path)
		require.NoError(t, statErr)
		assert.EqualValues(t, len(payloads[1]), stat.Size())
		_, err = file.Seek(0, io.SeekStart)
		require.NoError(t, err)
		actual, readErr := io.ReadAll(replay)
		require.NoError(t, readErr)
		assert.Equal(t, payloads[1], actual, "清理失败不能破坏调用方仍持有的独立reader")
		pending := common.GetDiskCacheStats()
		assert.Equal(t, created.ActiveDiskFiles, pending.ActiveDiskFiles)
		assert.Equal(t, created.CurrentDiskUsageBytes, pending.CurrentDiskUsageBytes)
	}
	require.NoError(t, replay.Close())
	readerClosed = true
	require.NoError(t, common.CleanupBodyStorage(c))
	cached, exists := c.Get(common.KeyBodyStorage)
	require.True(t, exists)
	assert.Nil(t, cached, "真实删除成功后才能断开Context的owner引用")
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
	after := common.GetDiskCacheStats()
	assert.Equal(t, before.ActiveDiskFiles+1, after.ActiveDiskFiles)
	assert.Equal(t, before.CurrentDiskUsageBytes+int64(len(payloads[0])), after.CurrentDiskUsageBytes)
	require.NoError(t, common.CleanupBodyStorage(c))
	repeated := common.GetDiskCacheStats()
	assert.Equal(t, after.ActiveDiskFiles, repeated.ActiveDiskFiles, "guard仍在，重复清理不得再次减数")
	assert.Equal(t, after.CurrentDiskUsageBytes, repeated.CurrentDiskUsageBytes)
}
