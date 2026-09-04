package retrycontrol

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	return c, recorder
}

// 头与 body 是同一个契约的两个载体，取值必须逐字相同。
// 这条断言遍历整个闭集，任何一侧被改成缩写、大小写变体或别名都会红。
func TestHeadersCarryTheSameClosedSetValuesAsTheBodyCopy(t *testing.T) {
	for _, reason := range AllReasons() {
		for _, state := range AllDispatchStates() {
			control := New(reason, state)
			headers := control.Headers()

			assert.Equal(t, string(control.DispatchState), headers[HeaderDispatchState])
			assert.Equal(t, string(control.Reason), headers[HeaderRetryReason])
			assert.Equal(t, strconv.FormatBool(control.Retryable), headers[HeaderRetryable])
			assert.Equal(t, strconv.Itoa(control.RetryAfterMs), headers[HeaderRetryAfterMs])

			// 闭集值本身不得被改写。
			assert.Equal(t, reason, control.Reason)
			assert.Equal(t, state, control.DispatchState)
		}
	}
}

// dispatch_state 不是 not_dispatched 时一律不可重放——设计 §13.2
// 「dispatched、unknown 都不得重放」。
func TestRetryableRequiresBothReplayableReasonAndNotDispatched(t *testing.T) {
	for _, reason := range AllReasons() {
		for _, state := range []DispatchState{DispatchStateDispatched, DispatchStateUnknown} {
			control := New(reason, state)
			assert.Falsef(t, control.Retryable, "reason=%s state=%s 不得可重放", reason, state)
			assert.Zerof(t, control.RetryAfterMs, "reason=%s state=%s 不可重放时不应给退避", reason, state)
		}
	}

	// 设计点名不得重放的六类，即使确证没发出去也不可重放。
	for _, reason := range []Reason{
		ReasonRequestInvalid,
		ReasonContextLengthExceeded,
		ReasonContentBlocked,
		ReasonQuotaInsufficient,
		ReasonCredentialRejected,
		ReasonCallerCancelled,
	} {
		control := New(reason, DispatchStateNotDispatched)
		assert.Falsef(t, control.Retryable, "reason=%s 属于设计点名的不可重放类", reason)
	}

	// 设计示例：容量耗尽 + 确证没发出 → 可重放，退避 500ms。
	capacity := New(ReasonModelCapacityExhausted, DispatchStateNotDispatched)
	assert.True(t, capacity.Retryable)
	assert.Equal(t, 500, capacity.RetryAfterMs)
}

// 词表外的值不得流出去。
func TestUnknownReasonAndStateDegradeToSafeValues(t *testing.T) {
	control := New(Reason("some_made_up_reason"), DispatchState("half_dispatched"))
	assert.Equal(t, ReasonUnclassifiedFailure, control.Reason)
	assert.Equal(t, DispatchStateUnknown, control.DispatchState)
	assert.False(t, control.Retryable)
}

// tracker 单调：任何一次 attempt 把字节发出去过，后续都不得回落到 not_dispatched。
func TestDispatchTrackerIsMonotonicAcrossAttempts(t *testing.T) {
	c, _ := newTestContext()
	tracker := Tracker(c)
	require.Equal(t, DispatchStateNotDispatched, tracker.State())

	tracker.MarkBytesWritten()
	assert.Equal(t, DispatchStateUnknown, tracker.State())

	tracker.MarkResponseReceived()
	assert.Equal(t, DispatchStateDispatched, tracker.State())

	// 后一次 attempt 连字节都没发出，整体状态不得回落。
	tracker.MarkInstrumentedAttempt()
	assert.Equal(t, DispatchStateDispatched, tracker.State())
	assert.Equal(t, DispatchStateDispatched, ResolveDispatchState(c, true))

	// 同一个 context 上取到的必须是同一个 tracker。
	assert.Same(t, tracker, Tracker(c))
}

