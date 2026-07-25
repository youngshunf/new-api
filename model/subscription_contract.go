package model

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"gorm.io/gorm"
)

// 外部合同投影（Cloud 履约事件驱动）。
//
// Cloud 只管「买了什么、何时生效、何时到期」，NewAPI 管「还剩多少、什么时候清零」。
// 这里的投影自带 cycle_seconds / cycle_count，不依赖本地 subscription_plan，
// 因此月付 30 天、年付 12×30 天的口径不受存量自然月枚举影响。

// CreditAccountSubscriptionRef 返回对外暴露的订阅标识。
// 外部合同用 Cloud 合同投影号；存量 plan 驱动的订阅用 legacy 前缀合成一个稳定标识，
// 使账户快照的形状对 Cloud 始终合法，同时一眼能看出它不是 Cloud 合同。
func (s *UserSubscription) CreditAccountSubscriptionRef() string {
	if s.ExternalSubscriptionId != "" {
		return s.ExternalSubscriptionId
	}
	return fmt.Sprintf("legacy:%d", s.Id)
}

// expiredSubscriptionResult 记录一次过期操作清空掉的剩余额度，用于回执 applied_credits。
type expiredSubscriptionResult struct {
	subscription          *UserSubscription
	expiredRemainingQuota int64
}

// activateExternalSubscriptionTx 幂等创建或命中一条外部合同投影。
//
// 语义要点：
//   - 同 external_subscription_id 重复 activate 直接返回原投影，不重复发额度；
//   - start_at > now 建成 scheduled（未来合同），到点由维护任务原子切换为 active；
//   - 立即生效的合同会先把该用户其它 active 外部合同投影过期并清零（升级语义），
//     保证同一时刻只有一个可用的 Cloud 合同池；
//   - 免费档 end_at = 0（无商业到期）、cycle_count = 0（无限期循环），但仍有 cycle_seconds。
func activateExternalSubscriptionTx(tx *gorm.DB, n *normalizedCreditOperation, now int64) (*UserSubscription, error) {
	// 按用户加锁串行化：唯一性与「只有一个 active」都靠这把锁保证，
	// 而不是靠三端语法不通用的部分唯一索引。
	var owned []UserSubscription
	if err := lockForUpdate(tx).Where("user_id = ?", n.userId).Find(&owned).Error; err != nil {
		return nil, err
	}

	var existing UserSubscription
	found := tx.Where("external_subscription_id = ?", n.externalSubscriptionId).Limit(1).Find(&existing)
	if found.Error != nil {
		return nil, found.Error
	}
	if found.RowsAffected > 0 {
		if existing.UserId != n.userId {
			return nil, terminalFailure(CreditFailureSubscriptionStateConflict,
				"external subscription %s already belongs to newapi user %d", n.externalSubscriptionId, existing.UserId)
		}
		return &existing, nil
	}

	if err := ensureCreditUserExistsTx(tx, n.userId); err != nil {
		return nil, err
	}

	status := SubscriptionStatusActive
	if n.startAt > now {
		status = SubscriptionStatusScheduled
	}

	if status == SubscriptionStatusActive {
		for i := range owned {
			superseded := owned[i]
			if superseded.ExternalSubscriptionId == "" || superseded.Status != SubscriptionStatusActive {
				continue
			}
			if err := closeSubscriptionCycleTx(tx, &superseded, now); err != nil {
				return nil, err
			}
		}
	}

	sub := &UserSubscription{
		UserId:                 n.userId,
		PlanId:                 0,
		AmountTotal:            n.quotaAmount,
		AmountUsed:             0,
		StartTime:              n.startAt,
		EndTime:                n.endAt,
		Status:                 status,
		Source:                 SubscriptionSourceCloudContract,
		ExternalSubscriptionId: n.externalSubscriptionId,
		CycleSeconds:           n.cycleSeconds,
		CycleCount:             n.cycleCount,
		AllowWalletOverflow:    n.walletOverflow,
		LastResetTime:          n.startAt,
	}
	next, err := nextSubscriptionResetTime(tx, sub, time.Unix(n.startAt, 0))
	if err != nil {
		return nil, err
	}
	sub.NextResetTime = next
	if err := tx.Create(sub).Error; err != nil {
		return nil, err
	}
	return sub, nil
}

// expireExternalSubscriptionTx 幂等过期一条外部合同投影，并显式清空剩余额度。
func expireExternalSubscriptionTx(tx *gorm.DB, externalSubscriptionId string, now int64) (*expiredSubscriptionResult, error) {
	var sub UserSubscription
	if err := lockForUpdate(tx).Where("external_subscription_id = ?", externalSubscriptionId).First(&sub).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, creditError(http.StatusNotFound, CreditErrorSubscriptionNotFound, false,
				"external subscription %s not found", externalSubscriptionId)
		}
		return nil, err
	}
	remaining := int64(0)
	if sub.AmountTotal > sub.AmountUsed {
		remaining = sub.AmountTotal - sub.AmountUsed
	}
	if sub.Status == SubscriptionStatusExpired {
		// 已经过期：幂等命中，回执按「本次清空 0」计，不再重复回收。
		return &expiredSubscriptionResult{subscription: &sub, expiredRemainingQuota: 0}, nil
	}
	if err := closeSubscriptionCycleTx(tx, &sub, now); err != nil {
		return nil, err
	}
	return &expiredSubscriptionResult{subscription: &sub, expiredRemainingQuota: remaining}, nil
}

