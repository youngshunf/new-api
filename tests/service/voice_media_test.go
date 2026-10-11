package servicetests

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVoiceMaterialSingleUseSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	store, err := service.NewVoiceMediaStore(root)
	require.NoError(t, err)
	content, err := os.ReadFile("../model/voice_schema_test.go")
	require.NoError(t, err)
	token, err := store.SaveMaterial(bytes.NewReader(content))
	require.NoError(t, err)
	require.Len(t, token, 64)
	restarted, err := service.NewVoiceMediaStore(root)
	require.NoError(t, err)
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reader, err := restarted.OpenMaterial(token)
			if err != nil {
				assert.ErrorIs(t, err, os.ErrNotExist)
				return
			}
			defer reader.Close()
			actual, readErr := io.ReadAll(reader)
			assert.NoError(t, readErr)
			assert.Equal(t, content, actual)
			mu.Lock()
			successes++
			mu.Unlock()
		}()
	}
	wg.Wait()
	assert.Equal(t, 1, successes, "并发取件也只能成功一次")
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	assert.Empty(t, entries, "消费句柄关闭必须清除元数据与原件")
}

func TestVoiceMediaExpiryBoundsAndSymlink(t *testing.T) {
	root := t.TempDir()
	store, err := service.NewVoiceMediaStore(root)
	require.NoError(t, err)
	_, err = store.SaveMaterial(bytes.NewReader(make([]byte, service.MaximumVoiceMaterialBytes+1)))
	require.Error(t, err, "超限原件不可进入取件存储")
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	assert.Empty(t, entries)
	ref := "01998dd2-0000-7000-8000-000000007701"
	require.NoError(t, store.SavePreview(ref, bytes.NewReader([]byte("RIFF"))))
	assert.True(t, store.HasPreview(ref))
	restarted, err := service.NewVoiceMediaStore(root)
	require.NoError(t, err)
	reader, err := restarted.OpenPreview(ref)
	require.NoError(t, err)
	actual, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	assert.Equal(t, []byte("RIFF"), actual)
	_, err = restarted.SaveMaterial(bytes.NewReader([]byte("reference")))
	require.NoError(t, err)
	require.NoError(t, restarted.Cleanup(time.Now().Add(2*time.Hour)))
	assert.False(t, restarted.HasPreview(ref))
	entries, err = os.ReadDir(root)
	require.NoError(t, err)
	assert.Empty(t, entries, "重启后仍可由持久TTL回收全部文件")
	_, err = restarted.OpenMaterial("../../outside")
	assert.ErrorIs(t, err, os.ErrNotExist)
	assert.Error(t, restarted.SavePreview("../outside", bytes.NewReader([]byte("data"))))
	outside := filepath.Join(t.TempDir(), "private")
	require.NoError(t, os.WriteFile(outside, []byte("never-read"), 0600))
	token, err := restarted.SaveMaterial(bytes.NewReader([]byte("owned")))
	require.NoError(t, err)
	payload := filepath.Join(root, fmt.Sprintf("material-%x", sha256.Sum256([]byte(token))), "payload")
	require.NoError(t, os.Remove(payload))
	require.NoError(t, os.Symlink(outside, payload))
	_, err = restarted.OpenMaterial(token)
	assert.Error(t, err, "文件存储不得跟随恶意符号链接")
}
