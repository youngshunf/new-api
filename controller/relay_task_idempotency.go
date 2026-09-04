package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/retrycontrol"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
)

// 异步任务创建的幂等入口（LLM 网关设计 §13.3）。
//
// 幂等键是四元组 (Token, operation, idempotency_key, 请求摘要)：
// 同键同摘要重发返回原任务，不再次创建、不再次扣费；同键不同摘要返回 409 冲突。
//
// ⚠️ 幂等键**只能**由调用方在 Idempotency-Key 请求头里显式给出。
// request_id / trace_id 这类观测关联 ID 每次重发都会变，拿它们去重等于没有去重，
// 设计 §13.3 明令禁止。

// HeaderIdempotencyKey 是幂等键的载体。用 IETF
// draft-ietf-httpapi-idempotency-key-header 的标准名字，不另造一个同义头。
const HeaderIdempotencyKey = "Idempotency-Key"

// HeaderIdempotentReplay 在响应上标出「这次返回的是原任务，本次没有新建也没有扣费」。
const HeaderIdempotentReplay = "X-Hasn-Idempotent-Replay"

// taskIdempotencyGuard 持有本次请求占到的那把键，负责在任务落库或失败之后收口。
type taskIdempotencyGuard struct {
	record *model.TaskIdempotencyRecord
}

// beginTaskIdempotency 在**任何扣费与上游发送之前**占位。
//
// 返回三态与 model.AcquireTaskIdempotency 一致：
//   - (guard, nil, nil)   继续正常创建流程；guard 可能为 nil（调用方没给幂等键）
//   - (nil, replay, nil)  命中既有终局记录，调用方直接回放
//   - (nil, nil, taskErr) 冲突、在途或存储故障
func beginTaskIdempotency(c *gin.Context, relayInfo *relaycommon.RelayInfo) (*taskIdempotencyGuard, *model.TaskIdempotencyReplay, *taskdto.TaskError) {
	rawKey := c.GetHeader(HeaderIdempotencyKey)
	if rawKey == "" {
		// 幂等是可选能力：没给键就是老行为，不强加。
		return nil, nil, nil
	}
	key, keyErr := model.ValidateTaskIdempotencyKey(rawKey)
	if keyErr != nil {
		return nil, nil, taskIdempotencyTaskError(keyErr)
	}
	digest, digestErr := taskRequestDigest(c)
	if digestErr != nil {
		return nil, nil, &taskdto.TaskError{
			Code:       model.TaskIdempotencyErrorStoreUnavailable,
			Message:    "failed to fingerprint request: " + digestErr.Error(),
			StatusCode: http.StatusInternalServerError,
			LocalError: true,
			Error:      digestErr,
		}
	}
	record, replay, acquireErr := model.AcquireTaskIdempotency(model.TaskIdempotencyScope{
		TokenId:        relayInfo.TokenId,
		Operation:      taskIdempotencyOperation(c),
		IdempotencyKey: key,
		RequestDigest:  digest,
	})
	if acquireErr != nil {
		return nil, nil, taskIdempotencyTaskError(acquireErr)
	}
	if replay != nil {
		return nil, replay, nil
	}
	return &taskIdempotencyGuard{record: record}, nil, nil
}

// succeed 在任务已经落库、计费已经结算之后把键钉成 succeeded。
func (g *taskIdempotencyGuard) succeed(c *gin.Context, taskID string) {
	if g == nil {
		return
	}
	if err := model.CompleteTaskIdempotency(g.record, taskID); err != nil {
		// 任务本身已经落库，这里失败只影响后续同键重发能否命中回放；
		// 占位行会被 TTL 接管，不会永久卡死这把键。
		logger.LogWarn(c, "failed to finalize task idempotency record: "+err.Error())
	}
}

