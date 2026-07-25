package model

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedCreditUser(t *testing.T, id int, quota int) *User {
	t.Helper()
	user := &User{
		Id:          id,
		Username:    fmt.Sprintf("credit-user-%d", id),
		Password:    "x",
		DisplayName: fmt.Sprintf("credit-user-%d", id),
		Role:        common.RoleCommonUser,
		Status:      common.UserStatusEnabled,
		Quota:       quota,
		Group:       "default",
		// users.aff_code 上有唯一索引，多个用户共用空串会撞 2067。
		AffCode: fmt.Sprintf("aff%d", id),
	}
	require.NoError(t, DB.Create(user).Error)
	return user
}

func walletQuotaOf(t *testing.T, userId int) int {
	t.Helper()
	var user User
	require.NoError(t, DB.Where("id = ?", userId).First(&user).Error)
	return user.Quota
}

func walletGrantRequest(userId int, amount string) *dto.CreditOperationRequest {
	return &dto.CreditOperationRequest{
		OperationType: CreditOperationWalletGrant,
		NewApiUserId:  userId,
		CreditAmount:  amount,
	}
}

func TestWalletGrantIsIdempotentPerEventId(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 7001, 0)

	outcome, err := ExecuteCreditOperation("evt-grant-1", walletGrantRequest(7001, "10"))
	require.Nil(t, err)
	require.Equal(t, CreditOperationStatusSucceeded, outcome.Operation.Status)
	assert.False(t, outcome.IdempotentReplay)
	assert.Equal(t, "10", outcome.Operation.AppliedCredits)
	assert.Equal(t, 5000000, walletQuotaOf(t, 7001))

	replay, err := ExecuteCreditOperation("evt-grant-1", walletGrantRequest(7001, "10"))
	require.Nil(t, err)
	assert.True(t, replay.IdempotentReplay)
	assert.Equal(t, CreditOperationStatusSucceeded, replay.Operation.Status)
	// 重放不得二次到账
	assert.Equal(t, 5000000, walletQuotaOf(t, 7001))
}

func TestSameEventIdWithDifferentPayloadConflicts(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 7002, 0)

	_, err := ExecuteCreditOperation("evt-conflict", walletGrantRequest(7002, "10"))
	require.Nil(t, err)

	_, err = ExecuteCreditOperation("evt-conflict", walletGrantRequest(7002, "11"))
	require.NotNil(t, err)
	assert.Equal(t, CreditErrorIdempotencyConflict, err.Code)
	assert.Equal(t, http.StatusConflict, err.HTTPStatus)
	// 冲突不得改变余额
	assert.Equal(t, 5000000, walletQuotaOf(t, 7002))
}

// reason 是自由文本，改文案不应把同一次履约判成载荷冲突。
func TestReasonIsNotPartOfIdempotencyFingerprint(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 7003, 0)

	first := walletGrantRequest(7003, "1")
	first.Reason = "campaign-a"
	_, err := ExecuteCreditOperation("evt-reason", first)
	require.Nil(t, err)

	second := walletGrantRequest(7003, "1")
	second.Reason = "campaign-b"
	outcome, err := ExecuteCreditOperation("evt-reason", second)
	require.Nil(t, err)
	assert.True(t, outcome.IdempotentReplay)
}

func TestWalletRevokeInsufficientIsTerminalAndPersisted(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 7004, 500000) // 1 积分

	outcome, err := ExecuteCreditOperation("evt-revoke", &dto.CreditOperationRequest{
		OperationType: CreditOperationWalletRevoke,
		NewApiUserId:  7004,
		CreditAmount:  "2",
	})
	require.Nil(t, err)
	require.Equal(t, CreditOperationStatusFailed, outcome.Operation.Status)
	assert.Equal(t, CreditFailureWalletInsufficient, outcome.Operation.FailureCode)
	// 余额不为负、也不被部分回收
	assert.Equal(t, 500000, walletQuotaOf(t, 7004))

	// 终局失败必须能被 GET 查到：Cloud 据此直接进 dead letter，不再重投
	persisted, getErr := GetCreditOperationByEventId("evt-revoke")
	require.Nil(t, getErr)
	assert.Equal(t, CreditOperationStatusFailed, persisted.Status)
}

func TestWalletGrantOverflowIsTerminal(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 7005, common.MaxQuota-1)

	outcome, err := ExecuteCreditOperation("evt-overflow", walletGrantRequest(7005, "1"))
	require.Nil(t, err)
	require.Equal(t, CreditOperationStatusFailed, outcome.Operation.Status)
	assert.Equal(t, CreditFailureWalletOverflow, outcome.Operation.FailureCode)
	// 宁可终局失败也不做环绕写入把余额变成负数
	assert.Equal(t, common.MaxQuota-1, walletQuotaOf(t, 7005))
}

// 瞬时/请求级失败不落库，保证 GET 404 == 这次操作确定没有发生。
func TestRejectedRequestsAreNotPersisted(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 7006, 0)

	_, err := ExecuteCreditOperation("evt-precision", walletGrantRequest(7006, "0.000001"))
	require.NotNil(t, err)
	assert.Equal(t, CreditErrorInvalidCreditAmount, err.Code)

	_, getErr := GetCreditOperationByEventId("evt-precision")
	require.NotNil(t, getErr)
	assert.Equal(t, http.StatusNotFound, getErr.HTTPStatus)
}

