package model

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreditUsageEntryUsesGovernedCreatedTimeField(t *testing.T) {
	payload, err := common.Marshal(CreditUsageEntry{CreatedTime: 1_800_000_000})
	require.NoError(t, err)
	assert.JSONEq(t, `{"id":0,"created_time":1800000000,"model_name":"","token_name":"","credits":"","prompt_tokens":0,"completion_tokens":0,"use_time":0,"is_stream":false}`, string(payload))
}

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

// seedConsumeLogAt 写一条指定 UTC 时刻的消费日志（1 积分 = 500000 quota）。
func seedConsumeLogAt(t *testing.T, userId int, at time.Time) {
	t.Helper()
	require.NoError(t, LOG_DB.Create(&Log{
		UserId:    userId,
		CreatedAt: at.Unix(),
		Type:      LogTypeConsume,
		Quota:     500_000,
	}).Error)
}

// usageDailyDays 把日聚合结果展开成「日期 → 请求数」，便于整表断言。
func usageDailyDays(daily *CreditUsageDaily) map[string]int {
	days := make(map[string]int, len(daily.Items))
	for _, item := range daily.Items {
		days[item.Day] = item.RequestCount
	}
	return days
}

func utcAt(year int, month time.Month, day, hour, minute, second int) time.Time {
	return time.Date(year, month, day, hour, minute, second, 0, time.UTC)
}

func TestUsageDailyBucketsByCallerTimezone(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 7703, 0)

	// 2026-07-25T16:30:00Z：UTC 是 25 日，东八区已经是 26 日 00:30。
	// 日边界必须跟调用方时区走，否则用户在账单上看到的日期和他自己的日历对不上。
	seedConsumeLogAt(t, 7703, utcAt(2026, time.July, 25, 16, 30, 0))

	utc, err := SummarizeCreditUsageDaily(7703, 0, 0, "UTC")
	require.Nil(t, err)
	assert.Equal(t, "UTC", utc.Timezone)
	assert.Equal(t, map[string]int{"2026-07-25": 1}, usageDailyDays(utc))

	shanghai, err := SummarizeCreditUsageDaily(7703, 0, 0, "Asia/Shanghai")
	require.Nil(t, err)
	assert.Equal(t, "Asia/Shanghai", shanghai.Timezone, "响应必须回显实际分桶所用的时区")
	require.Len(t, shanghai.Items, 1)
	assert.Equal(t, "2026-07-26", shanghai.Items[0].Day)
	assert.Equal(t, "1", shanghai.Items[0].ConsumedCredits)
	assert.Equal(t, 1, shanghai.Items[0].RequestCount)
}

// 设计 §6「历史统计」：纽约 7 月 1 日 00:30（EDT，-04）的记录，冬季（EST，-05）再查仍归 7 月 1 日。
// 用「查询当下的偏移」切历史日界（旧 tz_offset_minutes=-300）会把它错划到 6 月 30 日。
func TestUsageDailyUsesHistoricalOffsetOfEachRecord(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 7705, 0)
	seedConsumeLogAt(t, 7705, utcAt(2026, time.July, 1, 4, 30, 0))

	daily, err := SummarizeCreditUsageDaily(7705, 0, 0, "America/New_York")
	require.Nil(t, err)
	assert.Equal(t, map[string]int{"2026-07-01": 1}, usageDailyDays(daily))
}

// 设计 §6「春季日界」：纽约 2026-03-08 只有 23 小时，为 05:00Z 至次日 04:00Z。
func TestUsageDailySpringForwardDayBoundary(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 7706, 0)
	seedConsumeLogAt(t, 7706, utcAt(2026, time.March, 8, 4, 59, 59)) // 3 月 7 日 23:59:59 EST
	seedConsumeLogAt(t, 7706, utcAt(2026, time.March, 8, 5, 0, 0))   // 3 月 8 日 00:00 EST
	seedConsumeLogAt(t, 7706, utcAt(2026, time.March, 9, 3, 59, 59)) // 3 月 8 日 23:59:59 EDT
	seedConsumeLogAt(t, 7706, utcAt(2026, time.March, 9, 4, 0, 0))   // 3 月 9 日 00:00 EDT

	daily, err := SummarizeCreditUsageDaily(7706, 0, 0, "America/New_York")
	require.Nil(t, err)
	assert.Equal(t, map[string]int{"2026-03-07": 1, "2026-03-08": 2, "2026-03-09": 1}, usageDailyDays(daily))
}

