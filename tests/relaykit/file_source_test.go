package relaykit_test

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiskCachedFileDataCloseRetriesRemovalAfterReaderCloses(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("本反例依赖Windows独立reader阻止删除的真实语义，不声明其它平台通过")
	}
	path := filepath.Join(t.TempDir(), "cached-file.txt")
	payload := []byte("Y2FjaGUtZmlsZS1sb2NrZWQ=")
	require.NoError(t, os.WriteFile(path, payload, 0600))
	stat, err := os.Stat(path)
	require.NoError(t, err)
	require.EqualValues(t, len(payload), stat.Size())
	// 逻辑内容大小与真实缓存占用不同，统计回调必须使用实际磁盘字节数。
	cache := types.NewDiskCachedData(path, "text/plain", int64(len("cache-file-locked")))
	cache.DiskSize = stat.Size()
	var closedSizes []int64
	cache.OnClose = func(size int64) { closedSizes = append(closedSizes, size) }
	var reader *os.File
	readerClosed := false
	t.Cleanup(func() {
		if reader != nil && !readerClosed {
			assert.NoError(t, reader.Close())
		}
		assert.NoError(t, cache.Close())
		// 首红实现可能保留文件，只兜底删除本测试直接创建的临时路径。
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("清理本测试缓存文件失败: %v", err)
		}
	})
	require.True(t, cache.IsDisk(), "必须使用原公开构造函数创建真实磁盘缓存")
	reader, err = os.Open(path)
	require.NoError(t, err)
	actual, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, payload, actual)

	firstErr := cache.Close()
	require.Error(t, firstErr, "独立reader尚未关闭，第一次Close必须报告真实删除失败")
	var firstPathError *os.PathError
	require.ErrorAs(t, firstErr, &firstPathError)
	assert.Equal(t, "remove", firstPathError.Op)
	assert.Equal(t, path, firstPathError.Path)
	assert.Empty(t, closedSizes, "文件未移除不得调用统计回调")
	_, readErr := cache.GetBase64Data()
	require.Error(t, readErr, "已开始清理后即使移除失败，也必须禁止新的缓存读取")

	secondErr := cache.Close()
	require.Error(t, secondErr, "第一次删除失败后，第二次Close不得因diskClosed返回假nil")
	var secondPathError *os.PathError
	require.ErrorAs(t, secondErr, &secondPathError)
	assert.Equal(t, "remove", secondPathError.Op)
	assert.Equal(t, path, secondPathError.Path)
	assert.Empty(t, closedSizes, "重复删除失败仍不得提前扣减统计")
	pending, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, stat.Size(), pending.Size(), "失败时原缓存文件必须仍然存在")
	_, err = reader.Seek(0, io.SeekStart)
	require.NoError(t, err)
	actual, err = io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, payload, actual, "删除失败不得破坏已打开的独立reader")

	require.NoError(t, reader.Close())
	readerClosed = true
	require.NoError(t, cache.Close(), "reader关闭后必须允许显式Close重试并删除文件")
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Equal(t, []int64{stat.Size()}, closedSizes, "删除成功后统计回调恰一次且传入真实占用")
	require.NoError(t, cache.Close(), "删除完成后再次Close必须幂等")
	assert.Equal(t, []int64{stat.Size()}, closedSizes, "重复Close不得重复扣减统计")
}

func TestMemoryCachedFileDataCloseReleasesOwnedContent(t *testing.T) {
	cache := types.NewMemoryCachedData("owned-cached-content", "text/plain", 20)
	actual, err := cache.GetBase64Data()
	require.NoError(t, err)
	require.Equal(t, "owned-cached-content", actual)
	require.NoError(t, cache.Close())
	actual, err = cache.GetBase64Data()
	require.NoError(t, err)
	assert.Empty(t, actual, "关闭内存缓存必须释放所属内容")
	require.NoError(t, cache.Close())
}

func TestDiskCachedFileDataCloseWithEmptyPathIsIdempotent(t *testing.T) {
	cache := types.NewDiskCachedData("", "text/plain", 0)
	require.NoError(t, cache.Close())
	_, err := cache.GetBase64Data()
	require.Error(t, err, "开始关闭后必须禁止新读")
	require.NoError(t, cache.Close())
}

func TestDiskCachedFileDataCloseNeverRemovesReplacement(t *testing.T) {
	for _, alreadyRemoved := range []bool{false, true} {
		for _, withCallback := range []bool{false, true} {
			t.Run(fmt.Sprintf("removed=%t/callback=%t", alreadyRemoved, withCallback), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "owned-cache.txt")
				require.NoError(t, os.WriteFile(path, []byte("owned"), 0600))
				cache := types.NewDiskCachedData(path, "text/plain", 5)
				cache.DiskSize = 5
				var closedSizes []int64
				if withCallback {
					cache.OnClose = func(size int64) { closedSizes = append(closedSizes, size) }
				}
				if alreadyRemoved {
					require.NoError(t, os.Remove(path))
				}
				require.NoError(t, cache.Close())
				_, err := os.Stat(path)
				require.ErrorIs(t, err, os.ErrNotExist)
				require.NoError(t, os.WriteFile(path, []byte("replacement"), 0600))
				require.NoError(t, cache.Close(), "已完成清理的owner不得再次删除同名文件")
				actual, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, "replacement", string(actual))
				if withCallback {
					assert.Equal(t, []int64{5}, closedSizes)
				}
			})
		}
	}
}
