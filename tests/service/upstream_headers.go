package servicetests

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/retrycontrol"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// VerifyUpstreamHeaderOwnership 测试所有复制消费者共享的真实头过滤 Interface。
func VerifyUpstreamHeaderOwnership(t *testing.T, shouldCopy func(*gin.Context, string, []string) bool) {
	t.Helper()
	require.NotNil(t, shouldCopy)
	for _, name := range []string{
		retrycontrol.HeaderDispatchState,
		retrycontrol.HeaderRetryReason,
		retrycontrol.HeaderRetryable,
		retrycontrol.HeaderRetryAfterMs,
	} {
		for _, variant := range []string{name, strings.ToLower(name), strings.ToUpper(name)} {
			t.Run(variant, func(t *testing.T) {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				assert.False(t, shouldCopy(c, variant, []string{"provider-value", "duplicate-value"}), "供应商不得覆盖或叠加网关派发事实")
				assert.False(t, shouldCopy(nil, variant, nil), "过滤不以本地上下文或头值是否存在放行")
			})
		}
	}

	for _, tc := range []struct {
		name       string
		values     []string
		wantCopy   bool
		upstreamID string
	}{
		{name: "Content-Length", values: []string{"999"}, wantCopy: false},
		{name: "content-length", values: []string{"999"}, wantCopy: false},
		{name: common.RequestIdKey, values: []string{"provider-oneapi-id", "duplicate-id"}, wantCopy: false, upstreamID: "provider-oneapi-id"},
		{name: strings.ToLower(common.RequestIdKey), values: []string{"provider-oneapi-id"}, wantCopy: false, upstreamID: "provider-oneapi-id"},
		{name: strings.ToUpper(common.RequestIdKey), wantCopy: false},
		{name: "X-Request-Id", values: []string{"provider-request-id"}, wantCopy: true},
		{name: "Content-Type", values: []string{"audio/mpeg"}, wantCopy: true},
		{name: "Retry-After", values: []string{"2"}, wantCopy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			assert.Equal(t, tc.wantCopy, shouldCopy(c, tc.name, tc.values))
			assert.Equal(t, tc.upstreamID, c.GetString(common.UpstreamRequestIdKey))
		})
	}
}

// VerifyCopiedResponseKeepsGatewayFacts 从真实字节复制出口断言客户端收到的头，而非复制前的活表。
func VerifyCopiedResponseKeepsGatewayFacts(t *testing.T, copyBytes func(*gin.Context, *http.Response, []byte)) {
	t.Helper()
	require.NotNil(t, copyBytes)
	for _, publishLocalControl := range []bool{false, true} {
		name := "without_local_control"
		if publishLocalControl {
			name = "with_local_control"
		}
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", nil)
			const localID = "gateway-request-12"
			c.Set(common.RequestIdKey, localID)
			c.Header(common.RequestIdKey, localID)
			if publishLocalControl {
				retrycontrol.Tracker(c).MarkResponseReceived()
				retrycontrol.WriteHeaders(c, retrycontrol.New(retrycontrol.ReasonStreamInterrupted, retrycontrol.ResolveDispatchState(c, false)))
			}

			// 只提供确定的头与字节，不启动供应商替身或HTTP服务。
			upstream := &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					strings.ToLower(common.RequestIdKey): {"provider-oneapi-id"},
					"X-Request-Id":                       {"provider-request-id"},
					"Content-Type":                       {"audio/mpeg"},
					"Content-Length":                     {"999"},
					strings.ToLower(retrycontrol.HeaderDispatchState): {"not_dispatched", "unknown"},
					strings.ToUpper(retrycontrol.HeaderRetryReason):   {"model_capacity_exhausted"},
					retrycontrol.HeaderRetryable:                      {"true"},
					strings.ToUpper(retrycontrol.HeaderRetryAfterMs):  {"500"},
				},
			}
			copyBytes(c, upstream, []byte("audio"))

			response := recorder.Result()
			t.Cleanup(func() { require.NoError(t, response.Body.Close()) })
			assert.Equal(t, http.StatusOK, response.StatusCode)
			assert.Equal(t, "audio", recorder.Body.String())
			assert.Equal(t, "audio/mpeg", response.Header.Get("Content-Type"))
			assert.Equal(t, "5", response.Header.Get("Content-Length"))
			assert.Equal(t, []string{localID}, response.Header.Values(common.RequestIdKey))
			assert.Equal(t, localID, c.GetString(common.RequestIdKey))
			assert.Equal(t, "provider-oneapi-id", c.GetString(common.UpstreamRequestIdKey))
			assert.Equal(t, "provider-request-id", response.Header.Get("X-Request-Id"))
			for _, header := range []struct {
				name  string
				value string
			}{
				{name: retrycontrol.HeaderDispatchState, value: "dispatched"},
				{name: retrycontrol.HeaderRetryReason, value: "stream_interrupted"},
				{name: retrycontrol.HeaderRetryable, value: "false"},
				{name: retrycontrol.HeaderRetryAfterMs, value: "0"},
			} {
				if publishLocalControl {
					assert.Equal(t, []string{header.value}, response.Header.Values(header.name))
				} else {
					assert.Empty(t, response.Header.Values(header.name), "未产生本地事实时也不得借供应商头自造事实")
				}
			}
		})
	}
}
