package service

import (
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
)

// ---------------------------------------------------------------------------
// BillingSession — 统一计费会话
// ---------------------------------------------------------------------------

// BillingSession 封装单次请求的预扣费/结算/退款生命周期。
// 实现 relaycommon.BillingSettler 接口。
type BillingSession struct {
	relayInfo        *relaycommon.RelayInfo
	funding          FundingSource
	preConsumedQuota int  // 实际预扣额度（信任额度旁路时为 0）
	tokenConsumed    int  // 令牌额度实际扣减量
	trusted          bool // 是否命中信任额度旁路
	fundingSettled   bool // funding.Settle 已成功，资金来源已提交
	settled          bool // Settle 全部完成（资金 + 令牌）
	refunded         bool // Refund 已调用
	mu               sync.Mutex
}

func logFundingError(prefix string, err error) {
	if err == nil {
		return
	}
	common.SysLog(prefix + ": " + err.Error())
}

// Settle 根据实际消耗额度进行结算。
// 资金来源和令牌额度分两步提交：若资金来源已提交但令牌调整失败，
// 会标记 fundingSettled 防止 Refund 对已提交的资金来源执行退款。
func (s *BillingSession) Settle(actualQuota int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settled {
		return nil
	}
	delta := actualQuota - s.preConsumedQuota
	if delta == 0 {
		s.settled = true
		return nil
	}
	// 1) 调整资金来源（仅在尚未提交时执行，防止重复调用）
	if !s.fundingSettled {
		if err := s.funding.Settle(delta); err != nil {
			return err
		}
		s.fundingSettled = true
	}
	// 2) 调整令牌额度
	var tokenErr error
	if !s.relayInfo.IsPlayground {
		if delta > 0 {
			tokenErr = model.DecreaseTokenQuota(s.relayInfo.TokenId, s.relayInfo.TokenKey, delta)
		} else {
			tokenErr = model.IncreaseTokenQuota(s.relayInfo.TokenId, s.relayInfo.TokenKey, -delta)
		}
		if tokenErr != nil {
			// 资金来源已提交，令牌调整失败只能记录日志；标记 settled 防止 Refund 误退资金
			common.SysLog(fmt.Sprintf("error adjusting token quota after funding settled (userId=%d, tokenId=%d, delta=%d): %s",
				s.relayInfo.UserId, s.relayInfo.TokenId, delta, tokenErr.Error()))
		}
	}
	// 3) 结算后重新同步日志字段：两个资金池的最终扣减明细必须落进请求日志
	s.syncRelayInfo()
	s.settled = true
	return tokenErr
}

// Refund 退还所有预扣费，幂等安全，异步执行。
func (s *BillingSession) Refund(c *gin.Context) {
	s.mu.Lock()
	if s.settled || s.refunded || !s.needsRefundLocked() {
		s.mu.Unlock()
		return
	}
	s.refunded = true
	s.mu.Unlock()

	subscriptionPart, walletPart := s.funding.Allocation()
	logger.LogInfo(c, fmt.Sprintf("用户 %d 请求失败, 返还预扣费（token_quota=%s, subscription=%s, wallet=%s）",
		s.relayInfo.UserId,
		logger.FormatQuota(s.tokenConsumed),
		logger.FormatQuota(int(subscriptionPart)),
		logger.FormatQuota(int(walletPart)),
	))

	tokenId := s.relayInfo.TokenId
	tokenKey := s.relayInfo.TokenKey
	isPlayground := s.relayInfo.IsPlayground
	tokenConsumed := s.tokenConsumed
	funding := s.funding

	gopool.Go(func() {
		// 1) 退还资金来源（两个池都以 request_id 幂等，可安全重试）
		if err := funding.Refund(); err != nil {
			logFundingError("error refunding billing source", err)
		}
		// 2) 退还令牌额度
		if tokenConsumed > 0 && !isPlayground {
			if err := model.IncreaseTokenQuota(tokenId, tokenKey, tokenConsumed); err != nil {
				logFundingError("error refunding token quota", err)
			}
		}
	})
}

