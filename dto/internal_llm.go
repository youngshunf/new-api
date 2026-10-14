package dto

import (
	"errors"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
)

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
	// SupportedEndpointTypes 与 EnableGroups 已去重并按字典序排好：
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
	// SourceRevision 是数据库持久化的单调库存版本，与平台和节点的整数契约一致。
	SourceRevision int64 `json:"source_revision"`
	// SourceDigest 只用于内容判同；不能截断成版本号。
	SourceDigest string `json:"source_digest"`
	// MeasuredAt 是本次快照的读取时刻（RFC3339），只用于新鲜度判定，不进摘要。
	MeasuredAt string `json:"measured_at"`
	LlmModelInventorySnapshot
}

// 库存版本1资源界来自平台后台设计§1.7；只约束完整库存，不改变租约或模型组请求。
const (
	LlmInventoryMaxModels             = 4096
	LlmInventoryMaxModelBytes         = 4096
	LlmInventoryMaxResponseBytes      = 16 * 1024 * 1024
	LlmInventoryMaxEndpointTypes      = 32
	LlmInventoryMaxEnableGroups       = 128
	LlmInventoryMaxVendors            = 256
	LlmInventoryMaxGroupRatios        = 128
	LlmInventoryMaxSupportedEndpoints = 32
)

// ErrLlmInventoryLimits 表示库存不能作为完整成功快照交付；调用方不能截断后继续。
var ErrLlmInventoryLimits = errors.New("inventory_limits_exceeded")

// inventoryEncodedSize 在编码前以原字符串长度作必要下界，随后只编码一个有限DTO片。
// 字符串转义和map键长度仍由common.Marshal精确计算，不自写JSON编码器或估算拒绝合法边界。
func inventoryEncodedSize(value any, remaining int, strings ...string) (int, error) {
	if remaining < 0 {
		return 0, ErrLlmInventoryLimits
	}
	raw := 0
	for _, text := range strings {
		if len(text) > remaining-raw {
			return 0, ErrLlmInventoryLimits
		}
		raw += len(text)
	}
	encoded, err := common.Marshal(value)
	if err != nil {
		return 0, err
	}
	if len(encoded) > remaining {
		return 0, ErrLlmInventoryLimits
	}
	return len(encoded), nil
}

// ValidateResourceLimits 精确核对最终DTO编码大小，所有组件都先证明有限才完整编码。
// 固定DTO没有any或RawMessage；最多四层JSON容器，天然满足版本1深度≤8，文案不解释为JSON。
// 此方法不改输入；集合的复制、去重与排序由库存构建入口负责。
func (inventory *LlmModelInventory) ValidateResourceLimits() error {
	if inventory == nil || len(inventory.Models) > LlmInventoryMaxModels ||
		len(inventory.Vendors) > LlmInventoryMaxVendors || len(inventory.GroupRatio) > LlmInventoryMaxGroupRatios ||
		len(inventory.SupportedEndpoint) > LlmInventoryMaxSupportedEndpoints {
		return ErrLlmInventoryLimits
	}
	// 保留同DTO的nil/empty形态及整数水位，骨架让字段名、null、容器开销由成熟编码器负责。
	skeleton := *inventory
	if inventory.Models != nil {
		skeleton.Models = []LlmModelInventoryEntry{}
	}
	if inventory.Vendors != nil {
		skeleton.Vendors = []LlmInventoryVendor{}
	}
	if inventory.GroupRatio != nil {
		skeleton.GroupRatio = map[string]float64{}
	}
	if inventory.SupportedEndpoint != nil {
		skeleton.SupportedEndpoint = map[string]common.EndpointInfo{}
	}
	total, err := inventoryEncodedSize(skeleton, LlmInventoryMaxResponseBytes, inventory.SourceDigest, inventory.MeasuredAt)
	if err != nil {
		return err
	}
	for index, entry := range inventory.Models {
		if len(entry.ModelName) > 128 || !utf8.ValidString(entry.ModelName) ||
			len(entry.SupportedEndpointTypes) > LlmInventoryMaxEndpointTypes || len(entry.EnableGroups) > LlmInventoryMaxEnableGroups {
			return ErrLlmInventoryLimits
		}
		texts := []string{entry.ModelName, entry.Description, entry.Tags, entry.BillingFacts.BillingMode, entry.BillingFacts.BillingExpr}
		texts = append(texts, entry.SupportedEndpointTypes...)
		texts = append(texts, entry.EnableGroups...)
		size, err := inventoryEncodedSize(entry, LlmInventoryMaxModelBytes, texts...)
		if err != nil {
			return err
		}
		if index > 0 {
			total++
		}
		total += size
		if total > LlmInventoryMaxResponseBytes {
			return ErrLlmInventoryLimits
		}
	}
	for index, vendor := range inventory.Vendors {
		if index > 0 {
			total++
		}
		size, err := inventoryEncodedSize(vendor, LlmInventoryMaxResponseBytes-total, vendor.Name, vendor.Description)
		if err != nil {
			return err
		}
		total += size
	}
	index := 0
	for name, ratio := range inventory.GroupRatio {
		if index > 0 {
			total++
		}
		size, err := inventoryEncodedSize(map[string]float64{name: ratio}, LlmInventoryMaxResponseBytes-total+2, name)
		if err != nil {
			return err
		}
		total += size - 2
		index++
	}
	index = 0
	for name, endpoint := range inventory.SupportedEndpoint {
		if index > 0 {
			total++
		}
		size, err := inventoryEncodedSize(map[string]common.EndpointInfo{name: endpoint}, LlmInventoryMaxResponseBytes-total+2, name, endpoint.Path, endpoint.Method)
		if err != nil {
			return err
		}
		total += size - 2
		index++
	}
	if total > LlmInventoryMaxResponseBytes {
		return ErrLlmInventoryLimits
	}
	return nil
}

