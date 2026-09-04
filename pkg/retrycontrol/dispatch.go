package retrycontrol

import (
	"net/http"
	"net/http/httptrace"
	"sync"

	"github.com/gin-gonic/gin"
)

// contextKeyDispatchTracker 是 tracker 在 gin.Context 上的存放键。
const contextKeyDispatchTracker = "hasn_relay_dispatch_tracker"

// DispatchTracker 记录**这一次客户端请求**（含其全部 attempt）里，
// 上游到底有没有收到过字节。
//
// 它是单调的：not_dispatched → unknown → dispatched，只升不降。
// 第 1 次 attempt 把请求发给了渠道 A、第 2 次 attempt 连渠道都没选出来，
// 整体依然是「已经发出去过」——沿用第 2 次 attempt 的局部事实会让
// dispatch_state 回落成 not_dispatched，daemon 于是重放，渠道 A 那笔照样计费。
type DispatchTracker struct {
	mu sync.Mutex
	// state 是至今为止观察到的最强证据。
	state DispatchState
	// instrumented 记录是否至少走过一次**带 httptrace 的**上游发送路径。
	// 它是 not_dispatched 的正面证据来源：走过了，且 trace 一个字节都没报，
	// 才敢说「确证没发出去」。
	instrumented bool
}

// Tracker 取出（必要时创建）本次请求的 tracker。
func Tracker(c *gin.Context) *DispatchTracker {
	if c == nil {
		return &DispatchTracker{}
	}
	if existing, ok := c.Get(contextKeyDispatchTracker); ok {
		if tracker, ok := existing.(*DispatchTracker); ok && tracker != nil {
			return tracker
		}
	}
	tracker := &DispatchTracker{}
	c.Set(contextKeyDispatchTracker, tracker)
	return tracker
}

// raise 把状态抬到 next，只升不降。
func (t *DispatchTracker) raise(next DispatchState) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if next.rank() > t.state.rank() {
		t.state = next
	}
}

// MarkInstrumentedAttempt 在进入**已插桩**的上游发送路径时调用。
//
// 它只声明「这条路径上的字节写出会被 httptrace 观测到」，不声明请求已发出。
func (t *DispatchTracker) MarkInstrumentedAttempt() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.instrumented = true
}

// MarkBytesWritten 在确认已经有请求字节写向上游时调用。
//
// 写出去 ≠ 上游一定处理了（连接可能在冲刷时断开），所以这里只抬到 unknown。
// unknown 与 dispatched 对 daemon 的效果相同（都不得重放），差别只在可读性。
func (t *DispatchTracker) MarkBytesWritten() {
	t.raise(DispatchStateUnknown)
}

// MarkResponseReceived 在拿到上游响应（任何状态码）时调用：上游确实处理过这个请求。
func (t *DispatchTracker) MarkResponseReceived() {
	t.raise(DispatchStateDispatched)
}

// MarkUninstrumentedAttempt 给**没有 httptrace 可插**的上游发送路径使用
// （gorilla websocket 拨号、厂商 SDK 自带传输等）。
//
// 这类路径无法区分「连接失败」和「发出后失败」，一律记为 unknown——
// 宁可少一次可安全重放的机会，也不能给出一个可能错的 not_dispatched。
func (t *DispatchTracker) MarkUninstrumentedAttempt() {
	t.raise(DispatchStateUnknown)
}

// observed 返回目前的证据：状态，以及是否走过插桩路径。
func (t *DispatchTracker) observed() (DispatchState, bool) {
	if t == nil {
		return DispatchStateNotDispatched, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state == "" {
		// 零值即「还没有任何证据」，对外一律说成 not_dispatched，
		// 不让空串泄漏成第四个状态。
		return DispatchStateNotDispatched, t.instrumented
	}
	return t.state, t.instrumented
}

// State 只读当前状态，供日志与测试使用。
func (t *DispatchTracker) State() DispatchState {
	state, _ := t.observed()
	return state
}

// TraceRequest 给出站请求挂上 httptrace，把「请求头/请求体已经写向连接」这件事
// 反馈进 tracker。
//
// 为什么用 httptrace 而不是「Do 返回错误就算没发出去」：http.Client.Do 的错误
// 同时覆盖「拨号失败（确实没发出）」和「写完请求等响应超时（已经发出）」两类，
// 从错误值上分不开。httptrace 的 WroteHeaders 由 transport 在把请求头写进连接时
// 触发，HTTP/1 与 HTTP/2 都会触发，是唯一能把两类分开的正面信号。
func TraceRequest(req *http.Request, tracker *DispatchTracker) *http.Request {
	if req == nil || tracker == nil {
		return req
	}
	trace := &httptrace.ClientTrace{
		WroteHeaders: func() {
			tracker.MarkBytesWritten()
		},
		WroteRequest: func(httptrace.WroteRequestInfo) {
			tracker.MarkBytesWritten()
		},
	}
	return req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
}

// ResolveDispatchState 给出本次请求终局的 dispatch_state。
//
// 判据按证据强度排序，**not_dispatched 只在有正面证据时才产出**：
//
//  1. tracker 已经观测到字节写出或收到响应 → 直接用观测值（unknown / dispatched）；
//  2. 走过插桩路径、而 httptrace 一个字节都没报 → 确证没发出，not_dispatched；
//  3. 没走过任何上游发送路径，但失败点在**闭集内的发送前阶段**
//     （校验、计价、预扣费、选渠道、构造请求体）→ not_dispatched；
//  4. 其余一律 unknown——包括厂商 SDK 这类没有插桩的发送路径。
func ResolveDispatchState(c *gin.Context, preDispatchFailure bool) DispatchState {
	state, instrumented := Tracker(c).observed()
	if state.rank() > DispatchStateNotDispatched.rank() {
		return state
	}
	if instrumented {
		return DispatchStateNotDispatched
	}
	if preDispatchFailure {
		return DispatchStateNotDispatched
	}
	return DispatchStateUnknown
}

// ArmStreamHeaders 在流式响应真正开始之前，先把四个头写成**安全兜底值**。
//
// 为什么必须提前写：流式响应的头在第一个字节落地时就冲刷出去了，而 SSE 保活
// ping 可能在上游响应回来之前就写出第一帧。头一旦冲刷，之后再改头表毫无效果。
// 于是「开流那一刻头表里是什么」就是 daemon 将来能看到的全部事实。
//
// 兜底值取 dispatch_state 至少 unknown、reason=stream_interrupted、retryable=false：
// 客户端能看见流，就说明这次调用已经进到了不可重放的阶段。设计 §13.2 的
// 「已发首帧一律按 dispatched 处理，直接返回 llm.stream.interrupted，不做任何重放」
// 说的就是这一格。
//
// 拿到上游响应后再调用一次，dispatch_state 会升到 dispatched；那时若头还没冲刷，
// 客户端看到的就是更精确的值，冲刷过了也不会退回一个更危险的值。
func ArmStreamHeaders(c *gin.Context) {
	if c == nil {
		return
	}
	state := Tracker(c).State()
	if state.rank() < DispatchStateUnknown.rank() {
		state = DispatchStateUnknown
	}
	WriteHeaders(c, New(ReasonStreamInterrupted, state))
}
