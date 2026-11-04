package relay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildAlphaSearchRequestBodyPreservesUnknownFields(t *testing.T) {
	raw := []byte(`{
		"id":"req_1",
		"model":"gpt-5.1",
		"input":[{"role":"user","content":"hi"}],
		"commands":{"search_query":[{"q":"weather","recency":1}]},
		"settings":{"locale":"en"},
		"future_field":{"nested":true}
	}`)

	out, err := buildAlphaSearchRequestBody(raw, "gpt-5.1", "gpt-5.1-mapped")
	require.NoError(t, err)

	var body map[string]any
	require.NoError(t, common.Unmarshal(out, &body))
	assert.Equal(t, "gpt-5.1-mapped", body["model"])
	assert.Equal(t, "req_1", body["id"])
	require.Contains(t, body, "commands")
	require.Contains(t, body, "settings")
	require.Contains(t, body, "future_field")
	require.Contains(t, body, "input")

	commands, ok := body["commands"].(map[string]any)
	require.True(t, ok)
	require.Contains(t, commands, "search_query")

	future, ok := body["future_field"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, future["nested"])
}

func TestBuildAlphaSearchRequestBodyNoMappingKeepsRawBytes(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.1","commands":{"search_query":[{"q":"x"}]},"future_field":1}`)
	out, err := buildAlphaSearchRequestBody(raw, "gpt-5.1", "gpt-5.1")
	require.NoError(t, err)
	assert.Equal(t, raw, out)
}

