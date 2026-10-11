package model

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	VoiceStateUnknown   = "unknown"
	VoiceStateDeploying = "deploying"
	VoiceStateReady     = "ready"
	VoiceStateFailed    = "failed"
	VoiceStateDeleted   = "deleted"
	VoiceRetention      = "provider_unused_one_year"
)

var (
	ErrVoiceIdempotencyConflict = errors.New("voice_idempotency_conflict")
	ErrVoiceUnavailable         = errors.New("voice_unavailable")
	ErrVoiceInvalidState        = errors.New("voice_invalid_state")
)

// VoiceCreationSpec 不含状态或供应商ID，调用者不能跳过持久unknown直接造ready。
type VoiceCreationSpec struct {
	UserId         int
	CreatorTokenId int
	ChannelId      int
	IdempotencyKey string
	RequestDigest  string
	Kind           string
	Language       string
	TargetModel    string
}

func voiceKeyDigest(key string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
}

// ValidVoiceRef 只接受规范小写UUIDv7，防止供应商音色名误入网关资源路径。
func ValidVoiceRef(ref string) bool {
	id, err := uuid.Parse(ref)
	return err == nil && id.Version() == 7 && id.Variant() == uuid.RFC4122 && id.String() == ref
}

func validVoiceCreation(spec VoiceCreationSpec) bool {
	if spec.UserId <= 0 || spec.CreatorTokenId <= 0 || spec.ChannelId <= 0 || len(spec.IdempotencyKey) < 1 || len(spec.IdempotencyKey) > 128 {
		return false
	}
	for _, value := range spec.IdempotencyKey {
		if value < 33 || value > 126 {
			return false
		}
	}
	if len(spec.RequestDigest) != 64 || strings.Trim(spec.RequestDigest, "0123456789abcdef") != "" {
		return false
	}
	return (spec.Kind == "clone" || spec.Kind == "design") && (spec.Language == "zh" || spec.Language == "en") &&
		spec.TargetModel != "" && utf8.RuneCountInString(spec.TargetModel) <= 256
}

// BeginVoiceCreation 通过数据库唯一键占用唯一派发资格；事务提交前不能调用供应商。
// replay始终返回原记录，连unknown也不能获得第二次创建资格。
func BeginVoiceCreation(ctx context.Context, spec VoiceCreationSpec) (*VoiceProfile, bool, error) {
	if !validVoiceCreation(spec) {
		return nil, false, errors.New("invalid voice creation metadata")
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, false, err
	}
	now := DatabaseTime{Time: time.Now().UTC()}
	profile := VoiceProfile{VoiceRef: id.String(), CreatedTime: now, UpdatedTime: now, Revision: 1,
		UserId: int64(spec.UserId), CreatorTokenId: int64(spec.CreatorTokenId), ChannelId: int64(spec.ChannelId),
		IdempotencyKeyDigest: voiceKeyDigest(spec.IdempotencyKey), RequestDigest: spec.RequestDigest,
		Kind: spec.Kind, Language: spec.Language, TargetModel: spec.TargetModel, State: VoiceStateUnknown, RetentionPolicy: VoiceRetention}
	result := DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&profile)
	if result.Error != nil {
		return nil, false, result.Error
	}
	created := result.RowsAffected == 1
	stored, err := FindVoiceByIdempotencyKey(ctx, spec.UserId, spec.IdempotencyKey)
	if err != nil {
		return nil, false, err
	}
	if stored.RequestDigest != spec.RequestDigest {
		return nil, false, ErrVoiceIdempotencyConflict
	}
	// MySQL的clientFoundRows也可能把无变化UPDATE报作1；只有自己铸造的主键才有派发资格。
	return stored, created && stored.VoiceRef == profile.VoiceRef, nil
}

func FindVoiceByIdempotencyKey(ctx context.Context, userId int, key string) (*VoiceProfile, error) {
	var profile VoiceProfile
	if err := DB.WithContext(ctx).Where("user_id = ? AND idempotency_key_digest = ?", userId, voiceKeyDigest(key)).First(&profile).Error; err != nil {
		return nil, err
	}
	return &profile, nil
}

func FindVoiceProfile(ctx context.Context, userId int, ref string) (*VoiceProfile, error) {
	if !ValidVoiceRef(ref) {
		return nil, gorm.ErrRecordNotFound
	}
	var profile VoiceProfile
	if err := DB.WithContext(ctx).Where("user_id = ? AND voice_ref = ?", userId, ref).First(&profile).Error; err != nil {
		return nil, err
	}
	return &profile, nil
}

// mutateVoice 锁定归属记录并按revision更新，SQLite沿用共享锁适配，不发送不支持的FOR UPDATE。
func mutateVoice(ctx context.Context, userId int, ref string, update func(*VoiceProfile) error) (*VoiceProfile, error) {
	if !ValidVoiceRef(ref) {
		return nil, gorm.ErrRecordNotFound
	}
	var profile VoiceProfile
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockForUpdate(tx).Where("user_id = ? AND voice_ref = ?", userId, ref).First(&profile).Error; err != nil {
			return err
		}
		revision := profile.Revision
		if err := update(&profile); err != nil {
			return err
		}
		profile.UpdatedTime = DatabaseTime{Time: time.Now().UTC()}
		profile.Revision++
		result := tx.Model(&VoiceProfile{}).Where("user_id = ? AND voice_ref = ? AND revision = ?", userId, ref, revision).Select("*").Updates(&profile)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrVoiceInvalidState
		}
		return nil
	})
	return &profile, err
}