// EncodeResourceBounded 仅在精确大小已验证后编码整包，不把LimitedWriter包无限Marshal冒称流式。
func (inventory *LlmModelInventory) EncodeResourceBounded() ([]byte, error) {
	if err := inventory.ValidateResourceLimits(); err != nil {
		return nil, err
	}
	encoded, err := common.Marshal(inventory)
	if err != nil {
		return nil, err
	}
	// 整包此前已证明有界；复核长度防止DTO后续演进遗漏分片计量，不是事后无限编码门。
	if len(encoded) > LlmInventoryMaxResponseBytes {
		return nil, ErrLlmInventoryLimits
	}
	return encoded, nil
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
	// ModelGroup 是这条 lease 所属的模型组（设计 §8.2.2），必填。可调用集合由组映射决定，
	// 发布只改组映射、不逐条改写 lease。
	ModelGroup string `json:"model_group"`
	// ModelLimits 只表达「这把令牌比所在组更窄」：generic 的 Runtime 为空，普通 call 恰好一个模型，
	// 三种独立音频用途必须恰好一个模型（保持单模型同步账务边界）。
	// NewAPI 侧落库时按逗号拼进 tokens.model_limits（同一个列表的物理表示，不是改名）。
	ModelLimits []string `json:"model_limits"`
	// ExpiresTime 是 lease 到期时刻（RFC3339），必填且必须在将来。
	ExpiresTime    string `json:"expires_time"`
	Purpose        string `json:"purpose"`
	AccountingMode string `json:"accounting_mode"`
	// Operation 仅普通call携带；Runtime省略，不接受显式空值。
	Operation string `json:"operation,omitempty"`
}

// LlmRelayLeaseView 是签发/更新的回执。
type LlmRelayLeaseView struct {
	ExternalLeaseId      string   `json:"external_lease_id"`
	NewApiUserId         int      `json:"newapi_user_id"`
	NewApiTokenId        int      `json:"newapi_token_id"`
	CredentialGeneration int64    `json:"credential_generation"`
	ModelGroup           string   `json:"model_group"`
	ModelLimits          []string `json:"model_limits"`
	ExpiresTime          string   `json:"expires_time"`
	Purpose              string   `json:"purpose,omitempty"`
	AccountingMode       string   `json:"accounting_mode,omitempty"`
	Operation            string   `json:"operation,omitempty"`
	// TokenPrefix 是明文 Token 的前缀，供平台存进 `llm.credential_lease.token_prefix` 做人可读定位。
	TokenPrefix string `json:"token_prefix"`
	// RelayToken 是**明文 Relay Token**，可直接作为 Bearer 使用。
	// 它只允许出现在这个响应体里：不进日志、不进错误体、不进平台数据库（设计 §17.2）。
	RelayToken string `json:"relay_token"`
	// Created 本次调用是否新建了 Token；Rotated 本次调用是否换了 Key。
	// 两者都为 false 就是完整同规格的幂等重投。
	Created bool `json:"created"`
	Rotated bool `json:"rotated"`
}

// LlmModelGroupRequest 是 PUT /api/internal/v1/llm/model-groups/{model_group} 的入参。
//
// 组名只在路径里，不在 body 里重复（同 lease 的幂等键）。
type LlmModelGroupRequest struct {
	// PublicationRevision 是平台本次发布世代；组映射按它单调推进，迟到的旧发布被拒。
	PublicationRevision int64 `json:"publication_revision"`
	// Models 是该组可调用的模型，允许为空（全停发布）。
	Models []string `json:"models"`
}

// LlmModelGroupView 是组写入的回执：写后存量，不是请求回声。
type LlmModelGroupView struct {
	ModelGroup          string   `json:"model_group"`
	PublicationRevision int64    `json:"publication_revision"`
	Models              []string `json:"models"`
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

// AudioRequestSettlementResult 是音频账务回执查询的闭集出参。
// 只返回终局判定所需字段，不泄露账户、Token、金额、模型或供应商信息。
type AudioRequestSettlementResult struct {
	ExternalLeaseId          string  `json:"external_lease_id"`
	GatewayRequestId         string  `json:"gateway_request_id"`
	AudioRequestSettlementId *string `json:"audio_request_settlement_id"`
	BillingStatus            string  `json:"billing_status"`
	DispatchState            string  `json:"dispatch_state"`
	Revision                 *int64  `json:"revision"`
	CompletedTime            *string `json:"completed_time"`
}
