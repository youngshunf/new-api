package model

import (
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"
)

// 组合预扣：一次请求可同时扣订阅池与永久钱包。
//
// 旧实现是「二选一 + 整笔回退」：订阅池覆盖不了整次预扣时，整笔改扣钱包，
// 订阅池里的零碎额度就永远用不掉。这里改成按优先级拆分——
// 订阅剩 2、钱包 10、请求需 5 时，必须订阅扣 2、钱包扣 3。
//
// 两个池的预扣在同一个数据库事务内完成，并共用同一个 request_id 幂等键，
// 因此并发请求的成功消费总额不会超过两池之和。
//
// 加锁顺序固定为「先 user_subscriptions、后 users」，与履约侧一致，避免互锁。

// CombinedPreConsumeOptions 表达用户计费偏好允许动用哪些池、以及先动哪个。
type CombinedPreConsumeOptions struct {
	AllowSubscription bool
	AllowWallet       bool
	// PreferWallet 为 true 时先耗钱包再耗订阅（wallet_first）；
	// 缺省先耗订阅再耗钱包（subscription_first）。
	PreferWallet bool
}

// CombinedPreConsumeResult 是一次组合预扣的分配明细。
type CombinedPreConsumeResult struct {
	SubscriptionId   int
	SubscriptionPart int64
	WalletPart       int64

	// 以下字段供请求日志展示订阅池状态。
	AmountTotal     int64
	AmountUsedAfter int64

	IdempotentReplay bool
}

// Reserved 返回本次预扣的总额。
func (r *CombinedPreConsumeResult) Reserved() int64 {
	return r.SubscriptionPart + r.WalletPart
}

// CombinedQuotaInsufficientError 表示允许动用的资金池合计不足以覆盖本次请求。
// 它携带用户请求侧错误契约需要的三个数值（单位是内部 quota，调用方换算成积分）。
type CombinedQuotaInsufficientError struct {
	SubscriptionRemaining int64
	WalletRemaining       int64
	Required              int64
}

func (e *CombinedQuotaInsufficientError) Error() string {
	return fmt.Sprintf("insufficient credits: subscription_remaining=%d wallet_remaining=%d required=%d",
		e.SubscriptionRemaining, e.WalletRemaining, e.Required)
}

