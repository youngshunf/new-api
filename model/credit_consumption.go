package model

import (
	"fmt"

	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
)

// 存量余额 rebase 需要的「真实消费」汇总（doc94 R1）。
//
// 为什么由 NewAPI 出这个数：请求日志在 NewAPI 这边，NewAPI 是消费的唯一权威。
// Cloud 早已与 NewAPI 的库解耦，只能通过内部接口来问，不能自己去翻日志表。
//
// **关键在于诚实**：从 doc94 N3 起，每条消费日志都会把两个资金池的实际扣减分别写进
// other（funding_subscription_part / funding_wallet_part）。而 N3 之前的历史日志没有
// 这个拆分。本汇总把「拆得出来的」和「拆不出来的」分开返回，**绝不**把拆不出来的部分
// 按比例摊派或猜成某一个池——rebase 会把结果当成用户余额写回去，猜错就是真金白银的错。

// CreditConsumptionSummary 是单个用户的历史消费拆分汇总。
type CreditConsumptionSummary struct {
	NewApiUserId int `json:"newapi_user_id"`
	// WalletConsumedQuota 明确记在永久钱包上的消费（quota 单位）
	WalletConsumedQuota int64 `json:"wallet_consumed_quota"`
	// SubscriptionConsumedQuota 明确记在订阅周期额度上的消费
	SubscriptionConsumedQuota int64 `json:"subscription_consumed_quota"`
	// UnattributedQuota 日志里没有资金池拆分明细、无法判定来源的消费
	UnattributedQuota int64 `json:"unattributed_quota"`
	// UnattributedCount 上述日志条数，便于人工复核时定位
	UnattributedCount int64 `json:"unattributed_count"`
	// HasSubscriptionHistory 该用户是否曾经有过订阅
	HasSubscriptionHistory bool `json:"has_subscription_history"`
	// Determinate 为 true 表示 WalletConsumedQuota 可直接用于 rebase；
	// 为 false 表示存在无法归因的消费，调用方必须把该用户放进人工清单。
	Determinate bool `json:"determinate"`
	// IndeterminateReason 在 Determinate=false 时说明原因
	IndeterminateReason string `json:"indeterminate_reason,omitempty"`

	// 以下是同一批数字的积分表示。quota↔credit 的换算只在 NewAPI 这一侧做：
	// NewAPI 是积分权威，云端不该再持有换算常量（doc94 §0.4）。
	WalletConsumedCredits       string `json:"wallet_consumed_credits"`
	SubscriptionConsumedCredits string `json:"subscription_consumed_credits"`
	UnattributedCredits         string `json:"unattributed_credits"`
}

// fillCreditFields 把 quota 汇总换算成积分字符串。
func (s *CreditConsumptionSummary) fillCreditFields() {
	s.WalletConsumedCredits = common.FormatQuotaAsCredits(s.WalletConsumedQuota)
	s.SubscriptionConsumedCredits = common.FormatQuotaAsCredits(s.SubscriptionConsumedQuota)
	s.UnattributedCredits = common.FormatQuotaAsCredits(s.UnattributedQuota)
}

// consumptionScanBatch 是日志分批扫描的批量大小。
// rebase 是一次性离线任务，宁可多几轮往返，也不要一次把海量日志读进内存。
const consumptionScanBatch = 2000

// SummarizeCreditConsumption 汇总某个用户的历史消费来源拆分。
//
// 扫描该用户全部消费日志，按 other 里的资金池拆分归因；拆分缺失的记入
// UnattributedQuota。仅当「不存在无法归因的消费」或「该用户从未有过订阅
// （历史消费必然全部来自永久钱包）」时，Determinate 才为 true。
func SummarizeCreditConsumption(userId int) (*CreditConsumptionSummary, error) {
	if userId <= 0 {
		return nil, fmt.Errorf("newapi_user_id must be a positive integer")
	}

	var subscriptionCount int64
	if err := DB.Model(&UserSubscription{}).Where("user_id = ?", userId).Count(&subscriptionCount).Error; err != nil {
		return nil, fmt.Errorf("count subscriptions: %w", err)
	}

	summary := &CreditConsumptionSummary{
		NewApiUserId:           userId,
		HasSubscriptionHistory: subscriptionCount > 0,
	}

	type logRow struct {
		Id    int
		Quota int
		Other string
	}

	lastId := 0
	for {
		var rows []logRow
		query := LOG_DB.Model(&Log{}).
			Select("id", "quota", "other").
			Where("user_id = ? AND type = ? AND id > ?", userId, LogTypeConsume, lastId).
			Order("id asc").
			Limit(consumptionScanBatch)
		if err := query.Scan(&rows).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				break
			}
			return nil, fmt.Errorf("scan consumption logs: %w", err)
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			lastId = row.Id
			subPart, walletPart, attributed := parseFundingParts(row.Other)
			if !attributed {
				summary.UnattributedQuota += int64(row.Quota)
				summary.UnattributedCount++
				continue
			}
			summary.SubscriptionConsumedQuota += subPart
			summary.WalletConsumedQuota += walletPart
		}
		if len(rows) < consumptionScanBatch {
			break
		}
	}

	switch {
	case summary.UnattributedQuota == 0:
		summary.Determinate = true
	case !summary.HasSubscriptionHistory:
		// 从未有过订阅，历史消费必然全部出自永久钱包——这不是猜测，是唯一可能。
		summary.WalletConsumedQuota += summary.UnattributedQuota
		summary.UnattributedQuota = 0
		summary.Determinate = true
	default:
		summary.Determinate = false
		summary.IndeterminateReason = fmt.Sprintf(
			"%d 条历史日志缺少资金池拆分明细（合计 %d quota），且该用户曾有订阅，无法判定这部分消费出自哪个池",
			summary.UnattributedCount, summary.UnattributedQuota)
	}

	summary.fillCreditFields()
	return summary, nil
}

// parseFundingParts 从日志 other 中读出两个资金池的实际扣减。
//
// 第三个返回值为 false 表示这条日志没有拆分明细（N3 之前的历史日志），
// 调用方必须把它计入「无法归因」，而不是当成 0 消费。
func parseFundingParts(other string) (subscriptionPart int64, walletPart int64, attributed bool) {
	if other == "" {
		return 0, 0, false
	}
	var payload map[string]any
	if err := common.Unmarshal([]byte(other), &payload); err != nil {
		return 0, 0, false
	}
	subPart, hasSub := numericField(payload, "funding_subscription_part")
	walletPartValue, hasWallet := numericField(payload, "funding_wallet_part")
	if !hasSub && !hasWallet {
		return 0, 0, false
	}
	return subPart, walletPartValue, true
}

// numericField 读取 JSON 数字字段。JSON 反序列化出来的数字是 float64，
// 这里统一转成 int64（quota 是整数，不存在精度问题）。
func numericField(payload map[string]any, key string) (int64, bool) {
	raw, ok := payload[key]
	if !ok {
		return 0, false
	}
	switch value := raw.(type) {
	case float64:
		return int64(value), true
	case int64:
		return value, true
	case int:
		return int64(value), true
	default:
		return 0, false
	}
}
