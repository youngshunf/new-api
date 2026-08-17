package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"

	"gorm.io/gorm"
)

// 积分履约幂等账本（doc94 N1）。
//
// NewAPI 是积分余额、用量、扣减与清零的唯一权威；Cloud 只发命令、读回执。
// 每个履约请求都在同一个数据库事务内完成「锁用户/订阅 → 校验 → 变更 → 写账本」，
// 同 event_id + 同 payload 返回首次结果，同 event_id + 不同 payload 返回 409。

// 履约操作类型。
const (
	CreditOperationWalletGrant          = "wallet_grant"
	CreditOperationWalletRevoke         = "wallet_revoke"
	CreditOperationSubscriptionActivate = "subscription_activate"
	CreditOperationSubscriptionExpire   = "subscription_expire"
)

// 账本状态。只有这两态会落库：
// succeeded=执行成功；failed=终局业务失败。
// 瞬时失败（存储不可用、超时、限流）一律不落库，从而保证
// 「GET 404 = 这次操作确定没有发生，Cloud 可安全用同 event_id 重投」。
const (
	CreditOperationStatusSucceeded = "succeeded"
	CreditOperationStatusFailed    = "failed"
	// creditOperationStatusProcessing 只在事务内短暂存在，用于借唯一索引串行化
	// 同 event_id 的并发请求；事务回滚后不会留下任何行。
	creditOperationStatusProcessing = "processing"
)

// 终局业务失败码：落库 status=failed，Cloud 收到 200+failed 后直接进 dead letter，不再重试。
const (
	CreditFailureWalletInsufficient        = "wallet_credit_insufficient"
	CreditFailureWalletOverflow            = "wallet_credit_overflow"
	CreditFailureSubscriptionStateConflict = "subscription_state_conflict"
	CreditFailureOperationNotAllowed       = "operation_not_allowed"
)

// 请求级/瞬时错误码：不落库，用 HTTP 状态码表达。
const (
	CreditErrorInvalidCreditAmount  = "invalid_credit_amount"
	CreditErrorInvalidCycle         = "invalid_cycle"
	CreditErrorInvalidRequest       = "invalid_request"
	CreditErrorUserNotFound         = "newapi_user_not_found"
	CreditErrorSubscriptionNotFound = "subscription_not_found"
	CreditErrorIdempotencyConflict  = "idempotency_conflict"
	CreditErrorStoreUnavailable     = "credit_store_unavailable"
	CreditErrorEventNotFound        = "credit_operation_not_found"
)

// CreditOperationError 是内部履约 API 的错误载体，携带 HTTP 语义与可重试标记。
type CreditOperationError struct {
	Code       string
	Message    string
	HTTPStatus int
	Retryable  bool
}

