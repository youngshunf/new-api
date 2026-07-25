package model

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/dto"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 固定周期口径：30 天 = 2592000 秒，年付 = 12 × 30 天 = 360 天。
// 这些用例用「把合同起点往前挪」的方式覆盖时间边界，等价于冻结时钟。

const day = int64(24 * 3600)

func activateContract(t *testing.T, eventId string, userId int, extId string, credits string, startAt int64, cycleCount *int) *UserSubscription {
	t.Helper()
	req := &dto.CreditOperationRequest{
		OperationType:          CreditOperationSubscriptionActivate,
		NewApiUserId:           userId,
		ExternalSubscriptionId: extId,
		CreditAmount:           credits,
		StartAt:                time.Unix(startAt, 0).UTC().Format(time.RFC3339),
		CycleSeconds:           CycleSecondsFixed,
	}
	if cycleCount != nil {
		endAt := time.Unix(startAt+int64(*cycleCount)*CycleSecondsFixed, 0).UTC().Format(time.RFC3339)
		req.EndAt = &endAt
		req.CycleCount = cycleCount
	}
	outcome, err := ExecuteCreditOperation(eventId, req)
	require.Nil(t, err)
	require.Equal(t, CreditOperationStatusSucceeded, outcome.Operation.Status)

	var sub UserSubscription
	require.NoError(t, DB.Where("external_subscription_id = ?", extId).First(&sub).Error)
	return &sub
}

func reloadSubscription(t *testing.T, id int) UserSubscription {
	t.Helper()
	var sub UserSubscription
	require.NoError(t, DB.Where("id = ?", id).First(&sub).Error)
	return sub
}

func runSubscriptionMaintenance(t *testing.T) {
	t.Helper()
	_, err := ExpireDueSubscriptions(500)
	require.NoError(t, err)
	_, err = ActivateDueSubscriptions(500)
	require.NoError(t, err)
	_, err = ResetDueSubscriptions(500)
	require.NoError(t, err)
}

func TestMonthlyContractExpiresWithoutResetting(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 8001, 0)
	now := GetDBTimestamp()
	one := 1

	// 第 29 天 23:59:59：尚未到期，额度照常可用。
	almost := activateContract(t, "evt-m1", 8001, "contract-monthly-a", "100", now-(30*day-1), &one)
	runSubscriptionMaintenance(t)
	reloaded := reloadSubscription(t, almost.Id)
	assert.Equal(t, SubscriptionStatusActive, reloaded.Status)
	assert.Equal(t, int64(0), reloaded.AmountUsed)

	// 第 30 天整：先到期并清空剩余额度，不执行重置。
	seedCreditUser(t, 8002, 0)
	expired := activateContract(t, "evt-m2", 8002, "contract-monthly-b", "100", now-30*day, &one)
	runSubscriptionMaintenance(t)
	reloadedExpired := reloadSubscription(t, expired.Id)
	assert.Equal(t, SubscriptionStatusExpired, reloadedExpired.Status)
	assert.Equal(t, reloadedExpired.AmountTotal, reloadedExpired.AmountUsed, "到期必须显式清空剩余额度")
}

func TestYearlyContractResetsEveryCycleAndStopsAtContractEnd(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 8003, 0)
	now := GetDBTimestamp()
	twelve := 12

	// 第 30 天：清零并重置为完整一期额度。
	sub := activateContract(t, "evt-y1", 8003, "contract-yearly", "1000", now-30*day, &twelve)
	require.NoError(t, DB.Model(&UserSubscription{}).Where("id = ?", sub.Id).Update("amount_used", 400*500000).Error)
	runSubscriptionMaintenance(t)
	afterReset := reloadSubscription(t, sub.Id)
	assert.Equal(t, SubscriptionStatusActive, afterReset.Status)
	assert.Equal(t, int64(0), afterReset.AmountUsed, "第 30 天必须清零重置")
	assert.Equal(t, int64(1000*500000), afterReset.AmountTotal, "重置只恢复一期额度，不叠加")
	assert.Equal(t, now, afterReset.LastResetTime, "周期锚点推进到当前期起点")
	assert.Equal(t, now+30*day, afterReset.NextResetTime, "下一次重置在 30 天后")
}

