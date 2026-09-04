package retrycontrol

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Surface 区分错误码来自哪条链路。
//
// 同一个错误码在两条链路上的**失败时点不同**，因此「是否发生在上游 I/O 之前」
// 必须分链路判：insufficient_user_quota 在同步 relay 上只可能出现在预扣费阶段
// （发送之前），在异步任务链路上却还有一个 reserve 阶段的产出点，
// 那是**上游任务已经创建之后**（controller/relay.go 的 stage="reserve"）。
// 用一张不分链路的表会把后者也判成 not_dispatched——直接导致重复扣费。
type Surface int

const (
	// SurfaceRelay 同步 relay 链路（controller.Relay）。
	SurfaceRelay Surface = iota
	// SurfaceTask 异步任务链路（controller.RelayTask / RelayTaskFetch）。
	SurfaceTask
)

// Classification 是一条终局错误在本契约下的分类结果。
type Classification struct {
	// Reason 是给 daemon 的失败原因（本契约词表）。
	Reason Reason
	// PreDispatchFailure 表示这个错误码在该链路上**只可能**在向上游发出任何
	// 字节之前产生。
	//
	// ⚠️ 它是 not_dispatched 的第二条正面证据来源（第一条是 httptrace），
	// 因此收录判据必须保守到偏执：只要该链路上存在「上游已经收到请求之后也
	// 可能出现」的产出点，它就不属于这里。判错方向的代价是重复扣费。
	PreDispatchFailure bool
}

// sharedPreDispatchErrorCodes 是两条链路上都只可能发生在上游 I/O 之前的错误码。
var sharedPreDispatchErrorCodes = map[string]bool{
	"read_request_body_failed": true, // 读客户端请求体
	"gen_relay_info_failed":    true, // 构造 RelayInfo
	"get_channel_failed":       true, // 选渠道
	"model_price_error":        true, // 计价
}

// relayOnlyPreDispatchErrorCodes 只在同步 relay 链路上成立。
var relayOnlyPreDispatchErrorCodes = map[string]bool{
	"invalid_request":                 true, // GetAndValidateRequest
	"bad_request_body":                true,
	"convert_request_failed":          true, // 转成上游格式
	"json_marshal_failed":             true,
	"invalid_api_type":                true,
	"sensitive_words_detected":        true, // 本地敏感词
	"count_token_failed":              true,
	"channel:no_available_key":        true, // 取渠道 key
	"channel:param_override_invalid":  true,
	"channel:header_override_invalid": true,
	"channel:model_mapped_error":      true,
	// 预扣费。同步链路上 PreConsumeBilling 只在 relayHandler 之前调用一次；
	// 异步任务链路另有一个 reserve 阶段产出同名码，故不能进 shared。
	"insufficient_user_quota":        true,
	"pre_consume_token_quota_failed": true,
}

// taskOnlyPreDispatchErrorCodes 只在异步任务链路上成立。
// 逐个核对过产出点都排在 adaptor.DoRequest（relay/relay_task.go 步骤 9）之前。
var taskOnlyPreDispatchErrorCodes = map[string]bool{
	"model_mapping_failed":         true, // 步骤 1/3
	"build_request_failed":         true, // 步骤 8，紧邻但仍在发送之前
	"plugin_usage_invalid":         true, // 计费事实提取
	"plugin_request_invalid":       true, // 插件 decoder 校验
	"setup_locked_channel_failed":  true, // 锁定渠道重建上下文
	"channel_not_found":            true,
	"channel_no_available_key":     true,
	"get_origin_task_failed":       true, // remix 溯源
	"origin_task_channel_disabled": true,
	"task_plugin_system_disabled":  true,
	"task_plugin_disabled":         true,
	"task_plugin_not_found":        true,
}

