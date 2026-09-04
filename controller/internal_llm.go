package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/ratio_setting"

	"github.com/gin-gonic/gin"
)

// Cloud → NewAPI 的内部 LLM 控制面端点（LLM 网关设计 §15.1）。
//
// 它只做两件事：交出**完整**模型库存，以及在**调用方给定的账户**下幂等签发/撤销 Relay lease。
// 这一面没有 principals 接口：账户由 commerce 通道按 workspace 创建，
// 在 LLM 链路上补建账户会让「账户按什么维度建」有第二个产生方（设计 §15.1）。

const (
	// maxInternalLlmRequestBody 是 lease 入参的大小上限。
	// 白名单可能装下整份已发布模型集合，所以比 credit 那条宽，但仍然有界。
	maxInternalLlmRequestBody = 64 * 1024
	// maxRelayLeaseModelLimits 是单条 lease 的模型白名单条目上限。
	maxRelayLeaseModelLimits = 1000
	// maxRelayLeaseModelNameLength 是单个模型名的长度上限。
	maxRelayLeaseModelNameLength = 128
)

// relayLeaseRequestAllowedFields 是 LlmRelayLeaseRequest 的字段白名单（additionalProperties: false）。
//
// 多余字段一律 400，不静默忽略：将来平台若发来一个本版本还不认识的控制字段
// （比如另一种轮换指令），静默忽略等于回一个「已经照办」的 200，而实际什么都没发生。
var relayLeaseRequestAllowedFields = map[string]struct{}{
	"newapi_user_id":        {},
	"credential_generation": {},
	"model_limits":          {},
	"expires_time":          {},
}

// GetInternalModelInventory 返回完整模型库存与源 revision。
//
// GET /api/internal/v1/llm/model-inventory
//
// **不做 usable-group 过滤。** /api/pricing 那条按当前登录用户的可用分组裁剪，
// 因为它服务的是「这个人能看到什么」；本接口服务的是「这台 NewAPI 上有什么」，
// 平台随后才会按账户分组、已发布状态与 Runtime 能力去求交（设计 §5.2）。
func GetInternalModelInventory(c *gin.Context) {
	// 复用 /api/pricing 的既有聚合：库存事实只有一个产生方，不在这里另起一套抓取。
	pricing := model.GetPricing()
	if len(pricing) == 0 {
		// 空库存不是一份合法快照。平台的 Reconciler 规则是「一次完整同步成功才允许 upsert
		// 和标记 missing」（设计 §5.1）——把空数组当成 200 交出去，会让它把全部模型标成
		// missing。上游 updatePricing 在数据库出错时正是静默留下空缓存，所以这里必须
		// fail closed。
		middleware.AbortWithInternalServiceError(c, http.StatusServiceUnavailable,
			"inventory_unavailable", "model inventory is empty; refusing to publish a snapshot that would look like a full retirement", true)
		return
	}
	inventory, err := buildModelInventory(pricing, model.GetVendors(), model.GetSupportedEndpointMap(),
		ratio_setting.GetGroupRatioCopy(), time.Now())
	if err != nil {
		logger.LogError(c, "failed to compute model inventory revision: "+err.Error())
		middleware.AbortWithInternalServiceError(c, http.StatusInternalServerError,
			"inventory_digest_failed", "failed to compute the inventory source_revision", false)
		return
	}
	c.JSON(http.StatusOK, inventory)
}

