package controllertests

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/retrycontrol"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// VerifyOrdinaryAudioSingleAttempt 由同包测试入口传入真实重试准入，表格只提供确定输入。
func VerifyOrdinaryAudioSingleAttempt(t *testing.T, shouldRetry func(*gin.Context, *types.NewAPIError, int) bool) {
	t.Helper()
	require.NotNil(t, shouldRetry)
	previousRetryTimes := common.RetryTimes
	previousRanges := operation_setting.AutomaticRetryStatusCodeRanges
	common.RetryTimes = 3
	operation_setting.AutomaticRetryStatusCodeRanges = []operation_setting.StatusCodeRange{{Start: 429, End: 429}, {Start: 500, End: 500}}
	t.Cleanup(func() {
		common.RetryTimes = previousRetryTimes
		operation_setting.AutomaticRetryStatusCodeRanges = previousRanges
	})

	for _, mode := range []struct {
		name string
		mode int
	}{
		{name: "speech", mode: relayconstant.RelayModeAudioSpeech},
		{name: "transcription", mode: relayconstant.RelayModeAudioTranscription},
		{name: "translation", mode: relayconstant.RelayModeAudioTranslation},
	} {
		for _, tc := range []struct {
			name       string
			errorCode  types.ErrorCode
			statusCode int
			retries    int
			dispatch   retrycontrol.DispatchState
			channel    bool
			skipRetry  bool
		}{
			{name: "500_before_dispatch", errorCode: types.ErrorCodeBadResponseStatusCode, statusCode: http.StatusInternalServerError, retries: 3, dispatch: retrycontrol.DispatchStateNotDispatched},
			{name: "429_after_bytes", errorCode: types.ErrorCodeBadResponseStatusCode, statusCode: http.StatusTooManyRequests, retries: 1, dispatch: retrycontrol.DispatchStateUnknown},
			{name: "channel_error_after_response", errorCode: types.ErrorCodeChannelResponseTimeExceeded, statusCode: http.StatusInternalServerError, retries: 3, dispatch: retrycontrol.DispatchStateDispatched, channel: true},
			{name: "channel_error_without_budget", errorCode: types.ErrorCodeChannelNoAvailableKey, statusCode: http.StatusServiceUnavailable, retries: 0, dispatch: retrycontrol.DispatchStateNotDispatched, channel: true},
			{name: "channel_error_negative_budget", errorCode: types.ErrorCodeChannelNoAvailableKey, statusCode: http.StatusServiceUnavailable, retries: -1, dispatch: retrycontrol.DispatchStateUnknown, channel: true},
			{name: "channel_error_with_skip", errorCode: types.ErrorCodeChannelResponseTimeExceeded, statusCode: http.StatusInternalServerError, retries: 3, dispatch: retrycontrol.DispatchStateDispatched, channel: true, skipRetry: true},
			{name: "body_read_failure_after_response", errorCode: types.ErrorCodeReadResponseBodyFailed, statusCode: http.StatusInternalServerError, retries: 2, dispatch: retrycontrol.DispatchStateDispatched},
			{name: "skip_retry_before_dispatch", errorCode: types.ErrorCodeBadResponseStatusCode, statusCode: http.StatusInternalServerError, retries: 3, dispatch: retrycontrol.DispatchStateNotDispatched, skipRetry: true},
		} {
			t.Run(mode.name+"/"+tc.name, func(t *testing.T) {
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				// 使用与 middleware 相同的可信模式键；路径刻意不含 audio，防止退化为路径猜测。
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				c.Set("relay_mode", mode.mode)
				tracker := retrycontrol.Tracker(c)
				switch tc.dispatch {
				case retrycontrol.DispatchStateNotDispatched:
					tracker.MarkInstrumentedAttempt()
				case retrycontrol.DispatchStateUnknown:
					tracker.MarkBytesWritten()
				case retrycontrol.DispatchStateDispatched:
					tracker.MarkResponseReceived()
				}
				require.Equal(t, tc.dispatch, retrycontrol.ResolveDispatchState(c, false))

				err := types.NewOpenAIError(errors.New("音频调用失败"), tc.errorCode, tc.statusCode)
				if tc.skipRetry {
					types.ErrOptionWithSkipRetry()(err)
				}
				require.Equal(t, tc.channel, types.IsChannelError(err))
				require.Equal(t, tc.skipRetry, types.IsSkipRetryError(err))
				assert.False(t, shouldRetry(c, err, tc.retries), "普通音频不得自动二次提交，不以错误、预算或派发状态放行")
			})
		}
	}
}

// VerifyNonAudioRetryUnchanged 保护非音频的既有状态码、预算与渠道错误策略。
func VerifyNonAudioRetryUnchanged(t *testing.T, shouldRetry func(*gin.Context, *types.NewAPIError, int) bool) {
	t.Helper()
	require.NotNil(t, shouldRetry)
	for _, tc := range []struct {
		name       string
		mode       int
		errorCode  types.ErrorCode
		statusCode int
		retries    int
		skipRetry  bool
		want       bool
	}{
		{name: "chat_500", mode: relayconstant.RelayModeChatCompletions, errorCode: types.ErrorCodeBadResponseStatusCode, statusCode: http.StatusInternalServerError, retries: 1, want: true},
		{name: "realtime_429", mode: relayconstant.RelayModeRealtime, errorCode: types.ErrorCodeBadResponseStatusCode, statusCode: http.StatusTooManyRequests, retries: 1, want: true},
		{name: "video_channel_error_without_budget", mode: relayconstant.RelayModeVideoSubmit, errorCode: types.ErrorCodeChannelNoAvailableKey, statusCode: http.StatusServiceUnavailable, retries: 0, want: true},
		{name: "chat_without_budget", mode: relayconstant.RelayModeChatCompletions, errorCode: types.ErrorCodeBadResponseStatusCode, statusCode: http.StatusInternalServerError, retries: 0, want: false},
		{name: "chat_success_status", mode: relayconstant.RelayModeChatCompletions, errorCode: types.ErrorCodeBadResponseStatusCode, statusCode: http.StatusOK, retries: 1, want: false},
		{name: "chat_bad_response_body", mode: relayconstant.RelayModeChatCompletions, errorCode: types.ErrorCodeBadResponseBody, statusCode: http.StatusInternalServerError, retries: 1, want: false},
		{name: "chat_skip_retry", mode: relayconstant.RelayModeChatCompletions, errorCode: types.ErrorCodeBadResponseStatusCode, statusCode: http.StatusInternalServerError, retries: 1, skipRetry: true, want: false},
		{name: "chat_without_error", mode: relayconstant.RelayModeChatCompletions, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			// 即使路径含 audio，也只以可信模式闭集限制普通音频。
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", nil)
			c.Set("relay_mode", tc.mode)
			retrycontrol.Tracker(c).MarkResponseReceived()
			var err *types.NewAPIError
			if tc.errorCode != "" {
				err = types.NewOpenAIError(errors.New("非音频调用失败"), tc.errorCode, tc.statusCode)
				if tc.skipRetry {
					types.ErrOptionWithSkipRetry()(err)
				}
			}
			assert.Equal(t, tc.want, shouldRetry(c, err, tc.retries))
		})
	}
}
