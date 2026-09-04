package common

import (
	"fmt"
	"math"

	"github.com/shopspring/decimal"
)

// Quota conversions are centralized here so every billing path shares one
// saturation + logging policy. Per-request amounts are bounded by int32
// because the per-request columns (logs.quota, tokens.remain_quota) are
// 32-bit integers, so an oversized product must clamp to the int32 range
// instead of wrapping around and turning a charge into a credit.
// Top-ups and wallet-priced purchases use a JavaScript-safe 64-bit domain
// (MaxWalletQuota).
//
// 注意：users.quota（永久钱包余额）不受 int32 上限约束——生产 PostgreSQL 里它是
// 64 位列，余额合法地可以超过 MaxInt32（实测 2,500,915,158）。曾把 MaxQuota 当成
// 余额上限，导致主账号一切钱包预扣 500 而分身全部不可用。
// 钱包余额的守卫只做 int64 防回绕，见 WalletQuotaTarget。
const (
	MaxQuota       = math.MaxInt32
	MinQuota       = math.MinInt32
	MaxWalletQuota = 1<<53 - 1
)

// WalletQuotaTarget 返回钱包余额 current+delta 的目标值。
// users.quota 是 64 位列，唯一要防的是 int64 加法回绕（Go 有符号溢出是定义
// 良好的回绕，因此用方向比较判定），不做任何业务上限截断。
//
// ⚠️ 它与上游的 ValidateWalletQuota 守的**不是同一件事**，两条都要留：
// 本函数守「余额加减不得回绕」，作用在扣费/退款路径；ValidateWalletQuota 守
// 「录入值不得超过 JS 安全整数」，作用在充值/兑换/管理员直接设值。
// MaxWalletQuota = 1<<53-1 远高于当年锁死主账号的 2,500,915,158，不会复现那次事故。
func WalletQuotaTarget(current, delta int64) (target int64, overflow bool) {
	target = current + delta
	if (delta > 0 && target < current) || (delta < 0 && target > current) {
		return 0, true
	}
	return target, false
}

// ValidateWalletQuota enforces the upper bound shared by wallet mutations.
// Negative balances remain valid because billing can temporarily overdraw a
// wallet; callers that accept credits must apply their own positive check.
func ValidateWalletQuota(quota int) error {
	if quota > MaxWalletQuota {
		return fmt.Errorf("wallet quota exceeds %d", MaxWalletQuota)
	}
	return nil
}

// QuotaClampKind identifies why a quota conversion had to be saturated.
type QuotaClampKind string

// Clamp kinds reported by QuotaClamp.Kind.
const (
	QuotaClampOverflow  QuotaClampKind = "overflow"
	QuotaClampUnderflow QuotaClampKind = "underflow"
	QuotaClampNaN       QuotaClampKind = "nan"
)

// QuotaClamp describes a single saturation event: a quota conversion whose
// input fell outside its supported range (or was NaN) and was
// therefore clamped. It is surfaced to billing callers so the event can be
// recorded on the related consume/task log for admin auditing.
type QuotaClamp struct {
	Op       string         `json:"op"`       // "QuotaFromFloat" | "QuotaRound" | "QuotaFromDecimal" | "WalletQuotaFromDecimal"
	Kind     QuotaClampKind `json:"kind"`     // "overflow" | "underflow" | "nan"
	Original float64        `json:"original"` // best-effort pre-clamp value (decimal -> float64 approx)
	Clamped  int            `json:"clamped"`  // the saturated result actually used
}

// Error lets the same typed value serve both as the settlement audit marker
// and as the fail-fast error returned by strict pre-consume conversions.
func (c *QuotaClamp) Error() string {
	if c == nil {
		return ""
	}
	return fmt.Sprintf("quota conversion (%s) %s: original=%g, clamped=%d", c.Op, c.Kind, c.Original, c.Clamped)
}

// AuditMap renders the clamp as the marker stored under a log's
// admin_info.quota_saturation. Centralized here so every billing path (consume
// logs, task billing logs, task compensation logs) records the same shape.
func (c *QuotaClamp) AuditMap() map[string]interface{} {
	if c == nil {
		return nil
	}
	return map[string]interface{}{
		"op":       c.Op,
		"kind":     c.Kind,
		"original": c.Original,
		"clamped":  c.Clamped,
	}
}

