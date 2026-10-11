package model

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
)

// Relay lease 在 NewAPI 侧的持久形态（LLM 网关设计 §10）。
//
// # 一条 lease 就是一枚普通 Token 行，不建立第二套凭据表
//
// external_lease_id、model_group、operation及账务模式由正式SQL迁移拥有；
// 凭据代号继续编码进已有的带索引name列：
//
//	name = "hasn-lease:" + external_lease_id + ":g" + credential_generation
//
// external_lease_id 的字符集被 IsValidExternalLeaseId 收窄到 [a-z0-9.-]，于是：
//   - 不含 ':'，代号与 lease id 的分界永不歧义；
//   - 不含 '%' 与 '_'，前缀 LIKE 查询不会撞上 LIKE 通配符，也就不需要 ESCAPE 子句；
//   - 全小写，MySQL 默认排序规则下 LIKE 大小写不敏感这件事无从制造碰撞
//     （两条不同的 lease id 要撞，必须仅靠大小写区分，而大写根本进不来）。
//
// # 这条通道绝不开账户
//
// 账户由 commerce 通道在 workspace 进入 Active 时创建（设计 §15.1）。这里只校验账户存在，
// 不存在就 404——「找不到账户就顺手建一个」会让「账户按什么维度建」有第二个产生方。

const (
	// relayLeaseNamePrefix 是 lease Token 的名字前缀。
	relayLeaseNamePrefix = "hasn-lease:"
	// relayLeaseGenerationSeparator 分隔 lease id 与凭据代号。
	relayLeaseGenerationSeparator = ":g"
	// relayLeaseTokenPrefixLength 是回执里 token_prefix 的长度（含 "sk-"）。
	relayLeaseTokenPrefixLength = 12
	// relayLeaseTokenBearerPrefix 是明文 Token 对外呈现时的前缀，
	// 与 middleware/auth.go 里 strings.TrimPrefix(key, "sk-") 的解析口径一致。
	relayLeaseTokenBearerPrefix = "sk-"
)

// 内部通道的稳定失败码。它们与 credit scope 共用同一张通道级词表：
// invalid_request 在两个 scope 上是同一个意思，不为「这次是 LLM」另造一个同义词。
const (
	RelayLeaseErrorInvalidRequest     = "invalid_request"
	RelayLeaseErrorAccountNotFound    = "account_not_found"
	RelayLeaseErrorAccountMismatch    = "lease_account_mismatch"
	RelayLeaseErrorStaleGeneration    = "stale_credential_generation"
	RelayLeaseErrorStorageUnavailable = "storage_unavailable"
	RelayLeaseErrorConflict           = "lease_conflict"
)

