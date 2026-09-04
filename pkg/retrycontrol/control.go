// Package retrycontrol 实现 LLM 网关的重试控制契约。
//
// 契约事实源：唤星父仓
// docs/产品与技术/技术设计/02-平台能力/Runtime与工具体系/运行时配置下发/
// 02-LLM网关、模型策略与Runtime调用设计.md §13.2。
//
// 两条不可动摇的规则：
//
//  1. **判定只读响应头**。daemon 的跨模型 failover 只读 X-Hasn-* 四个响应头；
//     非流式错误 body 里的 retry_control 对象只是同一事实的可读副本，给人和日志看。
//     不能只放 body——流式响应在首帧之后已经没有 body 位置可写，而这套判定
//     恰恰要在中断时知道 dispatch_state。
//  2. **头与 body 取值来自同一闭集，任一侧都不得缩写或改写**（not_dispatched
//     不得在头里写成 nd）。本包让两侧从同一个 Control 值派生，从结构上堵死分叉。
package retrycontrol

import (
	"strconv"

	"github.com/gin-gonic/gin"
)

// 四个响应头的名字。daemon 只认这四个。
const (
	HeaderDispatchState = "X-Hasn-Dispatch-State"
	HeaderRetryReason   = "X-Hasn-Retry-Reason"
	HeaderRetryable     = "X-Hasn-Retryable"
	HeaderRetryAfterMs  = "X-Hasn-Retry-After-Ms"
)

// DispatchState 回答「上游请求到底发出去了没有」。闭集只有三个值。
//
// ⚠️ 这是全线最关键的安全点：猜成 not_dispatched 会让 daemon 换模型重放，
// 而上游其实已经收到并计了费——重复扣费。只有**确证尚未发出**才可以是
// not_dispatched；已确证发出是 dispatched；剩下的一律 unknown。
type DispatchState string

const (
	// DispatchStateNotDispatched 确证上游一个字节都没有发出去。可安全重放。
	DispatchStateNotDispatched DispatchState = "not_dispatched"
	// DispatchStateDispatched 确证上游已经收到请求（我们拿到过响应）。禁止重放。
	DispatchStateDispatched DispatchState = "dispatched"
	// DispatchStateUnknown 无法确定。按已发出处理，禁止重放。
	DispatchStateUnknown DispatchState = "unknown"
)

// rank 给三态排一个单调序，用于跨多次 attempt 取最大值。
// 一次请求里只要有任何一次 attempt 把字节发出去过，整个请求就不可能再回落到
// not_dispatched。
func (s DispatchState) rank() int {
	switch s {
	case DispatchStateDispatched:
		return 2
	case DispatchStateUnknown:
		return 1
	default:
		return 0
	}
}

// Reason 是本契约**自己的**失败原因词表。
//
// ⚠️ 禁止复用可观测性域 diag.failure_class 那个九值闭集：那套词表回答的是
// 「这条错误属于哪一类以便聚合」，本词表回答的是「daemon 该不该换个模型再试」，
// 两者的判据、消费方和演进节奏都不同，合并会让任何一侧的改动波及另一侧。
type Reason string

const (
	// ReasonModelCapacityExhausted 该模型在本网关下没有可用渠道（或渠道全部被熔断）。
	ReasonModelCapacityExhausted Reason = "model_capacity_exhausted"
	// ReasonModelNotAvailable 模型不存在，或未对该分组开放。
	ReasonModelNotAvailable Reason = "model_not_available"
	// ReasonUpstreamRateLimited 上游或本网关限流。
	ReasonUpstreamRateLimited Reason = "upstream_rate_limited"
	// ReasonUpstreamUnavailable 上游连接失败或返回 5xx。
	ReasonUpstreamUnavailable Reason = "upstream_unavailable"
	// ReasonUpstreamTimeout 上游超时。
	ReasonUpstreamTimeout Reason = "upstream_timeout"
	// ReasonUpstreamRejectedRequest 上游明确拒绝了这个请求（非限流的 4xx）。
	ReasonUpstreamRejectedRequest Reason = "upstream_rejected_request"
	// ReasonRequestInvalid 请求本身非法：参数、格式、体过大。
	ReasonRequestInvalid Reason = "request_invalid"
	// ReasonContextLengthExceeded 上下文超过模型窗口。
	ReasonContextLengthExceeded Reason = "context_length_exceeded"
	// ReasonContentBlocked 内容审查拒绝。
	ReasonContentBlocked Reason = "content_blocked"
	// ReasonCredentialRejected 凭据或权限被拒。
	ReasonCredentialRejected Reason = "credential_rejected"
	// ReasonQuotaInsufficient 主人余额或配额不足。
	ReasonQuotaInsufficient Reason = "quota_insufficient"
	// ReasonCallerCancelled 调用方主动取消或断开。
	ReasonCallerCancelled Reason = "caller_cancelled"
	// ReasonStreamInterrupted 流式响应在首帧之后中断。
	ReasonStreamInterrupted Reason = "stream_interrupted"
	// ReasonGatewayInternalError 网关自身故障（数据库、序列化、内部不变量）。
	ReasonGatewayInternalError Reason = "gateway_internal_error"
	// ReasonUnclassifiedFailure 落不进上面任何一格。按不可重放处理。
	ReasonUnclassifiedFailure Reason = "unclassified_failure"
)