func TestResolveDispatchStateEvidenceLadder(t *testing.T) {
	t.Run("走过插桩路径且一个字节都没发出=确证未发出", func(t *testing.T) {
		c, _ := newTestContext()
		Tracker(c).MarkInstrumentedAttempt()
		assert.Equal(t, DispatchStateNotDispatched, ResolveDispatchState(c, false))
	})

	t.Run("没走插桩路径但失败点在发送前闭集内=确证未发出", func(t *testing.T) {
		c, _ := newTestContext()
		assert.Equal(t, DispatchStateNotDispatched, ResolveDispatchState(c, true))
	})

	t.Run("既没插桩也不在发送前闭集内=无法确定", func(t *testing.T) {
		c, _ := newTestContext()
		assert.Equal(t, DispatchStateUnknown, ResolveDispatchState(c, false))
	})

	t.Run("未插桩的发送路径把状态抬到unknown后不得再降为未发出", func(t *testing.T) {
		c, _ := newTestContext()
		Tracker(c).MarkUninstrumentedAttempt()
		assert.Equal(t, DispatchStateUnknown, ResolveDispatchState(c, true))
	})
}

// httptrace 是 not_dispatched 的第一证据来源。这里用真实的 TCP 与真实的
// http.Transport 验证两个方向，不做任何 mock。
func TestTraceRequestObservesRealTransportBehaviour(t *testing.T) {
	t.Run("请求真的发出去并拿到响应=dispatched", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}))
		defer server.Close()

		c, _ := newTestContext()
		tracker := Tracker(c)
		tracker.MarkInstrumentedAttempt()

		req, err := http.NewRequest(http.MethodGet, server.URL, nil)
		require.NoError(t, err)
		resp, err := server.Client().Do(TraceRequest(req, tracker))
		require.NoError(t, err)
		defer resp.Body.Close()

		// 上游确实写出去了：httptrace 已把状态抬到 unknown。
		assert.Equal(t, DispatchStateUnknown, tracker.State())
		tracker.MarkResponseReceived()
		assert.Equal(t, DispatchStateDispatched, ResolveDispatchState(c, false))
	})

	t.Run("连接被拒=一个字节都没发出=not_dispatched", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		deadURL := "http://" + listener.Addr().String()
		require.NoError(t, listener.Close()) // 端口立刻空出来，连接必被拒

		c, _ := newTestContext()
		tracker := Tracker(c)
		tracker.MarkInstrumentedAttempt()

		req, err := http.NewRequest(http.MethodGet, deadURL, nil)
		require.NoError(t, err)
		resp, err := (&http.Client{}).Do(TraceRequest(req, tracker))
		require.Error(t, err)
		require.Nil(t, resp)

		// httptrace 一次 WroteHeaders 都没触发，可以确证没发出去。
		assert.Equal(t, DispatchStateNotDispatched, tracker.State())
		assert.Equal(t, DispatchStateNotDispatched, ResolveDispatchState(c, false))
	})
}

// 流式响应的头必须在第一个字节之前就位，且**绝不能**写成 not_dispatched：
// 客户端看得见流就说明这次调用已经不可重放。
func TestArmStreamHeadersNeverPublishesNotDispatched(t *testing.T) {
	c, recorder := newTestContext()
	ArmStreamHeaders(c)

	assert.Equal(t, string(DispatchStateUnknown), recorder.Header().Get(HeaderDispatchState))
	assert.Equal(t, string(ReasonStreamInterrupted), recorder.Header().Get(HeaderRetryReason))
	assert.Equal(t, "false", recorder.Header().Get(HeaderRetryable))

	// 拿到上游响应之后重新武装，头升到更精确的 dispatched。
	Tracker(c).MarkResponseReceived()
	ArmStreamHeaders(c)
	assert.Equal(t, string(DispatchStateDispatched), recorder.Header().Get(HeaderDispatchState))
	assert.Equal(t, "false", recorder.Header().Get(HeaderRetryable))
}

