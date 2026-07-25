package service

import (
	"time"

	"github.com/QuantumNous/new-api/model"
)

// ---------------------------------------------------------------------------
// FundingSource — 资金来源（订阅池 + 永久钱包的组合）
// ---------------------------------------------------------------------------

// FundingSource 抽象一次请求的预扣、结算与退还。
//
// 唯一实现是 CreditFunding：它同时持有订阅池与永久钱包两条分配，
// 因为同一个请求必须允许拆分扣减——不能因为订阅池不足以覆盖整次预扣，
// 就把整次请求改扣钱包而遗留零碎订阅额度。
type FundingSource interface {
	// Source 返回资金来源标识：wallet / subscription / composite
	Source() string
	// PreConsume 预扣 amount 额度
	PreConsume(amount int) error
	// Settle 根据差额调整（正数补扣，负数退还）
	Settle(delta int) error
	// Refund 退还全部预扣
	Refund() error
	// Reserve 在请求发出后追加预扣（流式输出超出预估时使用）
	Reserve(delta int) error
	// RollbackReserve 回滚一次失败的 Reserve
	RollbackReserve(delta int)
	// HasReservation 表示当前是否仍有需要退还的预扣
	HasReservation() bool
	// Allocation 返回两个资金池上的实际扣减明细
	Allocation() (subscriptionPart int64, walletPart int64)
	// SubscriptionSnapshot 返回订阅池状态，供请求日志展示
	SubscriptionSnapshot() SubscriptionFundingSnapshot
}

// SubscriptionFundingSnapshot 是订阅池在本次请求中的状态快照。
type SubscriptionFundingSnapshot struct {
	SubscriptionId  int
	PreConsumed     int64
	AmountTotal     int64
	AmountUsedAfter int64
	PlanId          int
	PlanTitle       string
}

// CreditFunding 是组合资金来源：先按偏好顺序拆分预扣，再按反向顺序退还。
type CreditFunding struct {
	requestId string
	userId    int
	options   model.CombinedPreConsumeOptions

	subscriptionPart int64
	walletPart       int64
	subscriptionId   int

	amountTotal     int64
	amountUsedAfter int64
	planId          int
	planTitle       string
}

func (f *CreditFunding) Source() string {
	switch {
	case f.subscriptionPart > 0 && f.walletPart > 0:
		return BillingSourceComposite
	case f.subscriptionPart > 0:
		return BillingSourceSubscription
	default:
		return BillingSourceWallet
	}
}

func (f *CreditFunding) PreConsume(amount int) error {
	if amount <= 0 {
		return nil
	}
	result, err := model.PreConsumeCombined(f.requestId, f.userId, int64(amount), f.options)
	if err != nil {
		return err
	}
	f.applyAllocation(result)
	return nil
}

func (f *CreditFunding) applyAllocation(result *model.CombinedPreConsumeResult) {
	f.subscriptionPart = result.SubscriptionPart
	f.walletPart = result.WalletPart
	f.subscriptionId = result.SubscriptionId
	f.amountTotal = result.AmountTotal
	f.amountUsedAfter = result.AmountUsedAfter
	if result.SubscriptionId > 0 {
		if planInfo, err := model.GetSubscriptionPlanInfoByUserSubscriptionId(result.SubscriptionId); err == nil && planInfo != nil {
			f.planId = planInfo.PlanId
			f.planTitle = planInfo.PlanTitle
		}
	}
}

// Settle 按「补扣先订阅后钱包、退还先钱包后订阅」的反向分配调整两个池。
func (f *CreditFunding) Settle(delta int) error {
	if delta == 0 {
		return nil
	}
	if delta > 0 {
		return f.chargeMore(int64(delta))
	}
	return f.giveBack(int64(-delta))
}

