package model

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChannelRequestBudgetPeriodKeyUsesBeijingCalendar(t *testing.T) {
	tests := []struct {
		name   string
		period string
		now    time.Time
		want   string
	}{
		{name: "day inside Beijing day", period: "day", now: time.Date(2026, 11, 3, 2, 0, 0, 0, time.UTC), want: "2026-11-03"},
		{name: "day crosses to next Beijing day at 16:00 UTC", period: "day", now: time.Date(2026, 11, 3, 16, 0, 0, 0, time.UTC), want: "2026-11-04"},
		{name: "day just before Beijing midnight", period: "day", now: time.Date(2026, 11, 3, 15, 59, 59, 0, time.UTC), want: "2026-11-03"},
		{name: "month end UTC is next Beijing month", period: "month", now: time.Date(2026, 10, 31, 16, 30, 0, 0, time.UTC), want: "2026-11"},
		{name: "month end UTC is next Beijing day", period: "day", now: time.Date(2026, 10, 31, 16, 30, 0, 0, time.UTC), want: "2026-11-01"},
		{name: "month before Beijing midnight stays", period: "month", now: time.Date(2026, 10, 31, 15, 59, 59, 0, time.UTC), want: "2026-10"},
		{name: "year boundary", period: "month", now: time.Date(2026, 12, 31, 16, 0, 0, 0, time.UTC), want: "2027-01"},
		{name: "non-UTC input location", period: "day", now: time.Date(2026, 11, 3, 23, 30, 0, 0, time.FixedZone("PST", -8*3600)), want: "2026-11-04"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ChannelRequestBudgetPeriodKey(tt.period, tt.now))
		})
	}
}

// 用满当期上限的高优先级渠道不再被选中（首选与重试都落到下一档），且计数来自库里的单条 upsert，
// 渠道缓存同步时从库里重载快照。内存缓存与数据库两条选择路径同一判定。
func TestChannelRequestBudgetExhaustedChannelFallsToNextPriority(t *testing.T) {
	truncateTables(t)
	originalMemoryCache := common.MemoryCacheEnabled
	t.Cleanup(func() {
		common.MemoryCacheEnabled = originalMemoryCache
		DB.Exec("DELETE FROM " + channelRequestBudgetUsageTable)
		channelRequestBudgetUsageLock.Lock()
		channelRequestBudgetUsages = map[channelRequestBudgetUsageKey]int64{}
		channelRequestBudgetUsageLock.Unlock()
		InitChannelCache()
	})
	high, low := int64(10), int64(0)
	budget := `{"request_budget":{"period":"month","limit":2}}`
	free := Channel{Id: 940001, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Name: "free", Models: "web-search", Group: "default", Priority: &high, Setting: &budget}
	paid := Channel{Id: 940002, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Name: "paid", Models: "web-search", Group: "default", Priority: &low}
	require.NoError(t, free.Insert())
	require.NoError(t, paid.Insert())

	selectByPath := func(retry int) map[bool]string {
		names := map[bool]string{}
		for _, memoryCache := range []bool{false, true} {
			common.MemoryCacheEnabled = memoryCache
			InitChannelCache()
			selected, err := GetRandomSatisfiedChannel("default", "web-search", retry, nil)
			require.NoError(t, err)
			require.NotNil(t, selected, "memory cache %v", memoryCache)
			names[memoryCache] = selected.Name
		}
		return names
	}

	assert.Equal(t, map[bool]string{false: "free", true: "free"}, selectByPath(0))
	RecordChannelRequestBudgetUse(&free)
	assert.Equal(t, map[bool]string{false: "free", true: "free"}, selectByPath(0), "未用满仍优先免费渠道")

	RecordChannelRequestBudgetUse(&free)
	RecordChannelRequestBudgetUse(&paid) // 无上限的渠道不计数
	assert.True(t, ChannelRequestBudgetExhausted(&free, time.Now()))
	assert.False(t, ChannelRequestBudgetExhausted(&paid, time.Now()))
	var rows []struct {
		ChannelId int
		PeriodKey string
		UsedCount int64
	}
	require.NoError(t, DB.Table(channelRequestBudgetUsageTable).Select("channel_id, period_key, used_count").Scan(&rows).Error)
	require.Len(t, rows, 1)
	assert.Equal(t, 940001, rows[0].ChannelId)
	assert.Equal(t, ChannelRequestBudgetPeriodKey("month", time.Now()), rows[0].PeriodKey)
	assert.EqualValues(t, 2, rows[0].UsedCount)

	// 用满后首选与重试都落到下一档；分档在剔除之后计算，所以重试档位不会越过付费渠道。
	assert.Equal(t, map[bool]string{false: "paid", true: "paid"}, selectByPath(0))
	assert.Equal(t, map[bool]string{false: "paid", true: "paid"}, selectByPath(1))
	ok, kind := ChannelSatisfiesFilters(&free, "web-search", nil)
	assert.False(t, ok, "置顶与亲和渠道同样受上限约束")
	assert.Equal(t, dto.FilterRequestBudget, kind)
	channelSyncLock.RLock()
	kept, emptiedBy := filterCandidateIDs([]int{940001}, "web-search", nil)
	channelSyncLock.RUnlock()
	assert.Empty(t, kept)
	assert.Equal(t, dto.FilterRequestBudget, emptiedBy)

	// 新实例或重启后快照为空：渠道缓存同步从库里重载当期计数。
	channelRequestBudgetUsageLock.Lock()
	channelRequestBudgetUsages = map[channelRequestBudgetUsageKey]int64{}
	channelRequestBudgetUsageLock.Unlock()
	assert.False(t, ChannelRequestBudgetExhausted(&free, time.Now()))
	InitChannelCache()
	assert.True(t, ChannelRequestBudgetExhausted(&free, time.Now()))

	// 下一期自动恢复。
	nextMonth := time.Now().In(channelRequestBudgetLocation).AddDate(0, 1, 0)
	assert.False(t, ChannelRequestBudgetExhausted(&free, nextMonth))
}