// 设计 §6「秋季日界」：纽约 2026-11-01 有 25 小时，为 04:00Z 至次日 05:00Z。
func TestUsageDailyFallBackDayBoundary(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 7707, 0)
	seedConsumeLogAt(t, 7707, utcAt(2026, time.November, 1, 3, 59, 59)) // 10 月 31 日 23:59:59 EDT
	seedConsumeLogAt(t, 7707, utcAt(2026, time.November, 1, 4, 0, 0))   // 11 月 1 日 00:00 EDT
	seedConsumeLogAt(t, 7707, utcAt(2026, time.November, 2, 4, 59, 59)) // 11 月 1 日 23:59:59 EST
	seedConsumeLogAt(t, 7707, utcAt(2026, time.November, 2, 5, 0, 0))   // 11 月 2 日 00:00 EST

	daily, err := SummarizeCreditUsageDaily(7707, 0, 0, "America/New_York")
	require.Nil(t, err)
	assert.Equal(t, map[string]int{"2026-10-31": 1, "2026-11-01": 2, "2026-11-02": 1}, usageDailyDays(daily))
}

// 冻结基线：start/end 为半开区间 [start,end)，恰好落在 end 的记录不计入。
func TestUsageDailyRangeIsHalfOpen(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 7708, 0)
	start := utcAt(2026, time.July, 1, 0, 0, 0)
	end := utcAt(2026, time.July, 2, 0, 0, 0)
	seedConsumeLogAt(t, 7708, start.Add(-time.Second))
	seedConsumeLogAt(t, 7708, start)
	seedConsumeLogAt(t, 7708, end.Add(-time.Second))
	seedConsumeLogAt(t, 7708, end)

	daily, err := SummarizeCreditUsageDaily(7708, start.Unix(), end.Unix(), "UTC")
	require.Nil(t, err)
	assert.Equal(t, map[string]int{"2026-07-01": 2}, usageDailyDays(daily))
}

// 时区名唯一的校验方是 new-api：缺失、空串、非 IANA 名一律 400 invalid_request，
// 不回落 UTC，也不接受依赖进程环境的 "Local"。
func TestUsageDailyRejectsInvalidTimezone(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 7709, 0)

	for _, timezone := range []string{"", "Local", "Mars/Olympus_Mons", "+08:00", "Asia/Shanghai "} {
		t.Run(timezone, func(t *testing.T) {
			_, err := SummarizeCreditUsageDaily(7709, 0, 0, timezone)
			require.NotNil(t, err)
			assert.Equal(t, 400, err.HTTPStatus)
			assert.Equal(t, CreditErrorInvalidRequest, err.Code)
			assert.False(t, err.Retryable)
		})
	}
}

// 限窗按 timezone 下的日历日数判 ≤366，不按 (end-start)/86400：
// 纽约 2026-01-01 00:00 起到 2027-01-02 00:00（366 个日历日）放行，
// 再延到 2027-01-02 23:00（触及第 367 个日历日，但秒数整除仍是 366）必须拒绝。
func TestUsageDailyRangeLimitCountsCalendarDays(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 7704, 0)
	newYork, loadErr := time.LoadLocation("America/New_York")
	require.NoError(t, loadErr)
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, newYork).Unix()

	_, err := SummarizeCreditUsageDaily(7704, start, time.Date(2027, time.January, 2, 0, 0, 0, 0, newYork).Unix(), "America/New_York")
	require.Nil(t, err, "366 个日历日是合法范围")

	tooLong := time.Date(2027, time.January, 2, 23, 0, 0, 0, newYork).Unix()
	require.Equal(t, int64(366), (tooLong-start)/86400, "前提：旧的秒数整除口径会放行它")
	_, err = SummarizeCreditUsageDaily(7704, start, tooLong, "America/New_York")
	require.NotNil(t, err, "超长区间必须直接拒绝，而不是慢慢扫全表")
	assert.Equal(t, 400, err.HTTPStatus)
	assert.Equal(t, CreditErrorInvalidRequest, err.Code)
}
