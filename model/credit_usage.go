package model

import (
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
)

// 积分用量读接缝（doc94 D1）。
//
// Cloud 过去自己维护一张 credit_transaction 流水表，并用 `quota / NEWAPI_QUOTA_PER_DOLLAR`
// 把 NewAPI 的 quota 换算成积分——于是「一次调用扣了多少积分」在两侧各有一套算法，
// 两套一旦不一致，用户看到的流水就和真实扣费对不上。
//
// D1 之后 Cloud 不再持有流水表，也不再持有换算常量：用量与日聚合都从这里读，
// **金额一律由 NewAPI 换算成积分字符串**，Cloud 原样透传。

// CreditUsageEntry 是一条消费流水（面向用户展示）。
type CreditUsageEntry struct {
	Id          int    `json:"id"`
	CreatedTime int64  `json:"created_time"`
	ModelName   string `json:"model_name"`
	TokenName   string `json:"token_name"`
	// Credits 是本次消费的积分数（正数表示消耗）
	Credits          string `json:"credits"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	UseTime          int    `json:"use_time"`
	IsStream         bool   `json:"is_stream"`
	// FundingSource 资金来源：subscription / wallet / composite；历史日志可能为空
	FundingSource string `json:"funding_source,omitempty"`
}

// CreditUsagePage 是分页后的流水。
type CreditUsagePage struct {
	Items      []CreditUsageEntry `json:"items"`
	Total      int64              `json:"total"`
	Page       int                `json:"page"`
	Size       int                `json:"size"`
	MeasuredAt string             `json:"measured_at"`
}

// CreditUsageDailyEntry 是按本地日聚合的消费。
type CreditUsageDailyEntry struct {
	// Day 是响应 timezone 下的日历日，YYYY-MM-DD
	Day string `json:"day"`
	// ConsumedCredits 当日消耗积分（正数）
	ConsumedCredits string `json:"consumed_credits"`
	RequestCount    int    `json:"request_count"`
	TokenCount      int    `json:"token_count"`
}

// CreditUsageDaily 是日聚合结果。
type CreditUsageDaily struct {
	// Timezone 回显实际分桶所用的 IANA 时区
	Timezone   string                  `json:"timezone"`
	Items      []CreditUsageDailyEntry `json:"items"`
	MeasuredAt string                  `json:"measured_at"`
}

const (
	// creditUsageMaxSize 单页上限，防止一次拉爆内存
	creditUsageMaxSize = 100
	// creditUsageDailyMaxDays 日聚合最大跨度（timezone 下的日历日数），超过直接 400
	creditUsageDailyMaxDays = 366
)

// ListCreditUsage 分页读某用户的消费流水。
//
// start/end 为 Unix 秒，0 表示不限。金额已换算成积分字符串，调用方不做算术。
func ListCreditUsage(userId int, start, end int64, page, size int) (*CreditUsagePage, error) {
	if userId <= 0 {
		return nil, fmt.Errorf("newapi_user_id must be a positive integer")
	}
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 20
	}
	if size > creditUsageMaxSize {
		size = creditUsageMaxSize
	}

	// 每次都从头构造查询：GORM 的链式条件会累积在同一个 Statement 上，
	// 复用同一个 *gorm.DB 做 Count + Find 会把条件叠进去，查出来的不是同一批。
	base := func() *gorm.DB {
		q := LOG_DB.Model(&Log{}).Where("user_id = ? AND type = ?", userId, LogTypeConsume)
		if start > 0 {
			q = q.Where("created_at >= ?", start)
		}
		if end > 0 {
			q = q.Where("created_at <= ?", end)
		}
		return q
	}

	var total int64
	if err := base().Count(&total).Error; err != nil {
		return nil, fmt.Errorf("count usage logs: %w", err)
	}

	var rows []Log
	if err := base().Order("id desc").Offset((page - 1) * size).Limit(size).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list usage logs: %w", err)
	}

	items := make([]CreditUsageEntry, 0, len(rows))
	for _, row := range rows {
		items = append(items, CreditUsageEntry{
			Id:               row.Id,
			CreatedTime:      row.CreatedAt,
			ModelName:        row.ModelName,
			TokenName:        row.TokenName,
			Credits:          common.FormatQuotaAsCredits(int64(row.Quota)),
			PromptTokens:     row.PromptTokens,
			CompletionTokens: row.CompletionTokens,
			UseTime:          row.UseTime,
			IsStream:         row.IsStream,
			FundingSource:    fundingSourceOf(row.Other),
		})
	}

	return &CreditUsagePage{
		Items:      items,
		Total:      total,
		Page:       page,
		Size:       size,
		MeasuredAt: time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// SummarizeCreditUsageDaily 按调用方时区的日历日聚合消费。
//
// timezone 是 IANA 时区名（如 Asia/Shanghai），由本函数用 time.LoadLocation 校验——
// 统计链上只有这里校验时区名（统一时间设计 §4.4 冻结基线），上游 daemon 与云端只判非空。
// 每条记录按**它自己那一刻**的历史规则换算日界，不能用查询当下的固定偏移（DST 会错划日期）。
//
// start/end 为 Unix 秒、半开区间 [start,end)，0 表示该侧不限；
// 两侧都给出时按 timezone 下的日历日数限窗。
func SummarizeCreditUsageDaily(userId int, start, end int64, timezone string) (*CreditUsageDaily, *CreditOperationError) {
	if userId <= 0 {
		return nil, creditError(http.StatusBadRequest, CreditErrorInvalidRequest, false, "newapi_user_id must be a positive integer")
	}
	// 空串在 LoadLocation 里是 UTC、"Local" 是进程所在时区：都不是调用方给出的 IANA 名，直接拒绝，不回落。
	if timezone == "" || timezone == "Local" {
		return nil, creditError(http.StatusBadRequest, CreditErrorInvalidRequest, false, "timezone must be an IANA time zone name")
	}
	zone, loadErr := time.LoadLocation(timezone)
	if loadErr != nil {
		return nil, creditError(http.StatusBadRequest, CreditErrorInvalidRequest, false,
			"timezone %q is not a known IANA time zone", timezone)
	}
	// 限窗数的是 [start,end) 在 timezone 下触及的日历日，不是 (end-start)/86400：
	// 后者既会放行跨到第 367 个日历日的范围，也受 23/25 小时的 DST 日影响。
	// 两个 civil 日期都落到 UTC 午夜再相减，UTC 没有 DST，差值恰为整日。
	if start > 0 && end > start {
		firstYear, firstMonth, firstDay := time.Unix(start, 0).In(zone).Date()
		lastYear, lastMonth, lastDay := time.Unix(end-1, 0).In(zone).Date()
		first := time.Date(firstYear, firstMonth, firstDay, 0, 0, 0, 0, time.UTC)
		last := time.Date(lastYear, lastMonth, lastDay, 0, 0, 0, 0, time.UTC)
		if days := int(last.Sub(first).Hours()/24) + 1; days > creditUsageDailyMaxDays {
			return nil, creditError(http.StatusBadRequest, CreditErrorInvalidRequest, false,
				"time range covers %d calendar days in %s, exceeds %d", days, timezone, creditUsageDailyMaxDays)
		}
	}

	// 同上：每轮重建查询，避免链式条件在同一个 Statement 上累积。
	batchQuery := func(afterId int) *gorm.DB {
		q := LOG_DB.Model(&Log{}).
			Select("id", "created_at", "quota", "prompt_tokens", "completion_tokens").
			Where("user_id = ? AND type = ? AND id > ?", userId, LogTypeConsume, afterId)
		if start > 0 {
			q = q.Where("created_at >= ?", start)
		}
		if end > 0 {
			q = q.Where("created_at < ?", end)
		}
		return q.Order("id asc").Limit(consumptionScanBatch)
	}

	type usageRow struct {
		Id               int
		CreatedAt        int64
		Quota            int
		PromptTokens     int
		CompletionTokens int
	}

	type bucket struct {
		quota    int64
		requests int
		tokens   int
	}
	acc := map[string]*bucket{}

	lastId := 0
	for {
		var rows []usageRow
		if err := batchQuery(lastId).Scan(&rows).Error; err != nil {
			return nil, creditError(http.StatusServiceUnavailable, CreditErrorStoreUnavailable, true, "scan usage logs: %s", err.Error())
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			lastId = row.Id
			day := time.Unix(row.CreatedAt, 0).In(zone).Format("2006-01-02")
			entry, ok := acc[day]
			if !ok {
				entry = &bucket{}
				acc[day] = entry
			}
			entry.quota += int64(row.Quota)
			entry.requests++
			entry.tokens += row.PromptTokens + row.CompletionTokens
		}
		if len(rows) < consumptionScanBatch {
			break
		}
	}

	items := make([]CreditUsageDailyEntry, 0, len(acc))
	for day, entry := range acc {
		items = append(items, CreditUsageDailyEntry{
			Day:             day,
			ConsumedCredits: common.FormatQuotaAsCredits(entry.quota),
			RequestCount:    entry.requests,
			TokenCount:      entry.tokens,
		})
	}
	// 按日倒序：展示侧总是先看最近的
	sort.Slice(items, func(i, j int) bool { return items[i].Day > items[j].Day })

	return &CreditUsageDaily{Timezone: timezone, Items: items, MeasuredAt: time.Now().UTC().Format(time.RFC3339)}, nil
}

// fundingSourceOf 从日志 other 取资金来源标记；历史日志没有该字段时返回空串。
func fundingSourceOf(other string) string {
	if other == "" {
		return ""
	}
	var payload map[string]any
	if err := common.Unmarshal([]byte(other), &payload); err != nil {
		return ""
	}
	if value, ok := payload["billing_source"].(string); ok {
		return value
	}
	return ""
}
