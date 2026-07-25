package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// doc94 R1：存量 rebase 的消费归因。锁住的是「不许猜」这条底线——
// 拆不出资金来源的历史消费必须显式暴露成 indeterminate，而不是被摊派进某个池。

func seedConsumeLog(t *testing.T, userId int, quota int, other string) {
	t.Helper()
	entry := &Log{
		UserId:    userId,
		CreatedAt: 1_700_000_000,
		Type:      LogTypeConsume,
		Quota:     quota,
		Other:     other,
	}
	require.NoError(t, LOG_DB.Create(entry).Error)
}

func TestConsumptionSplitsByFundingPartsWhenLogged(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 7601, 0)

	// N3 之后的日志带资金池拆分：组合扣费 2+3
	seedConsumeLog(t, 7601, 5, `{"funding_subscription_part":2,"funding_wallet_part":3}`)
	// 纯钱包扣费（订阅部分为 0 时不写该键，仍属于「拆得出来」）
	seedConsumeLog(t, 7601, 4, `{"funding_wallet_part":4}`)

	summary, err := SummarizeCreditConsumption(7601)
	require.NoError(t, err)

	assert.True(t, summary.Determinate)
	assert.Equal(t, int64(7), summary.WalletConsumedQuota)
	assert.Equal(t, int64(2), summary.SubscriptionConsumedQuota)
	assert.Equal(t, int64(0), summary.UnattributedQuota)
}

func TestConsumptionWithoutSubscriptionHistoryAllBelongsToWallet(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 7602, 0)

	// N3 之前的历史日志没有拆分明细。该用户从未有过订阅，
	// 所以这些消费必然出自永久钱包——这是唯一可能，不是猜测。
	seedConsumeLog(t, 7602, 11, `{"model_ratio":1}`)
	seedConsumeLog(t, 7602, 9, ``)

	summary, err := SummarizeCreditConsumption(7602)
	require.NoError(t, err)

	assert.True(t, summary.Determinate)
	assert.False(t, summary.HasSubscriptionHistory)
	assert.Equal(t, int64(20), summary.WalletConsumedQuota)
	assert.Equal(t, int64(0), summary.UnattributedQuota)
}

func TestConsumptionWithSubscriptionHistoryAndMissingSplitIsIndeterminate(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 7603, 0)
	require.NoError(t, DB.Create(&UserSubscription{
		UserId:      7603,
		Status:      SubscriptionStatusExpired,
		AmountTotal: 100,
		AmountUsed:  100,
	}).Error)

	seedConsumeLog(t, 7603, 8, `{"funding_wallet_part":8}`)
	// 这一条拆不出来：用户有过订阅，无法判定它出自钱包还是订阅额度。
	seedConsumeLog(t, 7603, 13, `{"model_ratio":1}`)

	summary, err := SummarizeCreditConsumption(7603)
	require.NoError(t, err)

	assert.False(t, summary.Determinate, "拆不出资金来源时必须显式不确定，绝不摊派")
	assert.Equal(t, int64(13), summary.UnattributedQuota)
	assert.Equal(t, int64(1), summary.UnattributedCount)
	assert.NotEmpty(t, summary.IndeterminateReason)
	// 已归因的部分仍然如实返回，供人工复核时对照
	assert.Equal(t, int64(8), summary.WalletConsumedQuota)
}

func TestConsumptionRejectsInvalidUserId(t *testing.T) {
	_, err := SummarizeCreditConsumption(0)
	assert.Error(t, err)
}
