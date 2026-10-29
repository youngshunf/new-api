package model

import (
	"errors"
	"github.com/QuantumNous/new-api/common"
)

var ErrAudioAccountingUnavailable = errors.New("音频同步账务尚未装配，禁止派发")

// CheckTokenAccountingMode 只以持久令牌事实分流，S1底座不允许未接S2的同步令牌消费。
func CheckTokenAccountingMode(token *Token) error {
	if token == nil {
		return ErrTokenInvalid
	}
	if token.Purpose == "" || token.Purpose == "generic" {
		if token.AccountingMode == "" || token.AccountingMode == "legacy" {
			// generic relay lease 仍由可信内部租约入口签发；ExternalLeaseId
			// 只标识租约归属，不应把普通 LLM lease 误判成音频同步令牌。
			return nil
		}
		return ErrTokenInvalid
	}
	if token.AccountingMode != "synchronous" || token.ExternalLeaseId == nil || *token.ExternalLeaseId == "" {
		return ErrTokenInvalid
	}
	switch token.Purpose {
	case "audio.transcribe", "audio.translate", "audio.tts":
		return ErrAudioAccountingUnavailable
	default:
		return ErrTokenInvalid
	}
}

// checkQuotaTokenAccounting 在共享额度写入口核DB模式；不能让缓存余额或调用者参数选择同步分支。
func checkQuotaTokenAccounting(id int) error {
	var token Token
	if err := DB.Where("id = ?", id).First(&token).Error; err != nil {
		return err
	}
	return CheckTokenAccountingMode(&token)
}

func checkManagedTokenMutation(token *Token) error {
	var stored Token
	if err := DB.Unscoped().Where("id = ?", token.Id).First(&stored).Error; err != nil {
		return err
	}
	if stored.AccountingMode == "synchronous" || stored.ExternalLeaseId != nil {
		return errors.New("受管音频令牌的资格只能由可信租约控制面修改")
	}
	return nil
}

// cachedManagedToken 必须现读DB校验当前Key/撤销与模式，不能凭旧缓存继续授权。
// cachedManagedToken 不替代通用余额准入；受管metadata命中时仍核真实凭据当前状态。
func cachedManagedToken(token *Token, key string) (*Token, error) {
	if token.AccountingMode == "synchronous" || token.ExternalLeaseId != nil {
		var stored Token
		if err := DB.Where(commonKeyCol+" = ?", key).First(&stored).Error; err != nil {
			return nil, err
		}
		if stored.Key != key || stored.Status != common.TokenStatusEnabled {
			return nil, ErrTokenInvalid
		}
		return &stored, nil
	}
	return token, nil
}
