// 本文件由tools/modelgen读取真实数据库生成，禁止手写新表字段。

package model

const TableNameAudioRequestSettlement = "audio_request_settlements"

// AudioRequestSettlement 由正式SQL建表后的真实列生成，分类为platform_mutable_entity。
type AudioRequestSettlement struct {
	AudioRequestSettlementId string        `gorm:"column:audio_request_settlement_id;primaryKey" json:"audio_request_settlement_id"`
	CreatedTime              DatabaseTime  `gorm:"column:created_time;not null" json:"created_time"`
	UpdatedTime              DatabaseTime  `gorm:"column:updated_time;not null" json:"updated_time"`
	Revision                 int64         `gorm:"column:revision;not null" json:"revision"`
	CompletedTime            *DatabaseTime `gorm:"column:completed_time" json:"completed_time"`
	ExternalLeaseId          string        `gorm:"column:external_lease_id;not null" json:"external_lease_id"`
	GatewayRequestId         string        `gorm:"column:gateway_request_id;not null" json:"gateway_request_id"`
	TokenId                  int64         `gorm:"column:token_id;not null" json:"token_id"`
	UserId                   int64         `gorm:"column:user_id;not null" json:"user_id"`
	CredentialGeneration     int64         `gorm:"column:credential_generation;not null" json:"credential_generation"`
	RelayMode                int64         `gorm:"column:relay_mode;not null" json:"relay_mode"`
	ModelName                string        `gorm:"column:model_name;not null" json:"model_name"`
	PreConsumedQuota         int64         `gorm:"column:pre_consumed_quota;not null" json:"pre_consumed_quota"`
	FundingPreference        string        `gorm:"column:funding_preference;not null" json:"funding_preference"`
	SubscriptionAppliedQuota int64         `gorm:"column:subscription_applied_quota;not null" json:"subscription_applied_quota"`
	WalletAppliedQuota       int64         `gorm:"column:wallet_applied_quota;not null" json:"wallet_applied_quota"`
	TokenAppliedQuota        int64         `gorm:"column:token_applied_quota;not null" json:"token_applied_quota"`
	SubscriptionId           *int64        `gorm:"column:subscription_id" json:"subscription_id"`
	SubscriptionPeriodAnchor *int64        `gorm:"column:subscription_period_anchor" json:"subscription_period_anchor"`
	BillingStatus            string        `gorm:"column:billing_status;not null" json:"billing_status"`
	TerminalAction           *string       `gorm:"column:terminal_action" json:"terminal_action"`
	TargetQuota              *int64        `gorm:"column:target_quota" json:"target_quota"`
	DispatchState            string        `gorm:"column:dispatch_state;not null" json:"dispatch_state"`
	FailureCode              *string       `gorm:"column:failure_code" json:"failure_code"`
}

func (*AudioRequestSettlement) TableName() string {
	return TableNameAudioRequestSettlement
}
