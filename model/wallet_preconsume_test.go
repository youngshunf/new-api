package model

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 回归（2026-08-17 生产事故）：主账号钱包余额 2,500,915,158 quota（5001.83 积分，
// 已越过 int32 上限 2,147,483,647），旧守卫把 common.MaxQuota 当成 users.quota 的
// 存储上限，于是一切钱包预扣都报「overflow the storage ceiling」500，分身全不可用。
// users.quota 实际是 64 位列，越过 int32 上限的预扣必须成功。
func TestPreConsumeWalletAboveInt32Ceiling(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 7101, 2500915158) // 生产事故当时的真实余额

	applied, err := PreConsumeUserWallet("req-above-int32", 7101, 3750000) // 7.5 积分
	require.NoError(t, err)
	assert.Equal(t, int64(3750000), applied)
	assert.Equal(t, 2497165158, walletQuotaOf(t, 7101))
}

// 钱包余额守卫的唯一职责是防 int64 加法回绕：贴顶再发放必须报错且不动余额。
func TestShiftWalletQuotaInt64WrapIsRejected(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 7102, math.MaxInt64-100)

	err := DB.Transaction(func(tx *gorm.DB) error {
		return shiftWalletQuotaTx(tx, 7102, 500000)
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "overflow")
	assert.Equal(t, math.MaxInt64-100, walletQuotaOf(t, 7102))
}
