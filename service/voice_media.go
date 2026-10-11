package service

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

const (
	MaximumVoiceMaterialBytes = 10 * 1024 * 1024
	MaximumVoicePreviewBytes  = 8 * 1024 * 1024
	maximumVoiceMediaBytes    = 256 * 1024 * 1024
	maximumVoiceMediaEntries  = 1024
	voiceMaterialTTL          = 10 * time.Minute
	voicePreviewTTL           = time.Hour
)

// VoiceMediaStore只保存有界临时文件；持久TTL元数据保证进程重启后仍能追踪和回收。
// 每个部署实例独占目录，取件令牌只返回给供应商，不把令牌明文写盘。
type VoiceMediaStore struct {
	root string
	mu   sync.Mutex
}

type voiceMediaMetadata struct {
	Kind        string `json:"kind"`
	ExpiresTime int64  `json:"expires_time"`
	Ready       bool   `json:"ready"`
	ByteSize    int64  `json:"byte_size"`
}

func NewVoiceMediaStore(root string) (*VoiceMediaStore, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("voice media directory must be a real directory")
	}
	if err = os.Chmod(root, 0700); err != nil {
		return nil, err
	}
	store := &VoiceMediaStore{root: root}
	if err = store.Cleanup(time.Now()); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *VoiceMediaStore) SaveMaterial(source io.Reader) (string, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(random[:])
	name := fmt.Sprintf("material-%x", sha256.Sum256([]byte(token)))
	if err := s.save(name, "material", source, MaximumVoiceMaterialBytes, voiceMaterialTTL); err != nil {
		return "", err
	}
	return token, nil
}

func (s *VoiceMediaStore) SavePreview(voiceRef string, source io.Reader) error {
	if !model.ValidVoiceRef(voiceRef) {
		return errors.New("invalid voice_ref")
	}
	return s.save("preview-"+voiceRef, "preview", source, MaximumVoicePreviewBytes, voicePreviewTTL)
}

func (s *VoiceMediaStore) save(name, kind string, source io.Reader, limit int64, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if source == nil {
		return errors.New("voice media source is missing")
	}
	if err := s.cleanup(time.Now()); err != nil {
		return err
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}
	var total int64
	for _, entry := range entries {
		if !validVoiceMediaName(entry.Name()) || !entry.IsDir() {
			continue
		}
		for _, filename := range []string{"payload", "payload.pending", "consumed"} {
			payload, statErr := os.Lstat(filepath.Join(s.root, entry.Name(), filename))
			if statErr == nil && payload.Mode().IsRegular() {
				total += payload.Size()
			}
		}
	}
	if len(entries) >= maximumVoiceMediaEntries || total > maximumVoiceMediaBytes-limit {
		return errors.New("voice media storage limit reached")
	}
	dir := filepath.Join(s.root, name)
	if err = os.Mkdir(dir, 0700); err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(dir)
		}
	}()
	metadata := voiceMediaMetadata{Kind: kind, ExpiresTime: time.Now().Add(ttl).UnixMilli()}
	if err = writeVoiceMediaMetadata(dir, metadata); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(dir, "payload.pending"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	length, copyErr := io.Copy(file, io.LimitReader(source, limit+1))
	syncErr := file.Sync()
	closeErr := file.Close()
	if err = errors.Join(copyErr, syncErr, closeErr); err != nil {
		return err
	}
	if length == 0 || length > limit {
		return errors.New("voice media size is outside the allowed range")
	}
	if err = os.Rename(filepath.Join(dir, "payload.pending"), filepath.Join(dir, "payload")); err != nil {
		return err
	}
	metadata.Ready, metadata.ByteSize = true, length
	if err = writeVoiceMediaMetadata(dir, metadata); err != nil {
		return err
	}
	complete = true
	return nil
}

func writeVoiceMediaMetadata(dir string, metadata voiceMediaMetadata) error {
	data, err := common.Marshal(metadata)
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(dir, "metadata.pending"), data, 0600); err != nil {
		return err
	}
	return os.Rename(filepath.Join(dir, "metadata.pending"), filepath.Join(dir, "metadata.json"))
}

func validVoiceMediaName(name string) bool {
	if strings.HasPrefix(name, "preview-") {
		return model.ValidVoiceRef(strings.TrimPrefix(name, "preview-"))
	}
	if strings.HasPrefix(name, "material-") {
		digest := strings.TrimPrefix(name, "material-")
		decoded, err := hex.DecodeString(digest)
		return err == nil && len(decoded) == 32 && digest == strings.ToLower(digest)
	}
	return false
}

