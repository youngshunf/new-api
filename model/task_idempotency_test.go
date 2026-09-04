package model

import (
	"net/http"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func videoCreateScope(tokenId int, key, digest string) TaskIdempotencyScope {
	return TaskIdempotencyScope{
		TokenId:        tokenId,
		Operation:      "POST /v1/video/generations",
		IdempotencyKey: key,
		RequestDigest:  digest,
	}
}

// 同键同摘要重发必须拿回原任务，不再次创建。
func TestSameKeySameDigestReplaysTheOriginalTask(t *testing.T) {
	truncateTables(t)
	scope := videoCreateScope(9001, "idem-video-1", "digest-a")

	record, replay, acquireErr := AcquireTaskIdempotency(scope)
	require.Nil(t, acquireErr)
	require.Nil(t, replay)
	require.NotNil(t, record)
	require.Equal(t, TaskIdempotencyStatusProcessing, record.Status)
	require.NoError(t, CompleteTaskIdempotency(record, "task_original_1"))

	secondRecord, secondReplay, secondErr := AcquireTaskIdempotency(scope)
	require.Nil(t, secondErr)
	require.Nil(t, secondRecord, "重发不得再占一把新键")
	require.NotNil(t, secondReplay)
	assert.Equal(t, "task_original_1", secondReplay.TaskID)

	var rows int64
	require.NoError(t, DB.Model(&TaskIdempotencyRecord{}).Count(&rows).Error)
	assert.EqualValues(t, 1, rows, "同键重发不得新增账本行")
}

// 同键不同摘要是冲突，不是回放：回放会把 B 请求的结果谎报成 A 请求的。
func TestSameKeyDifferentDigestConflicts(t *testing.T) {
	truncateTables(t)
	record, _, acquireErr := AcquireTaskIdempotency(videoCreateScope(9002, "idem-video-2", "digest-a"))
	require.Nil(t, acquireErr)
	require.NoError(t, CompleteTaskIdempotency(record, "task_original_2"))

	conflictRecord, conflictReplay, conflictErr := AcquireTaskIdempotency(videoCreateScope(9002, "idem-video-2", "digest-b"))
	require.Nil(t, conflictRecord)
	require.Nil(t, conflictReplay)
	require.NotNil(t, conflictErr)
	assert.Equal(t, http.StatusConflict, conflictErr.HTTPStatus)
	assert.Equal(t, TaskIdempotencyErrorConflict, conflictErr.Code)
}

// 幂等键的作用域是 (Token, operation, key)：换 Token 或换 operation 都是另一把键。
func TestIdempotencyKeyIsScopedByTokenAndOperation(t *testing.T) {
	truncateTables(t)
	base := videoCreateScope(9003, "shared-key", "digest-a")
	record, _, acquireErr := AcquireTaskIdempotency(base)
	require.Nil(t, acquireErr)
	require.NoError(t, CompleteTaskIdempotency(record, "task_base"))

	otherToken := base
	otherToken.TokenId = 9004
	otherTokenRecord, otherTokenReplay, err := AcquireTaskIdempotency(otherToken)
	require.Nil(t, err)
	require.Nil(t, otherTokenReplay, "另一个 Token 的同名键不得命中别人的任务")
	require.NotNil(t, otherTokenRecord)

	otherOperation := base
	otherOperation.Operation = "POST /v1/videos/{video_id}/remix"
	otherOpRecord, otherOpReplay, err := AcquireTaskIdempotency(otherOperation)
	require.Nil(t, err)
	require.Nil(t, otherOpReplay, "另一个 operation 的同名键不得命中原任务")
	require.NotNil(t, otherOpRecord)

	// 三把键互不相同。
	assert.NotEqual(t, base.ScopeHash(), otherToken.ScopeHash())
	assert.NotEqual(t, base.ScopeHash(), otherOperation.ScopeHash())
}

// 请求摘要不参与 ScopeHash：参与了「同键不同摘要」就会变成两把互不相干的键，
// 冲突永远判不出来。
func TestRequestDigestDoesNotParticipateInScopeHash(t *testing.T) {
	a := videoCreateScope(9005, "k", "digest-a")
	b := videoCreateScope(9005, "k", "digest-b")
	assert.Equal(t, a.ScopeHash(), b.ScopeHash())
}

// 在途的同键并发重发必须被拒，否则两个请求会各自去上游创建一次任务。
func TestInFlightKeyRejectsConcurrentResend(t *testing.T) {
	truncateTables(t)
	scope := videoCreateScope(9006, "idem-inflight", "digest-a")
	record, _, acquireErr := AcquireTaskIdempotency(scope)
	require.Nil(t, acquireErr)
	require.NotNil(t, record)

	secondRecord, secondReplay, secondErr := AcquireTaskIdempotency(scope)
	require.Nil(t, secondRecord)
	require.Nil(t, secondReplay)
	require.NotNil(t, secondErr)
	assert.Equal(t, http.StatusConflict, secondErr.HTTPStatus)
	assert.Equal(t, TaskIdempotencyErrorInProgress, secondErr.Code)
}

// 确证上游没收到请求时释放键：什么都没发生，同键理应还能再试。
func TestReleaseAfterNotDispatchedFailureFreesTheKey(t *testing.T) {
	truncateTables(t)
	scope := videoCreateScope(9007, "idem-release", "digest-a")
	record, _, acquireErr := AcquireTaskIdempotency(scope)
	require.Nil(t, acquireErr)
	require.NoError(t, ReleaseTaskIdempotency(record))

	retryRecord, retryReplay, retryErr := AcquireTaskIdempotency(scope)
	require.Nil(t, retryErr)
	require.Nil(t, retryReplay)
	require.NotNil(t, retryRecord, "释放后同键必须能重新占用")
	require.NoError(t, CompleteTaskIdempotency(retryRecord, "task_after_release"))

	_, replay, err := AcquireTaskIdempotency(scope)
	require.Nil(t, err)
	require.NotNil(t, replay)
	assert.Equal(t, "task_after_release", replay.TaskID)
}

// 无法确证上游没收到时不得释放键：释放等于允许再发一次可能已经在上游跑着的任务。
func TestFailedRecordReplaysTheFailureInsteadOfResubmitting(t *testing.T) {
	truncateTables(t)
	scope := videoCreateScope(9008, "idem-failed", "digest-a")
	record, _, acquireErr := AcquireTaskIdempotency(scope)
	require.Nil(t, acquireErr)
	require.NoError(t, FailTaskIdempotency(record, "fail_to_fetch_task", "upstream returned 502", http.StatusBadGateway))

	replayRecord, replay, err := AcquireTaskIdempotency(scope)
	require.Nil(t, err)
	require.Nil(t, replayRecord, "终局失败的键不得被重新占用")
	require.NotNil(t, replay)
	assert.Empty(t, replay.TaskID)
	assert.Equal(t, "fail_to_fetch_task", replay.FailureCode)
	assert.Equal(t, "upstream returned 502", replay.FailureMessage)
	assert.Equal(t, http.StatusBadGateway, replay.FailureStatusCode)
}

// 进程在占位与落库之间崩溃会留下永久 processing 行；没有接管机制那把键就此报废。
func TestStaleInFlightRecordIsTakenOver(t *testing.T) {
	truncateTables(t)
	scope := videoCreateScope(9009, "idem-stale", "digest-a")
	record, _, acquireErr := AcquireTaskIdempotency(scope)
	require.Nil(t, acquireErr)

	// 把占位行推到接管时限之外，模拟上一个进程已经死掉。
	stale := common.GetTimestamp() - int64(taskIdempotencyInFlightTTL.Seconds()) - 60
	require.NoError(t, DB.Model(&TaskIdempotencyRecord{}).
		Where("id = ?", record.Id).
		Update("updated_at", stale).Error)

	takenOver, replay, err := AcquireTaskIdempotency(scope)
	require.Nil(t, err)
	require.Nil(t, replay)
	require.NotNil(t, takenOver)
	assert.Equal(t, record.Id, takenOver.Id, "接管的应是同一行，而不是新插一行")

	var rows int64
	require.NoError(t, DB.Model(&TaskIdempotencyRecord{}).Count(&rows).Error)
	assert.EqualValues(t, 1, rows)
}

// 并发重发只能有一个占到键。唯一索引是这里唯一可靠的互斥点。
func TestConcurrentAcquireLetsExactlyOneWin(t *testing.T) {
	truncateTables(t)
	scope := videoCreateScope(9010, "idem-race", "digest-a")

	const racers = 8
	var (
		mu       sync.Mutex
		reserved int
		rejected int
		wg       sync.WaitGroup
	)
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			record, replay, err := AcquireTaskIdempotency(scope)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case record != nil:
				reserved++
			case replay != nil:
				t.Error("尚无终局记录时不应回放")
			case err != nil:
				rejected++
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, 1, reserved, "并发重发只能有一个占到键")
	assert.Equal(t, racers-1, rejected)
}

func TestValidateTaskIdempotencyKeyRejectsUnusableShapes(t *testing.T) {
	for _, bad := range []string{
		"",
		"   ",
		string(make([]byte, MaxTaskIdempotencyKeyLength+1)),
		"key with space",
		"key\twith\ttab",
		"键",
	} {
		_, err := ValidateTaskIdempotencyKey(bad)
		require.NotNilf(t, err, "键 %q 应被拒绝", bad)
		assert.Equal(t, TaskIdempotencyErrorInvalidKey, err.Code)
		assert.Equal(t, http.StatusBadRequest, err.HTTPStatus)
	}

	key, err := ValidateTaskIdempotencyKey("  idem-01_ABC.~-  ")
	require.Nil(t, err)
	assert.Equal(t, "idem-01_ABC.~-", key)
}
