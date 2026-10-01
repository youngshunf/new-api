package model

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

const (
	AudioBillingStatusPending  = "pending"
	AudioBillingStatusSettled  = "settled"
	AudioBillingStatusRefunded = "refunded"
)

const (
	AudioSettlementActionSettle = "settle"
	AudioSettlementActionRefund = "refund"
)

const (
	AudioSettlementDispatchUnknown       = "unknown"
	AudioSettlementDispatchNotDispatched = "not_dispatched"
	AudioSettlementDispatchDispatched    = "dispatched"
)

var (
	ErrAudioSettlementConflict             = errors.New("audio settlement terminal target conflict")
	ErrAudioSettlementInsufficientToken    = errors.New("audio settlement token quota insufficient")
	ErrAudioSettlementInvalidDispatch      = errors.New("audio settlement dispatch state is invalid")
	ErrAudioSettlementRequiresManagedToken = errors.New("audio settlement requires a managed synchronous token")
)

// AudioSettlementInput 是NewAPI内部预扣接缝；不暴露Core/Node wire。
type AudioSettlementInput struct {
	ExternalLeaseId      string
	GatewayRequestId     string
	TokenId              int
	UserId               int
	CredentialGeneration int64
	RelayMode            int
	ModelName            string
	PreConsumedQuota     int64
	FundingPreference    string
}

func (i AudioSettlementInput) validate() error {
	if strings.TrimSpace(i.ExternalLeaseId) == "" || strings.TrimSpace(i.GatewayRequestId) == "" {
		return errors.New("audio settlement identity is empty")
	}
	if i.TokenId <= 0 || i.UserId <= 0 || i.CredentialGeneration <= 0 || i.RelayMode <= 0 {
		return errors.New("audio settlement identity is invalid")
	}
	if strings.TrimSpace(i.ModelName) == "" || strings.TrimSpace(i.FundingPreference) == "" {
		return errors.New("audio settlement metadata is empty")
	}
	if i.PreConsumedQuota < 0 {
		return errors.New("audio settlement pre-consumed quota is negative")
	}
	return nil
}

