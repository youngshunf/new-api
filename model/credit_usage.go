package model

import (
	"fmt"
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
	Id        int    `json:"id"`
	CreatedAt int64  `json:"created_at"`
	ModelName string `json:"model_name"`
	TokenName string `json:"token_name"`
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
	Day string `json:"day"`
	// ConsumedCredits 当日消耗积分（正数）
	ConsumedCredits string `json:"consumed_credits"`
	RequestCount    int    `json:"request_count"`
	TokenCount      int    `json:"token_count"`
}

// CreditUsageDaily 是日聚合结果。
type CreditUsageDaily struct {
	Items      []CreditUsageDailyEntry `json:"items"`
	MeasuredAt string                  `json:"measured_at"`
}

const (
	// creditUsageMaxSize 单页上限，防止一次拉爆内存
	creditUsageMaxSize = 100
	// creditUsageDailyMaxDays 日聚合最大跨度，超过直接 400
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
			CreatedAt:        row.CreatedAt,
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

// SummarizeCreditUsageDaily 按本地日聚合消费。
//
// tzOffsetMinutes 是调用方所在时区相对 UTC 的分钟偏移（如 Asia/Shanghai 为 480）。
// 日边界由**调用方的时区**决定：把它交给 NewAPI 计算，两侧就不会各切各的日。
func SummarizeCreditUsageDaily(userId int, start, end int64, tzOffsetMinutes int) (*CreditUsageDaily, error) {
	if userId <= 0 {
		return nil, fmt.Errorf("newapi_user_id must be a positive integer")
	}
	if start > 0 && end > 0 {
		if days := (end - start) / 86400; days > creditUsageDailyMaxDays {
			return nil, fmt.Errorf("time range exceeds %d days", creditUsageDailyMaxDays)
		}
	}
	zone := time.FixedZone("caller", tzOffsetMinutes*60)

	// 同上：每轮重建查询，避免链式条件在同一个 Statement 上累积。
	batchQuery := func(afterId int) *gorm.DB {
		q := LOG_DB.Model(&Log{}).
			Select("id", "created_at", "quota", "prompt_tokens", "completion_tokens").
			Where("user_id = ? AND type = ? AND id > ?", userId, LogTypeConsume, afterId)
		if start > 0 {
			q = q.Where("created_at >= ?", start)
		}
		if end > 0 {
			q = q.Where("created_at <= ?", end)
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
			return nil, fmt.Errorf("scan usage logs: %w", err)
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

	return &CreditUsageDaily{Items: items, MeasuredAt: time.Now().UTC().Format(time.RFC3339)}, nil
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
