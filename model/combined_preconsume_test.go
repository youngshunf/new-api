package model

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedActiveContract(t *testing.T, userId int, extId string, total int64, used int64) *UserSubscription {
	t.Helper()
	now := GetDBTimestamp()
	sub := &UserSubscription{
		UserId:                 userId,
		AmountTotal:            total,
		AmountUsed:             used,
		StartTime:              now - 3600,
		EndTime:                now + CycleSecondsFixed,
		Status:                 SubscriptionStatusActive,
		Source:                 SubscriptionSourceCloudContract,
		ExternalSubscriptionId: extId,
		CycleSeconds:           CycleSecondsFixed,
		CycleCount:             1,
		AllowWalletOverflow:    true,
		LastResetTime:          now - 3600,
		NextResetTime:          now + CycleSecondsFixed,
	}
	require.NoError(t, DB.Create(sub).Error)
	return sub
}

func bothPools() CombinedPreConsumeOptions {
	return CombinedPreConsumeOptions{AllowSubscription: true, AllowWallet: true}
}

// doc94 §0.3 的验收样例：订阅剩 2、钱包 10、请求需 5 → 订阅扣 2、钱包扣 3。
// 旧实现会因为订阅覆盖不了整次预扣而整笔改扣钱包，遗留 2 积分永远用不掉。
func TestCombinedPreConsumeSplitsAcrossBothPools(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 9001, 10*500000)
	sub := seedActiveContract(t, 9001, "contract-split", 10*500000, 8*500000)

	result, err := PreConsumeCombined("req-split", 9001, 5*500000, bothPools())
	require.NoError(t, err)
	assert.Equal(t, int64(2*500000), result.SubscriptionPart)
	assert.Equal(t, int64(3*500000), result.WalletPart)
	assert.Equal(t, int64(5*500000), result.Reserved())

	assert.Equal(t, 7*500000, walletQuotaOf(t, 9001))
	assert.Equal(t, int64(10*500000), reloadSubscription(t, sub.Id).AmountUsed)
}

func TestCombinedPreConsumeIsIdempotentPerRequestId(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 9002, 10*500000)
	seedActiveContract(t, 9002, "contract-idem", 10*500000, 8*500000)

	first, err := PreConsumeCombined("req-idem", 9002, 5*500000, bothPools())
	require.NoError(t, err)
	replay, err := PreConsumeCombined("req-idem", 9002, 5*500000, bothPools())
	require.NoError(t, err)

	assert.True(t, replay.IdempotentReplay)
	assert.Equal(t, first.SubscriptionPart, replay.SubscriptionPart)
	assert.Equal(t, first.WalletPart, replay.WalletPart)
	assert.Equal(t, 7*500000, walletQuotaOf(t, 9002), "重放不得二次扣减")
}

func TestCombinedRefundRestoresBothPools(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 9003, 10*500000)
	sub := seedActiveContract(t, 9003, "contract-refund", 10*500000, 8*500000)

	_, err := PreConsumeCombined("req-refund", 9003, 5*500000, bothPools())
	require.NoError(t, err)

	require.NoError(t, RefundCombinedPreConsume("req-refund"))
	assert.Equal(t, 10*500000, walletQuotaOf(t, 9003))
	assert.Equal(t, int64(8*500000), reloadSubscription(t, sub.Id).AmountUsed)

	// 幂等：重复退还不得多退
	require.NoError(t, RefundCombinedPreConsume("req-refund"))
	assert.Equal(t, 10*500000, walletQuotaOf(t, 9003))
	assert.Equal(t, int64(8*500000), reloadSubscription(t, sub.Id).AmountUsed)
}

func TestCombinedPreConsumeReportsBothRemainingsWhenInsufficient(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 9004, 1*500000)
	seedActiveContract(t, 9004, "contract-short", 10*500000, 9*500000)

	_, err := PreConsumeCombined("req-short", 9004, 5*500000, bothPools())
	require.Error(t, err)

	var insufficient *CombinedQuotaInsufficientError
	require.True(t, errors.As(err, &insufficient))
	assert.Equal(t, int64(1*500000), insufficient.SubscriptionRemaining)
	assert.Equal(t, int64(1*500000), insufficient.WalletRemaining)
	assert.Equal(t, int64(5*500000), insufficient.Required)

	// 门禁必须在扣减之前生效：拒绝的请求不得留下任何扣减痕迹
	assert.Equal(t, 1*500000, walletQuotaOf(t, 9004))
}