// chargeMore 追加扣减：先订阅池（若仍有剩余），再钱包。
func (f *CreditFunding) chargeMore(amount int64) error {
	remainingSubscription := int64(0)
	if f.subscriptionId > 0 && f.amountTotal > 0 && f.amountTotal > f.amountUsedAfter {
		remainingSubscription = f.amountTotal - f.amountUsedAfter
	} else if f.subscriptionId > 0 && f.amountTotal == 0 {
		// amount_total = 0 是存量的不限量语义。
		remainingSubscription = amount
	}
	subscriptionDelta := int64(0)
	if f.options.AllowSubscription && remainingSubscription > 0 {
		subscriptionDelta = remainingSubscription
		if subscriptionDelta > amount {
			subscriptionDelta = amount
		}
	}
	walletDelta := amount - subscriptionDelta

	if subscriptionDelta > 0 {
		if f.subscriptionPart == 0 {
			// 本次请求原先没动订阅池，无幂等记录可调整；整笔转钱包，
			// 避免创建一条来源不明的补扣记录。
			walletDelta = amount
			subscriptionDelta = 0
		} else if err := model.AdjustSubscriptionPreConsume(f.requestId, subscriptionDelta); err != nil {
			return err
		} else {
			f.subscriptionPart += subscriptionDelta
			f.amountUsedAfter += subscriptionDelta
		}
	}
	if walletDelta > 0 {
		if f.walletPart == 0 {
			if _, err := model.PreConsumeUserWallet(f.requestId, f.userId, walletDelta); err != nil {
				return err
			}
		} else if err := model.AdjustWalletPreConsume(f.requestId, f.userId, walletDelta); err != nil {
			return err
		}
		f.walletPart += walletDelta
	}
	return nil
}

// giveBack 退还差额：先钱包，再订阅池（与分配顺序相反）。
func (f *CreditFunding) giveBack(amount int64) error {
	walletDelta := amount
	if walletDelta > f.walletPart {
		walletDelta = f.walletPart
	}
	subscriptionDelta := amount - walletDelta
	if subscriptionDelta > f.subscriptionPart {
		subscriptionDelta = f.subscriptionPart
	}
	if walletDelta > 0 {
		if err := model.AdjustWalletPreConsume(f.requestId, f.userId, -walletDelta); err != nil {
			return err
		}
		f.walletPart -= walletDelta
	}
	if subscriptionDelta > 0 {
		if err := model.AdjustSubscriptionPreConsume(f.requestId, -subscriptionDelta); err != nil {
			return err
		}
		f.subscriptionPart -= subscriptionDelta
		f.amountUsedAfter -= subscriptionDelta
	}
	return nil
}

func (f *CreditFunding) Reserve(delta int) error {
	if delta <= 0 {
		return nil
	}
	if f.subscriptionPart == 0 && f.walletPart == 0 {
		// 尚未预扣（信任额度旁路）：这一步就是首次预扣。
		return f.PreConsume(delta)
	}
	return f.chargeMore(int64(delta))
}

func (f *CreditFunding) RollbackReserve(delta int) {
	if delta <= 0 {
		return
	}
	if err := f.giveBack(int64(delta)); err != nil {
		logFundingError("error rolling back funding reserve", err)
	}
}

func (f *CreditFunding) HasReservation() bool {
	return f.subscriptionPart > 0 || f.walletPart > 0
}

func (f *CreditFunding) Allocation() (int64, int64) {
	return f.subscriptionPart, f.walletPart
}

func (f *CreditFunding) SubscriptionSnapshot() SubscriptionFundingSnapshot {
	return SubscriptionFundingSnapshot{
		SubscriptionId:  f.subscriptionId,
		PreConsumed:     f.subscriptionPart,
		AmountTotal:     f.amountTotal,
		AmountUsedAfter: f.amountUsedAfter,
		PlanId:          f.planId,
		PlanTitle:       f.planTitle,
	}
}

// Refund 退还本次请求在两个池上的全部预扣。
// 两侧都以 request_id 幂等，因此可以安全重试。
func (f *CreditFunding) Refund() error {
	if !f.HasReservation() {
		return nil
	}
	return refundWithRetry(func() error {
		return model.RefundCombinedPreConsume(f.requestId)
	})
}

// refundWithRetry 尝试多次执行退款操作以提高成功率，只能用于幂等的退款函数。
func refundWithRetry(fn func() error) error {
	if fn == nil {
		return nil
	}
	const maxAttempts = 3
	var lastErr error
	for i := 0; i < maxAttempts; i++ {
		if err := fn(); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if i < maxAttempts-1 {
			time.Sleep(time.Duration(200*(i+1)) * time.Millisecond)
		}
	}
	return lastErr
}
