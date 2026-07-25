package model

import (
	"errors"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"

	"github.com/bytedance/gopkg/util/gopool"
	"gorm.io/gorm"
)

// 永久钱包预扣的幂等账本。
//
// 原来的钱包退款走 IncreaseUserQuota（quota += N，非幂等），一旦重试就会多退额度，
// 因此那条路径根本不能重试；而订阅侧早就有 request_id 幂等记录。
// 这里把钱包也纳入同一套 request_id 幂等语义，让「预扣 → 结算 → 失败退还」
// 三步在并发与重试下都保证总额度守恒。

const (
	walletPreConsumeStatusConsumed = "consumed"
	walletPreConsumeStatusRefunded = "refunded"
)

// ErrWalletQuotaInsufficient 表示钱包余额不足以完成本次预扣或补扣。
var ErrWalletQuotaInsufficient = errors.New("insufficient wallet quota")

// WalletPreConsumeRecord 按 request_id 唯一记录一次请求对永久钱包的预扣。
type WalletPreConsumeRecord struct {
	Id          int    `json:"id"`
	RequestId   string `json:"request_id" gorm:"type:varchar(64);uniqueIndex"`
	UserId      int    `json:"user_id" gorm:"index"`
	PreConsumed int64  `json:"pre_consumed" gorm:"type:bigint;not null;default:0"`
	Status      string `json:"status" gorm:"type:varchar(32);index"`
	CreatedAt   int64  `json:"created_at" gorm:"bigint"`
	UpdatedAt   int64  `json:"updated_at" gorm:"bigint;index"`
}

func (r *WalletPreConsumeRecord) BeforeCreate(tx *gorm.DB) error {
	now := common.GetTimestamp()
	r.CreatedAt = now
	r.UpdatedAt = now
	return nil
}

func (r *WalletPreConsumeRecord) BeforeUpdate(tx *gorm.DB) error {
	r.UpdatedAt = common.GetTimestamp()
	return nil
}

// shiftWalletQuotaTx 以行锁原子调整钱包余额，余额永不为负、永不越过存储上限。
func shiftWalletQuotaTx(tx *gorm.DB, userId int, delta int64) error {
	if delta == 0 {
		return nil
	}
	var user User
	if err := lockForUpdate(tx).Where("id = ?", userId).First(&user).Error; err != nil {
		return err
	}
	target := int64(user.Quota) + delta
	if target < 0 {
		return fmt.Errorf("%w: have %d, need %d", ErrWalletQuotaInsufficient, user.Quota, -delta)
	}
	if target > int64(common.MaxQuota) {
		// users.quota 是 32 位整数列，环绕写入会把余额变成负数。
		return fmt.Errorf("wallet quota would overflow the storage ceiling (current %d, delta %d)", user.Quota, delta)
	}
	return tx.Model(&User{}).Where("id = ?", userId).Update("quota", target).Error
}

// syncWalletQuotaCache 让缓存跟上刚提交的余额变化。
// 用增量而非整值，避免与并发写互相覆盖。
func syncWalletQuotaCache(userId int, delta int64) {
	if delta == 0 {
		return
	}
	gopool.Go(func() {
		if err := cacheIncrUserQuota(userId, delta); err != nil {
			common.SysLog("failed to sync wallet quota cache: " + err.Error())
		}
	})
}

// PreConsumeUserWallet 从永久钱包幂等预扣 amount。
// 同 requestId 重复调用返回首次预扣量，不会二次扣减。
func PreConsumeUserWallet(requestId string, userId int, amount int64) (int64, error) {
	if strings.TrimSpace(requestId) == "" {
		return 0, errors.New("requestId is empty")
	}
	if userId <= 0 {
		return 0, errors.New("invalid userId")
	}
	if amount <= 0 {
		return 0, nil
	}
	var applied int64
	err := DB.Transaction(func(tx *gorm.DB) error {
		var existing WalletPreConsumeRecord
		query := lockForUpdate(tx).Where("request_id = ?", requestId).Limit(1).Find(&existing)
		if query.Error != nil {
			return query.Error
		}
		if query.RowsAffected > 0 {
			if existing.Status == walletPreConsumeStatusRefunded {
				return errors.New("wallet pre-consume already refunded")
			}
			applied = existing.PreConsumed
			return nil
		}
		if err := shiftWalletQuotaTx(tx, userId, -amount); err != nil {
			return err
		}
		record := &WalletPreConsumeRecord{
			RequestId:   requestId,
			UserId:      userId,
			PreConsumed: amount,
			Status:      walletPreConsumeStatusConsumed,
		}
		if err := tx.Create(record).Error; err != nil {
			return err
		}
		applied = amount
		return nil
	})
	if err != nil {
		return 0, err
	}
	if applied == amount {
		syncWalletQuotaCache(userId, -amount)
	}
	return applied, nil
}

// AdjustWalletPreConsume 调整已有预扣：delta > 0 补扣，delta < 0 退还部分。
// 账本上的 pre_consumed 同步变化，因此后续 Refund 退还的一定是当前实际预扣量。
func AdjustWalletPreConsume(requestId string, userId int, delta int64) error {
	if strings.TrimSpace(requestId) == "" {
		return errors.New("requestId is empty")
	}
	if delta == 0 {
		return nil
	}
	err := DB.Transaction(func(tx *gorm.DB) error {
		var record WalletPreConsumeRecord
		if err := lockForUpdate(tx).Where("request_id = ?", requestId).First(&record).Error; err != nil {
			return err
		}
		if record.Status == walletPreConsumeStatusRefunded {
			return errors.New("wallet pre-consume already refunded")
		}
		if err := shiftWalletQuotaTx(tx, userId, -delta); err != nil {
			return err
		}
		record.PreConsumed += delta
		if record.PreConsumed < 0 {
			record.PreConsumed = 0
		}
		return tx.Save(&record).Error
	})
	if err != nil {
		return err
	}
	syncWalletQuotaCache(userId, -delta)
	return nil
}

// RefundWalletPreConsume 幂等退还本次请求对钱包的全部预扣。
func RefundWalletPreConsume(requestId string) error {
	if strings.TrimSpace(requestId) == "" {
		return errors.New("requestId is empty")
	}
	var refunded int64
	var userId int
	err := DB.Transaction(func(tx *gorm.DB) error {
		var record WalletPreConsumeRecord
		if err := lockForUpdate(tx).Where("request_id = ?", requestId).First(&record).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				// 从未预扣过钱包，无需退还。
				return nil
			}
			return err
		}
		if record.Status == walletPreConsumeStatusRefunded {
			return nil
		}
		if record.PreConsumed > 0 {
			if err := shiftWalletQuotaTx(tx, record.UserId, record.PreConsumed); err != nil {
				return err
			}
			refunded = record.PreConsumed
			userId = record.UserId
		}
		record.Status = walletPreConsumeStatusRefunded
		return tx.Save(&record).Error
	})
	if err != nil {
		return err
	}
	if refunded > 0 {
		syncWalletQuotaCache(userId, refunded)
	}
	return nil
}

// CleanupWalletPreConsumeRecords 清理陈旧幂等记录，保持表规模可控。
func CleanupWalletPreConsumeRecords(olderThanSeconds int64) (int64, error) {
	if olderThanSeconds <= 0 {
		olderThanSeconds = 7 * 24 * 3600
	}
	cutoff := GetDBTimestamp() - olderThanSeconds
	res := DB.Where("updated_at < ?", cutoff).Delete(&WalletPreConsumeRecord{})
	return res.RowsAffected, res.Error
}
