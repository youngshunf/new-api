package controller

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
)

// Cloud → NewAPI 的内部积分履约端点（doc94 §3）。
//
// 这一面只做三件事：幂等执行履约、查询终局结果、读权威账户快照。
// 任何「按 Cloud 计算值设置绝对余额」的入口都不在这里，也不会被加回来——
// 那正是造成「超额仍可用」的反向数据流。

const maxInternalCreditRequestBody = 8 * 1024

// creditRequestAllowedFields 是 CreditOperationRequest 的字段白名单。
// 对外契约是 additionalProperties: false：多余字段直接 400，
// 免得把 credit_amount 拼错的请求当成「没带金额」静默走另一条分支。
var creditRequestAllowedFields = map[string]struct{}{
	"operation_type":           {},
	"newapi_user_id":           {},
	"credit_amount":            {},
	"external_subscription_id": {},
	"start_at":                 {},
	"end_at":                   {},
	"cycle_seconds":            {},
	"cycle_count":              {},
	"wallet_overflow":          {},
	"reason":                   {},
}

func decodeCreditOperationRequest(c *gin.Context) (*dto.CreditOperationRequest, error) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxInternalCreditRequestBody+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read request body: %w", err)
	}
	if len(body) > maxInternalCreditRequestBody {
		return nil, fmt.Errorf("request body exceeds %d bytes", maxInternalCreditRequestBody)
	}
	var probe map[string]any
	if err := common.Unmarshal(body, &probe); err != nil {
		return nil, fmt.Errorf("request body must be a JSON object")
	}
	unknown := make([]string, 0, 2)
	for key := range probe {
		if _, ok := creditRequestAllowedFields[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("unknown field(s): %s", strings.Join(unknown, ", "))
	}
	var req dto.CreditOperationRequest
	if err := common.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("request body does not match the credit operation schema")
	}
	return &req, nil
}

func abortWithCreditOperationError(c *gin.Context, err *model.CreditOperationError) {
	middleware.AbortWithCreditError(c, err.HTTPStatus, err.Code, err.Message, err.Retryable)
}

// auditCreditOperation 记录履约审计日志。
// 只落操作类型、目标、事件 ID 与终局状态；不落凭据，也不落请求原文。
func auditCreditOperation(c *gin.Context, eventId string, op *model.CreditOperation, replay bool) {
	logger.LogInfo(c, fmt.Sprintf("credit operation event_id=%s type=%s user=%d status=%s failure=%s applied=%s replay=%t",
		eventId, op.OperationType, op.UserId, op.Status, op.FailureCode, op.AppliedCredits, replay))
}

// PutCreditOperation 幂等执行一次积分或订阅履约。
//
// PUT /api/internal/v1/credit-operations/{event_id}
func PutCreditOperation(c *gin.Context) {
	eventId := c.Param("event_id")
	req, decodeErr := decodeCreditOperationRequest(c)
	if decodeErr != nil {
		middleware.AbortWithCreditError(c, http.StatusBadRequest, model.CreditErrorInvalidRequest, decodeErr.Error(), false)
		return
	}
	outcome, opErr := model.ExecuteCreditOperation(eventId, req)
	if opErr != nil {
		logger.LogWarn(c, fmt.Sprintf("credit operation rejected event_id=%s code=%s: %s", eventId, opErr.Code, opErr.Message))
		abortWithCreditOperationError(c, opErr)
		return
	}
	auditCreditOperation(c, eventId, &outcome.Operation, outcome.IdempotentReplay)

	// 回执附带权威账户快照，让 Cloud 一次往返就能拿到 measured_at；
	// 快照读失败不影响履约结论，只是回执里没有 account。
	var account *dto.CreditAccount
	if snapshot, snapshotErr := model.BuildCreditAccount(outcome.Operation.UserId); snapshotErr == nil {
		account = snapshot
	} else {
		logger.LogWarn(c, fmt.Sprintf("credit operation %s succeeded but account snapshot failed: %s", eventId, snapshotErr.Message))
	}
	c.JSON(http.StatusOK, outcome.Operation.ToCreditOperationResult(outcome.IdempotentReplay, account))
}

// GetCreditOperation 查询履约终局结果，用于 Cloud 超时后的确定性对账。
//
// 200 + succeeded：已成功，不要重投。
// 200 + failed：已终局失败，进 dead letter，不要重投。
// 404：这次操作确定没有发生，可用同 event_id 安全重投。
func GetCreditOperation(c *gin.Context) {
	eventId := c.Param("event_id")
	op, opErr := model.GetCreditOperationByEventId(eventId)
	if opErr != nil {
		abortWithCreditOperationError(c, opErr)
		return
	}
	c.JSON(http.StatusOK, op.ToCreditOperationResult(true, nil))
}

// GetCreditAccount 返回 NewAPI 权威积分余额与当前订阅周期。
//
// GET /api/internal/v1/credit-accounts/{newapi_user_id}
func GetCreditAccount(c *gin.Context) {
	userId, err := strconv.Atoi(strings.TrimSpace(c.Param("newapi_user_id")))
	if err != nil || userId <= 0 {
		middleware.AbortWithCreditError(c, http.StatusBadRequest, model.CreditErrorInvalidRequest,
			"newapi_user_id must be a positive integer", false)
		return
	}
	account, accountErr := model.BuildCreditAccount(userId)
	if accountErr != nil {
		abortWithCreditOperationError(c, accountErr)
		return
	}
	c.JSON(http.StatusOK, account)
}