// 同一个错误码在两条链路上的失败时点不同，preDispatch 判定必须分链路。
func TestClassifyIsSurfaceScopedForPreDispatchEvidence(t *testing.T) {
	relayQuota := Classify(SurfaceRelay, "insufficient_user_quota", http.StatusForbidden)
	assert.Equal(t, ReasonQuotaInsufficient, relayQuota.Reason)
	assert.True(t, relayQuota.PreDispatchFailure)

	// 异步任务链路上同名码还有一个 reserve 阶段产出点，那是上游任务已经创建之后。
	taskQuota := Classify(SurfaceTask, "insufficient_user_quota", http.StatusForbidden)
	assert.Equal(t, ReasonQuotaInsufficient, taskQuota.Reason)
	assert.False(t, taskQuota.PreDispatchFailure)

	// 任务链路自己的发送前错误码在 relay 链路上不成立（那里没有这些阶段）。
	assert.True(t, Classify(SurfaceTask, "build_request_failed", http.StatusInternalServerError).PreDispatchFailure)
	assert.False(t, Classify(SurfaceRelay, "build_request_failed", http.StatusInternalServerError).PreDispatchFailure)

	// 两条链路共有的发送前错误码两边都成立。
	for _, surface := range []Surface{SurfaceRelay, SurfaceTask} {
		assert.True(t, Classify(surface, "get_channel_failed", http.StatusInternalServerError).PreDispatchFailure)
	}
}

// 落在发送之后的错误码绝不能被判成「发送前失败」。
func TestPostDispatchErrorCodesAreNeverPreDispatch(t *testing.T) {
	for _, code := range []string{
		"do_request_failed",
		"bad_response_status_code",
		"read_response_body_failed",
		"empty_response",
		"fail_to_fetch_task",
		"task_insert_failed",
		"task_billing_settlement_failed",
		"plugin_submit_response_invalid",
		"request_cancelled",
		"channel:response_time_exceeded",
	} {
		for _, surface := range []Surface{SurfaceRelay, SurfaceTask} {
			assert.Falsef(t, Classify(surface, code, http.StatusInternalServerError).PreDispatchFailure,
				"错误码 %q 不得进入发送前闭集", code)
		}
	}
}

func TestClassifyFallsBackToStatusCode(t *testing.T) {
	cases := []struct {
		status int
		reason Reason
	}{
		{http.StatusTooManyRequests, ReasonUpstreamRateLimited},
		{http.StatusGatewayTimeout, ReasonUpstreamTimeout},
		{http.StatusUnauthorized, ReasonCredentialRejected},
		{http.StatusNotFound, ReasonModelNotAvailable},
		{http.StatusRequestEntityTooLarge, ReasonRequestInvalid},
		{http.StatusBadGateway, ReasonUpstreamUnavailable},
		{http.StatusBadRequest, ReasonUpstreamRejectedRequest},
		{0, ReasonUnclassifiedFailure},
	}
	for _, tc := range cases {
		got := Classify(SurfaceRelay, "some_upstream_code_we_do_not_know", tc.status)
		assert.Equalf(t, tc.reason, got.Reason, "status=%d", tc.status)
		assert.False(t, got.PreDispatchFailure)
	}
}

// 每个 reason 都必须在重放策略表里有一格，漏一个就会被 New 静默降级成
// unclassified_failure——那种降级在生产里是看不见的。
func TestEveryReasonHasARetryPolicy(t *testing.T) {
	for _, reason := range AllReasons() {
		_, ok := reasonRetryPolicy[reason]
		assert.Truef(t, ok, "reason %q 缺少重放策略", reason)
	}
	assert.Len(t, reasonRetryPolicy, len(AllReasons()), "策略表里有词表之外的项")
}

// reason 词表不得与可观测性域 diag.failure_class 那个九值闭集重名。
func TestReasonVocabularyDoesNotReuseDiagFailureClassTokens(t *testing.T) {
	diagFailureClassTokens := map[string]bool{
		"invalid_input": true, "not_found": true, "permission_denied": true,
		"unavailable": true, "timeout": true, "conflict": true,
		"exhausted": true, "internal": true, "cancelled": true,
	}
	for _, reason := range AllReasons() {
		assert.Falsef(t, diagFailureClassTokens[string(reason)],
			"reason %q 与 diag.failure_class 闭集重名", reason)
	}
}

func TestWriteHeadersEmitsAllFourCarriers(t *testing.T) {
	c, recorder := newTestContext()
	WriteHeaders(c, New(ReasonUpstreamRateLimited, DispatchStateNotDispatched))

	assert.Equal(t, "not_dispatched", recorder.Header().Get(HeaderDispatchState))
	assert.Equal(t, "upstream_rate_limited", recorder.Header().Get(HeaderRetryReason))
	assert.Equal(t, "true", recorder.Header().Get(HeaderRetryable))
	assert.Equal(t, "1000", recorder.Header().Get(HeaderRetryAfterMs))
}