// buildModelInventory 把 /api/pricing 的聚合结果整理成库存快照，并算出 source_revision。
//
// 排序不是排版：上游的分组来自集合、端点映射来自 map，迭代顺序天然不稳定。
// 不排序的话同一份库存每次都会算出不同的摘要，Reconciler 就会把每次轮询都当成一次变更。
func buildModelInventory(pricing []model.Pricing, vendors []model.PricingVendor,
	endpoints map[string]common.EndpointInfo, groupRatio map[string]float64,
	measuredAt time.Time) (*dto.LlmModelInventory, error) {

	entries := make([]dto.LlmModelInventoryEntry, 0, len(pricing))
	for _, item := range pricing {
		// 全部切片都先复制再排序：pricing 是 model 包的共享缓存，就地排序会改到别人的数据。
		enableGroups := append([]string(nil), item.EnableGroup...)
		sort.Strings(enableGroups)
		endpointTypes := make([]string, 0, len(item.SupportedEndpointTypes))
		for _, endpointType := range item.SupportedEndpointTypes {
			endpointTypes = append(endpointTypes, string(endpointType))
		}
		sort.Strings(endpointTypes)

		entries = append(entries, dto.LlmModelInventoryEntry{
			ModelName:              item.ModelName,
			SupportedEndpointTypes: endpointTypes,
			EnableGroups:           enableGroups,
			VendorId:               item.VendorID,
			Description:            item.Description,
			Tags:                   item.Tags,
			BillingFacts: dto.LlmModelBillingFacts{
				QuotaType:            item.QuotaType,
				ModelRatio:           item.ModelRatio,
				ModelPrice:           item.ModelPrice,
				CompletionRatio:      item.CompletionRatio,
				CacheRatio:           item.CacheRatio,
				CreateCacheRatio:     item.CreateCacheRatio,
				ImageRatio:           item.ImageRatio,
				AudioRatio:           item.AudioRatio,
				AudioCompletionRatio: item.AudioCompletionRatio,
				BillingMode:          item.BillingMode,
				BillingExpr:          item.BillingExpr,
			},
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ModelName < entries[j].ModelName })

	inventoryVendors := make([]dto.LlmInventoryVendor, 0, len(vendors))
	for _, vendor := range vendors {
		inventoryVendors = append(inventoryVendors, dto.LlmInventoryVendor{
			VendorId:    vendor.ID,
			Name:        vendor.Name,
			Description: vendor.Description,
		})
	}
	sort.Slice(inventoryVendors, func(i, j int) bool { return inventoryVendors[i].VendorId < inventoryVendors[j].VendorId })

	// 端点映射同样是 model 包的共享 map，复制一份再交出去。
	supportedEndpoint := make(map[string]common.EndpointInfo, len(endpoints))
	for name, info := range endpoints {
		supportedEndpoint[name] = info
	}
	if groupRatio == nil {
		groupRatio = map[string]float64{}
	}

	snapshot := dto.LlmModelInventorySnapshot{
		Models:            entries,
		Vendors:           inventoryVendors,
		GroupRatio:        groupRatio,
		SupportedEndpoint: supportedEndpoint,
	}
	revision, err := modelInventoryRevision(snapshot)
	if err != nil {
		return nil, err
	}
	return &dto.LlmModelInventory{
		SourceRevision:            revision,
		MeasuredAt:                measuredAt.UTC().Format(time.RFC3339),
		LlmModelInventorySnapshot: snapshot,
	}, nil
}

// modelInventoryRevision 对快照取 sha256。
//
// common.Marshal 走 encoding/json，map 键按字典序输出，结构体字段按声明序输出，
// 而所有切片在上面已经排过序——因此同一份库存一定得到同一个摘要。
func modelInventoryRevision(snapshot dto.LlmModelInventorySnapshot) (string, error) {
	payload, err := common.Marshal(snapshot)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

// PutInternalRelayLease 幂等签发或更新一条 Relay lease。
//
// PUT /api/internal/v1/llm/relay-leases/{external_lease_id}
func PutInternalRelayLease(c *gin.Context) {
	externalLeaseId := c.Param("external_lease_id")
	if !model.IsValidExternalLeaseId(externalLeaseId) {
		middleware.AbortWithInternalServiceError(c, http.StatusBadRequest, model.RelayLeaseErrorInvalidRequest,
			"external_lease_id must match ^[a-z0-9][a-z0-9.-]{0,63}$", false)
		return
	}
	spec, err := decodeRelayLeaseRequest(c, externalLeaseId)
	if err != nil {
		middleware.AbortWithInternalServiceError(c, err.HTTPStatus, err.Code, err.Message, err.Retryable)
		return
	}
	outcome, leaseErr := model.UpsertRelayLease(*spec)
	if leaseErr != nil {
		logger.LogWarn(c, fmt.Sprintf("relay lease rejected external_lease_id=%s code=%s", externalLeaseId, leaseErr.Code))
		middleware.AbortWithInternalServiceError(c, leaseErr.HTTPStatus, leaseErr.Code, leaseErr.Message, leaseErr.Retryable)
		return
	}
	// 审计只落身份与结论，不落明文（设计 §17.2）。
	logger.LogInfo(c, fmt.Sprintf("relay lease issued external_lease_id=%s user=%d generation=%d models=%d created=%t rotated=%t",
		externalLeaseId, spec.NewApiUserId, outcome.Generation, len(spec.ModelLimits), outcome.Created, outcome.Rotated))

	// 明文只允许存在于这一个响应体里，不能被任何中间层缓存下来（设计 §10.2）。
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, dto.LlmRelayLeaseView{
		ExternalLeaseId:      externalLeaseId,
		NewApiUserId:         outcome.Token.UserId,
		NewApiTokenId:        outcome.Token.Id,
		CredentialGeneration: outcome.Generation,
		ModelLimits:          outcome.Token.GetModelLimits(),
		ExpiresTime:          time.Unix(outcome.Token.ExpiredTime, 0).UTC().Format(time.RFC3339),
		TokenPrefix:          model.RelayTokenPrefix(outcome.Token),
		RelayToken:           model.RelayTokenPlaintext(outcome.Token),
		Created:              outcome.Created,
		Rotated:              outcome.Rotated,
	})
}

// DeleteInternalRelayLease 幂等撤销一条 Relay lease。
//
// DELETE /api/internal/v1/llm/relay-leases/{external_lease_id}
func DeleteInternalRelayLease(c *gin.Context) {
	externalLeaseId := c.Param("external_lease_id")
	if !model.IsValidExternalLeaseId(externalLeaseId) {
		middleware.AbortWithInternalServiceError(c, http.StatusBadRequest, model.RelayLeaseErrorInvalidRequest,
			"external_lease_id must match ^[a-z0-9][a-z0-9.-]{0,63}$", false)
		return
	}
	revoked, leaseErr := model.RevokeRelayLease(externalLeaseId)
	if leaseErr != nil {
		logger.LogWarn(c, fmt.Sprintf("relay lease revocation rejected external_lease_id=%s code=%s", externalLeaseId, leaseErr.Code))
		middleware.AbortWithInternalServiceError(c, leaseErr.HTTPStatus, leaseErr.Code, leaseErr.Message, leaseErr.Retryable)
		return
	}
	result := dto.LlmRelayLeaseRevocation{ExternalLeaseId: externalLeaseId}
	if revoked != nil {
		result.Revoked = true
		tokenId := revoked.Id
		result.NewApiTokenId = &tokenId
	}
	logger.LogInfo(c, fmt.Sprintf("relay lease revocation external_lease_id=%s revoked=%t", externalLeaseId, result.Revoked))
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, result)
}