// A single model is rewritten where it stands. Routing reads the first
// "model" and most upstreams read the last, so a repeated model comes out as
// one member carrying the mapped value.
func TestBuildAlphaSearchRequestBodyRewritesModel(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"single model keeps its place", `{"model":"gpt-5.1","input":[]}`, `{"model":"gpt-5.1-mapped","input":[]}`},
		{"repeated model collapses", `{"model":"gpt-5.1","input":[],"model":"gpt-5.5-pro"}`, `{"input":[],"model":"gpt-5.1-mapped"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := buildAlphaSearchRequestBody([]byte(tt.raw), "gpt-5.1", "gpt-5.1-mapped")
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(out))
		})
	}
}

type recordedSettlement struct {
	settled  []int
	refunded bool
}

func (s *recordedSettlement) Settle(quota int) error {
	s.settled = append(s.settled, quota)
	return nil
}
func (s *recordedSettlement) Refund(*gin.Context)    { s.refunded = true }
func (*recordedSettlement) NeedsRefund() bool        { return false }
func (*recordedSettlement) GetPreConsumedQuota() int { return 0 }
func (*recordedSettlement) Reserve(int) error        { return nil }

func (*recordedSettlement) ReserveBeforeSubmit(int) error { return nil }

// /v1/tools/search and /v1/tools/fetch follow /v1/alpha/search: the raw body
// reaches /v1/tools/* unchanged and the response returns unchanged. With only
// the built-in prices, a response DeepSeek bills (results, page content)
// settles one call at the routing name's request price and no tokens, an
// empty one releases the reservation, an upstream error keeps its status, and
// a channel that cannot relay the endpoint fails retryably without sending
// anything.
func TestDeepSeekToolsHelper(t *testing.T) {
	service.InitHttpClient()
	gin.SetMode(gin.TestMode)
	batch, logConsume := common.BatchUpdateEnabled, common.LogConsumeEnabled
	common.BatchUpdateEnabled, common.LogConsumeEnabled = true, false
	t.Cleanup(func() { common.BatchUpdateEnabled, common.LogConsumeEnabled = batch, logConsume })

	const search, fetch = "/v1/tools/search", "/v1/tools/fetch"
	const results = `{"results":[{"title":"DeepSeek","url":"https://deepseek.com/","rerank_score":2.1}]}`
	tests := []struct {
		name, path, body, response string
		channelType, status        int
		settled                    []int
		refunded                   bool
	}{
		{name: "search with results", path: search, body: `{"query":"deepseek","count":2,"future_field":{"nested":true}}`,
			response: results, channelType: constant.ChannelTypeDeepSeek, status: http.StatusOK, settled: []int{375}},
		{name: "search without results", path: search, body: `{"query":"site:none.invalid deepseek"}`,
			response: `{"results":[]}`, channelType: constant.ChannelTypeDeepSeek, status: http.StatusOK, refunded: true},
		{name: "fetch with page content", path: fetch, body: `{"url":"https://example.com/"}`,
			response: `{"title":"Example Domain","content":"# Example Domain"}`, channelType: constant.ChannelTypeDeepSeek, status: http.StatusOK, settled: []int{375}},
		{name: "fetch failure keeps its status", path: fetch, body: `{"url":"https://example.com/missing"}`,
			response:    `{"error":{"message":"failed to fetch URL","type":"not_found_error","param":null,"code":"invalid_request_error"}}`,
			channelType: constant.ChannelTypeDeepSeek, status: http.StatusNotFound},
		{name: "search through a New API channel", path: search, body: `{"query":"deepseek"}`,
			response: results, channelType: constant.ChannelTypeNewAPI, status: http.StatusOK, settled: []int{375}},
		{name: "channel without the endpoint", path: search, body: `{"query":"deepseek"}`, channelType: constant.ChannelTypeOpenAI},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			var gotBody []byte
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.response)
			}))
			t.Cleanup(upstream.Close)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")
			routingName := map[string]string{search: constant.DeepSeekSearchModel, fetch: constant.DeepSeekFetchModel}[tc.path]
			common.SetContextKey(c, constant.ContextKeyOriginalModel, routingName)
			common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
			common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
			common.SetContextKey(c, constant.ContextKeyChannelType, tc.channelType)
			common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, upstream.URL)
			common.SetContextKey(c, constant.ContextKeyChannelKey, "sk-test")

			request, err := helper.GetAndValidateRequest(c, types.RelayFormatDeepSeekTools)
			require.NoError(t, err)
			info, err := relaycommon.GenRelayInfo(c, types.RelayFormatDeepSeekTools, request, nil)
			require.NoError(t, err)
			info.BillingRequestInput = &billingexpr.RequestInput{Body: []byte(tc.body)}
			_, err = helper.ModelPriceHelper(c, info, 0, request.GetTokenCountMeta())
			require.NoError(t, err, "the routing name is priced out of the box")
			settlement := &recordedSettlement{}
			info.Billing = settlement

			apiErr := DeepSeekToolsHelper(c, info)
			if tc.status == 0 {
				require.NotNil(t, apiErr)
				assert.False(t, types.IsSkipRetryError(apiErr))
				assert.Empty(t, gotPath)
				return
			}
			assert.Equal(t, tc.path, gotPath)
			assert.Equal(t, tc.body, string(gotBody))
			assert.Equal(t, tc.settled, settlement.settled)
			assert.Equal(t, tc.refunded, settlement.refunded)
			if tc.status != http.StatusOK {
				require.NotNil(t, apiErr)
				assert.Equal(t, tc.status, apiErr.StatusCode)
				return
			}
			require.Nil(t, apiErr)
			assert.Equal(t, tc.status, recorder.Code)
			assert.Equal(t, tc.response, recorder.Body.String())
		})
	}
}

// /v1/tools/search 走统一处理器：提供方按渠道映射后的上游模型名选取，答复归一为统一格式；
// 有结果按路由名单价结算一次，零结果退回预扣；上游错误归一为统一状态，400 不换渠道，
// 其余（含提供方未登记）可换渠道，且不发出任何上游请求就失败的情形不碰上游。
func TestWebSearchHelper(t *testing.T) {
	service.InitHttpClient()
	gin.SetMode(gin.TestMode)
	batch, logConsume := common.BatchUpdateEnabled, common.LogConsumeEnabled
	common.BatchUpdateEnabled, common.LogConsumeEnabled = true, false
	t.Cleanup(func() { common.BatchUpdateEnabled, common.LogConsumeEnabled = batch, logConsume })

	const toWSA = `{"deepseek-search":"tencent-wsa-search"}`
	const wsaPages = `{"Response":{"RequestId":"r-1","Pages":["{\"title\":\"南极冰下\",\"url\":\"https://www.81.cn/yw_208727/16492942.html\",\"date\":\"2026/10/14 06:56:39\",\"passage\":\"冰下湖泊\",\"site\":\"中国军网\"}"],"Version":"lite"}}`
	tests := []struct {
		name, body, mapping string
		upstreamStatus      int
		upstreamBody        string
		wantPath, wantSent  string
		wantStatus          int
		wantResponse        string
		skipRetry           bool
		settled             []int
		refunded            bool
	}{
		{name: "deepseek results settle once", body: `{"model":"deepseek-search","query":"deepseek","count":2}`,
			upstreamStatus: http.StatusOK, upstreamBody: `{"results":[{"title":"DeepSeek","url":"https://deepseek.com/","snippet":"s","rerank_score":2.1}]}`,
			wantPath: "/v1/tools/search", wantSent: `{"query":"deepseek","count":2}`, wantStatus: http.StatusOK,
			wantResponse: `{"results":[{"title":"DeepSeek","url":"https://deepseek.com/","snippet":"s","summary":"","site_name":""}]}`, settled: []int{375}},
		{name: "deepseek upstream outage is retryable 502", body: `{"query":"deepseek"}`,
			upstreamStatus: http.StatusServiceUnavailable, upstreamBody: `{"error":{"message":"busy"}}`,
			wantPath: "/v1/tools/search", wantSent: `{"query":"deepseek","count":8}`, wantStatus: http.StatusBadGateway},
		{name: "tencent wsa results settle once", body: `{"query":"南极 site:81.cn"}`, mapping: toWSA,
			upstreamStatus: http.StatusOK, upstreamBody: wsaPages,
			wantPath: "/SearchPro", wantSent: `{"Query":"南极","Site":"81.cn"}`, wantStatus: http.StatusOK,
			wantResponse: `{"results":[{"title":"南极冰下","url":"https://www.81.cn/yw_208727/16492942.html","snippet":"冰下湖泊","summary":"冰下湖泊","site_name":"中国军网","date":"2026-10-14T06:56:39+08:00"}]}`,
			settled:      []int{375}},
		{name: "tencent wsa zero results refund", body: `{"query":"南极"}`, mapping: toWSA,
			upstreamStatus: http.StatusOK, upstreamBody: `{"Response":{"RequestId":"r-2","Pages":[]}}`,
			wantPath: "/SearchPro", wantSent: `{"Query":"南极"}`, wantStatus: http.StatusOK, wantResponse: `{"results":[]}`, refunded: true},
		{name: "tencent wsa unauthorized answers 200 but fails 401", body: `{"query":"南极"}`, mapping: toWSA,
			upstreamStatus: http.StatusOK, upstreamBody: `{"Response":{"Error":{"Code":"UnauthorizedOperation","Message":"未授权操作。"},"RequestId":"a546"}}`,
			wantPath: "/SearchPro", wantSent: `{"Query":"南极"}`, wantStatus: http.StatusUnauthorized},
		{name: "tencent wsa rejected query does not retry", body: `{"query":"南极"}`, mapping: toWSA,
			upstreamStatus: http.StatusOK, upstreamBody: `{"Response":{"Error":{"Code":"InvalidParameter","Message":"bad"}}}`,
			wantPath: "/SearchPro", wantSent: `{"Query":"南极"}`, wantStatus: http.StatusBadRequest, skipRetry: true},
		{name: "operators only never reach tencent wsa", body: `{"query":"site:81.cn"}`, mapping: toWSA,
			wantStatus: http.StatusBadRequest, skipRetry: true},
		{name: "count out of range never reaches upstream", body: `{"query":"deepseek","count":21}`,
			wantStatus: http.StatusBadRequest, skipRetry: true},
		{name: "unknown provider fails retryably", body: `{"query":"deepseek"}`, mapping: `{"deepseek-search":"gpt-4o"}`,
			wantStatus: http.StatusInternalServerError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotAuth string
			var gotBody []byte
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
				gotBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.upstreamStatus)
				_, _ = io.WriteString(w, tc.upstreamBody)
			}))
			t.Cleanup(upstream.Close)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/tools/search", strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")
			common.SetContextKey(c, constant.ContextKeyOriginalModel, constant.DeepSeekSearchModel)
			common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
			common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
			common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeDeepSeek)
			common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, upstream.URL)
			common.SetContextKey(c, constant.ContextKeyChannelKey, "sk-test")
			if tc.mapping != "" {
				c.Set("model_mapping", tc.mapping)
			}

			request, err := helper.GetAndValidateRequest(c, types.RelayFormatDeepSeekTools)
			require.NoError(t, err)
			info, err := relaycommon.GenRelayInfo(c, types.RelayFormatDeepSeekTools, request, nil)
			require.NoError(t, err)
			info.BillingRequestInput = &billingexpr.RequestInput{Body: []byte(tc.body)}
			_, err = helper.ModelPriceHelper(c, info, 0, request.GetTokenCountMeta())
			require.NoError(t, err)
			settlement := &recordedSettlement{}
			info.Billing = settlement

			apiErr := WebSearchHelper(c, info)
			assert.Equal(t, tc.wantPath, gotPath)
			if tc.wantSent != "" {
				assert.JSONEq(t, tc.wantSent, string(gotBody))
				assert.Equal(t, "Bearer sk-test", gotAuth)
			}
			assert.Equal(t, tc.settled, settlement.settled)
			assert.Equal(t, tc.refunded, settlement.refunded)
			if tc.wantStatus != http.StatusOK {
				require.NotNil(t, apiErr)
				assert.Equal(t, tc.wantStatus, apiErr.StatusCode)
				assert.Equal(t, tc.skipRetry, types.IsSkipRetryError(apiErr))
				assert.False(t, types.IsChannelError(apiErr), "ordinary search failures must not auto-disable the channel")
				return
			}
			require.Nil(t, apiErr)
			assert.Equal(t, http.StatusOK, recorder.Code)
			assert.JSONEq(t, tc.wantResponse, recorder.Body.String())
		})
	}
}