// RecordVoiceProvider 持久真实创建回执中的ID；未查询审核通过前只记deploying。
func RecordVoiceProvider(ctx context.Context, userId int, ref, providerId string) (*VoiceProfile, error) {
	if providerId == "" || utf8.RuneCountInString(providerId) > 256 {
		return nil, ErrVoiceInvalidState
	}
	return mutateVoice(ctx, userId, ref, func(profile *VoiceProfile) error {
		if profile.State != VoiceStateUnknown || profile.ProviderVoiceId != nil {
			return ErrVoiceInvalidState
		}
		profile.ProviderVoiceId, profile.State, profile.FailureReason = &providerId, VoiceStateDeploying, nil
		return nil
	})
}

// RecordVoiceFailure 只记录可证明的终局拒绝；响应不确定时继续unknown。
func RecordVoiceFailure(ctx context.Context, userId int, ref, reason string, definitive bool) (*VoiceProfile, error) {
	if !utf8.ValidString(reason) || utf8.RuneCountInString(reason) > 512 {
		return nil, ErrVoiceInvalidState
	}
	return mutateVoice(ctx, userId, ref, func(profile *VoiceProfile) error {
		if profile.State != VoiceStateUnknown || profile.ProviderVoiceId != nil {
			return ErrVoiceInvalidState
		}
		profile.FailureReason = &reason
		if definitive {
			profile.State = VoiceStateFailed
		}
		return nil
	})
}

// RecordVoiceStatus 只接受官方已知审核状态，不把缺席或未知状态猜成ready/failed。
func RecordVoiceStatus(ctx context.Context, userId int, ref, providerId, targetModel, status string) (*VoiceProfile, error) {
	state, ok := map[string]string{"DEPLOYING": VoiceStateDeploying, "OK": VoiceStateReady, "UNDEPLOYED": VoiceStateFailed}[status]
	if !ok {
		return nil, ErrVoiceInvalidState
	}
	return mutateVoice(ctx, userId, ref, func(profile *VoiceProfile) error {
		if profile.ProviderVoiceId == nil || *profile.ProviderVoiceId != providerId || profile.TargetModel != targetModel || profile.State == VoiceStateDeleted {
			return ErrVoiceInvalidState
		}
		profile.State, profile.FailureReason = state, nil
		if state == VoiceStateFailed {
			reason := "provider_voice_undeployed"
			profile.FailureReason = &reason
		}
		return nil
	})
}

// BeginVoiceDelete 先持久封禁新合成，再占用单次删除资格。供应商ID丢失时只保留pending。
func BeginVoiceDelete(ctx context.Context, userId int, ref string) (*VoiceProfile, bool, error) {
	dispatch := false
	profile, err := mutateVoice(ctx, userId, ref, func(profile *VoiceProfile) error {
		if profile.State == VoiceStateDeleted {
			return nil
		}
		now := DatabaseTime{Time: time.Now().UTC()}
		if profile.DeleteRequestedTime == nil {
			profile.DeleteRequestedTime = &now
		}
		if profile.ProviderVoiceId != nil && profile.DeleteDispatchedTime == nil {
			profile.DeleteDispatchedTime, dispatch = &now, true
		}
		return nil
	})
	return profile, dispatch && err == nil, err
}

// ConfirmVoiceDeletion 仅由成功供应商delete_voice回执调用，超时和404均不能确认删除。
func ConfirmVoiceDeletion(ctx context.Context, userId int, ref string) (*VoiceProfile, error) {
	return mutateVoice(ctx, userId, ref, func(profile *VoiceProfile) error {
		if profile.ProviderVoiceId == nil || profile.DeleteDispatchedTime == nil || profile.DeleteRequestedTime == nil {
			return ErrVoiceInvalidState
		}
		if profile.DeletedTime == nil {
			now := DatabaseTime{Time: time.Now().UTC()}
			profile.DeletedTime = &now
		}
		profile.State, profile.FailureReason = VoiceStateDeleted, nil
		return nil
	})
}

// ValidateVoiceSynthesis 不接受Token归属替代账号归属；删除请求一经持久即封禁合成。
func ValidateVoiceSynthesis(ctx context.Context, userId int, ref, targetModel string) (*VoiceProfile, error) {
	profile, err := FindVoiceProfile(ctx, userId, ref)
	if err != nil {
		return nil, err
	}
	if profile.State != VoiceStateReady || profile.ProviderVoiceId == nil || profile.DeleteRequestedTime != nil || profile.TargetModel != targetModel {
		return nil, ErrVoiceUnavailable
	}
	return profile, nil
}
