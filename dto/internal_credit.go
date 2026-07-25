package dto

// 面向 Cloud 的内部积分履约契约（doc94 §3）。
//
// 所有额度字段都是「十进制积分字符串」，不是内部 quota 微单位，也不用 JSON 浮点数
// （浮点会在跨语言序列化时丢精度）。入参最多 5 位小数，见 common.CreditScale。

// CreditOperationRequest 是一次幂等履约的入参。
type CreditOperationRequest struct {
	OperationType string `json:"operation_type"`
	NewApiUserId  int    `json:"newapi_user_id"`

	// CreditAmount 是发放/回收/订阅周期额度的积分数量。
	CreditAmount string `json:"credit_amount"`

	// ExternalSubscriptionId 是 Cloud 合同号在 NewAPI 侧的投影标识。
	ExternalSubscriptionId string `json:"external_subscription_id"`

	// StartAt/EndAt 是 RFC3339 时间；EndAt 为空表示免费档无商业到期。
	StartAt string  `json:"start_at"`
	EndAt   *string `json:"end_at"`

	// CycleSeconds 恒为 2592000（30 天），含免费档；它定义「多久清零重置一次」。
	CycleSeconds int64 `json:"cycle_seconds"`

	// CycleCount 月付 1、年付 12、免费档 null（无限期循环）。
	CycleCount *int `json:"cycle_count"`

	// WalletOverflow 表示订阅池耗尽后是否允许回落永久钱包，缺省 true。
	WalletOverflow *bool `json:"wallet_overflow"`

	Reason string `json:"reason"`
}

// CreditSubscriptionView 是一条订阅投影的权威快照。
type CreditSubscriptionView struct {
	ExternalSubscriptionId string  `json:"external_subscription_id"`
	Status                 string  `json:"status"`
	CycleLimitCredits      string  `json:"cycle_limit_credits"`
	CycleUsedCredits       string  `json:"cycle_used_credits"`
	CycleRemainingCredits  string  `json:"cycle_remaining_credits"`
	CycleStartAt           string  `json:"cycle_start_at"`
	CycleEndAt             *string `json:"cycle_end_at"`
}

// CreditWalletView 是永久钱包的权威快照。
type CreditWalletView struct {
	RemainingCredits string `json:"remaining_credits"`
}

// CreditAccount 是 NewAPI 权威积分账户快照。
// measured_at 必须原样透传到 Cloud/daemon/WebUI，用于展示新鲜度判定。
type CreditAccount struct {
	NewApiUserId          int                      `json:"newapi_user_id"`
	Wallet                CreditWalletView         `json:"wallet"`
	Subscriptions         []CreditSubscriptionView `json:"subscriptions"`
	TotalAvailableCredits string                   `json:"total_available_credits"`
	MeasuredAt            string                   `json:"measured_at"`
}

// CreditOperationResult 是履约回执。
// Status 只有 succeeded / failed 两态：failed 仅用于终局业务失败；
// 瞬时失败不落库、不回 200，由 HTTP 错误码表达。
type CreditOperationResult struct {
	EventId          string         `json:"event_id"`
	OperationType    string         `json:"operation_type"`
	Status           string         `json:"status"`
	FailureCode      *string        `json:"failure_code"`
	AppliedCredits   *string        `json:"applied_credits"`
	IdempotentReplay bool           `json:"idempotent_replay"`
	CompletedAt      string         `json:"completed_at"`
	Account          *CreditAccount `json:"account,omitempty"`
}

// CreditErrorResponse 是内部 API 的统一错误体。
type CreditErrorResponse struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	TraceId   string `json:"trace_id"`
	Retryable bool   `json:"retryable"`
}
