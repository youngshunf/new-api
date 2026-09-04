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
//
// **三个时间字段各管各的，不要互相代用**（消费方按此渲染「本周期 X → Y」与「Z 重置」）：
//
//   - CycleStartAt  当前这一期的起点（LastResetTime，含「到点未跑」的只读推进）；
//   - NextResetAt   当前这一期的**终点**，也就是额度清零重置的时刻（NextResetTime）；
//   - CycleEndAt    **合同终止时刻**（EndTime），不是周期终点。免费合同无限期循环，此处为 null。
//
// 历史上只有 CycleEndAt，消费方拿它当「周期结束」用，于是免费合同永远拿到 null、
// 付费合同拿到的又是合同末日而非本期末日——两种都渲染不出正确的重置日。
// NextResetAt 就是为补这个缺口而加的，展示「重置日」一律用它。
type CreditSubscriptionView struct {
	ExternalSubscriptionId string  `json:"external_subscription_id"`
	Status                 string  `json:"status"`
	CycleLimitCredits      string  `json:"cycle_limit_credits"`
	CycleUsedCredits       string  `json:"cycle_used_credits"`
	CycleRemainingCredits  string  `json:"cycle_remaining_credits"`
	CycleStartAt           string  `json:"cycle_start_at"`
	// NextResetAt 本期额度清零重置的时刻；不再重置（已到合同末期）时为 null。
	NextResetAt *string `json:"next_reset_at"`
	// CycleEndAt 合同终止时刻，**不是**本周期终点；免费合同无限期循环时为 null。
	CycleEndAt *string `json:"cycle_end_at"`
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

// InternalErrorResponse 是内部服务通道 /api/internal/v1 的统一错误体。
//
// 它是**通道级**的，不属于 credit 这一个 scope：鉴权、限流这类错误发生在任何 scope 的路由上，
// 用 credit 命名会让新增 scope 的人以为要另造一个错误体。
// ⚠️ 只改 Go 类型名，四个 json tag 不动——它们是 wire 契约，云端按名解析。
type InternalErrorResponse struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	TraceId   string `json:"trace_id"`
	Retryable bool   `json:"retryable"`
}