func TestYearlyContractDoesNotResetOnItsFinalDay(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 8004, 0)
	now := GetDBTimestamp()
	twelve := 12

	sub := activateContract(t, "evt-y2", 8004, "contract-yearly-end", "1000", now-360*day, &twelve)
	require.NoError(t, DB.Model(&UserSubscription{}).Where("id = ?", sub.Id).Update("amount_used", 100*500000).Error)
	runSubscriptionMaintenance(t)

	final := reloadSubscription(t, sub.Id)
	assert.Equal(t, SubscriptionStatusExpired, final.Status)
	assert.Equal(t, final.AmountTotal, final.AmountUsed, "第 360 天清零过期")
	assert.Equal(t, int64(0), final.NextResetTime, "不得发生第 13 次重置")
}

// 停机跨过多个周期时只推进到当前应处周期并清零一次，不累加漏掉月份的额度。
func TestResetCatchUpAcrossMultipleCyclesGrantsOneCycleOnly(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 8005, 0)
	now := GetDBTimestamp()
	twelve := 12

	sub := activateContract(t, "evt-y3", 8005, "contract-downtime", "1000", now-95*day, &twelve)
	require.NoError(t, DB.Model(&UserSubscription{}).Where("id = ?", sub.Id).Update("amount_used", 900*500000).Error)
	runSubscriptionMaintenance(t)

	caught := reloadSubscription(t, sub.Id)
	assert.Equal(t, int64(0), caught.AmountUsed)
	assert.Equal(t, int64(1000*500000), caught.AmountTotal)
	assert.Equal(t, now-95*day+90*day, caught.LastResetTime, "锚点落在包含当下的那一期起点")
	assert.Equal(t, now-95*day+120*day, caught.NextResetTime)
}

func TestFreeContractHasNoCommercialExpiryButStillResets(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 8006, 0)
	now := GetDBTimestamp()

	sub := activateContract(t, "evt-free", 8006, "contract-free", "100", now-31*day, nil)
	assert.Equal(t, int64(0), sub.EndTime, "免费档无商业到期")
	assert.Equal(t, int64(0), int64(sub.CycleCount), "免费档 cycle_count 为空（无限期循环）")
	assert.Equal(t, CycleSecondsFixed, sub.CycleSeconds, "免费档仍必须有周期长度")

	require.NoError(t, DB.Model(&UserSubscription{}).Where("id = ?", sub.Id).Update("amount_used", 80*500000).Error)
	runSubscriptionMaintenance(t)

	reloaded := reloadSubscription(t, sub.Id)
	assert.Equal(t, SubscriptionStatusActive, reloaded.Status, "免费档不因时间到期")
	assert.Equal(t, int64(0), reloaded.AmountUsed, "免费档每 30 天清零重置")
}

func TestScheduledContractCannotBeConsumedBeforeItStarts(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 8007, 0)
	now := GetDBTimestamp()
	one := 1

	future := activateContract(t, "evt-sched", 8007, "contract-future", "100", now+10*day, &one)
	assert.Equal(t, SubscriptionStatusScheduled, future.Status)

	subscriptionRemaining, walletRemaining, err := AvailableCreditQuota(8007)
	require.NoError(t, err)
	assert.Equal(t, int64(0), subscriptionRemaining, "未来合同不得提前计入可用额度")
	assert.Equal(t, int64(0), walletRemaining)

	_, preErr := PreConsumeCombined("req-future", 8007, 1000, CombinedPreConsumeOptions{AllowSubscription: true, AllowWallet: true})
	require.Error(t, preErr, "未来合同不得被提前消费")
}