// saturateQuota converts an already-rounded single-request quota to int.
// Whenever clamping (what would otherwise be an integer wraparound) or a NaN
// fallback is triggered it logs a warning, because in
// normal operation a single request never approaches these bounds — hitting
// them signals a bug or an abusive request. `op` names the caller. When a
// clamp occurs it returns a non-nil *QuotaClamp so callers can additionally
// record the event (e.g. on the consume log); the returned pointer is nil for
// in-range values.
func saturateQuota(value float64, op string) (int, *QuotaClamp) {
	return saturateQuotaBounded(value, op, MaxQuota, MinQuota)
}

func saturateQuotaBounded(value float64, op string, maxQuota int, minQuota int) (int, *QuotaClamp) {
	var clamp *QuotaClamp
	switch {
	case math.IsNaN(value):
		clamp = &QuotaClamp{Op: op, Kind: QuotaClampNaN, Original: value, Clamped: 0}
	case value > float64(maxQuota):
		clamp = &QuotaClamp{Op: op, Kind: QuotaClampOverflow, Original: value, Clamped: maxQuota}
	case value < float64(minQuota):
		clamp = &QuotaClamp{Op: op, Kind: QuotaClampUnderflow, Original: value, Clamped: minQuota}
	default:
		return int(value), nil
	}
	SysError(clamp.Error())
	return clamp.Clamped, clamp
}

func strictQuota(quota int, clamp *QuotaClamp) (int, error) {
	if clamp != nil {
		return 0, clamp
	}
	return quota, nil
}

// QuotaFromFloat converts a computed quota value to int, truncating toward
// zero, with saturation. Use for float products of prices, ratios, and
// user-controlled multipliers (image n, video seconds, resolution ratios).
func QuotaFromFloat(value float64) int {
	quota, _ := QuotaFromFloatChecked(value)
	return quota
}

// QuotaFromFloatChecked is QuotaFromFloat but also returns a non-nil
// *QuotaClamp when the value was clamped, so billing callers can audit it.
func QuotaFromFloatChecked(value float64) (int, *QuotaClamp) {
	return saturateQuota(value, "QuotaFromFloat")
}

// QuotaFromFloatStrict converts an in-range value and returns a typed
// *QuotaClamp error instead of allowing a saturated result to reach billing.
func QuotaFromFloatStrict(value float64) (int, error) {
	return strictQuota(QuotaFromFloatChecked(value))
}

// QuotaRound converts a float64 quota value to int using half-away-from-zero
// rounding, with saturation. Every tiered billing path (pre-consume,
// settlement, breakdown validation, log fields) MUST use this to avoid +-1
// discrepancies.
func QuotaRound(value float64) int {
	quota, _ := QuotaRoundChecked(value)
	return quota
}

// QuotaRoundChecked is QuotaRound but also returns a non-nil *QuotaClamp when
// the value was clamped, so billing callers can audit it.
func QuotaRoundChecked(value float64) (int, *QuotaClamp) {
	return saturateQuota(math.Round(value), "QuotaRound")
}

// QuotaRoundStrict rounds an in-range value and returns a typed *QuotaClamp
// error instead of allowing a saturated result to reach billing.
func QuotaRoundStrict(value float64) (int, error) {
	return strictQuota(QuotaRoundChecked(value))
}

// QuotaFromDecimal converts a computed quota decimal to int with saturation.
// The decimal is rounded (half away from zero) before conversion.
func QuotaFromDecimal(d decimal.Decimal) int {
	quota, _ := QuotaFromDecimalChecked(d)
	return quota
}

// QuotaFromDecimalChecked is QuotaFromDecimal but also returns a non-nil
// *QuotaClamp when the value was clamped, so billing callers can audit it.
func QuotaFromDecimalChecked(d decimal.Decimal) (int, *QuotaClamp) {
	f, _ := d.Round(0).Float64()
	return saturateQuota(f, "QuotaFromDecimal")
}

// QuotaFromDecimalStrict converts an in-range single-request quota and rejects
// a value that would otherwise be saturated at the int32 boundary.
func QuotaFromDecimalStrict(d decimal.Decimal) (int, error) {
	return strictQuota(QuotaFromDecimalChecked(d))
}

// WalletQuotaFromDecimalStrict converts wallet and top-up values within the
// JavaScript-safe integer range, which is also exactly representable by float64.
func WalletQuotaFromDecimalStrict(d decimal.Decimal) (int, error) {
	f, _ := d.Round(0).Float64()
	return strictQuota(saturateQuotaBounded(f, "WalletQuotaFromDecimal", MaxWalletQuota, -MaxWalletQuota))
}