// reasonByErrorCode 把已知错误码映射到本契约词表。
// 没命中的落到状态码兜底（classifyByStatus）。
var reasonByErrorCode = map[string]Reason{
	// 请求本身不合法
	"invalid_request":                 ReasonRequestInvalid,
	"bad_request_body":                ReasonRequestInvalid,
	"read_request_body_failed":        ReasonRequestInvalid,
	"convert_request_failed":          ReasonRequestInvalid,
	"invalid_api_type":                ReasonRequestInvalid,
	"model_price_error":               ReasonRequestInvalid,
	"count_token_failed":              ReasonRequestInvalid,
	"channel:param_override_invalid":  ReasonRequestInvalid,
	"channel:header_override_invalid": ReasonRequestInvalid,
	"model_mapping_failed":            ReasonRequestInvalid,
	"build_request_failed":            ReasonRequestInvalid,
	"plugin_usage_invalid":            ReasonRequestInvalid,
	"plugin_request_invalid":          ReasonRequestInvalid,

	// 上下文与内容
	"context_length_exceeded":  ReasonContextLengthExceeded,
	"sensitive_words_detected": ReasonContentBlocked,
	"prompt_blocked":           ReasonContentBlocked,
	"content_policy_violation": ReasonContentBlocked,

	// 额度与凭据
	"insufficient_user_quota":        ReasonQuotaInsufficient,
	"pre_consume_token_quota_failed": ReasonQuotaInsufficient,
	"access_denied":                  ReasonCredentialRejected,
	"channel:invalid_key":            ReasonCredentialRejected,

	// 模型与容量
	"get_channel_failed":          ReasonModelCapacityExhausted,
	"channel:no_available_key":    ReasonModelCapacityExhausted,
	"channel_no_available_key":    ReasonModelCapacityExhausted,
	"channel_not_found":           ReasonModelCapacityExhausted,
	"channel:model_mapped_error":  ReasonModelCapacityExhausted,
	"model_not_found":             ReasonModelNotAvailable,
	"task_plugin_system_disabled": ReasonModelNotAvailable,
	"task_plugin_disabled":        ReasonModelNotAvailable,
	"task_plugin_not_found":       ReasonModelNotAvailable,

	// 上游
	"do_request_failed":              ReasonUpstreamUnavailable,
	"channel:response_time_exceeded": ReasonUpstreamTimeout,
	"empty_response":                 ReasonUpstreamUnavailable,
	"read_response_body_failed":      ReasonUpstreamUnavailable,
	"copy_response_body_failed":      ReasonUpstreamUnavailable,
	"rate_limit_exceeded":            ReasonUpstreamRateLimited,

	// 网关自身
	"json_marshal_failed":            ReasonGatewayInternalError,
	"gen_relay_info_failed":          ReasonGatewayInternalError,
	"query_data_error":               ReasonGatewayInternalError,
	"update_data_error":              ReasonGatewayInternalError,
	"aws_invoke_error":               ReasonGatewayInternalError,
	"channel:aws_client_error":       ReasonGatewayInternalError,
	"setup_locked_channel_failed":    ReasonGatewayInternalError,
	"marshal_response_failed":        ReasonGatewayInternalError,
	"task_insert_failed":             ReasonGatewayInternalError,
	"task_billing_settlement_failed": ReasonGatewayInternalError,
	"task_submit_failed":             ReasonGatewayInternalError,

	// 调用方
	"request_cancelled": ReasonCallerCancelled,
}

// Classify 由链路、错误码与 HTTP 状态码得出 reason 与「是否发送前失败」。
//
// errorCode 传各链路自己的稳定错误码字符串：relay 链路传
// types.NewAPIError.GetErrorCode()，task 链路传 dto.TaskError.Code。
// reason 词表两条链路共用，因为它们最终产出的是同一个对外契约。
func Classify(surface Surface, errorCode string, statusCode int) Classification {
	reason, known := reasonByErrorCode[errorCode]
	if !known {
		reason = classifyByStatus(statusCode)
	}
	preDispatch := sharedPreDispatchErrorCodes[errorCode]
	if !preDispatch {
		switch surface {
		case SurfaceRelay:
			preDispatch = relayOnlyPreDispatchErrorCodes[errorCode]
		case SurfaceTask:
			preDispatch = taskOnlyPreDispatchErrorCodes[errorCode]
		}
	}
	return Classification{Reason: reason, PreDispatchFailure: preDispatch}
}

// classifyByStatus 是错误码没命中时的状态码兜底。
// 上游透传回来的错误码五花八门，状态码是唯一稳定可用的信号。
func classifyByStatus(statusCode int) Reason {
	switch {
	case statusCode == http.StatusTooManyRequests:
		return ReasonUpstreamRateLimited
	case statusCode == http.StatusRequestTimeout || statusCode == http.StatusGatewayTimeout:
		return ReasonUpstreamTimeout
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		return ReasonCredentialRejected
	case statusCode == http.StatusNotFound:
		return ReasonModelNotAvailable
	case statusCode == http.StatusRequestEntityTooLarge:
		return ReasonRequestInvalid
	case statusCode >= 500 && statusCode <= 599:
		return ReasonUpstreamUnavailable
	case statusCode >= 400 && statusCode <= 499:
		return ReasonUpstreamRejectedRequest
	default:
		return ReasonUnclassifiedFailure
	}
}

// Resolve 是终局失败的一站式入口：分类 → 判定 dispatch_state → 组装 Control。
// 调用方拿到的 Control 同时喂给响应头和 body 副本，两侧不可能分叉。
func Resolve(c *gin.Context, surface Surface, errorCode string, statusCode int) Control {
	classification := Classify(surface, errorCode, statusCode)
	return New(classification.Reason, ResolveDispatchState(c, classification.PreDispatchFailure))
}