// externalLeaseIdPattern 收窄 lease id 的字符集，理由见文件头注。
// 首字符另外要求是字母或数字，避免 ".." 这类看起来像路径的标识符。
var externalLeaseIdPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,63}$`)

func stringPointer(value string) *string { return &value }

// NormalizeRelayLeaseAccounting 校验并归一租约的用途与记账模式。
//
// 私有契约 llm-relay-lease-internal-v1 要求 generic 租约显式携带 purpose=generic、
// accounting_mode=legacy，不接受空串默认。历史受管音频只接受三种音频用途且必须是synchronous。
func NormalizeRelayLeaseAccounting(purpose, accountingMode string) (string, string, error) {
	if purpose == "generic" && accountingMode == "legacy" {
		return "generic", "legacy", nil
	}
	if accountingMode != "synchronous" {
		return "", "", errors.New("managed audio lease requires synchronous accounting_mode")
	}
	switch purpose {
	case "audio.transcribe", "audio.translate", "audio.tts":
		return purpose, accountingMode, nil
	default:
		return "", "", errors.New("managed audio lease purpose is invalid")
	}
}

// relayLeaseUpsertLock 把同进程内的 lease 签发串行化。
//
// 同进程串行减少控制面争用；跨副本依赖external_lease_id唯一约束及更新旧代/旧Key/用途CAS。
var relayLeaseUpsertLock sync.Mutex

// RelayLeaseError 是 lease 操作的终局失败，携带内部通道要返回的完整语义。
type RelayLeaseError struct {
	HTTPStatus int
	Code       string
	Message    string
	Retryable  bool
}

func (e *RelayLeaseError) Error() string {
	return e.Code + ": " + e.Message
}

func relayLeaseError(status int, code string, retryable bool, format string, args ...any) *RelayLeaseError {
	return &RelayLeaseError{
		HTTPStatus: status,
		Code:       code,
		Message:    fmt.Sprintf(format, args...),
		Retryable:  retryable,
	}
}

// IsValidExternalLeaseId 判定平台传来的幂等键是否落在约定字符集内。
func IsValidExternalLeaseId(externalLeaseId string) bool {
	return externalLeaseIdPattern.MatchString(externalLeaseId)
}

// RelayLeaseTokenName 拼出 lease Token 的名字。
func RelayLeaseTokenName(externalLeaseId string, generation int64) string {
	return relayLeaseNamePrefix + externalLeaseId + relayLeaseGenerationSeparator +
		strconv.FormatInt(generation, 10)
}

// ParseRelayLeaseTokenName 从 Token 名字反解出 lease id 与凭据代号。
//
// 解析失败返回 ok=false：那多半是主人自己建的、恰好长得像 lease 的 Token，
// 不是本通道签发的，绝不能被当成 lease 拿去改写。
func ParseRelayLeaseTokenName(name string) (externalLeaseId string, generation int64, ok bool) {
	if !strings.HasPrefix(name, relayLeaseNamePrefix) {
		return "", 0, false
	}
	body := strings.TrimPrefix(name, relayLeaseNamePrefix)
	separator := strings.LastIndex(body, relayLeaseGenerationSeparator)
	if separator <= 0 {
		return "", 0, false
	}
	externalLeaseId = body[:separator]
	if !IsValidExternalLeaseId(externalLeaseId) {
		return "", 0, false
	}
	parsed, err := strconv.ParseInt(body[separator+len(relayLeaseGenerationSeparator):], 10, 64)
	if err != nil || parsed < 1 {
		return "", 0, false
	}
	return externalLeaseId, parsed, true
}

// FindRelayLeaseToken 按幂等键查在册 lease。
//
// **刻意不按 user_id 过滤**：同一个 external_lease_id 出现在另一个账户下是必须被判出来的冲突
// （lease 换账户等于换扣费主体，设计 §10.1），把 user_id 写进 WHERE 会让它变成静默新建。
func FindRelayLeaseToken(externalLeaseId string) (*Token, int64, error) {
	if !IsValidExternalLeaseId(externalLeaseId) {
		return nil, 0, errors.New("invalid external_lease_id")
	}
	var candidates []*Token
	pattern := relayLeaseNamePrefix + externalLeaseId + relayLeaseGenerationSeparator + "%"
	if err := DB.Unscoped().Where("name LIKE ?", pattern).Order("id asc").Find(&candidates).Error; err != nil {
		return nil, 0, err
	}
	var (
		lease      *Token
		generation int64
		matched    int
	)
	for _, candidate := range candidates {
		parsedId, parsedGeneration, ok := ParseRelayLeaseTokenName(candidate.Name)
		if !ok || parsedId != externalLeaseId {
			continue
		}
		matched++
		if lease == nil {
			lease, generation = candidate, parsedGeneration
		}
	}
	if matched > 1 {
		// 只可能来自跨副本并发签发。取 id 最小的那条当权威，并把异常喊出来——
		// 静默取一条会让多出来的那枚 Token 永远撤销不掉。
		common.SysLog("relay lease " + externalLeaseId + " resolves to " + strconv.Itoa(matched) +
			" live tokens; using the lowest id and leaving the rest for reconciliation")
	}
	return lease, generation, nil
}

// relayLeaseAccountExists 判定账户是否已存在，不创建。
func relayLeaseAccountExists(newApiUserId int) (bool, error) {
	var count int64
	if err := DB.Model(&User{}).Where("id = ?", newApiUserId).Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// RelayLeaseSpec 是一次幂等签发所需的全部输入，已完成校验与归一。
type RelayLeaseSpec struct {
	ExternalLeaseId      string
	NewApiUserId         int
	CredentialGeneration int64
	// ModelGroup 是令牌所属模型组；可调用集合由组映射决定（设计 §8.2.2）。
	ModelGroup string
	// ModelLimits 只表达「比所在组更窄」：Runtime 为空，普通 call 与独立音频恰好一个模型。
	ModelLimits []string
	// ExpiredTime 是 Unix 秒。lease 一律有期限，这里不接受 -1（永不过期）。
	ExpiredTime int64
	// Purpose 与 AccountingMode 是可信租约控制面的受管音频资格；generic/legacy 保持旧租约。
	Purpose        string
	AccountingMode string
	Operation      string
}

// RelayLeaseOutcome 是一次幂等签发的结果。
type RelayLeaseOutcome struct {
	Token      *Token
	Generation int64
	Created    bool
	Rotated    bool
}

// RelayTokenPlaintext 返回可直接当 Bearer 用的明文 Token。
func RelayTokenPlaintext(token *Token) string {
	return relayLeaseTokenBearerPrefix + token.Key
}

// RelayTokenPrefix 返回明文 Token 的前缀，用于平台侧人可读定位。
func RelayTokenPrefix(token *Token) string {
	plaintext := RelayTokenPlaintext(token)
	if len(plaintext) <= relayLeaseTokenPrefixLength {
		return plaintext
	}
	return plaintext[:relayLeaseTokenPrefixLength]
}

// UpsertRelayLease 幂等签发或更新一条 Relay lease。
//
// 幂等语义（幂等键是 external_lease_id）：
//   - 不在册            → 新建，created=true；
//   - 在册且同代        → 仅完整同规格且未撤销时返回同一枚Token；异规格或撤销后重投为409；
//   - 在册且未撤销、代号更大 → 换 Key（rotated=true），Token 行不变，旧明文当场作废；
//   - 在册且已撤销      → 任何代号均为409，重新授权必须使用新的lease身份；
//   - 在册且代号更小    → 409，这是一条过期请求，绝不把已经轮换过的凭据倒回去；
//   - 在册但属于别的账户 → 409，lease 换账户等于换扣费主体。
func UpsertRelayLease(spec RelayLeaseSpec) (*RelayLeaseOutcome, *RelayLeaseError) {
	purpose, accountingMode, accountingErr := NormalizeRelayLeaseAccounting(spec.Purpose, spec.AccountingMode)
	if accountingErr != nil {
		return nil, relayLeaseError(http.StatusBadRequest, RelayLeaseErrorInvalidRequest, false, "%s", accountingErr)
	}
	spec.Purpose, spec.AccountingMode = purpose, accountingMode
	if err := ValidateRelayLeaseOperation(spec.Operation, spec.Purpose, spec.ModelLimits); err != nil {
		return nil, relayLeaseError(http.StatusBadRequest, RelayLeaseErrorInvalidRequest, false, "%s", err)
	}
	relayLeaseUpsertLock.Lock()
	defer relayLeaseUpsertLock.Unlock()

	exists, err := relayLeaseAccountExists(spec.NewApiUserId)
	if err != nil {
		return nil, relayLeaseError(http.StatusServiceUnavailable, RelayLeaseErrorStorageUnavailable, true,
			"failed to look up the target account")
	}
	if !exists {
		return nil, relayLeaseError(http.StatusNotFound, RelayLeaseErrorAccountNotFound, false,
			"newapi_user_id %d does not exist; accounts are provisioned by the account channel, not by this one",
			spec.NewApiUserId)
	}

	existing, existingGeneration, err := FindRelayLeaseToken(spec.ExternalLeaseId)
	if err != nil {
		return nil, relayLeaseError(http.StatusServiceUnavailable, RelayLeaseErrorStorageUnavailable, true,
			"failed to look up the lease")
	}
	modelLimits := strings.Join(spec.ModelLimits, ",")

	if existing == nil {
		key, keyErr := common.GenerateKey()
		if keyErr != nil {
			return nil, relayLeaseError(http.StatusServiceUnavailable, RelayLeaseErrorStorageUnavailable, true,
				"failed to generate a relay token")
		}
		now := common.GetTimestamp()
		lease := &Token{
			UserId:       spec.NewApiUserId,
			Name:         RelayLeaseTokenName(spec.ExternalLeaseId, spec.CredentialGeneration),
			Key:          key,
			Status:       common.TokenStatusEnabled,
			CreatedTime:  now,
			AccessedTime: now,
			ExpiredTime:  spec.ExpiredTime,
			// lease 不建立第二套积分余额：额度从发起空间的账户扣（设计 §10.1）。
			UnlimitedQuota:     true,
			ModelLimitsEnabled: len(spec.ModelLimits) > 0,
			ModelLimits:        modelLimits,
			Purpose:            spec.Purpose,
			AccountingMode:     spec.AccountingMode,
			ExternalLeaseId:    stringPointer(spec.ExternalLeaseId),
			ModelGroup:         spec.ModelGroup,
			Operation:          spec.Operation,
		}
		if insertErr := DB.Table("tokens").Create(map[string]any{
			"user_id": lease.UserId, "name": lease.Name, "key": lease.Key, "status": lease.Status,
			"created_time": lease.CreatedTime, "accessed_time": lease.AccessedTime, "expired_time": lease.ExpiredTime,
			"unlimited_quota": lease.UnlimitedQuota, "model_limits_enabled": lease.ModelLimitsEnabled,
			"model_limits": lease.ModelLimits, "purpose": lease.Purpose, "accounting_mode": lease.AccountingMode,
			"external_lease_id": spec.ExternalLeaseId, "model_group": lease.ModelGroup, "operation": lease.Operation,
		}).Error; insertErr != nil {
			return nil, relayLeaseError(http.StatusServiceUnavailable, RelayLeaseErrorStorageUnavailable, true,
				"failed to persist the lease")
		}
		var stored Token
		if reloadErr := DB.Where("external_lease_id = ?", spec.ExternalLeaseId).First(&stored).Error; reloadErr != nil {
			return nil, relayLeaseError(http.StatusServiceUnavailable, RelayLeaseErrorStorageUnavailable, true, "failed to reload the persisted lease")
		}
		lease = &stored
		return &RelayLeaseOutcome{Token: lease, Generation: spec.CredentialGeneration, Created: true}, nil
	}

	if existing.UserId != spec.NewApiUserId {
		return nil, relayLeaseError(http.StatusConflict, RelayLeaseErrorAccountMismatch, false,
			"lease %s is already issued under another account; revoke it before re-issuing",
			spec.ExternalLeaseId)
	}
	if existing.DeletedAt.Valid {
		return nil, relayLeaseError(http.StatusConflict, RelayLeaseErrorConflict, false,
			"a revoked relay lease cannot be reissued; use a new external_lease_id")
	}
	if spec.CredentialGeneration < existingGeneration {
		return nil, relayLeaseError(http.StatusConflict, RelayLeaseErrorStaleGeneration, false,
			"lease %s is already at credential_generation %d",
			spec.ExternalLeaseId, existingGeneration)
	}
	if existing.Operation != spec.Operation || existing.Purpose != spec.Purpose || existing.AccountingMode != spec.AccountingMode {
		return nil, relayLeaseError(http.StatusConflict, RelayLeaseErrorConflict, false,
			"the purpose, accounting mode and operation of an existing relay lease cannot change")
	}

	if spec.CredentialGeneration == existingGeneration {
		if existing.DeletedAt.Valid || existing.Status != common.TokenStatusEnabled || existing.ModelGroup != spec.ModelGroup ||
			existing.ModelLimits != modelLimits || existing.ModelLimitsEnabled != (len(spec.ModelLimits) > 0) || existing.ExpiredTime != spec.ExpiredTime {
			return nil, relayLeaseError(http.StatusConflict, RelayLeaseErrorConflict, false, "same-generation relay lease must match the active complete specification")
		}
		return &RelayLeaseOutcome{Token: existing, Generation: existingGeneration}, nil
	}

	rotated := spec.CredentialGeneration > existingGeneration
	updated := *existing
	updated.Name = RelayLeaseTokenName(spec.ExternalLeaseId, spec.CredentialGeneration)
	updated.Status = common.TokenStatusEnabled
	updated.ExpiredTime = spec.ExpiredTime
	updated.UnlimitedQuota = true
	updated.ModelLimitsEnabled = len(spec.ModelLimits) > 0
	updated.ModelLimits = modelLimits
	updated.ModelGroup = spec.ModelGroup
	updated.Purpose = spec.Purpose
	updated.AccountingMode = spec.AccountingMode
	updated.ExternalLeaseId = stringPointer(spec.ExternalLeaseId)
	if rotated {
		key, keyErr := common.GenerateKey()
		if keyErr != nil {
			return nil, relayLeaseError(http.StatusServiceUnavailable, RelayLeaseErrorStorageUnavailable, true,
				"failed to generate a relay token")
		}
		updated.Key = key
	}

	// 失效的必须是**旧** Key 的缓存。Token.Update() 用结构体上的 Key 去失效，轮换时那已经是新
	// Key 了——旧 Key 会继续留在 Redis 里可用，也就是撤销不掉。所以这里显式按旧 Key 失效，
	// 再走一次不碰缓存的定点更新。
	if cacheErr := invalidateTokenCacheForMutation(existing.Key); cacheErr != nil {
		common.SysLog("failed to invalidate relay lease token cache: " + cacheErr.Error())
	}
	// 走 Table 而不是 Model(&Token{})：purpose/accounting_mode/external_lease_id/model_group 在结构体上
	// 是只读映射（正式迁移拥有这些列），经 Model 的 map Updates 会被 GORM 静默丢弃。
	result := DB.Unscoped().Table("tokens").Where(map[string]any{
		"id": existing.Id, "name": existing.Name, "key": existing.Key, "operation": existing.Operation,
		"purpose": existing.Purpose, "accounting_mode": existing.AccountingMode, "model_group": existing.ModelGroup,
		"model_limits": existing.ModelLimits, "expired_time": existing.ExpiredTime, "deleted_at": existing.DeletedAt,
		"status": existing.Status, "model_limits_enabled": existing.ModelLimitsEnabled,
	}).Updates(map[string]any{
		"name": updated.Name, "key": updated.Key, "status": updated.Status, "expired_time": updated.ExpiredTime,
		"unlimited_quota": updated.UnlimitedQuota, "model_limits_enabled": updated.ModelLimitsEnabled,
		"model_limits": updated.ModelLimits, "purpose": updated.Purpose, "accounting_mode": updated.AccountingMode,
		"external_lease_id": spec.ExternalLeaseId, "model_group": updated.ModelGroup,
	})
	if updateErr := result.Error; updateErr != nil {
		return nil, relayLeaseError(http.StatusServiceUnavailable, RelayLeaseErrorStorageUnavailable, true,
			"failed to update the lease")
	}
	if result.RowsAffected == 0 {
		// MySQL无变化重投会报0行；现读必须仍与完整目标规格一致，才能作为幂等成功。
		var current Token
		if err := DB.Unscoped().First(&current, existing.Id).Error; err != nil || current.Name != updated.Name || current.Key != updated.Key ||
			current.Operation != updated.Operation || current.Purpose != updated.Purpose || current.AccountingMode != updated.AccountingMode ||
			current.ModelGroup != updated.ModelGroup || current.ModelLimits != updated.ModelLimits || current.ModelLimitsEnabled != updated.ModelLimitsEnabled ||
			current.ExpiredTime != updated.ExpiredTime || current.UserId != updated.UserId || current.Status != updated.Status || current.DeletedAt.Valid {
			return nil, relayLeaseError(http.StatusConflict, RelayLeaseErrorConflict, false, "relay lease changed during update")
		}
	}
	return &RelayLeaseOutcome{Token: &updated, Generation: spec.CredentialGeneration, Rotated: rotated}, nil
}

// RevokeRelayLease 幂等撤销一条 Relay lease。
//
// 软删除保留代号和用途墓碑；重复撤销返回(nil,nil)，任何代号的PUT都不得复活凭据。
func RevokeRelayLease(externalLeaseId string) (*Token, *RelayLeaseError) {
	relayLeaseUpsertLock.Lock()
	defer relayLeaseUpsertLock.Unlock()

	existing, _, err := FindRelayLeaseToken(externalLeaseId)
	if err != nil {
		return nil, relayLeaseError(http.StatusServiceUnavailable, RelayLeaseErrorStorageUnavailable, true,
			"failed to look up the lease")
	}
	if existing == nil || existing.DeletedAt.Valid {
		return nil, nil
	}
	if deleteErr := existing.Delete(); deleteErr != nil && !errors.Is(deleteErr, gorm.ErrRecordNotFound) {
		return nil, relayLeaseError(http.StatusServiceUnavailable, RelayLeaseErrorStorageUnavailable, true,
			"failed to revoke the lease")
	}
	return existing, nil
}
