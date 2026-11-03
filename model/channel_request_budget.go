package model

import (
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 渠道每期调用上限：计数在 channel_request_budget_usages（V006），一渠道一期一行；
// 选择渠道时只读本实例内存快照，快照随渠道缓存同步从库里重载，本实例每次加一同时更新。

const channelRequestBudgetUsageTable = "channel_request_budget_usages"

// channelRequestBudgetLocation 是分期所用的北京时间（固定 UTC+8，无夏令时）。
var channelRequestBudgetLocation = time.FixedZone("CST", 8*3600)

type channelRequestBudgetUsageKey struct {
	channelID int
	periodKey string
}

var (
	channelRequestBudgetUsageLock sync.RWMutex
	channelRequestBudgetUsages    = map[channelRequestBudgetUsageKey]int64{}
)

// ChannelRequestBudgetPeriodKey 返回 now 所在北京时间分期的键：day 为 2026-11-03，month 为 2026-11。
func ChannelRequestBudgetPeriodKey(period string, now time.Time) string {
	local := now.In(channelRequestBudgetLocation)
	if period == dto.ChannelRequestBudgetPeriodDay {
		return local.Format("2006-01-02")
	}
	return local.Format("2006-01")
}

// channelRequestBudget 读渠道的每期调用上限；未配置或配置无效（保存时已校验）即不限。
// 选择时在渠道缓存读锁内对每个候选调用，所以先做字符串预判，绝大多数渠道不解析 JSON，
// 且不走 GetSetting（解析失败时会写库）。
func channelRequestBudget(channel *Channel) *dto.ChannelRequestBudget {
	if channel == nil || channel.Setting == nil || !strings.Contains(*channel.Setting, "request_budget") {
		return nil
	}
	var setting struct {
		RequestBudget *dto.ChannelRequestBudget `json:"request_budget"`
	}
	if err := common.UnmarshalJsonStr(*channel.Setting, &setting); err != nil || setting.RequestBudget.Validate() != nil {
		return nil
	}
	return setting.RequestBudget
}

// RecordChannelRequestBudgetUse 在上游成功答复后为渠道当期计数加一；渠道无上限时不做事。
// 写库失败只记日志，不影响本次请求。
func RecordChannelRequestBudgetUse(channel *Channel) {
	budget := channelRequestBudget(channel)
	if budget == nil {
		return
	}
	now := time.Now()
	key := channelRequestBudgetUsageKey{channelID: channel.Id, periodKey: ChannelRequestBudgetPeriodKey(budget.Period, now)}
	// 单条 upsert：PostgreSQL/SQLite 渲染为 ON CONFLICT ... DO UPDATE，MySQL 为 ON DUPLICATE KEY UPDATE。
	err := DB.Table(channelRequestBudgetUsageTable).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "channel_id"}, {Name: "period_key"}},
		DoUpdates: clause.Assignments(map[string]any{
			"used_count":   gorm.Expr(channelRequestBudgetUsageTable + ".used_count + 1"),
			"updated_time": DatabaseTime{Time: now},
		}),
	}).Create(map[string]any{
		"channel_id":   key.channelID,
		"period_key":   key.periodKey,
		"used_count":   1,
		"updated_time": DatabaseTime{Time: now},
	}).Error
	if err != nil {
		common.SysError(common.LogText("failed to record channel request budget use: channel_id=%d, period=%s, error=%v", key.channelID, key.periodKey, err))
		return
	}
	// 回读库里的计数，一并收进其他实例的用量；回读失败时只按本次加一。
	var used []int64
	readErr := DB.Table(channelRequestBudgetUsageTable).
		Where("channel_id = ? AND period_key = ?", key.channelID, key.periodKey).
		Pluck("used_count", &used).Error
	channelRequestBudgetUsageLock.Lock()
	defer channelRequestBudgetUsageLock.Unlock()
	if readErr == nil && len(used) == 1 {
		channelRequestBudgetUsages[key] = max(channelRequestBudgetUsages[key], used[0])
		return
	}
	channelRequestBudgetUsages[key]++
}

// ChannelRequestBudgetExhausted 判断渠道在 now 所在分期是否已用满上限；只读内存快照，
// 可在渠道缓存读锁内调用。
func ChannelRequestBudgetExhausted(channel *Channel, now time.Time) bool {
	budget := channelRequestBudget(channel)
	if budget == nil {
		return false
	}
	key := channelRequestBudgetUsageKey{channelID: channel.Id, periodKey: ChannelRequestBudgetPeriodKey(budget.Period, now)}
	channelRequestBudgetUsageLock.RLock()
	used := channelRequestBudgetUsages[key]
	channelRequestBudgetUsageLock.RUnlock()
	return used >= budget.Limit
}

// reloadChannelRequestBudgetUsages 从库里重载当前分期（北京时间今日与本月）的计数快照。
// 同一分期计数只增不减，所以与快照取大，避免覆盖重载期间本实例刚加的一次。
func reloadChannelRequestBudgetUsages(now time.Time) {
	dayKey := ChannelRequestBudgetPeriodKey(dto.ChannelRequestBudgetPeriodDay, now)
	monthKey := ChannelRequestBudgetPeriodKey(dto.ChannelRequestBudgetPeriodMonth, now)
	var rows []struct {
		ChannelId int
		PeriodKey string
		UsedCount int64
	}
	err := DB.Table(channelRequestBudgetUsageTable).
		Select("channel_id, period_key, used_count").
		Where("period_key IN ?", []string{dayKey, monthKey}).
		Scan(&rows).Error
	if err != nil {
		common.SysError(common.LogText("failed to reload channel request budget usages: %v", err))
		return
	}
	loaded := make(map[channelRequestBudgetUsageKey]int64, len(rows))
	for _, row := range rows {
		loaded[channelRequestBudgetUsageKey{channelID: row.ChannelId, periodKey: row.PeriodKey}] = row.UsedCount
	}
	channelRequestBudgetUsageLock.Lock()
	defer channelRequestBudgetUsageLock.Unlock()
	for key, used := range channelRequestBudgetUsages {
		if key.periodKey == dayKey || key.periodKey == monthKey {
			loaded[key] = max(loaded[key], used)
		}
	}
	channelRequestBudgetUsages = loaded
}