func readVoiceMediaMetadata(dir string) (voiceMediaMetadata, error) {
	var metadata voiceMediaMetadata
	path := filepath.Join(dir, "metadata.json")
	info, err := os.Lstat(path)
	if err != nil {
		return metadata, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1024 {
		return metadata, errors.New("invalid voice media metadata")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return metadata, err
	}
	err = common.Unmarshal(data, &metadata)
	if err == nil && (metadata.ExpiresTime <= 0 || metadata.ByteSize < 0 || (metadata.Kind != "preview" && metadata.Kind != "material")) {
		err = errors.New("invalid voice media metadata")
	}
	return metadata, err
}

// Cleanup也在服务启动和定时任务中执行；pending、已消费与过期件均由磁盘事实回收。
func (s *VoiceMediaStore) Cleanup(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cleanup(now)
}

func (s *VoiceMediaStore) cleanup(now time.Time) error {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !validVoiceMediaName(entry.Name()) {
			continue
		}
		dir := filepath.Join(s.root, entry.Name())
		if !entry.IsDir() {
			if err = os.Remove(dir); err != nil {
				return err
			}
			continue
		}
		metadata, metaErr := readVoiceMediaMetadata(dir)
		_, consumedErr := os.Lstat(filepath.Join(dir, "consumed"))
		if metaErr != nil || metadata.ExpiresTime <= now.UnixMilli() || consumedErr == nil {
			if err = os.RemoveAll(dir); err != nil {
				return err
			}
		}
	}
	return nil
}

type voiceMaterialReader struct {
	*os.File
	dir string
}

func (r *voiceMaterialReader) Close() error {
	return errors.Join(r.File.Close(), os.RemoveAll(r.dir))
}

func (s *VoiceMediaStore) OpenMaterial(token string) (io.ReadCloser, error) {
	decoded, err := hex.DecodeString(token)
	if err != nil || len(decoded) != 32 || strings.ToLower(token) != token {
		return nil, os.ErrNotExist
	}
	name := fmt.Sprintf("material-%x", sha256.Sum256([]byte(token)))
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Join(s.root, name)
	if _, err = s.openable(dir, "material"); err != nil {
		return nil, err
	}
	// 原子改名在返回句柄前消耗资格；并发进程也不能再次打开原payload。
	if err = os.Rename(filepath.Join(dir, "payload"), filepath.Join(dir, "consumed")); err != nil {
		return nil, os.ErrNotExist
	}
	file, err := os.Open(filepath.Join(dir, "consumed"))
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return &voiceMaterialReader{File: file, dir: dir}, nil
}

func (s *VoiceMediaStore) RemoveMaterial(token string) error {
	decoded, err := hex.DecodeString(token)
	if err != nil || len(decoded) != 32 || token != strings.ToLower(token) {
		return errors.New("invalid material token")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return os.RemoveAll(filepath.Join(s.root, fmt.Sprintf("material-%x", sha256.Sum256([]byte(token)))))
}

func (s *VoiceMediaStore) openable(dir, kind string) (voiceMediaMetadata, error) {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return voiceMediaMetadata{}, os.ErrNotExist
	}
	metadata, err := readVoiceMediaMetadata(dir)
	if err != nil || metadata.Kind != kind || !metadata.Ready || metadata.ExpiresTime <= time.Now().UnixMilli() {
		return metadata, os.ErrNotExist
	}
	info, err = os.Lstat(filepath.Join(dir, "payload"))
	if err != nil || !info.Mode().IsRegular() || info.Size() != metadata.ByteSize {
		return metadata, os.ErrNotExist
	}
	return metadata, nil
}

func (s *VoiceMediaStore) OpenPreview(voiceRef string) (io.ReadCloser, error) {
	if !model.ValidVoiceRef(voiceRef) {
		return nil, os.ErrNotExist
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Join(s.root, "preview-"+voiceRef)
	if _, err := s.openable(dir, "preview"); err != nil {
		return nil, err
	}
	return os.Open(filepath.Join(dir, "payload"))
}

func (s *VoiceMediaStore) HasPreview(voiceRef string) bool {
	file, err := s.OpenPreview(voiceRef)
	if err != nil {
		return false
	}
	_ = file.Close()
	return true
}

func (s *VoiceMediaStore) RemovePreview(voiceRef string) error {
	if !model.ValidVoiceRef(voiceRef) {
		return errors.New("invalid voice_ref")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return os.RemoveAll(filepath.Join(s.root, "preview-"+voiceRef))
}