// CreateAudioSettlement 在模型请求前一次事务内预扣两池、Token并登记receipt。
func CreateAudioSettlement(input AudioSettlementInput) (*AudioRequestSettlement, error) {
	if err := input.validate(); err != nil {
		return nil, err
	}
	var result AudioRequestSettlement
	nowTimestamp := GetDBTimestamp()
	err := DB.Transaction(func(tx *gorm.DB) error {
		var existing AudioRequestSettlement
		lookup := lockForUpdate(tx).Where("external_lease_id = ? AND gateway_request_id = ?", input.ExternalLeaseId, input.GatewayRequestId).First(&existing)
		if lookup.Error == nil {
			if !sameAudioSettlementInput(&existing, input) {
				return ErrAudioSettlementConflict
			}
			result = existing
			return nil
		}
		if !errors.Is(lookup.Error, gorm.ErrRecordNotFound) {
			return lookup.Error
		}

		var token Token
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", input.TokenId, input.UserId).First(&token).Error; err != nil {
			return err
		}
		accountingErr := CheckTokenAccountingMode(&token)
		if accountingErr == nil {
			return ErrAudioSettlementRequiresManagedToken
		}
		if !errors.Is(accountingErr, ErrAudioAccountingUnavailable) {
			return accountingErr
		}
		if input.PreConsumedQuota > int64(token.RemainQuota) {
			return ErrAudioSettlementInsufficientToken
		}

		now := DatabaseTime{Time: time.Now().UTC()}
		settlementID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		result = AudioRequestSettlement{
			AudioRequestSettlementId: settlementID.String(), CreatedTime: now, UpdatedTime: now, Revision: 1,
			ExternalLeaseId: input.ExternalLeaseId, GatewayRequestId: input.GatewayRequestId,
			TokenId: int64(input.TokenId), UserId: int64(input.UserId), CredentialGeneration: input.CredentialGeneration,
			RelayMode: int64(input.RelayMode), ModelName: input.ModelName, PreConsumedQuota: input.PreConsumedQuota,
			FundingPreference: input.FundingPreference, BillingStatus: AudioBillingStatusPending,
			DispatchState: AudioSettlementDispatchUnknown,
		}
		if input.PreConsumedQuota > 0 {
			allocation, err := preConsumeAudioFundsTx(tx, input, nowTimestamp)
			if err != nil {
				return err
			}
			result.SubscriptionAppliedQuota = allocation.SubscriptionPart
			result.WalletAppliedQuota = allocation.WalletPart
			result.TokenAppliedQuota = input.PreConsumedQuota
			if allocation.SubscriptionId > 0 {
				subscriptionID := int64(allocation.SubscriptionId)
				result.SubscriptionId = &subscriptionID
			}
			result := tx.Model(&Token{}).Where("id = ? AND user_id = ? AND remain_quota >= ?", input.TokenId, input.UserId, input.PreConsumedQuota).Updates(map[string]any{
				"remain_quota":  gorm.Expr("remain_quota - ?", input.PreConsumedQuota),
				"used_quota":    gorm.Expr("used_quota + ?", input.PreConsumedQuota),
				"accessed_time": common.GetTimestamp(),
			})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrAudioSettlementInsufficientToken
			}
		}
		return tx.Create(&result).Error
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func sameAudioSettlementInput(existing *AudioRequestSettlement, input AudioSettlementInput) bool {
	return existing.TokenId == int64(input.TokenId) && existing.UserId == int64(input.UserId) &&
		existing.CredentialGeneration == input.CredentialGeneration && existing.RelayMode == int64(input.RelayMode) &&
		existing.ModelName == input.ModelName && existing.PreConsumedQuota == input.PreConsumedQuota &&
		existing.FundingPreference == input.FundingPreference
}

type audioSettlementAllocation struct {
	SubscriptionPart int64
	WalletPart       int64
	SubscriptionId   int
}

func preConsumeAudioFundsTx(tx *gorm.DB, input AudioSettlementInput, now int64) (audioSettlementAllocation, error) {
	var allocation audioSettlementAllocation
	remaining := input.PreConsumedQuota
	var wallet User
	if err := lockForUpdate(tx).Where("id = ?", input.UserId).First(&wallet).Error; err != nil {
		return allocation, err
	}
	walletAvailable := int64(wallet.Quota)
	var sub UserSubscription
	query := lockForUpdate(tx).Where("user_id = ? AND status = ? AND start_time <= ? AND end_time >= ?", input.UserId, SubscriptionStatusActive, now, now).Order(perpetualLastOrdering).First(&sub)
	if query.Error == nil {
		if sub.AmountTotal <= 0 {
			allocation.SubscriptionPart = remaining
		} else if available := sub.AmountTotal - sub.AmountUsed; available > 0 {
			allocation.SubscriptionPart = available
			if allocation.SubscriptionPart > remaining {
				allocation.SubscriptionPart = remaining
			}
		}
		allocation.SubscriptionId = sub.Id
	} else if !errors.Is(query.Error, gorm.ErrRecordNotFound) {
		return allocation, query.Error
	}
	remaining -= allocation.SubscriptionPart
	if remaining > walletAvailable {
		return allocation, &CombinedQuotaInsufficientError{SubscriptionRemaining: allocation.SubscriptionPart, WalletRemaining: walletAvailable, Required: input.PreConsumedQuota}
	}
	if allocation.SubscriptionPart > 0 {
		if err := tx.Model(&UserSubscription{}).Where("id = ?", allocation.SubscriptionId).Update("amount_used", gorm.Expr("amount_used + ?", allocation.SubscriptionPart)).Error; err != nil {
			return allocation, err
		}
	}
	if remaining > 0 {
		if err := tx.Model(&User{}).Where("id = ? AND quota >= ?", input.UserId, remaining).Update("quota", gorm.Expr("quota - ?", remaining)).Error; err != nil {
			return allocation, err
		}
		allocation.WalletPart = remaining
	}
	return allocation, nil
}

// SettleAudioSettlement 以absolute target结算；重复相同终局只返回原ACK。
// GetAudioSettlement 按租约与网关请求号读取真实receipt；查无记录由调用方映射为unknown。
func GetAudioSettlement(leaseID, requestID string) (*AudioRequestSettlement, error) {
	if strings.TrimSpace(leaseID) == "" || strings.TrimSpace(requestID) == "" {
		return nil, errors.New("audio settlement lookup identity is empty")
	}
	var receipt AudioRequestSettlement
	if err := DB.Where("external_lease_id = ? AND gateway_request_id = ?", leaseID, requestID).First(&receipt).Error; err != nil {
		return nil, err
	}
	return &receipt, nil
}

// MarkAudioSettlementNotDispatched 记录发送前已确认的零效果事实。
// 只有真实发送前失败才能写入；未知不能被调用方降级成未派发。
func MarkAudioSettlementNotDispatched(leaseID, requestID string) (*AudioRequestSettlement, error) {
	return markAudioSettlementDispatch(leaseID, requestID, AudioSettlementDispatchNotDispatched)
}

// MarkAudioSettlementDispatched 记录已收到真实上游响应或其它确定派发证据。
func MarkAudioSettlementDispatched(leaseID, requestID string) (*AudioRequestSettlement, error) {
	return markAudioSettlementDispatch(leaseID, requestID, AudioSettlementDispatchDispatched)
}

func markAudioSettlementDispatch(leaseID, requestID, nextState string) (*AudioRequestSettlement, error) {
	if strings.TrimSpace(leaseID) == "" || strings.TrimSpace(requestID) == "" {
		return nil, errors.New("audio settlement dispatch identity is empty")
	}
	if nextState != AudioSettlementDispatchNotDispatched && nextState != AudioSettlementDispatchDispatched {
		return nil, ErrAudioSettlementInvalidDispatch
	}
	var result AudioRequestSettlement
	err := DB.Transaction(func(tx *gorm.DB) error {
		var receipt AudioRequestSettlement
		if err := lockForUpdate(tx).Where("external_lease_id = ? AND gateway_request_id = ?", leaseID, requestID).First(&receipt).Error; err != nil {
			return err
		}
		if receipt.DispatchState == nextState {
			result = receipt
			return nil
		}
		if receipt.DispatchState == AudioSettlementDispatchDispatched && nextState == AudioSettlementDispatchNotDispatched {
			return ErrAudioSettlementInvalidDispatch
		}
		if receipt.DispatchState != AudioSettlementDispatchUnknown && receipt.DispatchState != AudioSettlementDispatchNotDispatched {
			return ErrAudioSettlementInvalidDispatch
		}
		now := DatabaseTime{Time: time.Now().UTC()}
		receipt.DispatchState = nextState
		receipt.UpdatedTime = now
		receipt.Revision++
		if err := tx.Save(&receipt).Error; err != nil {
			return fmt.Errorf("save audio settlement dispatch state: %w", err)
		}
		result = receipt
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func SettleAudioSettlement(leaseID, requestID string, target int64) (*AudioRequestSettlement, error) {
	return terminalizeAudioSettlement(leaseID, requestID, AudioSettlementActionSettle, target)
}

// RefundAudioSettlement 将本receipt实际应用的资金与Token全部退回。
func RefundAudioSettlement(leaseID, requestID string) (*AudioRequestSettlement, error) {
	return terminalizeAudioSettlement(leaseID, requestID, AudioSettlementActionRefund, 0)
}

func terminalizeAudioSettlement(leaseID, requestID, action string, target int64) (*AudioRequestSettlement, error) {
	if strings.TrimSpace(leaseID) == "" || strings.TrimSpace(requestID) == "" || target < 0 {
		return nil, errors.New("audio settlement terminal input is invalid")
	}
	var result AudioRequestSettlement
	err := DB.Transaction(func(tx *gorm.DB) error {
		var receipt AudioRequestSettlement
		if err := lockForUpdate(tx).Where("external_lease_id = ? AND gateway_request_id = ?", leaseID, requestID).First(&receipt).Error; err != nil {
			return err
		}
		if receipt.BillingStatus != AudioBillingStatusPending {
			if receipt.TerminalAction == nil || *receipt.TerminalAction != action || receipt.TargetQuota == nil || *receipt.TargetQuota != target {
				return ErrAudioSettlementConflict
			}
			result = receipt
			return nil
		}
		current := receipt.SubscriptionAppliedQuota + receipt.WalletAppliedQuota
		delta := target - current
		if delta > 0 {
			if receipt.FundingPreference == "subscription_only" {
				return ErrAudioSettlementInsufficientToken
			}
			if err := lockForUpdate(tx).Where("id = ? AND quota >= ?", receipt.UserId, delta).Update("quota", gorm.Expr("quota - ?", delta)).Error; err != nil {
				return err
			}
			receipt.WalletAppliedQuota += delta
		} else if delta < 0 {
			refund := -delta
			walletRefund := refund
			if walletRefund > receipt.WalletAppliedQuota {
				walletRefund = receipt.WalletAppliedQuota
			}
			if walletRefund > 0 {
				if err := tx.Model(&User{}).Where("id = ?", receipt.UserId).Update("quota", gorm.Expr("quota + ?", walletRefund)).Error; err != nil {
					return err
				}
				receipt.WalletAppliedQuota -= walletRefund
				refund -= walletRefund
			}
			if refund > 0 && receipt.SubscriptionId != nil {
				if err := tx.Model(&UserSubscription{}).Where("id = ?", *receipt.SubscriptionId).Update("amount_used", gorm.Expr("CASE WHEN amount_used >= ? THEN amount_used - ? ELSE 0 END", refund, refund)).Error; err != nil {
					return err
				}
				receipt.SubscriptionAppliedQuota -= refund
			}
		}
		if action == AudioSettlementActionRefund || (action == AudioSettlementActionSettle && target == 0) {
			result := tx.Model(&Token{}).Where("id = ? AND user_id = ?", receipt.TokenId, receipt.UserId).Updates(map[string]any{
				"remain_quota":  gorm.Expr("remain_quota + ?", receipt.TokenAppliedQuota),
				"used_quota":    gorm.Expr("used_quota - ?", receipt.TokenAppliedQuota),
				"accessed_time": common.GetTimestamp(),
			})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return gorm.ErrRecordNotFound
			}
			receipt.TokenAppliedQuota = 0
		}
		completed := DatabaseTime{Time: time.Now().UTC()}
		receipt.BillingStatus = AudioBillingStatusSettled
		if action == AudioSettlementActionRefund {
			receipt.BillingStatus = AudioBillingStatusRefunded
		}
		receipt.TerminalAction = &action
		receipt.TargetQuota = &target
		receipt.CompletedTime = &completed
		receipt.Revision++
		if err := tx.Save(&receipt).Error; err != nil {
			return fmt.Errorf("save audio settlement: %w", err)
		}
		result = receipt
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}