func (e *CreditOperationError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func creditError(status int, code string, retryable bool, format string, args ...any) *CreditOperationError {
	return &CreditOperationError{
		Code:       code,
		Message:    fmt.Sprintf(format, args...),
		HTTPStatus: status,
		Retryable:  retryable,
	}
}

// CreditOperation 是一条履约操作的终局记录。
// 它是幂等账本与审计凭证，不是积分流水，也不保存余额。
type CreditOperation struct {
	Id      int    `json:"id"`
	EventId string `json:"event_id" gorm:"type:varchar(64);uniqueIndex"`

	OperationType string `json:"operation_type" gorm:"type:varchar(32);index"`
	UserId        int    `json:"user_id" gorm:"index"`

	ExternalSubscriptionId string `json:"external_subscription_id" gorm:"type:varchar(128);index"`

	// CreditAmount/AppliedCredits 都是十进制积分字符串。
	// AppliedCredits 是实际入账/回收额，Cloud 以它入审计，不以请求值反推。
	CreditAmount   string `json:"credit_amount" gorm:"type:varchar(48);default:''"`
	AppliedCredits string `json:"applied_credits" gorm:"type:varchar(48);default:''"`

	// PayloadHash 用于同 event_id 的载荷冲突检测。
	PayloadHash string `json:"payload_hash" gorm:"type:varchar(64);default:''"`

	Status      string `json:"status" gorm:"type:varchar(16);index"`
	FailureCode string `json:"failure_code" gorm:"type:varchar(64);default:''"`

	Reason string `json:"reason" gorm:"type:varchar(64);default:''"`

	CompletedAt int64 `json:"completed_at" gorm:"bigint"`
	CreatedAt   int64 `json:"created_at" gorm:"bigint"`
	UpdatedAt   int64 `json:"updated_at" gorm:"bigint;index"`
}

func (o *CreditOperation) BeforeCreate(tx *gorm.DB) error {
	now := common.GetTimestamp()
	o.CreatedAt = now
	o.UpdatedAt = now
	return nil
}

func (o *CreditOperation) BeforeUpdate(tx *gorm.DB) error {
	o.UpdatedAt = common.GetTimestamp()
	return nil
}

// CreditOperationOutcome 是执行器的返回：终局记录 + 是否幂等重放。
type CreditOperationOutcome struct {
	Operation        CreditOperation
	IdempotentReplay bool
}

// normalizedCreditOperation 是通过校验后的规范化入参。
// 时间统一为 UNIX 秒；额度统一为内部 quota 整数。
type normalizedCreditOperation struct {
	operationType          string
	userId                 int
	quotaAmount            int64
	creditAmount           string
	externalSubscriptionId string
	startAt                int64
	endAt                  int64 // 0 = 免费档无商业到期
	cycleSeconds           int64
	cycleCount             int // 0 = 无限期循环（免费档）
	walletOverflow         bool
	reason                 string
}

// CycleSecondsFixed 是固定周期长度：30 天。
// 所有周期都用它计算，不使用自然月、AddDate(month) 或本地夏令时。
const CycleSecondsFixed int64 = 30 * 24 * 60 * 60

const maxCreditCycleCount = 12

// payloadFingerprint 是参与幂等冲突检测的字段集合。
// 故意排除 reason（自由文本，改文案不应把重投判成载荷冲突）。
type payloadFingerprint struct {
	OperationType          string `json:"operation_type"`
	UserId                 int    `json:"user_id"`
	QuotaAmount            int64  `json:"quota_amount"`
	ExternalSubscriptionId string `json:"external_subscription_id"`
	StartAt                int64  `json:"start_at"`
	EndAt                  int64  `json:"end_at"`
	CycleSeconds           int64  `json:"cycle_seconds"`
	CycleCount             int    `json:"cycle_count"`
	WalletOverflow         bool   `json:"wallet_overflow"`
}

func (n *normalizedCreditOperation) payloadHash() (string, error) {
	raw, err := common.Marshal(payloadFingerprint{
		OperationType:          n.operationType,
		UserId:                 n.userId,
		QuotaAmount:            n.quotaAmount,
		ExternalSubscriptionId: n.externalSubscriptionId,
		StartAt:                n.startAt,
		EndAt:                  n.endAt,
		CycleSeconds:           n.cycleSeconds,
		CycleCount:             n.cycleCount,
		WalletOverflow:         n.walletOverflow,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func parseCreditTimestamp(value string, field string) (int64, *CreditOperationError) {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		return 0, creditError(http.StatusBadRequest, CreditErrorInvalidRequest, false, "%s must be an RFC3339 timestamp", field)
	}
	return parsed.Unix(), nil
}

// normalizeCreditOperationRequest 按操作类型校验字段组合。
// 不接受「字段虽合法但语义残缺」的请求：例如 wallet_grant 带订阅周期字段、
// subscription_expire 带 credit_amount、付费合同缺 end_at/cycle_count。
func normalizeCreditOperationRequest(req *dto.CreditOperationRequest) (*normalizedCreditOperation, *CreditOperationError) {
	if req == nil {
		return nil, creditError(http.StatusBadRequest, CreditErrorInvalidRequest, false, "request body is required")
	}
	if req.NewApiUserId <= 0 {
		return nil, creditError(http.StatusBadRequest, CreditErrorInvalidRequest, false, "newapi_user_id must be a positive integer")
	}
	normalized := &normalizedCreditOperation{
		operationType:          strings.TrimSpace(req.OperationType),
		userId:                 req.NewApiUserId,
		externalSubscriptionId: strings.TrimSpace(req.ExternalSubscriptionId),
		walletOverflow:         true,
		reason:                 strings.TrimSpace(req.Reason),
	}
	if req.WalletOverflow != nil {
		normalized.walletOverflow = *req.WalletOverflow
	}
	if len(normalized.reason) > 64 {
		normalized.reason = normalized.reason[:64]
	}
	if len(normalized.externalSubscriptionId) > 128 {
		return nil, creditError(http.StatusBadRequest, CreditErrorInvalidRequest, false, "external_subscription_id exceeds 128 chars")
	}

	hasAmount := strings.TrimSpace(req.CreditAmount) != ""
	if hasAmount {
		quota, err := common.ParseCreditAmountToQuota(req.CreditAmount)
		if err != nil {
			// 精度不整除也走这里：服务端拒绝，绝不静默四舍五入。
			return nil, creditError(http.StatusBadRequest, CreditErrorInvalidCreditAmount, false,
				"credit_amount %q is not representable: it must have at most %d decimals and credit × QuotaPerUnit must be an integer",
				req.CreditAmount, common.CreditScale)
		}
		normalized.quotaAmount = quota
		normalized.creditAmount = strings.TrimSpace(req.CreditAmount)
	}

	switch normalized.operationType {
	case CreditOperationWalletGrant, CreditOperationWalletRevoke:
		if !hasAmount || normalized.quotaAmount <= 0 {
			// credit_amount="0" 格式合法但语义残缺，同样拒绝。
			return nil, creditError(http.StatusBadRequest, CreditErrorInvalidCreditAmount, false, "credit_amount must be greater than 0 for %s", normalized.operationType)
		}
		if normalized.externalSubscriptionId != "" || req.CycleSeconds != 0 || req.CycleCount != nil || strings.TrimSpace(req.StartAt) != "" || req.EndAt != nil {
			return nil, creditError(http.StatusBadRequest, CreditErrorInvalidRequest, false, "%s must not carry subscription cycle fields", normalized.operationType)
		}
	case CreditOperationSubscriptionActivate:
		if !hasAmount || normalized.quotaAmount <= 0 {
			return nil, creditError(http.StatusBadRequest, CreditErrorInvalidCreditAmount, false, "credit_amount must be greater than 0 for subscription_activate")
		}
		if normalized.externalSubscriptionId == "" {
			return nil, creditError(http.StatusBadRequest, CreditErrorInvalidRequest, false, "external_subscription_id is required for subscription_activate")
		}
		if req.CycleSeconds != CycleSecondsFixed {
			return nil, creditError(http.StatusBadRequest, CreditErrorInvalidCycle, false, "cycle_seconds must be %d", CycleSecondsFixed)
		}
		normalized.cycleSeconds = req.CycleSeconds
		startAt, tsErr := parseCreditTimestamp(req.StartAt, "start_at")
		if tsErr != nil {
			return nil, tsErr
		}
		normalized.startAt = startAt

		// 免费合同：end_at 与 cycle_count 同时为 null，但 cycle_seconds 仍必填。
		// 付费合同：两者都必填，且 end_at - start_at 必须恰好等于 cycle_count × cycle_seconds。
		freeContract := req.EndAt == nil && req.CycleCount == nil
		if !freeContract {
			if req.EndAt == nil || req.CycleCount == nil {
				return nil, creditError(http.StatusBadRequest, CreditErrorInvalidCycle, false,
					"paid contracts require both end_at and cycle_count; free contracts require both to be null")
			}
			endAt, tsErr := parseCreditTimestamp(*req.EndAt, "end_at")
			if tsErr != nil {
				return nil, tsErr
			}
			count := *req.CycleCount
			if count < 1 || count > maxCreditCycleCount {
				return nil, creditError(http.StatusBadRequest, CreditErrorInvalidCycle, false, "cycle_count must be between 1 and %d", maxCreditCycleCount)
			}
			if endAt-startAt != int64(count)*normalized.cycleSeconds {
				return nil, creditError(http.StatusBadRequest, CreditErrorInvalidCycle, false,
					"end_at - start_at must equal cycle_count × cycle_seconds (got %d, want %d)", endAt-startAt, int64(count)*normalized.cycleSeconds)
			}
			normalized.endAt = endAt
			normalized.cycleCount = count
		}
	case CreditOperationSubscriptionExpire:
		if normalized.externalSubscriptionId == "" {
			return nil, creditError(http.StatusBadRequest, CreditErrorInvalidRequest, false, "external_subscription_id is required for subscription_expire")
		}
		if hasAmount {
			// 过期的是订阅池而非钱包，携带金额说明调用方语义错了。
			return nil, creditError(http.StatusBadRequest, CreditErrorInvalidRequest, false, "subscription_expire must not carry credit_amount")
		}
	default:
		return nil, creditError(http.StatusBadRequest, CreditErrorInvalidRequest, false, "unsupported operation_type: %q", normalized.operationType)
	}
	return normalized, nil
}

func validateCreditEventId(eventId string) (string, *CreditOperationError) {
	trimmed := strings.TrimSpace(eventId)
	if trimmed == "" || len(trimmed) > 64 {
		return "", creditError(http.StatusBadRequest, CreditErrorInvalidRequest, false, "event_id must be a non-empty string of at most 64 chars")
	}
	return trimmed, nil
}

// GetCreditOperationByEventId 查询终局记录。
// 找不到即返回 CreditErrorEventNotFound（HTTP 404），其含义是
// 「这次操作确定没有发生」，Cloud 可安全用同 event_id 重投。
func GetCreditOperationByEventId(eventId string) (*CreditOperation, *CreditOperationError) {
	trimmed, err := validateCreditEventId(eventId)
	if err != nil {
		return nil, err
	}
	var op CreditOperation
	query := DB.Where("event_id = ? AND status <> ?", trimmed, creditOperationStatusProcessing).Limit(1).Find(&op)
	if query.Error != nil {
		return nil, creditError(http.StatusServiceUnavailable, CreditErrorStoreUnavailable, true, "credit store unavailable: %s", query.Error.Error())
	}
	if query.RowsAffected == 0 {
		return nil, creditError(http.StatusNotFound, CreditErrorEventNotFound, false, "credit operation %s has not been executed", trimmed)
	}
	return &op, nil
}

// terminalCreditFailure 是「业务上确定失败、必须落库」的内部信号。
type terminalCreditFailure struct {
	code    string
	message string
}

func (t *terminalCreditFailure) Error() string { return t.code + ": " + t.message }

func terminalFailure(code string, format string, args ...any) *terminalCreditFailure {
	return &terminalCreditFailure{code: code, message: fmt.Sprintf(format, args...)}
}

// ExecuteCreditOperation 幂等执行一次积分或订阅履约。
//
// 返回值三态与 §2.2 落库规则严格对应：
//   - (outcome{status=succeeded}, nil)：执行成功或幂等命中；
//   - (outcome{status=failed}, nil)：终局业务失败，已落库，Cloud 应进 dead letter；
//   - (nil, err)：请求非法或瞬时失败，未落库，Cloud 可按 HTTP 语义重试或进 dead letter。
func ExecuteCreditOperation(eventId string, req *dto.CreditOperationRequest) (*CreditOperationOutcome, *CreditOperationError) {
	trimmedEventId, idErr := validateCreditEventId(eventId)
	if idErr != nil {
		return nil, idErr
	}
	normalized, normErr := normalizeCreditOperationRequest(req)
	if normErr != nil {
		return nil, normErr
	}
	payloadHash, hashErr := normalized.payloadHash()
	if hashErr != nil {
		return nil, creditError(http.StatusInternalServerError, CreditErrorInvalidRequest, false, "failed to fingerprint payload: %s", hashErr.Error())
	}

	now := GetDBTimestamp()
	var outcome CreditOperationOutcome
	var conflictErr *CreditOperationError

	err := DB.Transaction(func(tx *gorm.DB) error {
		var existing CreditOperation
		found := tx.Where("event_id = ?", trimmedEventId).Limit(1).Find(&existing)
		if found.Error != nil {
			return found.Error
		}
		if found.RowsAffected > 0 {
			if existing.PayloadHash != payloadHash {
				conflictErr = creditError(http.StatusConflict, CreditErrorIdempotencyConflict, false,
					"event_id %s was already used with a different payload", trimmedEventId)
				return nil
			}
			outcome.Operation = existing
			outcome.IdempotentReplay = true
			return nil
		}

		// 先插入 processing 行：唯一索引会把同 event_id 的并发请求串行化，
		// 且任何后续失败都随事务回滚，不留痕迹。
		record := CreditOperation{
			EventId:                trimmedEventId,
			OperationType:          normalized.operationType,
			UserId:                 normalized.userId,
			ExternalSubscriptionId: normalized.externalSubscriptionId,
			CreditAmount:           normalized.creditAmount,
			PayloadHash:            payloadHash,
			Status:                 creditOperationStatusProcessing,
			Reason:                 normalized.reason,
		}
		if err := tx.Create(&record).Error; err != nil {
			// 并发下的重复键：等对方提交后按幂等命中处理。
			var raced CreditOperation
			raced2 := tx.Where("event_id = ?", trimmedEventId).Limit(1).Find(&raced)
			if raced2.Error == nil && raced2.RowsAffected > 0 {
				if raced.PayloadHash != payloadHash {
					conflictErr = creditError(http.StatusConflict, CreditErrorIdempotencyConflict, false,
						"event_id %s was already used with a different payload", trimmedEventId)
					return nil
				}
				outcome.Operation = raced
				outcome.IdempotentReplay = true
				return nil
			}
			return err
		}

		appliedQuota, subscription, applyErr := applyCreditOperationTx(tx, normalized, now)
		if applyErr != nil {
			var terminal *terminalCreditFailure
			if errors.As(applyErr, &terminal) {
				record.Status = CreditOperationStatusFailed
				record.FailureCode = terminal.code
				record.CompletedAt = now
				if err := tx.Save(&record).Error; err != nil {
					return err
				}
				outcome.Operation = record
				return nil
			}
			return applyErr
		}

		record.Status = CreditOperationStatusSucceeded
		record.AppliedCredits = common.FormatQuotaAsCredits(appliedQuota)
		record.CompletedAt = now
		if subscription != nil {
			record.ExternalSubscriptionId = subscription.ExternalSubscriptionId
		}
		if err := tx.Save(&record).Error; err != nil {
			return err
		}
		outcome.Operation = record
		return nil
	})
	if err != nil {
		var notFound *CreditOperationError
		if errors.As(err, &notFound) {
			return nil, notFound
		}
		return nil, creditError(http.StatusServiceUnavailable, CreditErrorStoreUnavailable, true, "credit store unavailable: %s", err.Error())
	}
	if conflictErr != nil {
		return nil, conflictErr
	}

	// 事务提交后再失效用户缓存：钱包余额变更必须让后续读路径看到权威值。
	if outcome.Operation.Status == CreditOperationStatusSucceeded && !outcome.IdempotentReplay {
		switch normalized.operationType {
		case CreditOperationWalletGrant, CreditOperationWalletRevoke:
			if cacheErr := invalidateUserCache(normalized.userId); cacheErr != nil {
				common.SysLog("failed to invalidate user cache after credit operation: " + cacheErr.Error())
			}
		}
	}
	return &outcome, nil
}

// applyCreditOperationTx 在事务内执行实际变更。
// 返回 (实际入账/回收的 quota, 受影响的订阅投影, error)；
// error 为 *terminalCreditFailure 时表示终局业务失败，需落库 status=failed。
func applyCreditOperationTx(tx *gorm.DB, n *normalizedCreditOperation, now int64) (int64, *UserSubscription, error) {
	switch n.operationType {
	case CreditOperationWalletGrant:
		return n.quotaAmount, nil, adjustWalletQuotaTx(tx, n.userId, n.quotaAmount)
	case CreditOperationWalletRevoke:
		return n.quotaAmount, nil, adjustWalletQuotaTx(tx, n.userId, -n.quotaAmount)
	case CreditOperationSubscriptionActivate:
		sub, err := activateExternalSubscriptionTx(tx, n, now)
		if err != nil {
			return 0, nil, err
		}
		return sub.AmountTotal, sub, nil
	case CreditOperationSubscriptionExpire:
		sub, err := expireExternalSubscriptionTx(tx, n.externalSubscriptionId, now)
		if err != nil {
			return 0, nil, err
		}
		// 回收额度 = 过期时刻被清空的订阅剩余额度。
		return sub.expiredRemainingQuota, sub.subscription, nil
	default:
		return 0, nil, terminalFailure(CreditFailureOperationNotAllowed, "unsupported operation_type %q", n.operationType)
	}
}

// adjustWalletQuotaTx 以行锁原子调整永久钱包余额。
// delta > 0 为发放，delta < 0 为回收。余额永不为负，也永不溢出存储上限。
func adjustWalletQuotaTx(tx *gorm.DB, userId int, delta int64) error {
	var user User
	if err := lockForUpdate(tx).Where("id = ?", userId).First(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return creditError(http.StatusNotFound, CreditErrorUserNotFound, false, "newapi user %d not found", userId)
		}
		return err
	}
	current := int64(user.Quota)
	target, overflow := common.WalletQuotaTarget(current, delta)
	if !overflow && target < 0 {
		return terminalFailure(CreditFailureWalletInsufficient,
			"wallet has %s credits, cannot revoke %s",
			common.FormatQuotaAsCredits(current), common.FormatQuotaAsCredits(-delta))
	}
	// users.quota 是 64 位列，int32 不是这里的上限（2026-08-17 实测主账号曾合法
	// 持有 2,500,915,158 quota，误用 common.MaxQuota 曾把该账号一切钱包操作判成
	// 溢出）；这里只防 int64 加法回绕。
	if overflow {
		return terminalFailure(CreditFailureWalletOverflow,
			"wallet grant would overflow int64 storage (current %s, delta %s)",
			common.FormatQuotaAsCredits(current),
			common.FormatQuotaAsCredits(delta))
	}
	return tx.Model(&User{}).Where("id = ?", userId).Update("quota", target).Error
}

// BuildCreditAccount 组装 NewAPI 权威积分账户快照。
// 余额直接读数据库（不读缓存），并对「到点但维护任务尚未跑」的订阅做只读的
// 清零投影，使返回值等于此刻真实可用额度。
func BuildCreditAccount(userId int) (*dto.CreditAccount, *CreditOperationError) {
	if userId <= 0 {
		return nil, creditError(http.StatusBadRequest, CreditErrorInvalidRequest, false, "newapi_user_id must be a positive integer")
	}
	var user User
	if err := DB.Where("id = ?", userId).First(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, creditError(http.StatusNotFound, CreditErrorUserNotFound, false, "newapi user %d not found", userId)
		}
		return nil, creditError(http.StatusServiceUnavailable, CreditErrorStoreUnavailable, true, "credit store unavailable: %s", err.Error())
	}
	now := GetDBTimestamp()
	var subs []UserSubscription
	if err := DB.Where("user_id = ? AND status IN (?)", userId, []string{SubscriptionStatusActive, SubscriptionStatusScheduled}).
		Order("start_time asc, id asc").
		Find(&subs).Error; err != nil {
		return nil, creditError(http.StatusServiceUnavailable, CreditErrorStoreUnavailable, true, "credit store unavailable: %s", err.Error())
	}

	walletQuota := int64(user.Quota)
	totalQuota := walletQuota
	views := make([]dto.CreditSubscriptionView, 0, len(subs))
	for _, sub := range subs {
		used := sub.AmountUsed
		cycleStart := sub.LastResetTime
		if cycleStart <= 0 {
			cycleStart = sub.StartTime
		}
		// 到点未跑的重置：只读投影，不写库。
		if sub.Status == SubscriptionStatusActive && sub.NextResetTime > 0 && sub.NextResetTime <= now {
			used = 0
			cycleStart = sub.NextResetTime
		}
		remaining := int64(0)
		if sub.AmountTotal > 0 {
			remaining = sub.AmountTotal - used
			if remaining < 0 {
				remaining = 0
			}
			if sub.Status == SubscriptionStatusActive && sub.StartTime <= now {
				totalQuota += remaining
			}
		}
		var cycleEnd *string
		if sub.EndTime > 0 {
			formatted := time.Unix(sub.EndTime, 0).UTC().Format(time.RFC3339)
			cycleEnd = &formatted
		}
		views = append(views, dto.CreditSubscriptionView{
			ExternalSubscriptionId: sub.CreditAccountSubscriptionRef(),
			Status:                 sub.Status,
			CycleLimitCredits:      common.FormatQuotaAsCredits(sub.AmountTotal),
			CycleUsedCredits:       common.FormatQuotaAsCredits(used),
			CycleRemainingCredits:  common.FormatQuotaAsCredits(remaining),
			CycleStartAt:           time.Unix(cycleStart, 0).UTC().Format(time.RFC3339),
			CycleEndAt:             cycleEnd,
		})
	}
	return &dto.CreditAccount{
		NewApiUserId:          userId,
		Wallet:                dto.CreditWalletView{RemainingCredits: common.FormatQuotaAsCredits(walletQuota)},
		Subscriptions:         views,
		TotalAvailableCredits: common.FormatQuotaAsCredits(totalQuota),
		MeasuredAt:            time.Unix(now, 0).UTC().Format(time.RFC3339),
	}, nil
}

// ToCreditOperationResult 把账本记录转成对外回执。
func (o *CreditOperation) ToCreditOperationResult(idempotentReplay bool, account *dto.CreditAccount) *dto.CreditOperationResult {
	result := &dto.CreditOperationResult{
		EventId:          o.EventId,
		OperationType:    o.OperationType,
		Status:           o.Status,
		IdempotentReplay: idempotentReplay,
		CompletedAt:      time.Unix(o.CompletedAt, 0).UTC().Format(time.RFC3339),
		Account:          account,
	}
	if o.FailureCode != "" {
		code := o.FailureCode
		result.FailureCode = &code
	}
	if o.AppliedCredits != "" {
		applied := o.AppliedCredits
		result.AppliedCredits = &applied
	}
	return result
}