// 同时刻「旧合同到期 → 新合同激活」必须串行，不得出现两个可用订阅池。
func TestActivationSupersedesPreviousContract(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 8008, 0)
	now := GetDBTimestamp()
	one := 1

	old := activateContract(t, "evt-old", 8008, "contract-old", "100", now-10*day, &one)
	fresh := activateContract(t, "evt-new", 8008, "contract-new", "200", now, &one)

	assert.Equal(t, SubscriptionStatusActive, fresh.Status)
	supersededOld := reloadSubscription(t, old.Id)
	assert.Equal(t, SubscriptionStatusExpired, supersededOld.Status)
	assert.Equal(t, supersededOld.AmountTotal, supersededOld.AmountUsed, "被顶替的合同剩余额度清零")

	active, err := CountActiveCloudContracts(8008)
	require.NoError(t, err)
	assert.Equal(t, int64(1), active)
}

func TestRepeatedActivateReturnsTheSameProjection(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 8009, 0)
	now := GetDBTimestamp()
	one := 1

	first := activateContract(t, "evt-dup-1", 8009, "contract-dup", "100", now, &one)

	// 换一个 event_id 重复 activate 同一份合同：返回原投影，不重复发额度。
	second := activateContract(t, "evt-dup-2", 8009, "contract-dup", "100", now, &one)
	assert.Equal(t, first.Id, second.Id)

	var count int64
	require.NoError(t, DB.Model(&UserSubscription{}).Where("user_id = ?", 8009).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

func TestSubscriptionExpireOperationZeroesRemainingAndIsIdempotent(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 8010, 0)
	now := GetDBTimestamp()
	one := 1

	sub := activateContract(t, "evt-exp-setup", 8010, "contract-expire", "100", now, &one)
	require.NoError(t, DB.Model(&UserSubscription{}).Where("id = ?", sub.Id).Update("amount_used", 30*500000).Error)

	outcome, err := ExecuteCreditOperation("evt-exp-1", &dto.CreditOperationRequest{
		OperationType:          CreditOperationSubscriptionExpire,
		NewApiUserId:           8010,
		ExternalSubscriptionId: "contract-expire",
	})
	require.Nil(t, err)
	assert.Equal(t, CreditOperationStatusSucceeded, outcome.Operation.Status)
	assert.Equal(t, "70", outcome.Operation.AppliedCredits, "回执是本次被清空的剩余额度")

	closed := reloadSubscription(t, sub.Id)
	assert.Equal(t, SubscriptionStatusExpired, closed.Status)
	assert.Equal(t, closed.AmountTotal, closed.AmountUsed)

	// 换一个 event_id 再次过期：幂等，不重复回收。
	again, err := ExecuteCreditOperation("evt-exp-2", &dto.CreditOperationRequest{
		OperationType:          CreditOperationSubscriptionExpire,
		NewApiUserId:           8010,
		ExternalSubscriptionId: "contract-expire",
	})
	require.Nil(t, err)
	assert.Equal(t, "0", again.Operation.AppliedCredits)
}

// 积分包永久有效：订阅的 reset / expire 都不得触碰永久钱包。
func TestWalletIsUntouchedBySubscriptionResetAndExpiry(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 8011, 0)
	now := GetDBTimestamp()
	twelve := 12

	_, err := ExecuteCreditOperation("evt-pack", walletGrantRequest(8011, "50"))
	require.Nil(t, err)
	require.Equal(t, 50*500000, walletQuotaOf(t, 8011))

	sub := activateContract(t, "evt-pack-sub", 8011, "contract-pack", "100", now-30*day, &twelve)
	require.NoError(t, DB.Model(&UserSubscription{}).Where("id = ?", sub.Id).Update("amount_used", 60*500000).Error)
	runSubscriptionMaintenance(t)
	assert.Equal(t, 50*500000, walletQuotaOf(t, 8011), "订阅重置不得改变永久钱包")

	_, expireErr := ExecuteCreditOperation("evt-pack-expire", &dto.CreditOperationRequest{
		OperationType:          CreditOperationSubscriptionExpire,
		NewApiUserId:           8011,
		ExternalSubscriptionId: "contract-pack",
	})
	require.Nil(t, expireErr)
	assert.Equal(t, 50*500000, walletQuotaOf(t, 8011), "订阅过期不得改变永久钱包")
}
