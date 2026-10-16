package retrycontroltests

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/pkg/retrycontrol"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLocalResourceBusyExtendsReasonSetExactlyOnce(t *testing.T) {
	// 从原公开API取全集；只允许在原十五值之外新增本地资源忙，不引用待实现常量造编译红。
	reasons := retrycontrol.AllReasons()
	require.NotEmpty(t, reasons)
	expected := []retrycontrol.Reason{
		"model_capacity_exhausted", "model_not_available", "upstream_rate_limited",
		"upstream_unavailable", "upstream_timeout", "upstream_rejected_request",
		"request_invalid", "context_length_exceeded", "content_blocked", "credential_rejected",
		"quota_insufficient", "caller_cancelled", "stream_interrupted", "gateway_internal_error",
		"unclassified_failure", "local_resource_busy",
	}
	assert.ElementsMatch(t, expected, reasons, "必须保留原十五值且只增加具名资源忙，不能改名或混入第二词表")
	assert.Len(t, reasons, 16)
	busyCount := 0
	for _, reason := range reasons {
		if reason == retrycontrol.Reason("local_resource_busy") {
			busyCount++
		}
	}
	assert.Equal(t, 1, busyCount, "local_resource_busy必须恰好出现一次")
}

func TestLocalResourceBusyClassificationIsTaskOnlyPreDispatch(t *testing.T) {
	t.Run("task", func(t *testing.T) {
		classification := retrycontrol.Classify(retrycontrol.SurfaceTask, "plugin_resource_busy", http.StatusServiceUnavailable)
		assert.Equal(t, retrycontrol.Classification{
			Reason:             retrycontrol.Reason("local_resource_busy"),
			PreDispatchFailure: true,
		}, classification, "宿主容量忙不是参数错误，也不是允许换模型的上游失败")
	})
	t.Run("relay", func(t *testing.T) {
		classification := retrycontrol.Classify(retrycontrol.SurfaceRelay, "plugin_resource_busy", http.StatusServiceUnavailable)
		assert.False(t, classification.PreDispatchFailure, "Task局部机器码不能给Relay链凭空增加未发送证据")
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		require.NotNil(t, c)
		control := retrycontrol.Resolve(c, retrycontrol.SurfaceRelay, "plugin_resource_busy", http.StatusServiceUnavailable)
		assert.Equal(t, retrycontrol.DispatchStateUnknown, control.DispatchState)
	})
}

func TestLocalResourceBusyResolvePreservesDispatchEvidence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state retrycontrol.DispatchState
	}{
		{name: "without_tracker", state: retrycontrol.DispatchStateNotDispatched},
		{name: "bytes_written", state: retrycontrol.DispatchStateUnknown},
		{name: "uninstrumented_attempt", state: retrycontrol.DispatchStateUnknown},
		{name: "response_received", state: retrycontrol.DispatchStateDispatched},
		{name: "response_then_uninstrumented_attempt", state: retrycontrol.DispatchStateDispatched},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			require.NotNil(t, c)
			// 不手填context状态；无tracker一项在Resolve前不调用Tracker。
			switch tc.name {
			case "bytes_written":
				retrycontrol.Tracker(c).MarkBytesWritten()
			case "uninstrumented_attempt":
				retrycontrol.Tracker(c).MarkUninstrumentedAttempt()
			case "response_received":
				retrycontrol.Tracker(c).MarkResponseReceived()
			case "response_then_uninstrumented_attempt":
				tracker := retrycontrol.Tracker(c)
				tracker.MarkResponseReceived()
				tracker.MarkUninstrumentedAttempt()
			}

			control := retrycontrol.Resolve(c, retrycontrol.SurfaceTask, "plugin_resource_busy", http.StatusServiceUnavailable)

			assert.Equal(t, retrycontrol.Control{
				Reason:        retrycontrol.Reason("local_resource_busy"),
				DispatchState: tc.state,
				Retryable:     false,
				RetryAfterMs:  0,
			}, control, "未发送只来自Task发送前证据；已有unknown/dispatched不得因容量拒绝降级")
			// 再标记一个未写字节的插桩attempt，不能清除本请求已有的发送证据。
			retrycontrol.Tracker(c).MarkInstrumentedAttempt()
			assert.Equal(t, control, retrycontrol.Resolve(c, retrycontrol.SurfaceTask, "plugin_resource_busy", http.StatusServiceUnavailable))
		})
	}
}

func TestLocalResourceBusyControlIsTerminalWithMatchingCarriers(t *testing.T) {
	for _, state := range []retrycontrol.DispatchState{
		retrycontrol.DispatchStateNotDispatched,
		retrycontrol.DispatchStateUnknown,
		retrycontrol.DispatchStateDispatched,
	} {
		t.Run(string(state), func(t *testing.T) {
			control := retrycontrol.New(retrycontrol.Reason("local_resource_busy"), state)
			assert.Equal(t, retrycontrol.Reason("local_resource_busy"), control.Reason, "不能把新增具名原因降成unclassified_failure")
			assert.Equal(t, state, control.DispatchState)
			assert.False(t, control.Retryable)
			assert.Zero(t, control.RetryAfterMs)
			expectedHeaders := map[string]string{
				retrycontrol.HeaderDispatchState: string(state),
				retrycontrol.HeaderRetryReason:   "local_resource_busy",
				retrycontrol.HeaderRetryable:     "false",
				retrycontrol.HeaderRetryAfterMs:  "0",
			}
			assert.Equal(t, expectedHeaders, control.Headers())

			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			require.NotNil(t, c)
			// 仅验证原Control的两个载体，不冒充生产middleware/controller错误出口已接通。
			retrycontrol.WriteHeaders(c, control)
			c.JSON(http.StatusServiceUnavailable, gin.H{"retry_control": control})

			assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
			for name, value := range expectedHeaders {
				assert.Equal(t, []string{value}, recorder.Header().Values(name), "四头取值必须逐字一致且没有重复头：%s", name)
			}
			assert.JSONEq(t, fmt.Sprintf(`{"retry_control":{"reason":"local_resource_busy","dispatch_state":%q,"retryable":false,"retry_after_ms":0}}`, state), recorder.Body.String())
		})
	}
}