func TestUnknownUserReturnsNotFoundWithoutPersisting(t *testing.T) {
	truncateTables(t)

	_, err := ExecuteCreditOperation("evt-missing-user", walletGrantRequest(999999, "1"))
	require.NotNil(t, err)
	assert.Equal(t, CreditErrorUserNotFound, err.Code)
	assert.Equal(t, http.StatusNotFound, err.HTTPStatus)

	_, getErr := GetCreditOperationByEventId("evt-missing-user")
	require.NotNil(t, getErr)
	assert.Equal(t, http.StatusNotFound, getErr.HTTPStatus)
}

func TestOperationFieldCombinationValidation(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 7007, 0)

	start := time.Now().UTC().Format(time.RFC3339)
	end := time.Now().UTC().Add(31 * 24 * time.Hour).Format(time.RFC3339)
	count := 1

	cases := []struct {
		name string
		req  *dto.CreditOperationRequest
		code string
	}{
		{
			name: "wallet_grant 金额为 0",
			req:  walletGrantRequest(7007, "0"),
			code: CreditErrorInvalidCreditAmount,
		},
		{
			name: "wallet_grant 携带订阅周期字段",
			req: &dto.CreditOperationRequest{
				OperationType: CreditOperationWalletGrant,
				NewApiUserId:  7007,
				CreditAmount:  "1",
				CycleSeconds:  CycleSecondsFixed,
			},
			code: CreditErrorInvalidRequest,
		},
		{
			name: "subscription_expire 携带金额",
			req: &dto.CreditOperationRequest{
				OperationType:          CreditOperationSubscriptionExpire,
				NewApiUserId:           7007,
				ExternalSubscriptionId: "contract-x",
				CreditAmount:           "1",
			},
			code: CreditErrorInvalidRequest,
		},
		{
			name: "subscription_activate 周期长度不是 30 天",
			req: &dto.CreditOperationRequest{
				OperationType:          CreditOperationSubscriptionActivate,
				NewApiUserId:           7007,
				ExternalSubscriptionId: "contract-x",
				CreditAmount:           "100",
				StartAt:                start,
				CycleSeconds:           28 * 24 * 3600,
			},
			code: CreditErrorInvalidCycle,
		},
		{
			name: "付费合同 end_at 与 cycle_count 对不上",
			req: &dto.CreditOperationRequest{
				OperationType:          CreditOperationSubscriptionActivate,
				NewApiUserId:           7007,
				ExternalSubscriptionId: "contract-x",
				CreditAmount:           "100",
				StartAt:                start,
				EndAt:                  &end,
				CycleSeconds:           CycleSecondsFixed,
				CycleCount:             &count,
			},
			code: CreditErrorInvalidCycle,
		},
		{
			name: "付费合同只给了 end_at 没给 cycle_count",
			req: &dto.CreditOperationRequest{
				OperationType:          CreditOperationSubscriptionActivate,
				NewApiUserId:           7007,
				ExternalSubscriptionId: "contract-x",
				CreditAmount:           "100",
				StartAt:                start,
				EndAt:                  &end,
				CycleSeconds:           CycleSecondsFixed,
			},
			code: CreditErrorInvalidCycle,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ExecuteCreditOperation("evt-"+tc.name, tc.req)
			require.NotNil(t, err)
			assert.Equal(t, tc.code, err.Code)
		})
	}
}

// 同一个 event_id 被并发重投时只能变更一次余额。
func TestConcurrentSameEventGrantsOnlyOnce(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 7008, 0)

	const attempts = 100
	var wg sync.WaitGroup
	succeeded := make(chan bool, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outcome, err := ExecuteCreditOperation("evt-concurrent", walletGrantRequest(7008, "1"))
			if err == nil && outcome.Operation.Status == CreditOperationStatusSucceeded {
				succeeded <- outcome.IdempotentReplay
			}
		}()
	}
	wg.Wait()
	close(succeeded)

	firstWrites := 0
	total := 0
	for replay := range succeeded {
		total++
		if !replay {
			firstWrites++
		}
	}
	assert.Equal(t, attempts, total, "所有重投都应拿到成功结论")
	assert.Equal(t, 1, firstWrites, "只有一次是首次执行，其余都是幂等重放")
	assert.Equal(t, 500000, walletQuotaOf(t, 7008), "余额只增加一次")
}

func TestCreditAccountSnapshotReportsAuthoritativeBalances(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 7009, 1500000) // 3 积分钱包

	now := GetDBTimestamp()
	require.NoError(t, DB.Create(&UserSubscription{
		UserId:                 7009,
		AmountTotal:            5000000, // 10 积分
		AmountUsed:             2000000, // 已用 4 积分
		StartTime:              now - 3600,
		EndTime:                now + CycleSecondsFixed,
		Status:                 SubscriptionStatusActive,
		Source:                 SubscriptionSourceCloudContract,
		ExternalSubscriptionId: "contract-account",
		CycleSeconds:           CycleSecondsFixed,
		CycleCount:             1,
		LastResetTime:          now - 3600,
		NextResetTime:          now + CycleSecondsFixed,
	}).Error)

	account, err := BuildCreditAccount(7009)
	require.Nil(t, err)
	assert.Equal(t, "3", account.Wallet.RemainingCredits)
	require.Len(t, account.Subscriptions, 1)
	assert.Equal(t, "contract-account", account.Subscriptions[0].ExternalSubscriptionId)
	assert.Equal(t, "10", account.Subscriptions[0].CycleLimitCredits)
	assert.Equal(t, "4", account.Subscriptions[0].CycleUsedCredits)
	assert.Equal(t, "6", account.Subscriptions[0].CycleRemainingCredits)
	// 钱包 3 + 订阅剩余 6 = 9，绝不是 quota - used_quota 那套算法
	assert.Equal(t, "9", account.TotalAvailableCredits)
	assert.NotEmpty(t, account.MeasuredAt)
}