// closeSubscriptionCycleTx 关闭一条订阅投影：置为 expired 并把剩余额度清零。
func closeSubscriptionCycleTx(tx *gorm.DB, sub *UserSubscription, now int64) error {
	sub.Status = SubscriptionStatusExpired
	sub.AmountUsed = sub.AmountTotal
	sub.NextResetTime = 0
	if sub.EndTime == 0 || sub.EndTime > now {
		// 提前终止（升级、退款、免费政策撤销）时把到期时刻定在当下，
		// 否则 end_time 会留在未来，读路径会误判它仍在窗口内。
		sub.EndTime = now
	}
	return tx.Save(sub).Error
}

// ensureCreditUserExistsTx 校验履约目标用户存在。
// 不存在时返回 404 而不是建号——凭空创建用户会让错配的 newapi_user_id 静默生效。
func ensureCreditUserExistsTx(tx *gorm.DB, userId int) error {
	var count int64
	if err := tx.Model(&User{}).Where("id = ?", userId).Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		return creditError(http.StatusNotFound, CreditErrorUserNotFound, false, "newapi user %d not found", userId)
	}
	return nil
}

// ActivateDueSubscriptions 把到点的 scheduled 投影切换为 active。
//
// 维护顺序必须是「先 expire、再 activate、最后 reset」：
// 旧合同到期与新合同激活在同一时刻发生时按「旧到期 → 新激活」串行，
// 不允许出现两个同时可用的订阅池。
func ActivateDueSubscriptions(limit int) (int, error) {
	if limit <= 0 {
		limit = 200
	}
	now := GetDBTimestamp()
	var due []UserSubscription
	if err := DB.Where("status = ? AND start_time <= ?", SubscriptionStatusScheduled, now).
		Order("start_time asc, id asc").
		Limit(limit).
		Find(&due).Error; err != nil {
		return 0, err
	}
	if len(due) == 0 {
		return 0, nil
	}
	activated := 0
	for _, candidate := range due {
		pending := candidate
		err := DB.Transaction(func(tx *gorm.DB) error {
			var owned []UserSubscription
			if err := lockForUpdate(tx).Where("user_id = ?", pending.UserId).Find(&owned).Error; err != nil {
				return err
			}
			var locked UserSubscription
			if err := tx.Where("id = ? AND status = ?", pending.Id, SubscriptionStatusScheduled).Limit(1).Find(&locked).Error; err != nil {
				return err
			}
			if locked.Id == 0 {
				// 已被其它 worker 处理。
				return nil
			}
			if locked.EndTime > 0 && locked.EndTime <= now {
				// 整个合同窗口都已过去（长时间停机）：直接关闭，不发额度。
				return closeSubscriptionCycleTx(tx, &locked, now)
			}
			for i := range owned {
				superseded := owned[i]
				if superseded.Id == locked.Id || superseded.ExternalSubscriptionId == "" || superseded.Status != SubscriptionStatusActive {
					continue
				}
				if err := closeSubscriptionCycleTx(tx, &superseded, now); err != nil {
					return err
				}
			}
			locked.Status = SubscriptionStatusActive
			if err := tx.Save(&locked).Error; err != nil {
				return err
			}
			// 激活后按当前时刻重新对齐周期锚点，避免 scheduled 期间累积的
			// next_reset_time 落在过去而被立刻当成一次重置。
			if err := realignSubscriptionCycleTx(tx, &locked, now); err != nil {
				return err
			}
			activated++
			return nil
		})
		if err != nil {
			return activated, err
		}
	}
	return activated, nil
}

// realignSubscriptionCycleTx 把周期锚点推进到包含 now 的那一期，并清零该期用量。
func realignSubscriptionCycleTx(tx *gorm.DB, sub *UserSubscription, now int64) error {
	anchor := sub.LastResetTime
	if anchor <= 0 {
		anchor = sub.StartTime
	}
	base := time.Unix(anchor, 0)
	next, err := nextSubscriptionResetTime(tx, sub, base)
	if err != nil {
		return err
	}
	for next > 0 && next <= now {
		base = time.Unix(next, 0)
		next, err = nextSubscriptionResetTime(tx, sub, base)
		if err != nil {
			return err
		}
	}
	sub.LastResetTime = base.Unix()
	sub.NextResetTime = next
	sub.AmountUsed = 0
	return tx.Save(sub).Error
}

// CountActiveCloudContracts 返回该用户当前可用的 Cloud 合同投影数量。
// 用于「同一时刻只允许一个 active 投影」的回归测试与运营核对。
func CountActiveCloudContracts(userId int) (int64, error) {
	now := GetDBTimestamp()
	var count int64
	err := DB.Model(&UserSubscription{}).
		Where(activeSubscriptionWindow, SubscriptionStatusActive, now, now).
		Where("user_id = ? AND external_subscription_id <> ?", userId, "").
		Count(&count).Error
	return count, err
}
