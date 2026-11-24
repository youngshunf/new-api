package controller

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/retrycontrol"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
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

func TestResponsesHTTPFailoverOnUpstreamAccountArrearage(t *testing.T) {
	require.NoError(t, i18n.Init())
	const providerID = "bailian-request-123"
	for _, tc := range []struct {
		name, errorBody string
		chat            bool
		withBackup      bool
		retries         int
		wantStatus      int
	}{
		{"结构化百炼错误切换渠道", `{"error":{"code":"Arrearage","message":"Account is past due"},"request_id":"` + providerID + `"}`, true, true, 1, http.StatusOK},
		{"代理只保留百炼文案时切换渠道", `{"error":{"code":"bad_response_status_code","message":"Access denied, please make sure your account is in good standing. For details, see: https://help.aliyun.com/zh/model-studio/error-code#overdue-payment"},"request_id":"` + providerID + `"}`, false, true, 1, http.StatusOK},
		{"无后备时返回服务不可用", `{"code":"Arrearage","message":"Account is past due","request_id":"` + providerID + `"}`, false, false, 1, http.StatusServiceUnavailable},
		{"零重试预算时返回服务不可用", `{"code":"Arrearage","message":"Account is past due","request_id":"` + providerID + `"}`, true, true, 0, http.StatusServiceUnavailable},
		{"普通参数错误维持原错误", `{"error":{"code":"InvalidParameter","message":"unsupported input"}}`, false, true, 1, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(*websocket.Conn, *http.Request) {})
			oldRetries, oldErrorLog := common.RetryTimes, constant.ErrorLogEnabled
			common.RetryTimes, constant.ErrorLogEnabled = tc.retries, true
			t.Cleanup(func() { common.RetryTimes, constant.ErrorLogEnabled = oldRetries, oldErrorLog })
			var primaryCalls, backupCalls atomic.Int32
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				primaryCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, tc.errorBody)
			}))
			t.Cleanup(primary.Close)
			require.NoError(t, model.DB.Model(fixture.channel).Update("base_url", primary.URL).Error)
			require.NoError(t, model.DB.Model(&model.Ability{}).Where("channel_id = ?", fixture.channel.Id).Update("priority", 20).Error)
			if tc.withBackup {
				backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					backupCalls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					if tc.chat {
						_, _ = io.WriteString(w, `{"id":"backup-completed","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
						return
					}
					_, _ = io.WriteString(w, `{"id":"backup-completed","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
				}))
				t.Cleanup(backup.Close)
				channel := &model.Channel{Name: "responses-backup", Key: "backup-key", Status: common.ChannelStatusEnabled, Type: constant.ChannelTypeOpenAI, Group: "default", Models: "ws-billing", BaseURL: &backup.URL}
				require.NoError(t, model.DB.Create(channel).Error)
				require.NoError(t, model.DB.Create(&model.Ability{ChannelId: channel.Id, Model: "ws-billing", Group: "default", Enabled: true}).Error)
				t.Cleanup(func() {
					require.NoError(t, model.DB.Where("channel_id = ?", channel.Id).Delete(&model.Ability{}).Error)
					require.NoError(t, model.DB.Delete(channel).Error)
				})
			}
			path, requestBody := "/v1/responses", `{"model":"ws-billing","input":"hello"}`
			if tc.chat {
				path, requestBody = "/v1/chat/completions", `{"model":"ws-billing","messages":[{"role":"user","content":"hello"}]}`
			}
			request, err := http.NewRequest(http.MethodPost, fixture.gatewayURL+path, strings.NewReader(requestBody))
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer sk-"+fixture.token.Key)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept-Language", "zh-CN")
			response, err := http.DefaultClient.Do(request)
			require.NoError(t, err)
			body, err := io.ReadAll(response.Body)
			require.NoError(t, response.Body.Close())
			require.NoError(t, err)
			<-fixture.httpDone
			require.Equal(t, tc.wantStatus, response.StatusCode, string(body))
			assert.EqualValues(t, 1, primaryCalls.Load())
			switch tc.wantStatus {
			case http.StatusOK:
				assert.EqualValues(t, 1, backupCalls.Load())
				assert.Contains(t, string(body), "backup-completed")
			case http.StatusServiceUnavailable:
				assert.Zero(t, backupCalls.Load())
				assert.Contains(t, string(body), "模型服务暂不可用")
				assert.Contains(t, string(body), "responses-http-billing")
				assert.NotContains(t, string(body), providerID)
				assert.NotContains(t, string(body), "Account is past due")
			case http.StatusBadRequest:
				assert.Zero(t, backupCalls.Load())
				assert.Contains(t, string(body), "unsupported input")
			}
			if tc.wantStatus != http.StatusBadRequest {
				var entry model.Log
				require.NoError(t, model.LOG_DB.Where("channel_id = ? AND type = ?", fixture.channel.Id, model.LogTypeError).First(&entry).Error)
				assert.Contains(t, entry.Other, providerID)
				assert.NotContains(t, entry.Content, providerID)
				logs, err := model.GetLogByTokenId(fixture.token.Id)
				require.NoError(t, err)
				require.NotEmpty(t, logs)
				for _, entry := range logs {
					assert.NotContains(t, entry.Content, providerID)
					assert.NotContains(t, entry.Other, providerID)
				}
			}
		})
	}
}
