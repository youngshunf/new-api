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

// GetCreditConsumptionSummary 返回某个用户历史消费的资金池拆分汇总。
//
// 仅供 doc94 R1 的一次性存量余额 rebase 使用：Cloud 与 NewAPI 的库已解耦，
// 重建基线所需的「真实消费」只能由 NewAPI 这个消费权威给出。
//
// 汇总把「拆得出来的」和「拆不出来的」分开返回，无法归因时 determinate=false，
// 由调用方把该用户放进人工清单——这里绝不按比例摊派或猜测来源。
//
// GET /api/internal/v1/credit-consumption/{newapi_user_id}
func GetCreditConsumptionSummary(c *gin.Context) {
	userId, err := strconv.Atoi(strings.TrimSpace(c.Param("newapi_user_id")))
	if err != nil || userId <= 0 {
		middleware.AbortWithCreditError(c, http.StatusBadRequest, model.CreditErrorInvalidRequest,
			"newapi_user_id must be a positive integer", false)
		return
	}
	summary, summaryErr := model.SummarizeCreditConsumption(userId)
	if summaryErr != nil {
		logger.LogWarn(c, fmt.Sprintf("summarize consumption failed user=%d: %s", userId, summaryErr.Error()))
		middleware.AbortWithCreditError(c, http.StatusServiceUnavailable, model.CreditErrorStoreUnavailable,
			summaryErr.Error(), true)
		return
	}
	c.JSON(http.StatusOK, summary)
}

// GetCreditUsage 返回某用户的消费流水（积分口径，已分页）。
//
// Cloud 过去自己维护 credit_transaction 流水表并自行把 quota 换算成积分，
// 于是「扣了多少」在两侧各有一套算法。doc94 D1 起流水只有这一个来源，
// 金额由 NewAPI 换算成积分字符串，Cloud 原样透传。
//
// GET /api/internal/v1/credit-usage/{newapi_user_id}?start=&end=&page=&size=
func GetCreditUsage(c *gin.Context) {
	userId, ok := parseInternalUserId(c)
	if !ok {
		return
	}
	start := parseInt64Query(c, "start")
	end := parseInt64Query(c, "end")
	page, _ := strconv.Atoi(c.Query("page"))
	size, _ := strconv.Atoi(c.Query("size"))

	result, err := model.ListCreditUsage(userId, start, end, page, size)
	if err != nil {
		logger.LogWarn(c, fmt.Sprintf("list credit usage failed user=%d: %s", userId, err.Error()))
		middleware.AbortWithCreditError(c, http.StatusServiceUnavailable, model.CreditErrorStoreUnavailable,
			err.Error(), true)
		return
	}
	c.JSON(http.StatusOK, result)
}

// GetCreditUsageDaily 返回按本地日聚合的消费（积分口径）。
//
// tz_offset_minutes 由调用方给出（Asia/Shanghai 为 480）：日边界必须由展示时区决定，
// 否则云端和 NewAPI 会各切各的日，同一笔消费出现在两个不同的日期上。
//
// GET /api/internal/v1/credit-usage/{newapi_user_id}/daily?start=&end=&tz_offset_minutes=
func GetCreditUsageDaily(c *gin.Context) {
	userId, ok := parseInternalUserId(c)
	if !ok {
		return
	}
	tzOffset, err := strconv.Atoi(c.DefaultQuery("tz_offset_minutes", "0"))
	if err != nil || tzOffset < -720 || tzOffset > 840 {
		middleware.AbortWithCreditError(c, http.StatusBadRequest, model.CreditErrorInvalidRequest,
			"tz_offset_minutes must be an integer within [-720, 840]", false)
		return
	}

	result, summaryErr := model.SummarizeCreditUsageDaily(userId, parseInt64Query(c, "start"), parseInt64Query(c, "end"), tzOffset)
	if summaryErr != nil {
		logger.LogWarn(c, fmt.Sprintf("summarize daily usage failed user=%d: %s", userId, summaryErr.Error()))
		middleware.AbortWithCreditError(c, http.StatusServiceUnavailable, model.CreditErrorStoreUnavailable,
			summaryErr.Error(), true)
		return
	}
	c.JSON(http.StatusOK, result)
}

// parseInternalUserId 解析路径上的 newapi_user_id；非法时已经写好 400 响应并返回 false。
func parseInternalUserId(c *gin.Context) (int, bool) {
	userId, err := strconv.Atoi(strings.TrimSpace(c.Param("newapi_user_id")))
	if err != nil || userId <= 0 {
		middleware.AbortWithCreditError(c, http.StatusBadRequest, model.CreditErrorInvalidRequest,
			"newapi_user_id must be a positive integer", false)
		return 0, false
	}
	return userId, true
}

// parseInt64Query 读可选的 Unix 秒查询参数；缺失或非法一律按 0（不限）处理。
func parseInt64Query(c *gin.Context, key string) int64 {
	value, err := strconv.ParseInt(strings.TrimSpace(c.Query(key)), 10, 64)
	if err != nil || value < 0 {
		return 0
	}
	return value
}
