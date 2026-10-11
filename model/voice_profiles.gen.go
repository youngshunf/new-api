// 本文件由tools/modelgen读取真实数据库生成，禁止手写新表字段。

package model

const TableNameVoiceProfile = "voice_profiles"

// VoiceProfile 由正式SQL建表后的真实列生成，分类为platform_mutable_entity。
type VoiceProfile struct {
	VoiceRef             string        `gorm:"column:voice_ref;primaryKey" json:"voice_ref"`
	CreatedTime          DatabaseTime  `gorm:"column:created_time;not null" json:"created_time"`
	UpdatedTime          DatabaseTime  `gorm:"column:updated_time;not null" json:"updated_time"`
	Revision             int64         `gorm:"column:revision;not null" json:"revision"`
	UserId               int64         `gorm:"column:user_id;not null" json:"user_id"`
	CreatorTokenId       int64         `gorm:"column:creator_token_id;not null" json:"creator_token_id"`
	IdempotencyKeyDigest string        `gorm:"column:idempotency_key_digest;not null" json:"idempotency_key_digest"`
	RequestDigest        string        `gorm:"column:request_digest;not null" json:"request_digest"`
	Kind                 string        `gorm:"column:kind;not null" json:"kind"`
	Language             string        `gorm:"column:language;not null" json:"language"`
	TargetModel          string        `gorm:"column:target_model;not null" json:"target_model"`
	ChannelId            int64         `gorm:"column:channel_id;not null" json:"channel_id"`
	ProviderVoiceId      *string       `gorm:"column:provider_voice_id" json:"provider_voice_id"`
	State                string        `gorm:"column:state;not null" json:"state"`
	FailureReason        *string       `gorm:"column:failure_reason" json:"failure_reason"`
	RetentionPolicy      string        `gorm:"column:retention_policy;not null" json:"retention_policy"`
	DeleteRequestedTime  *DatabaseTime `gorm:"column:delete_requested_time" json:"delete_requested_time"`
	DeleteDispatchedTime *DatabaseTime `gorm:"column:delete_dispatched_time" json:"delete_dispatched_time"`
	DeletedTime          *DatabaseTime `gorm:"column:deleted_time" json:"deleted_time"`
}

func (*VoiceProfile) TableName() string {
	return TableNameVoiceProfile
}
