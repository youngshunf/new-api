package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// doc94 D1：流水与日聚合成为 Cloud 的唯一消费读源。
// 这里锁两件事：金额一律由 NewAPI 换算成积分（Cloud 不再持有换算常量）；
// 日边界由调用方时区决定（否则同一笔消费会在两侧落到不同日期）。

func TestUsageListReturnsCreditsNotQuota(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 7701, 0)

	// QuotaPerUnit = 500000，即 1 积分 = 500000 quota
	seedConsumeLog(t, 7701, 500_000, `{"billing_source":"wallet"}`)
	seedConsumeLog(t, 7701, 250_000, `{"billing_source":"subscription"}`)

	page, err := ListCreditUsage(7701, 0, 0, 1, 20)
	require.NoError(t, err)

	require.Len(t, page.Items, 2)
	assert.Equal(t, int64(2), page.Total)
	assert.NotEmpty(t, page.MeasuredAt, "必须带测量时刻，展示侧据此判断新鲜度")
	// 倒序：最新一条在前
	assert.Equal(t, "0.5", page.Items[0].Credits)
	assert.Equal(t, "subscription", page.Items[0].FundingSource)
	assert.Equal(t, "1", page.Items[1].Credits)
	assert.Equal(t, "wallet", page.Items[1].FundingSource)
}

func TestUsageListPaginationDoesNotAccumulateFilters(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 7702, 0)
	for range 3 {
		seedConsumeLog(t, 7702, 500_000, ``)
	}

	first, err := ListCreditUsage(7702, 0, 0, 1, 2)
	require.NoError(t, err)
	second, err := ListCreditUsage(7702, 0, 0, 2, 2)
	require.NoError(t, err)

	// total 必须是全量 3，而不是被分页条件污染后的数字
	assert.Equal(t, int64(3), first.Total)
	assert.Equal(t, int64(3), second.Total)
	assert.Len(t, first.Items, 2)
	assert.Len(t, second.Items, 1)
	assert.NotEqual(t, first.Items[0].Id, second.Items[0].Id)
}

func TestUsageDailyBucketsByCallerTimezone(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 7703, 0)

	// 2026-07-25T16:30:00Z：UTC 是 25 日，东八区已经是 26 日 00:30。
	// 日边界必须跟调用方时区走，否则用户在账单上看到的日期和他自己的日历对不上。
	seedConsumeLog(t, 7703, 500_000, ``)
	require.NoError(t, LOG_DB.Model(&Log{}).Where("user_id = ?", 7703).
		Update("created_at", 1784997000).Error)

	utc, err := SummarizeCreditUsageDaily(7703, 0, 0, 0)
	require.NoError(t, err)
	require.Len(t, utc.Items, 1)
	assert.Equal(t, "2026-07-25", utc.Items[0].Day)

	shanghai, err := SummarizeCreditUsageDaily(7703, 0, 0, 480)
	require.NoError(t, err)
	require.Len(t, shanghai.Items, 1)
	assert.Equal(t, "2026-07-26", shanghai.Items[0].Day)
	assert.Equal(t, "1", shanghai.Items[0].ConsumedCredits)
	assert.Equal(t, 1, shanghai.Items[0].RequestCount)
}

func TestUsageDailyRejectsOversizedRange(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 7704, 0)

	_, err := SummarizeCreditUsageDaily(7704, 1, 1+int64(creditUsageDailyMaxDays+2)*86400, 480)
	assert.Error(t, err, "超长区间必须直接拒绝，而不是慢慢扫全表")
}
