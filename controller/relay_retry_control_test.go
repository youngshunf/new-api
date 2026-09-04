package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/pkg/retrycontrol"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newRelayTestContext(t *testing.T, method, target string, body string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(method, target, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, recorder
}

// 头是 daemon 的判定载体，body 里的 retry_control 是同一事实的可读副本。
// 两侧任何一个取值不同，daemon 与人就会读出两个结论。
func TestAttachTaskRetryControlKeepsHeaderAndBodyCopyIdentical(t *testing.T) {
	c, recorder := newRelayTestContext(t, http.MethodPost, "/v1/video/generations", "{}")
	taskErr := &taskdto.TaskError{Code: "get_channel_failed", StatusCode: http.StatusInternalServerError}

	attachTaskRetryControl(c, taskErr)

	require.NotNil(t, taskErr.RetryControl)
	assert.Equal(t, string(taskErr.RetryControl.DispatchState), recorder.Header().Get(retrycontrol.HeaderDispatchState))
	assert.Equal(t, string(taskErr.RetryControl.Reason), recorder.Header().Get(retrycontrol.HeaderRetryReason))
	assert.Equal(t, "true", recorder.Header().Get(retrycontrol.HeaderRetryable))
	assert.Equal(t, "500", recorder.Header().Get(retrycontrol.HeaderRetryAfterMs))

	// 选不出渠道＝一个字节都没发出去，可以安全换模型重试。
	assert.Equal(t, retrycontrol.DispatchStateNotDispatched, taskErr.RetryControl.DispatchState)
	assert.Equal(t, retrycontrol.ReasonModelCapacityExhausted, taskErr.RetryControl.Reason)
	assert.True(t, taskErr.RetryControl.Retryable)
}

// 上游已经收到过请求之后再失败，绝不能报 not_dispatched。
func TestAttachTaskRetryControlNeverClaimsNotDispatchedAfterUpstreamResponded(t *testing.T) {
	c, recorder := newRelayTestContext(t, http.MethodPost, "/v1/video/generations", "{}")
	// 上游任务已经建好，随后本地落库失败——这是最危险的一格：
	// 报 not_dispatched 会让 daemon 再提交一次，主人被扣两次。
	retrycontrol.Tracker(c).MarkResponseReceived()

	taskErr := &taskdto.TaskError{Code: "task_insert_failed", StatusCode: http.StatusInternalServerError}
	attachTaskRetryControl(c, taskErr)

	require.NotNil(t, taskErr.RetryControl)
	assert.Equal(t, retrycontrol.DispatchStateDispatched, taskErr.RetryControl.DispatchState)
	assert.False(t, taskErr.RetryControl.Retryable)
	assert.Equal(t, "dispatched", recorder.Header().Get(retrycontrol.HeaderDispatchState))
	assert.Equal(t, "false", recorder.Header().Get(retrycontrol.HeaderRetryable))
}

// 异步任务链路的 reserve 阶段会产出 insufficient_user_quota，那已经在上游任务
// 创建之后。它不得凭错误码名字被判成发送前失败。
func TestTaskQuotaFailureIsNotTreatedAsPreDispatch(t *testing.T) {
	c, _ := newRelayTestContext(t, http.MethodPost, "/v1/video/generations", "{}")
	taskErr := &taskdto.TaskError{Code: "insufficient_user_quota", StatusCode: http.StatusForbidden}

	attachTaskRetryControl(c, taskErr)

	require.NotNil(t, taskErr.RetryControl)
	assert.Equal(t, retrycontrol.DispatchStateUnknown, taskErr.RetryControl.DispatchState)
	assert.False(t, taskErr.RetryControl.Retryable)
}

// 重复调用必须幂等：respondTaskSubmissionError 会先调一次，
// 它再转给 respondTaskError 又调一次。
func TestAttachTaskRetryControlIsIdempotent(t *testing.T) {
	c, _ := newRelayTestContext(t, http.MethodPost, "/v1/video/generations", "{}")
	taskErr := &taskdto.TaskError{Code: "get_channel_failed", StatusCode: http.StatusInternalServerError}

	attachTaskRetryControl(c, taskErr)
	first := *taskErr.RetryControl

	// 第二次调用时上游状态即使变了，也不得改写已经发给客户端的那份事实。
	retrycontrol.Tracker(c).MarkResponseReceived()
	attachTaskRetryControl(c, taskErr)

	assert.Equal(t, first, *taskErr.RetryControl)
}

