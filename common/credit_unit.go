package common

import (
	"errors"
	"math"
	"regexp"
	"strings"

	"github.com/shopspring/decimal"
)

// 积分（credit）与内部 quota 单位的换算。
//
// 商业口径固定为「1 积分 = 1 USD」；NewAPI 内部继续用定点整数 quota 存额度，
// 换算常数是 QuotaPerUnit。quota 是本服务的私有存储编码，不是商业汇率，
// 对外（面向 Cloud 的内部履约 API）暴露的所有额度字段都直接是积分。
//
// 精度：QuotaPerUnit = 500000，因此最小可表示积分是 1/500000 = 0.000002。
// 6 位小数不可整除（0.000001 积分 = 0.5 quota），5 位小数可以（0.00001 积分 = 5 quota），
// 故入参口径固定为「最多 5 位小数」，且必须满足 credit × QuotaPerUnit 为整数。
// 服务端只做校验，绝不静默四舍五入。
const CreditScale = 5

// creditAmountPattern 与内部 API 契约的 JSON Schema pattern 保持一致。
var creditAmountPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]{1,5})?$`)

// ErrInvalidCreditAmount 表示积分入参格式非法、小数位超限，或乘以 QuotaPerUnit 后不是整数。
var ErrInvalidCreditAmount = errors.New("invalid_credit_amount")

// quotaPerUnitDecimal 返回换算常数的 decimal 形式。
// QuotaPerUnit 是可配置变量；若被改成非正数或非整数，换算语义即失效，
// 这里直接返回错误而不是带着坏常数继续算。
func quotaPerUnitDecimal() (decimal.Decimal, error) {
	if QuotaPerUnit <= 0 || math.IsNaN(QuotaPerUnit) || math.IsInf(QuotaPerUnit, 0) {
		return decimal.Zero, errors.New("QuotaPerUnit must be a positive finite number")
	}
	d := decimal.NewFromFloat(QuotaPerUnit)
	if !d.IsInteger() {
		return decimal.Zero, errors.New("QuotaPerUnit must be an integer")
	}
	return d, nil
}

// ParseCreditAmountToQuota 把十进制积分字符串换算成内部 quota 整数。
//
// 拒绝而非修正的情形：格式不合法、小数位超过 CreditScale、
// credit × QuotaPerUnit 不是整数、结果超出 int64 表示范围。
func ParseCreditAmountToQuota(amount string) (int64, error) {
	trimmed := strings.TrimSpace(amount)
	if trimmed == "" {
		return 0, ErrInvalidCreditAmount
	}
	if !creditAmountPattern.MatchString(trimmed) {
		return 0, ErrInvalidCreditAmount
	}
	credits, err := decimal.NewFromString(trimmed)
	if err != nil {
		return 0, ErrInvalidCreditAmount
	}
	perUnit, err := quotaPerUnitDecimal()
	if err != nil {
		return 0, err
	}
	quota := credits.Mul(perUnit)
	if !quota.IsInteger() {
		return 0, ErrInvalidCreditAmount
	}
	big := quota.BigInt()
	if !big.IsInt64() {
		return 0, ErrInvalidCreditAmount
	}
	return big.Int64(), nil
}

// FormatQuotaAsCredits 把内部 quota 整数格式化成十进制积分字符串。
//
// 任意整数 quota 除以 QuotaPerUnit 都能精确表示（500000 = 2^5 × 5^6，
// 故最多 6 位小数），因此这里是无损输出，不做四舍五入；尾随零被去掉。
// 注意输出可能出现 6 位小数（如 quota=1 → "0.000002"）——这是权威余额的
// 真实值，与「入参最多 5 位小数」是两个方向的不同约束。
func FormatQuotaAsCredits(quota int64) string {
	perUnit, err := quotaPerUnitDecimal()
	if err != nil {
		// 换算常数异常时不能编造余额，返回零值字符串并由调用方的错误路径兜住。
		return "0"
	}
	return decimal.NewFromInt(quota).DivRound(perUnit, 12).String()
}
