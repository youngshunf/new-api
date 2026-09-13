package model

import (
	"errors"
	"math"
	"regexp"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// LlmInventoryState 持久化当前库存内容及其单调版本；摘要不是版本号。
// 单行状态由 LLM 内部控制面独占，不进入可编辑的 Option 配置集合。
type LlmInventoryState struct {
	Name           string `gorm:"primaryKey;size:32"`
	SourceDigest   string `gorm:"size:64;not null"`
	SourceRevision int64  `gorm:"not null"`
}

var inventoryDigestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// ObserveLlmInventory 原子观察完整快照；重复内容不增版，内容变化才推进，重启不归零。
func ObserveLlmInventory(db *gorm.DB, digest string) (int64, error) {
	if !inventoryDigestPattern.MatchString(digest) {
		return 0, errors.New("库存摘要必须为小写 SHA256")
	}
	var revision int64
	err := db.Transaction(func(tx *gorm.DB) error {
		state := LlmInventoryState{Name: "current", SourceDigest: digest, SourceRevision: 1}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&state).Error; err != nil {
			return err
		}
		if err := lockForUpdate(tx).Where("name = ?", "current").First(&state).Error; err != nil {
			return err
		}
		if state.SourceRevision < 1 || state.SourceRevision == math.MaxInt64 {
			return errors.New("库存版本不变量被破坏或已耗尽")
		}
		if state.SourceDigest != digest {
			state.SourceRevision++
			state.SourceDigest = digest
			if err := tx.Model(&state).Updates(map[string]any{
				"source_digest": digest, "source_revision": state.SourceRevision,
			}).Error; err != nil {
				return err
			}
		}
		revision = state.SourceRevision
		return nil
	})
	return revision, err
}