func TestSubscriptionOnlyPreferenceNeverTouchesWallet(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 9005, 100*500000)
	seedActiveContract(t, 9005, "contract-sub-only", 10*500000, 9*500000)

	_, err := PreConsumeCombined("req-sub-only", 9005, 5*500000, CombinedPreConsumeOptions{AllowSubscription: true})
	require.Error(t, err)
	assert.Equal(t, 100*500000, walletQuotaOf(t, 9005), "禁止钱包回落时不得动钱包")
}

func TestWalletFirstPreferenceReversesAllocationOrder(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 9006, 3*500000)
	sub := seedActiveContract(t, 9006, "contract-wallet-first", 10*500000, 0)

	result, err := PreConsumeCombined("req-wallet-first", 9006, 5*500000,
		CombinedPreConsumeOptions{AllowSubscription: true, AllowWallet: true, PreferWallet: true})
	require.NoError(t, err)
	assert.Equal(t, int64(3*500000), result.WalletPart)
	assert.Equal(t, int64(2*500000), result.SubscriptionPart)
	assert.Equal(t, 0, walletQuotaOf(t, 9006))
	assert.Equal(t, int64(2*500000), reloadSubscription(t, sub.Id).AmountUsed)
}

// 并发请求的成功消费总额不得超过两池之和。
func TestConcurrentPreConsumeNeverOverspends(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 9007, 3*500000)
	sub := seedActiveContract(t, 9007, "contract-concurrent", 2*500000, 0)

	const requests = 20
	const each = int64(500000) // 每次 1 积分；两池合计只够 5 次
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := int64(0)
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			result, err := PreConsumeCombined(fmt.Sprintf("req-concurrent-%d", idx), 9007, each, bothPools())
			if err != nil {
				return
			}
			mu.Lock()
			granted += result.Reserved()
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	assert.Equal(t, int64(5*500000), granted, "成功消费总额恰好等于两池之和")
	assert.Equal(t, 0, walletQuotaOf(t, 9007), "钱包不得为负")
	reloaded := reloadSubscription(t, sub.Id)
	assert.Equal(t, reloaded.AmountTotal, reloaded.AmountUsed, "订阅池不得超支")
}

func TestAdjustPreConsumeKeepsLedgerAndPoolsInSync(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 9008, 10*500000)
	sub := seedActiveContract(t, 9008, "contract-adjust", 10*500000, 8*500000)

	_, err := PreConsumeCombined("req-adjust", 9008, 5*500000, bothPools())
	require.NoError(t, err)

	// 结算退还 1 积分：先退钱包（与分配顺序相反）
	require.NoError(t, AdjustWalletPreConsume("req-adjust", 9008, -500000))
	assert.Equal(t, 8*500000, walletQuotaOf(t, 9008))

	// 剩余预扣全部退还时，账本已经跟着调整过，不会多退
	require.NoError(t, RefundCombinedPreConsume("req-adjust"))
	assert.Equal(t, 10*500000, walletQuotaOf(t, 9008))
	assert.Equal(t, int64(8*500000), reloadSubscription(t, sub.Id).AmountUsed)
}

func TestAvailableCreditQuotaExcludesExpiredAndFutureContracts(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 9009, 2*500000)
	now := GetDBTimestamp()

	// 已过期：剩余额度已被显式清零
	require.NoError(t, DB.Create(&UserSubscription{
		UserId: 9009, AmountTotal: 10 * 500000, AmountUsed: 10 * 500000,
		StartTime: now - 60*day, EndTime: now - day, Status: SubscriptionStatusExpired,
		ExternalSubscriptionId: "contract-past", CycleSeconds: CycleSecondsFixed,
	}).Error)
	// 未来合同：不得提前计入
	require.NoError(t, DB.Create(&UserSubscription{
		UserId: 9009, AmountTotal: 10 * 500000,
		StartTime: now + day, EndTime: now + 31*day, Status: SubscriptionStatusScheduled,
		ExternalSubscriptionId: "contract-future", CycleSeconds: CycleSecondsFixed,
	}).Error)
	// 当前合同
	seedActiveContract(t, 9009, "contract-now", 10*500000, 6*500000)

	subscriptionRemaining, walletRemaining, err := AvailableCreditQuota(9009)
	require.NoError(t, err)
	assert.Equal(t, int64(4*500000), subscriptionRemaining)
	assert.Equal(t, int64(2*500000), walletRemaining)
}