// 请求摘要必须覆盖方法、路径、查询串与请求体：任何一样变了都是另一个请求，
// 同键重发时必须被判成冲突而不是回放。
func TestTaskRequestDigestCoversEveryRequestComponent(t *testing.T) {
	baseline := digestOf(t, http.MethodPost, "/v1/video/generations", `{"model":"sora","prompt":"a cat"}`)

	assert.Equal(t, baseline, digestOf(t, http.MethodPost, "/v1/video/generations", `{"model":"sora","prompt":"a cat"}`),
		"同一个请求两次摘要必须相同")

	for name, other := range map[string]string{
		"请求体不同": digestOf(t, http.MethodPost, "/v1/video/generations", `{"model":"sora","prompt":"a dog"}`),
		"路径不同":  digestOf(t, http.MethodPost, "/v1/videos/vid_1/remix", `{"model":"sora","prompt":"a cat"}`),
		"查询串不同": digestOf(t, http.MethodPost, "/v1/video/generations?variant=hd", `{"model":"sora","prompt":"a cat"}`),
		"方法不同":  digestOf(t, http.MethodPut, "/v1/video/generations", `{"model":"sora","prompt":"a cat"}`),
	} {
		assert.NotEqualf(t, baseline, other, "%s 时摘要必须不同", name)
	}
}

// 摘要读请求体不得把体读干净：后面的 relay 链路还要原样重放同一份体。
func TestTaskRequestDigestLeavesTheBodyReadable(t *testing.T) {
	const body = `{"model":"sora","prompt":"a cat"}`
	c, _ := newRelayTestContext(t, http.MethodPost, "/v1/video/generations", body)

	_, err := taskRequestDigest(c)
	require.NoError(t, err)

	var payload map[string]any
	require.NoError(t, common.UnmarshalBodyReusable(c, &payload))
	assert.Equal(t, "sora", payload["model"])
}

func digestOf(t *testing.T, method, target, body string) string {
	t.Helper()
	c, _ := newRelayTestContext(t, method, target, body)
	digest, err := taskRequestDigest(c)
	require.NoError(t, err)
	require.Len(t, digest, 64)
	return digest
}

// 走真实的 Relay 终局路径：四个头 + 非流式 error body 里的同值副本。
// 这条测试盯的是设计 §13.2 的完整交付形状，不是某个内部函数。
func TestRelayTerminalFailureCarriesHeadersAndBodyCopy(t *testing.T) {
	for _, tc := range []struct {
		name        string
		relayFormat types.RelayFormat
		errorField  string
	}{
		{name: "openai", relayFormat: types.RelayFormatOpenAI, errorField: "error"},
		{name: "claude", relayFormat: types.RelayFormatClaude, errorField: "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, recorder := newRelayTestContext(t, http.MethodPost, "/v1/chat/completions", "not json")

			Relay(c, tc.relayFormat)

			require.Equal(t, http.StatusBadRequest, recorder.Code)
			// 必须读 Result().Header：那是 WriteHeader 那一刻冲刷出去的快照，
			// 也就是客户端真正收到的头。recorder.Header() 返回的是活的 map，
			// 写 body 之后再补的头照样能读到——用它断言等于没有断言顺序。
			sentHeaders := recorder.Result().Header
			assert.Equal(t, "not_dispatched", sentHeaders.Get(retrycontrol.HeaderDispatchState))
			assert.Equal(t, "request_invalid", sentHeaders.Get(retrycontrol.HeaderRetryReason))
			assert.Equal(t, "false", sentHeaders.Get(retrycontrol.HeaderRetryable))
			assert.Equal(t, "0", sentHeaders.Get(retrycontrol.HeaderRetryAfterMs))

			var body map[string]any
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &body))
			require.Contains(t, body, tc.errorField)

			control, ok := body["retry_control"].(map[string]any)
			require.True(t, ok, "非流式错误 body 必须带 retry_control 副本")
			// 副本与头逐字同值，不得缩写。
			assert.Equal(t, sentHeaders.Get(retrycontrol.HeaderDispatchState), control["dispatch_state"])
			assert.Equal(t, sentHeaders.Get(retrycontrol.HeaderRetryReason), control["reason"])
			assert.Equal(t, false, control["retryable"])
			assert.EqualValues(t, 0, control["retry_after_ms"])
		})
	}
}