// fail 按 dispatch_state 决定这把键的去向。
//
//   - not_dispatched：确证上游一个字节都没收到，删掉占位行，同键可以再试；
//   - dispatched / unknown：上游可能已经收到并开跑，把失败钉成终局记录，
//     同键重发原样回放这次失败——绝不能放开键让它再发一次。
//
// 这一步复用 S1-D 的 dispatch_state：两块能力判的是同一件事，
// 各判一次早晚会判出两个答案。
func (g *taskIdempotencyGuard) fail(c *gin.Context, taskErr *taskdto.TaskError) {
	if g == nil || taskErr == nil {
		return
	}
	attachTaskRetryControl(c, taskErr)
	state := retrycontrol.DispatchStateUnknown
	if taskErr.RetryControl != nil {
		state = taskErr.RetryControl.DispatchState
	}
	var err error
	if state == retrycontrol.DispatchStateNotDispatched {
		err = model.ReleaseTaskIdempotency(g.record)
	} else {
		err = model.FailTaskIdempotency(g.record, taskErr.Code, taskErr.Message, taskErr.StatusCode)
	}
	if err != nil {
		logger.LogWarn(c, "failed to settle task idempotency record: "+err.Error())
	}
}

// presentTaskIdempotentReplay 回放既有终局记录：要么是原任务，要么是上次的失败。
func presentTaskIdempotentReplay(c *gin.Context, relayInfo *relaycommon.RelayInfo, replay *model.TaskIdempotencyReplay) {
	c.Header(HeaderIdempotentReplay, "true")
	if replay.TaskID == "" {
		respondTaskSubmissionError(c, &taskdto.TaskError{
			Code:       replay.FailureCode,
			Message:    replay.FailureMessage,
			StatusCode: replay.FailureStatusCode,
			LocalError: true,
			Error:      errors.New(replay.FailureMessage),
		})
		return
	}
	task, exists, err := model.GetByTaskId(relayInfo.UserId, replay.TaskID)
	if err != nil {
		respondTaskSubmissionError(c, &taskdto.TaskError{
			Code:       "task_idempotent_replay_failed",
			Message:    "failed to load the original task",
			StatusCode: http.StatusInternalServerError,
			LocalError: true,
			Error:      err,
		})
		return
	}
	if !exists || task == nil {
		// 原任务被删了：如实报错，不能凭幂等记录编一个任务出来。
		respondTaskSubmissionError(c, &taskdto.TaskError{
			Code:       "task_idempotent_replay_failed",
			Message:    "the original task recorded for this idempotency key no longer exists",
			StatusCode: http.StatusGone,
			LocalError: true,
			Error:      errors.New("original task missing"),
		})
		return
	}
	if relayInfo.OriginModelName == "" {
		relayInfo.OriginModelName = task.Properties.OriginModelName
	}
	presentTaskSubmission(c, &taskSubmissionOutcome{Task: task, RelayInfo: relayInfo})
}

// taskIdempotencyOperation 是幂等键四元组里的 operation 分量：
// 方法 + 路由模式。用路由模式而不是具体路径，具体路径已经进了请求摘要。
func taskIdempotencyOperation(c *gin.Context) string {
	pattern := c.FullPath()
	if pattern == "" {
		pattern = c.Request.URL.Path
	}
	operation := c.Request.Method + " " + pattern
	if len(operation) > 128 {
		// 列宽 128，且必须是合法 UTF-8：按字节截断会切碎多字节字符，
		// 写进 MySQL/PostgreSQL 的 varchar 会直接报错。改用纯 ASCII 摘要。
		digest := sha256.Sum256([]byte(operation))
		operation = c.Request.Method + " sha256:" + hex.EncodeToString(digest[:])
	}
	return operation
}

// taskRequestDigest 计算请求摘要：方法、路径、查询串与完整请求体。
//
// 用 BodyStorage.NewReader 流式喂给 hash，不把请求体整块读进内存——
// 图像与视频创建带的 multipart 体可能很大，而且这条路径上还有别人要读同一份体。
func taskRequestDigest(c *gin.Context) (string, error) {
	hasher := sha256.New()
	fmt.Fprintf(hasher, "%s\n%s\n%s\n", c.Request.Method, c.Request.URL.Path, c.Request.URL.RawQuery)
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return "", err
	}
	reader, err := storage.NewReader()
	if err != nil {
		return "", err
	}
	defer reader.Close()
	if _, err := io.Copy(hasher, reader); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func taskIdempotencyTaskError(err *model.TaskIdempotencyError) *taskdto.TaskError {
	return &taskdto.TaskError{
		Code:       err.Code,
		Message:    err.Message,
		StatusCode: err.HTTPStatus,
		LocalError: true,
		Error:      err,
	}
}
