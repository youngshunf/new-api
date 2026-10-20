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

func TestVideoGenerateCleanupFailureClassificationIsNotPreDispatch(t *testing.T) {
	for _, tc := range []struct {
		name    string
		surface retrycontrol.Surface
	}{
		{name: "task", surface: retrycontrol.SurfaceTask},
		{name: "relay", surface: retrycontrol.SurfaceRelay},
	} {
		t.Run(tc.name, func(t *testing.T) {
			classification := retrycontrol.Classify(tc.surface, "video_generate_cleanup_failed", http.StatusInternalServerError)
			assert.Equal(t, retrycontrol.ReasonUnclassifiedFailure, classification.Reason, "清理失败必须终止，不能由500兜底成可重放的上游故障")
			assert.False(t, classification.PreDispatchFailure, "清理可能发生在创建之后，机器码本身不能证明未派发")
		})
	}
}

func TestVideoGenerateCleanupFailureResolveIsTerminalWithMatchingCarriers(t *testing.T) {
	for _, state := range []retrycontrol.DispatchState{
		retrycontrol.DispatchStateNotDispatched,
		retrycontrol.DispatchStateUnknown,
		retrycontrol.DispatchStateDispatched,
	} {
		t.Run(string(state), func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			require.NotNil(t, c)
			tracker := retrycontrol.Tracker(c)
			switch state {
			case retrycontrol.DispatchStateNotDispatched:
				tracker.MarkInstrumentedAttempt()
			case retrycontrol.DispatchStateUnknown:
				tracker.MarkBytesWritten()
			case retrycontrol.DispatchStateDispatched:
				tracker.MarkResponseReceived()
			}

			control := retrycontrol.Resolve(c, retrycontrol.SurfaceTask, "video_generate_cleanup_failed", http.StatusInternalServerError)
			assert.Equal(t, retrycontrol.ReasonUnclassifiedFailure, control.Reason)
			assert.Equal(t, state, control.DispatchState, "已有派发事实不得因清理失败而降级")
			assert.False(t, control.Retryable, "即使明确未派发，清理失败也不能授予新创建资格")
			assert.Zero(t, control.RetryAfterMs)
			assert.Equal(t, retrycontrol.New(retrycontrol.ReasonUnclassifiedFailure, state), control, "沿原Control策略，不另建清理重试词表")

			// 仅核原Control的头与body载体，不冒充controller/middleware已接通该错误出口。
			retrycontrol.WriteHeaders(c, control)
			c.JSON(http.StatusInternalServerError, gin.H{"retry_control": control})
			assert.Equal(t, http.StatusInternalServerError, recorder.Code)
			for name, value := range control.Headers() {
				assert.Equal(t, []string{value}, recorder.Header().Values(name), "头值必须逐字一致且无重复：%s", name)
			}
			assert.Equal(t, "unclassified_failure", recorder.Header().Get(retrycontrol.HeaderRetryReason))
			assert.Equal(t, string(state), recorder.Header().Get(retrycontrol.HeaderDispatchState))
			assert.Equal(t, "false", recorder.Header().Get(retrycontrol.HeaderRetryable))
			assert.Equal(t, "0", recorder.Header().Get(retrycontrol.HeaderRetryAfterMs))
			assert.JSONEq(t, fmt.Sprintf(`{"retry_control":{"reason":"unclassified_failure","dispatch_state":%q,"retryable":false,"retry_after_ms":0}}`, state), recorder.Body.String())
		})
	}
}