// decodeRelayLeaseRequest 解析并校验 lease 入参，产出已归一的 RelayLeaseSpec。
func decodeRelayLeaseRequest(c *gin.Context, externalLeaseId string) (*model.RelayLeaseSpec, *model.RelayLeaseError) {
	invalid := func(format string, args ...any) *model.RelayLeaseError {
		return &model.RelayLeaseError{
			HTTPStatus: http.StatusBadRequest,
			Code:       model.RelayLeaseErrorInvalidRequest,
			Message:    fmt.Sprintf(format, args...),
		}
	}
	body, readErr := io.ReadAll(io.LimitReader(c.Request.Body, maxInternalLlmRequestBody+1))
	if readErr != nil {
		return nil, invalid("failed to read request body")
	}
	if len(body) > maxInternalLlmRequestBody {
		return nil, invalid("request body exceeds %d bytes", maxInternalLlmRequestBody)
	}
	var probe map[string]any
	if err := common.Unmarshal(body, &probe); err != nil {
		return nil, invalid("request body must be a JSON object")
	}
	unknown := make([]string, 0, 2)
	for key := range probe {
		if _, ok := relayLeaseRequestAllowedFields[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, invalid("unknown field(s): %s", strings.Join(unknown, ", "))
	}
	var request dto.LlmRelayLeaseRequest
	if err := common.Unmarshal(body, &request); err != nil {
		return nil, invalid("request body does not match the relay lease schema")
	}

	if request.NewApiUserId <= 0 {
		return nil, invalid("newapi_user_id must be a positive integer")
	}
	if request.CredentialGeneration < 1 {
		return nil, invalid("credential_generation must be a positive integer")
	}
	modelLimits, limitsErr := normalizeRelayLeaseModelLimits(request.ModelLimits)
	if limitsErr != nil {
		return nil, invalid("%s", limitsErr.Error())
	}
	expiresTime, parseErr := time.Parse(time.RFC3339, strings.TrimSpace(request.ExpiresTime))
	if parseErr != nil {
		return nil, invalid("expires_time must be an RFC3339 timestamp")
	}
	// lease 一律有期限。永不过期的中继凭据不是本设计里的一档，缺省成它等于把撤销
	// 变成唯一的收回手段。
	if !expiresTime.After(time.Now()) {
		return nil, invalid("expires_time must be in the future")
	}

	return &model.RelayLeaseSpec{
		ExternalLeaseId:      externalLeaseId,
		NewApiUserId:         request.NewApiUserId,
		CredentialGeneration: request.CredentialGeneration,
		ModelLimits:          modelLimits,
		ExpiredTime:          expiresTime.Unix(),
	}, nil
}

// normalizeRelayLeaseModelLimits 校验模型白名单。
//
// 逗号是硬约束：NewAPI 把白名单按逗号拼进 tokens.model_limits，名字里带逗号会在读回来时
// 裂成两个模型名——那意味着 lease 实际放行的集合与平台以为的不是同一个。
func normalizeRelayLeaseModelLimits(models []string) ([]string, error) {
	if len(models) == 0 {
		return nil, fmt.Errorf("model_limits must contain at least one model; an unrestricted relay lease is not a supported shape")
	}
	if len(models) > maxRelayLeaseModelLimits {
		return nil, fmt.Errorf("model_limits exceeds %d entries", maxRelayLeaseModelLimits)
	}
	seen := make(map[string]struct{}, len(models))
	normalized := make([]string, 0, len(models))
	for _, name := range models {
		if name == "" || strings.TrimSpace(name) != name {
			return nil, fmt.Errorf("model_limits entries must be non-empty and free of surrounding whitespace")
		}
		if len(name) > maxRelayLeaseModelNameLength {
			return nil, fmt.Errorf("model_limits entry exceeds %d chars", maxRelayLeaseModelNameLength)
		}
		if strings.Contains(name, ",") {
			return nil, fmt.Errorf("model_limits entries must not contain a comma")
		}
		if _, duplicated := seen[name]; duplicated {
			return nil, fmt.Errorf("model_limits contains duplicate entry %q", name)
		}
		seen[name] = struct{}{}
		normalized = append(normalized, name)
	}
	return normalized, nil
}