// PreConsumeCombined 按偏好顺序在一个事务内拆分预扣。
//
// 分配只从「当前第一条可用订阅投影」取额度：同一时刻只允许一个 active Cloud 合同，
// 因此不需要跨多条订阅拆分；多余的复杂度只会让幂等账本无法表达分配来源。
func PreConsumeCombined(requestId string, userId int, amount int64, opts CombinedPreConsumeOptions) (*CombinedPreConsumeResult, error) {
	if strings.TrimSpace(requestId) == "" {
		return nil, errors.New("requestId is empty")
	}
	if userId <= 0 {
		return nil, errors.New("invalid userId")
	}
	if amount <= 0 {
		return nil, errors.New("amount must be > 0")
	}
	if !opts.AllowSubscription && !opts.AllowWallet {
		return nil, errors.New("no funding pool is allowed by the billing preference")
	}
	now := GetDBTimestamp()
	result := &CombinedPreConsumeResult{}

	err := DB.Transaction(func(tx *gorm.DB) error {
		replayed, err := loadCombinedPreConsumeReplayTx(tx, requestId, result)
		if err != nil {
			return err
		}
		if replayed {
			result.IdempotentReplay = true
			return nil
		}

		var chosen *UserSubscription
		subscriptionRemaining := int64(0)
		if opts.AllowSubscription {
			chosen, subscriptionRemaining, err = pickUsableSubscriptionTx(tx, userId, amount, now)
			if err != nil {
				return err
			}
		}
		walletRemaining := int64(0)
		if opts.AllowWallet {
			var user User
			if err := lockForUpdate(tx).Where("id = ?", userId).First(&user).Error; err != nil {
				return err
			}
			walletRemaining = int64(user.Quota)
		}

		if subscriptionRemaining+walletRemaining < amount {
			return &CombinedQuotaInsufficientError{
				SubscriptionRemaining: subscriptionRemaining,
				WalletRemaining:       walletRemaining,
				Required:              amount,
			}
		}

		subscriptionPart, walletPart := allocateAcrossPools(amount, subscriptionRemaining, walletRemaining, opts.PreferWallet)

		if subscriptionPart > 0 {
			if err := tx.Create(&SubscriptionPreConsumeRecord{
				RequestId:          requestId,
				UserId:             userId,
				UserSubscriptionId: chosen.Id,
				PreConsumed:        subscriptionPart,
				Status:             subscriptionPreConsumeStatusConsumed,
			}).Error; err != nil {
				return err
			}
			chosen.AmountUsed += subscriptionPart
			if err := tx.Save(chosen).Error; err != nil {
				return err
			}
			result.SubscriptionId = chosen.Id
			result.SubscriptionPart = subscriptionPart
			result.AmountTotal = chosen.AmountTotal
			result.AmountUsedAfter = chosen.AmountUsed
		}
		if walletPart > 0 {
			if err := tx.Create(&WalletPreConsumeRecord{
				RequestId:   requestId,
				UserId:      userId,
				PreConsumed: walletPart,
				Status:      walletPreConsumeStatusConsumed,
			}).Error; err != nil {
				return err
			}
			if err := shiftWalletQuotaTx(tx, userId, -walletPart); err != nil {
				return err
			}
			result.WalletPart = walletPart
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !result.IdempotentReplay && result.WalletPart > 0 {
		syncWalletQuotaCache(userId, -result.WalletPart)
	}
	return result, nil
}

// allocateAcrossPools 按优先级把 amount 拆到两个池上。
func allocateAcrossPools(amount, subscriptionRemaining, walletRemaining int64, preferWallet bool) (subscriptionPart, walletPart int64) {
	take := func(remaining int64) int64 {
		if remaining <= 0 {
			return 0
		}
		if remaining >= amount {
			taken := amount
			amount = 0
			return taken
		}
		amount -= remaining
		return remaining
	}
	if preferWallet {
		walletPart = take(walletRemaining)
		subscriptionPart = take(subscriptionRemaining)
		return subscriptionPart, walletPart
	}
	subscriptionPart = take(subscriptionRemaining)
	walletPart = take(walletRemaining)
	return subscriptionPart, walletPart
}

// pickUsableSubscriptionTx 选出第一条此刻可用的订阅投影并返回其剩余额度。
// 顺带执行到点的懒重置，使「周期已翻但维护任务还没跑」时也能立刻用上新周期额度。
func pickUsableSubscriptionTx(tx *gorm.DB, userId int, amount int64, now int64) (*UserSubscription, int64, error) {
	var subs []UserSubscription
	if err := lockForUpdate(tx).
		Where(activeSubscriptionWindow, SubscriptionStatusActive, now, now).
		Where("user_id = ?", userId).
		Order(perpetualLastOrdering).
		Find(&subs).Error; err != nil {
		return nil, 0, err
	}
	for i := range subs {
		candidate := subs[i]
		if err := maybeResetUserSubscriptionTx(tx, &candidate, now); err != nil {
			return nil, 0, err
		}
		if candidate.AmountTotal <= 0 {
			// 存量语义里 amount_total = 0 表示不限量：整笔走订阅池，不参与拆分。
			return &candidate, amount, nil
		}
		remaining := candidate.AmountTotal - candidate.AmountUsed
		if remaining <= 0 {
			continue
		}
		return &candidate, remaining, nil
	}
	return nil, 0, nil
}

// loadCombinedPreConsumeReplayTx 命中同 request_id 的既有预扣时回填分配明细。
func loadCombinedPreConsumeReplayTx(tx *gorm.DB, requestId string, result *CombinedPreConsumeResult) (bool, error) {
	replayed := false

	var subRecord SubscriptionPreConsumeRecord
	subQuery := lockForUpdate(tx).Where("request_id = ?", requestId).Limit(1).Find(&subRecord)
	if subQuery.Error != nil {
		return false, subQuery.Error
	}
	if subQuery.RowsAffected > 0 {
		if subRecord.Status == subscriptionPreConsumeStatusRefunded {
			return false, errors.New("subscription pre-consume already refunded")
		}
		replayed = true
		result.SubscriptionId = subRecord.UserSubscriptionId
		result.SubscriptionPart = subRecord.PreConsumed
		var sub UserSubscription
		if err := tx.Where("id = ?", subRecord.UserSubscriptionId).First(&sub).Error; err != nil {
			return false, err
		}
		result.AmountTotal = sub.AmountTotal
		result.AmountUsedAfter = sub.AmountUsed
	}

	var walletRecord WalletPreConsumeRecord
	walletQuery := lockForUpdate(tx).Where("request_id = ?", requestId).Limit(1).Find(&walletRecord)
	if walletQuery.Error != nil {
		return false, walletQuery.Error
	}
	if walletQuery.RowsAffected > 0 {
		if walletRecord.Status == walletPreConsumeStatusRefunded {
			return false, errors.New("wallet pre-consume already refunded")
		}
		replayed = true
		result.WalletPart = walletRecord.PreConsumed
	}
	return replayed, nil
}

// RefundCombinedPreConsume 幂等退还本次请求在两个池上的全部预扣。
// 退还顺序与分配相反：先还钱包，再还订阅池。
func RefundCombinedPreConsume(requestId string) error {
	if err := RefundWalletPreConsume(requestId); err != nil {
		return err
	}
	return RefundSubscriptionPreConsume(requestId)
}

// AvailableCreditQuota 返回用户此刻可用的订阅剩余额度与钱包余额（内部 quota 单位）。
// 只读，用于 relay 前的硬门禁与错误体，不产生任何扣减。
func AvailableCreditQuota(userId int) (subscriptionRemaining int64, walletRemaining int64, err error) {
	if userId <= 0 {
		return 0, 0, errors.New("invalid userId")
	}
	var user User
	if err := DB.Where("id = ?", userId).First(&user).Error; err != nil {
		return 0, 0, err
	}
	walletRemaining = int64(user.Quota)

	now := GetDBTimestamp()
	var subs []UserSubscription
	if err := DB.Where(activeSubscriptionWindow, SubscriptionStatusActive, now, now).
		Where("user_id = ?", userId).
		Find(&subs).Error; err != nil {
		return 0, walletRemaining, err
	}
	for _, sub := range subs {
		if sub.AmountTotal <= 0 {
			continue
		}
		used := sub.AmountUsed
		if sub.NextResetTime > 0 && sub.NextResetTime <= now {
			// 到点未跑的重置：只读投影，与实际预扣时的懒重置结论一致。
			used = 0
		}
		remaining := sub.AmountTotal - used
		if remaining > 0 {
			subscriptionRemaining += remaining
		}
	}
	return subscriptionRemaining, walletRemaining, nil
}