// NeedsRefund 返回是否存在需要退还的预扣状态。
func (s *BillingSession) NeedsRefund() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.needsRefundLocked()
}

func (s *BillingSession) needsRefundLocked() bool {
	if s.settled || s.refunded || s.fundingSettled {
		// fundingSettled 时资金来源已提交结算，不能再退预扣费
		return false
	}
	return s.tokenConsumed > 0 || s.funding.HasReservation()
}

// GetPreConsumedQuota 返回实际预扣的额度。
func (s *BillingSession) GetPreConsumedQuota() int {
	return s.preConsumedQuota
}

func (s *BillingSession) Reserve(targetQuota int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.settled || s.refunded || s.trusted || targetQuota <= s.preConsumedQuota {
		return nil
	}

	delta := targetQuota - s.preConsumedQuota
	if delta <= 0 {
		return nil
	}

	if err := s.funding.Reserve(delta); err != nil {
		return creditFundingError(err)
	}
	if err := s.reserveToken(delta); err != nil {
		s.funding.RollbackReserve(delta)
		return err
	}

	s.preConsumedQuota += delta
	s.tokenConsumed += delta
	s.syncRelayInfo()
	return nil
}

// ---------------------------------------------------------------------------
// PreConsume — 统一预扣费入口（含信任额度旁路）
// ---------------------------------------------------------------------------

