package model

import (
	"errors"
	"testing"

	"github.com/QuantumNous/new-api/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const audioSettlementQuota = int64(500000)

func seedManagedAudioToken(t *testing.T, id, userID int, remain int, leaseID string) *Token {
	t.Helper()
	token := &Token{
		Id: id, UserId: userID, Key: "sk-audio-" + leaseID, Status: common.TokenStatusEnabled,
		RemainQuota: remain,
	}
	require.NoError(t, DB.Table("tokens").Create(map[string]any{
		"id": id, "user_id": userID, "key": token.Key, "status": common.TokenStatusEnabled,
		"remain_quota": remain, "purpose": "audio.tts", "accounting_mode": "synchronous", "external_lease_id": leaseID,
	}).Error)
	return token
}

func clearAudioSettlementTables(t *testing.T) {
	t.Helper()
	require.NoError(t, DB.Exec("DELETE FROM audio_request_settlements").Error)
}

func audioSettlementInput(userID, tokenID int, leaseID, requestID string, amount int64) AudioSettlementInput {
	return AudioSettlementInput{
		ExternalLeaseId: leaseID, GatewayRequestId: requestID, TokenId: tokenID, UserId: userID,
		CredentialGeneration: 3, RelayMode: relayconstant.RelayModeAudioSpeech, ModelName: "tts-1",
		PreConsumedQuota: amount, FundingPreference: "subscription_first",
	}
}

func TestAudioSettlementPreConsumeAndAbsoluteSettleAreIdempotent(t *testing.T) {
	truncateTables(t)
	clearAudioSettlementTables(t)
	seedCreditUser(t, 18001, int(3*audioSettlementQuota))
	sub := seedActiveContract(t, 18001, "audio-contract-18001", 10*audioSettlementQuota, 8*audioSettlementQuota)
	token := seedManagedAudioToken(t, 180011, 18001, int(5*audioSettlementQuota), "lease-18001")
	input := audioSettlementInput(18001, token.Id, "lease-18001", "req-18001", 5*audioSettlementQuota)

	created, err := CreateAudioSettlement(input)
	require.NoError(t, err)
	require.NotEmpty(t, created.AudioRequestSettlementId)
	assert.Equal(t, int64(2*audioSettlementQuota), created.SubscriptionAppliedQuota)
	assert.Equal(t, int64(3*audioSettlementQuota), created.WalletAppliedQuota)
	assert.Equal(t, int64(5*audioSettlementQuota), created.TokenAppliedQuota)
	assert.Equal(t, "pending", created.BillingStatus)
	assert.Equal(t, int64(10*audioSettlementQuota), reloadSubscription(t, sub.Id).AmountUsed)
	assert.Equal(t, 0, walletQuotaOf(t, 18001))

	replay, err := CreateAudioSettlement(input)
	require.NoError(t, err)
	assert.Equal(t, created.AudioRequestSettlementId, replay.AudioRequestSettlementId)
	assert.Equal(t, created.Revision, replay.Revision, "重复登记只返回原receipt，不增加revision")

	settled, err := SettleAudioSettlement("lease-18001", "req-18001", 3*audioSettlementQuota)
	require.NoError(t, err)
	assert.Equal(t, "settled", settled.BillingStatus)
	require.NotNil(t, settled.TargetQuota)
	assert.Equal(t, int64(3*audioSettlementQuota), *settled.TargetQuota)
	assert.Equal(t, int64(audioSettlementQuota), settled.WalletAppliedQuota, "向下结算先退钱包")
	assert.Equal(t, int64(2*audioSettlementQuota), settled.SubscriptionAppliedQuota)
	assert.Equal(t, int64(5*audioSettlementQuota), settled.TokenAppliedQuota, "settle只调整资金池，Token按预扣保留")
	assert.Equal(t, int(2*audioSettlementQuota), walletQuotaOf(t, 18001))
	assert.Equal(t, int64(10*audioSettlementQuota), reloadSubscription(t, sub.Id).AmountUsed)

	repeat, err := SettleAudioSettlement("lease-18001", "req-18001", 3*audioSettlementQuota)
	require.NoError(t, err)
	assert.Equal(t, settled.Revision, repeat.Revision, "重复终局返回原ACK，不重复扣退")
	_, err = SettleAudioSettlement("lease-18001", "req-18001", 4*audioSettlementQuota)
	require.Error(t, err, "不同absolute target不得覆盖已提交终局")
}

func TestAudioSettlementRefundRestoresTokenAndBothPools(t *testing.T) {
	truncateTables(t)
	clearAudioSettlementTables(t)
	seedCreditUser(t, 18002, int(3*audioSettlementQuota))
	sub := seedActiveContract(t, 18002, "audio-contract-18002", 10*audioSettlementQuota, 8*audioSettlementQuota)
	token := seedManagedAudioToken(t, 180021, 18002, int(5*audioSettlementQuota), "lease-18002")
	input := audioSettlementInput(18002, token.Id, "lease-18002", "req-18002", 5*audioSettlementQuota)
	require.NoError(t, func() error { _, err := CreateAudioSettlement(input); return err }())

	refunded, err := RefundAudioSettlement("lease-18002", "req-18002")
	require.NoError(t, err)
	assert.Equal(t, "refunded", refunded.BillingStatus)
	require.NotNil(t, refunded.TargetQuota)
	assert.Equal(t, int64(0), *refunded.TargetQuota)
	assert.Equal(t, int64(0), refunded.SubscriptionAppliedQuota)
	assert.Equal(t, int64(0), refunded.WalletAppliedQuota)
	assert.Equal(t, int64(0), refunded.TokenAppliedQuota)
	assert.Equal(t, int(3*audioSettlementQuota), walletQuotaOf(t, 18002))
	assert.Equal(t, int64(8*audioSettlementQuota), reloadSubscription(t, sub.Id).AmountUsed)
	var reloaded Token
	require.NoError(t, DB.First(&reloaded, token.Id).Error)
	assert.Equal(t, int(5*audioSettlementQuota), reloaded.RemainQuota)
	assert.Equal(t, 0, reloaded.UsedQuota, "退款必须同步回滚令牌已用额度")

	repeat, err := RefundAudioSettlement("lease-18002", "req-18002")
	require.NoError(t, err)
	assert.Equal(t, refunded.Revision, repeat.Revision, "重复退款不得二次返还")
}

func TestAudioSettlementCreatesZeroReceiptAndRollsBackBeforeFundingMutation(t *testing.T) {
	truncateTables(t)
	clearAudioSettlementTables(t)
	seedCreditUser(t, 18003, int(3*audioSettlementQuota))
	seedActiveContract(t, 18003, "audio-contract-18003", 10*audioSettlementQuota, 8*audioSettlementQuota)
	token := seedManagedAudioToken(t, 180031, 18003, 0, "lease-18003")

	zero := audioSettlementInput(18003, token.Id, "lease-18003", "req-18003-zero", 0)
	created, err := CreateAudioSettlement(zero)
	require.NoError(t, err)
	assert.Equal(t, int64(0), created.PreConsumedQuota)
	assert.Equal(t, "pending", created.BillingStatus)

	failed := audioSettlementInput(18003, token.Id, "lease-18003", "req-18003-failed", audioSettlementQuota)
	_, err = CreateAudioSettlement(failed)
	require.Error(t, err)
	var count int64
	require.NoError(t, DB.Model(&AudioRequestSettlement{}).Where("gateway_request_id = ?", "req-18003-failed").Count(&count).Error)
	assert.Zero(t, count, "token不足时不得留下receipt")
	assert.Equal(t, int(3*audioSettlementQuota), walletQuotaOf(t, 18003))
	assert.ErrorIs(t, err, ErrAudioSettlementInsufficientToken)
	assert.False(t, errors.Is(err, ErrAudioSettlementConflict))
}
