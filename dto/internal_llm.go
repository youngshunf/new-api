package dto

import "github.com/QuantumNous/new-api/common"

// 面向 Cloud 的内部 LLM 控制面契约（LLM 网关设计 §15.1）。
//
// 这一面只做两件事：把 NewAPI 的**完整**模型库存交给平台 Reconciler，以及在**调用方给定的
// 账户**下幂等签发 / 撤销 Relay lease。它不开账户（账户由 commerce 通道按 workspace 建），
// 也不认识 workspace——平台把 workspace 解析成账户之后，只把不透明的账户标识原样转交过来。
//
// # 字段命名跟着平台侧的列名走，不在这一层另起名字
//
// `newapi_user_id` / `newapi_token_id` / `token_prefix` / `credential_generation` /
// `external_lease_id` / `expires_time` 都是 `hasn-platform-core` 的 `llm.credential_lease`
// 已经冻结的列名（设计 §5.3）。同一个概念在链路上必须同名，所以这里逐字沿用，
// **不做任何大小写或缩写转换**。

// LlmModelBillingFacts 是一个模型的计费事实。
//
// 平台侧把它整块存进 `llm.model_inventory.billing_facts`，并据此**算出** `cost_tier`
// （设计 §5.4：档位是算出来的不是标出来的）。因此这里给的是原始倍率与价格，
// 不给已经归档过的结论。
type LlmModelBillingFacts struct {
	// QuotaType 0=按量（model_ratio + completion_ratio），1=按次（model_price）。
	QuotaType            int      `json:"quota_type"`
	ModelRatio           float64  `json:"model_ratio"`
	ModelPrice           float64  `json:"model_price"`
	CompletionRatio      float64  `json:"completion_ratio"`
	CacheRatio           *float64 `json:"cache_ratio,omitempty"`
	CreateCacheRatio     *float64 `json:"create_cache_ratio,omitempty"`
	ImageRatio           *float64 `json:"image_ratio,omitempty"`
	AudioRatio           *float64 `json:"audio_ratio,omitempty"`
	AudioCompletionRatio *float64 `json:"audio_completion_ratio,omitempty"`
	// BillingMode 非空时表示该模型走表达式计费（tiered_expr），BillingExpr 是表达式原文。
	BillingMode string `json:"billing_mode,omitempty"`
	BillingExpr string `json:"billing_expr,omitempty"`
}

// LlmModelInventoryEntry 是一个模型在 NewAPI 侧的库存事实（设计 §5.1）。
type LlmModelInventoryEntry struct {
	ModelName string `json:"model_name"`
	// SupportedEndpointTypes 与 EnableGroups 已按字典序排好：
	// 上游是集合与 map 迭代，顺序天然不稳定，不排序会让同一份库存算出不同的 source_revision。
	SupportedEndpointTypes []string             `json:"supported_endpoint_types"`
	EnableGroups           []string             `json:"enable_groups"`
	VendorId               int                  `json:"vendor_id"`
	Description            string               `json:"description,omitempty"`
	Tags                   string               `json:"tags,omitempty"`
	BillingFacts           LlmModelBillingFacts `json:"billing_facts"`
}