// preConsume 执行预扣费：信任检查 -> 令牌预扣 -> 资金来源预扣。
// 任一步骤失败时原子回滚已完成的步骤。
func (s *BillingSession) preConsume(c *gin.Context, quota int) *types.NewAPIError {
	effectiveQuota := quota

	// ---- 信任额度旁路 ----
	if s.shouldTrust(c) {
		s.trusted = true
		effectiveQuota = 0
		logger.LogInfo(c, fmt.Sprintf("用户 %d 额度充足, 信任且不需要预扣费", s.relayInfo.UserId))
	} else if effectiveQuota > 0 {
		logger.LogInfo(c, fmt.Sprintf("用户 %d 需要预扣费 %s", s.relayInfo.UserId, logger.FormatQuota(effectiveQuota)))
	}

	// ---- 1) 预扣令牌额度 ----
	if effectiveQuota > 0 {
		if err := PreConsumeTokenQuota(s.relayInfo, effectiveQuota); err != nil {
			return types.NewErrorWithStatusCode(err, types.ErrorCodePreConsumeTokenQuotaFailed, http.StatusForbidden, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
		}
		s.tokenConsumed = effectiveQuota
	}

	// ---- 2) 预扣资金来源 ----
	if err := s.funding.PreConsume(effectiveQuota); err != nil {
		// 预扣费失败，回滚令牌额度
		if s.tokenConsumed > 0 && !s.relayInfo.IsPlayground {
			if rollbackErr := model.IncreaseTokenQuota(s.relayInfo.TokenId, s.relayInfo.TokenKey, s.tokenConsumed); rollbackErr != nil {
				common.SysLog(fmt.Sprintf("error rolling back token quota (userId=%d, tokenId=%d, amount=%d, fundingErr=%s): %s",
					s.relayInfo.UserId, s.relayInfo.TokenId, s.tokenConsumed, err.Error(), rollbackErr.Error()))
			}
			s.tokenConsumed = 0
		}
		return creditFundingError(err)
	}

	s.preConsumedQuota = effectiveQuota

	// ---- 同步 RelayInfo 兼容字段 ----
	s.syncRelayInfo()

	return nil
}

// creditFundingError 把资金来源错误映射为对外错误。
//
// 额度不足必须稳定返回 HTTP 403 + insufficient_user_quota，并在 metadata 里
// 带上两个资金池的剩余额度与本次所需额度，让 Cloud / daemon / WebUI 能统一
// 映射成付费墙；任何一层都不得吞错后继续请求或返回假结果。
func creditFundingError(err error) *types.NewAPIError {
	var insufficient *model.CombinedQuotaInsufficientError
	if errors.As(err, &insufficient) {
		metadata, marshalErr := common.Marshal(map[string]any{
			"subscription_remaining_credits": common.FormatQuotaAsCredits(insufficient.SubscriptionRemaining),
			"wallet_remaining_credits":       common.FormatQuotaAsCredits(insufficient.WalletRemaining),
			"required_credits":               common.FormatQuotaAsCredits(insufficient.Required),
		})
		if marshalErr != nil {
			metadata = nil
		}
		return types.WithOpenAIError(types.OpenAIError{
			Message: fmt.Sprintf("积分不足：订阅剩余 %s，钱包剩余 %s，本次需要 %s",
				common.FormatQuotaAsCredits(insufficient.SubscriptionRemaining),
				common.FormatQuotaAsCredits(insufficient.WalletRemaining),
				common.FormatQuotaAsCredits(insufficient.Required)),
			Type:     string(types.ErrorTypeNewAPIError),
			Code:     string(types.ErrorCodeInsufficientUserQuota),
			Metadata: metadata,
		}, http.StatusForbidden, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
	}
	if errors.Is(err, model.ErrWalletQuotaInsufficient) {
		return types.NewErrorWithStatusCode(err, types.ErrorCodeInsufficientUserQuota, http.StatusForbidden,
			types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
	}
	return types.NewError(err, types.ErrorCodeUpdateDataError, types.ErrOptionWithSkipRetry())
}

func (s *BillingSession) reserveToken(delta int) error {
	if delta <= 0 || s.relayInfo.IsPlayground {
		return nil
	}
	if err := PreConsumeTokenQuota(s.relayInfo, delta); err != nil {
		return types.NewErrorWithStatusCode(err, types.ErrorCodePreConsumeTokenQuotaFailed, http.StatusForbidden, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
	}
	return nil
}

// shouldTrust 统一信任额度检查。
//
// 只有「纯钱包扣减」才允许信任旁路：一旦涉及订阅池，预扣记录是订阅幂等与
// 分配明细的唯一凭证，跳过预扣会让 preConsumedQuota 与实际扣减不一致。
func (s *BillingSession) shouldTrust(c *gin.Context) bool {
	// 异步任务（ForcePreConsume=true）必须预扣全额，不允许信任旁路
	if s.relayInfo.ForcePreConsume {
		return false
	}
	trustQuota := common.GetTrustQuota()
	if trustQuota <= 0 {
		return false
	}
	// 检查令牌是否充足
	tokenTrusted := s.relayInfo.TokenUnlimited
	if !tokenTrusted {
		tokenQuota := c.GetInt("token_quota")
		tokenTrusted = tokenQuota > trustQuota
	}
	if !tokenTrusted {
		return false
	}
	if funding, ok := s.funding.(*CreditFunding); ok && funding.options.AllowSubscription {
		hasSub, err := model.HasActiveUserSubscription(s.relayInfo.UserId)
		if err != nil || hasSub {
			return false
		}
	}
	return s.relayInfo.UserQuota > trustQuota
}

// syncRelayInfo 将 BillingSession 的状态同步到 RelayInfo 的日志字段上。
func (s *BillingSession) syncRelayInfo() {
	info := s.relayInfo
	info.FinalPreConsumedQuota = s.preConsumedQuota
	info.BillingSource = s.funding.Source()

	subscriptionPart, walletPart := s.funding.Allocation()
	info.FundingSubscriptionPart = subscriptionPart
	info.FundingWalletPart = walletPart

	snapshot := s.funding.SubscriptionSnapshot()
	info.SubscriptionId = snapshot.SubscriptionId
	info.SubscriptionPreConsumed = snapshot.PreConsumed
	info.SubscriptionPostDelta = 0
	info.SubscriptionAmountTotal = snapshot.AmountTotal
	info.SubscriptionAmountUsedAfterPreConsume = snapshot.AmountUsedAfter
	info.SubscriptionPlanId = snapshot.PlanId
	info.SubscriptionPlanTitle = snapshot.PlanTitle
}

// ---------------------------------------------------------------------------
// NewBillingSession 工厂 — 按计费偏好构造组合资金来源
// ---------------------------------------------------------------------------

// NewBillingSession 根据用户计费偏好创建 BillingSession。
//
// 不再「二选一 + 整笔回退」：偏好只决定允许动用哪些池、先动哪个，
// 实际扣减由 model.PreConsumeCombined 在单事务内拆分完成。
func NewBillingSession(c *gin.Context, relayInfo *relaycommon.RelayInfo, preConsumedQuota int) (*BillingSession, *types.NewAPIError) {
	if relayInfo == nil {
		return nil, types.NewError(fmt.Errorf("relayInfo is nil"), types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
	}

	options, apiErr := resolveFundingOptions(relayInfo)
	if apiErr != nil {
		return nil, apiErr
	}

	if options.AllowWallet {
		userQuota, err := model.GetUserQuota(relayInfo.UserId, false)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeQueryDataError, types.ErrOptionWithSkipRetry())
		}
		relayInfo.UserQuota = userQuota
	}

	// 预估额度为 0 时不会走预扣，因此这里补一次只读硬门禁：
	// 两池合计为 0 的用户必须在模型请求发出前被拒，而不是等结算才发现不足。
	if preConsumedQuota <= 0 {
		if apiErr := ensureAnyCreditAvailable(relayInfo.UserId, options); apiErr != nil {
			return nil, apiErr
		}
	}

	session := &BillingSession{
		relayInfo: relayInfo,
		funding: &CreditFunding{
			requestId: relayInfo.RequestId,
			userId:    relayInfo.UserId,
			options:   options,
		},
	}
	if apiErr := session.preConsume(c, preConsumedQuota); apiErr != nil {
		return nil, apiErr
	}
	return session, nil
}

// resolveFundingOptions 把计费偏好翻译成允许动用的资金池组合。
func resolveFundingOptions(relayInfo *relaycommon.RelayInfo) (model.CombinedPreConsumeOptions, *types.NewAPIError) {
	switch common.NormalizeBillingPreference(relayInfo.UserSetting.BillingPreference) {
	case "subscription_only":
		return model.CombinedPreConsumeOptions{AllowSubscription: true}, nil
	case "wallet_only":
		return model.CombinedPreConsumeOptions{AllowWallet: true}, nil
	case "wallet_first":
		return model.CombinedPreConsumeOptions{AllowSubscription: true, AllowWallet: true, PreferWallet: true}, nil
	default:
		options := model.CombinedPreConsumeOptions{AllowSubscription: true, AllowWallet: true}
		// 活跃订阅显式禁止钱包回落时，订阅池耗尽即拒绝，不得偷偷动钱包。
		allowOverflow, err := model.UserActiveSubscriptionsAllowWalletOverflow(relayInfo.UserId)
		if err != nil {
			return options, types.NewError(err, types.ErrorCodeQueryDataError, types.ErrOptionWithSkipRetry())
		}
		if !allowOverflow {
			options.AllowWallet = false
		}
		return options, nil
	}
}

// ensureAnyCreditAvailable 在不产生任何扣减的前提下确认允许动用的池里还有额度。
func ensureAnyCreditAvailable(userId int, options model.CombinedPreConsumeOptions) *types.NewAPIError {
	subscriptionRemaining, walletRemaining, err := model.AvailableCreditQuota(userId)
	if err != nil {
		return types.NewError(err, types.ErrorCodeQueryDataError, types.ErrOptionWithSkipRetry())
	}
	available := int64(0)
	if options.AllowSubscription {
		available += subscriptionRemaining
	}
	if options.AllowWallet {
		available += walletRemaining
	}
	if available > 0 {
		return nil
	}
	return creditFundingError(&model.CombinedQuotaInsufficientError{
		SubscriptionRemaining: subscriptionRemaining,
		WalletRemaining:       walletRemaining,
		Required:              1,
	})
}
