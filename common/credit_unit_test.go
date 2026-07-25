package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 精度是跨端硬约束：QuotaPerUnit = 500000 决定 5 位小数可整除、6 位不可整除。
// 服务端只能拒绝，绝不静默四舍五入——四舍五入会让 Cloud 审计的发放量与实际入账量长期偏离。
func TestParseCreditAmountToQuotaPrecisionBoundary(t *testing.T) {
	cases := []struct {
		amount string
		quota  int64
		ok     bool
	}{
		{"1", 500000, true},
		{"0.00001", 5, true},      // 5 位小数：恰好可整除
		{"0.000001", 0, false},    // 6 位小数：0.5 quota，不可表示
		{"1000.00000", 500000000, true},
		{"0.1", 50000, true},
		{"0", 0, true}, // 格式合法；「必须大于 0」由操作类型校验负责
		{"", 0, false},
		{"-1", 0, false},
		{"1.234567", 0, false},
		{"01", 0, false},   // 前导零不是合法十进制积分
		{"1e3", 0, false},  // 科学计数法不在契约内
		{" 1 ", 500000, true},
	}
	for _, tc := range cases {
		quota, err := ParseCreditAmountToQuota(tc.amount)
		if tc.ok {
			require.NoErrorf(t, err, "amount=%q should be accepted", tc.amount)
			assert.Equalf(t, tc.quota, quota, "amount=%q", tc.amount)
			continue
		}
		require.Errorf(t, err, "amount=%q should be rejected", tc.amount)
	}
}

func TestFormatQuotaAsCreditsIsLossless(t *testing.T) {
	cases := []struct {
		quota int64
		want  string
	}{
		{0, "0"},
		{500000, "1"},
		{5, "0.00001"},
		{1, "0.000002"}, // 最小可表示积分，输出允许 6 位小数
		{750000, "1.5"},
		{500000000, "1000"},
	}
	for _, tc := range cases {
		assert.Equalf(t, tc.want, FormatQuotaAsCredits(tc.quota), "quota=%d", tc.quota)
	}
}

func TestCreditRoundTrip(t *testing.T) {
	for _, amount := range []string{"1", "0.00001", "1234.56789", "4294.96729"} {
		quota, err := ParseCreditAmountToQuota(amount)
		require.NoError(t, err)
		assert.Equal(t, amount, FormatQuotaAsCredits(quota))
	}
}