// allReasons 是词表闭集本体。校验与测试都从这里取，不另抄一份。
var allReasons = []Reason{
	ReasonModelCapacityExhausted,
	ReasonModelNotAvailable,
	ReasonUpstreamRateLimited,
	ReasonUpstreamUnavailable,
	ReasonUpstreamTimeout,
	ReasonUpstreamRejectedRequest,
	ReasonRequestInvalid,
	ReasonContextLengthExceeded,
	ReasonContentBlocked,
	ReasonCredentialRejected,
	ReasonQuotaInsufficient,
	ReasonCallerCancelled,
	ReasonStreamInterrupted,
	ReasonGatewayInternalError,
	ReasonUnclassifiedFailure,
}

// AllReasons 返回 reason 闭集的副本，供校验与测试遍历。
func AllReasons() []Reason {
	out := make([]Reason, len(allReasons))
	copy(out, allReasons)
	return out
}

// AllDispatchStates 返回 dispatch_state 闭集的副本。
func AllDispatchStates() []DispatchState {
	return []DispatchState{DispatchStateNotDispatched, DispatchStateDispatched, DispatchStateUnknown}
}

// reasonRetryPolicy 声明每个 reason **在 dispatch_state=not_dispatched 前提下**
// 是否允许 daemon 换模型重放，以及建议的退避毫秒数。
//
// 设计 §13.2 点名不得重放的：参数错误、上下文过长、内容拒绝、余额不足、
// 权限/凭据错误、用户取消。它们在这里一律 replayable=false。
var reasonRetryPolicy = map[Reason]struct {
	replayable   bool
	retryAfterMs int
}{
	ReasonModelCapacityExhausted:  {replayable: true, retryAfterMs: 500},
	ReasonUpstreamRateLimited:     {replayable: true, retryAfterMs: 1000},
	ReasonUpstreamUnavailable:     {replayable: true, retryAfterMs: 500},
	ReasonUpstreamTimeout:         {replayable: true, retryAfterMs: 0},
	ReasonGatewayInternalError:    {replayable: true, retryAfterMs: 500},
	ReasonModelNotAvailable:       {replayable: false},
	ReasonUpstreamRejectedRequest: {replayable: false},
	ReasonRequestInvalid:          {replayable: false},
	ReasonContextLengthExceeded:   {replayable: false},
	ReasonContentBlocked:          {replayable: false},
	ReasonCredentialRejected:      {replayable: false},
	ReasonQuotaInsufficient:       {replayable: false},
	ReasonCallerCancelled:         {replayable: false},
	ReasonStreamInterrupted:       {replayable: false},
	ReasonUnclassifiedFailure:     {replayable: false},
}

// Control 是一次终局失败的重试控制事实。响应头与 body 副本都从这一个值派生，
// 两侧因此不可能出现异名或异值。
type Control struct {
	Reason        Reason        `json:"reason"`
	DispatchState DispatchState `json:"dispatch_state"`
	Retryable     bool          `json:"retryable"`
	RetryAfterMs  int           `json:"retry_after_ms"`
}

// New 按 reason 与 dispatch_state 组合出终局的 Control。
//
// retryable 是两个条件的**合取**：reason 本身允许重放，且确证请求没有发出去。
// 只要 dispatch_state 不是 not_dispatched，retryable 一律 false——设计 §13.2 里
// 「dispatched、unknown 都不得重放」就是这一行。
func New(reason Reason, state DispatchState) Control {
	policy, known := reasonRetryPolicy[reason]
	if !known {
		// 词表外的值不允许悄悄流出去：降级成 unclassified_failure，按不可重放处理。
		reason = ReasonUnclassifiedFailure
		policy = reasonRetryPolicy[ReasonUnclassifiedFailure]
	}
	if state != DispatchStateNotDispatched && state != DispatchStateDispatched {
		state = DispatchStateUnknown
	}
	retryable := policy.replayable && state == DispatchStateNotDispatched
	retryAfterMs := 0
	if retryable {
		retryAfterMs = policy.retryAfterMs
	}
	return Control{
		Reason:        reason,
		DispatchState: state,
		Retryable:     retryable,
		RetryAfterMs:  retryAfterMs,
	}
}

// Headers 把 Control 摊平成四个响应头。取值逐字等于 Control 的字段，
// 不做任何缩写、大小写改写或别名。
func (ctl Control) Headers() map[string]string {
	return map[string]string{
		HeaderDispatchState: string(ctl.DispatchState),
		HeaderRetryReason:   string(ctl.Reason),
		HeaderRetryable:     strconv.FormatBool(ctl.Retryable),
		HeaderRetryAfterMs:  strconv.Itoa(ctl.RetryAfterMs),
	}
}

// WriteHeaders 把四个头写进响应头表。
//
// ⚠️ 必须在任何响应体字节落地之前调用：Go 在第一次写 body 时才把头表冲刷出去，
// 之后再改头表没有任何效果，而流式响应的第一个 ping 帧就可能触发冲刷。
func WriteHeaders(c *gin.Context, ctl Control) {
	if c == nil {
		return
	}
	for name, value := range ctl.Headers() {
		c.Header(name, value)
	}
}