// LlmInventoryVendor 是供应商信息，供平台填 `llm.model_profile.vendor`。
type LlmInventoryVendor struct {
	VendorId    int    `json:"vendor_id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// LlmModelInventorySnapshot 是**参与 source_revision 计算**的那部分内容。
//
// 它与 LlmModelInventory 分开，是因为 measured_at 每次都不同：把时间戳算进摘要，
// 会让「库存没变」也每次得到新 revision，Reconciler 就会每次都判成变更。
type LlmModelInventorySnapshot struct {
	Models  []LlmModelInventoryEntry `json:"models"`
	Vendors []LlmInventoryVendor     `json:"vendors"`
	// GroupRatio 是分组倍率，属于计费事实：同一个模型在不同分组下的实际价格由它决定。
	GroupRatio map[string]float64 `json:"group_ratio"`
	// SupportedEndpoint 是端点类型到实际路径与方法的映射，供平台解析 operation 的 protocol。
	SupportedEndpoint map[string]common.EndpointInfo `json:"supported_endpoint"`
}

// LlmModelInventory 是 GET /api/internal/v1/llm/model-inventory 的出参。
type LlmModelInventory struct {
	// SourceRevision 标识「这次库存快照是哪一版」：对 LlmModelInventorySnapshot 的规范化
	// JSON 取 sha256，十六进制小写。内容一致就一定同值，内容变了就一定变值——
	// Reconciler 用它判定是否需要 upsert，也写进 `llm.model_inventory.source_revision`。
	SourceRevision string `json:"source_revision"`
	// MeasuredAt 是本次快照的读取时刻（RFC3339），只用于新鲜度判定，不进摘要。
	MeasuredAt string `json:"measured_at"`
	LlmModelInventorySnapshot
}

// LlmRelayLeaseRequest 是 PUT /api/internal/v1/llm/relay-leases/{external_lease_id} 的入参。
//
// 幂等键是**路径里的** external_lease_id，不在 body 里重复一遍——body 里再放一份就有了
// 两个产生方，两者不一致时没有任何一侧是权威。
type LlmRelayLeaseRequest struct {
	// NewApiUserId 是平台从计费域拿到的不透明账户标识（`commerce.credit_accounts.external_account_ref`
	// 里装的就是它）。这条通道**只在这个账户下签发，绝不自建账户**（设计 §15.1）。
	NewApiUserId int `json:"newapi_user_id"`
	// CredentialGeneration 是平台侧的凭据代号，单调递增。
	// 它让「轮换」也幂等：同代重投返回同一枚 Token，代号更大才真正换 Key，代号更小是过期请求。
	CredentialGeneration int64 `json:"credential_generation"`
	// ModelLimits 是这条 lease 允许调用的模型白名单，必须非空。
	// **没有「不限制」这一档**：lease 的存在意义就是把权限收敛到当次策略算出的那个集合。
	// NewAPI 侧落库时按逗号拼进 tokens.model_limits（同一个列表的物理表示，不是改名）。
	ModelLimits []string `json:"model_limits"`
	// ExpiresTime 是 lease 到期时刻（RFC3339），必填且必须在将来。
	ExpiresTime string `json:"expires_time"`
}

// LlmRelayLeaseView 是签发/更新的回执。
type LlmRelayLeaseView struct {
	ExternalLeaseId      string   `json:"external_lease_id"`
	NewApiUserId         int      `json:"newapi_user_id"`
	NewApiTokenId        int      `json:"newapi_token_id"`
	CredentialGeneration int64    `json:"credential_generation"`
	ModelLimits          []string `json:"model_limits"`
	ExpiresTime          string   `json:"expires_time"`
	// TokenPrefix 是明文 Token 的前缀，供平台存进 `llm.credential_lease.token_prefix` 做人可读定位。
	TokenPrefix string `json:"token_prefix"`
	// RelayToken 是**明文 Relay Token**，可直接作为 Bearer 使用。
	// 它只允许出现在这个响应体里：不进日志、不进错误体、不进平台数据库（设计 §17.2）。
	RelayToken string `json:"relay_token"`
	// Created 本次调用是否新建了 Token；Rotated 本次调用是否换了 Key。
	// 两者都为 false 就是一次纯幂等重投（或只改了白名单与期限）。
	Created bool `json:"created"`
	Rotated bool `json:"rotated"`
}

// LlmRelayLeaseRevocation 是 DELETE 的回执。
//
// Revoked 表示**本次调用**是否真的撤销了一条在册 lease：重复撤销返回 200 且 revoked=false，
// 不是错误——幂等撤销的定义就是「调用之后它一定不在册」，而不是「每次都必须撤到一条」。
type LlmRelayLeaseRevocation struct {
	ExternalLeaseId string `json:"external_lease_id"`
	Revoked         bool   `json:"revoked"`
	NewApiTokenId   *int   `json:"newapi_token_id"`
}
