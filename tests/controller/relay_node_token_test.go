package controllertests_test

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"net/http"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOrdinaryLeaseStrictHTTPContract(t *testing.T) {
	db, _, server := setupOrdinaryCallHTTP(t)
	valid := map[string]any{"newapi_user_id": 771001, "credential_generation": 1, "model_group": "default", "model_limits": []string{},
		"expires_time": time.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339), "purpose": "generic", "accounting_mode": "legacy"}
	payloads := []string{}
	for _, field := range []string{"newapi_user_id", "credential_generation", "model_group", "model_limits", "expires_time", "purpose", "accounting_mode"} {
		missing := maps.Clone(valid)
		delete(missing, field)
		payload, err := common.Marshal(missing)
		require.NoError(t, err)
		payloads = append(payloads, string(payload))
		missing[field] = nil
		payload, err = common.Marshal(missing)
		require.NoError(t, err)
		payloads = append(payloads, string(payload))
	}
	validPayload, err := common.Marshal(valid)
	require.NoError(t, err)
	for _, extra := range []string{`,"purpose":"generic"}`, `,"pur\u0070ose":"generic"}`, `,"operation":"web.search"}`, `,"unknown":true}`} {
		payloads = append(payloads, string(validPayload[:len(validPayload)-1])+extra)
	}
	for _, purpose := range []string{"", " generic "} {
		invalid := maps.Clone(valid)
		invalid["purpose"] = purpose
		payload, err := common.Marshal(invalid)
		require.NoError(t, err)
		payloads = append(payloads, string(payload))
	}
	payloads = append(payloads, string(validPayload)+`{}`, `[]`, `null`)
	for index, payload := range append(payloads, string(validPayload)) {
		id := fmt.Sprintf("vc7.strict.%d", index)
		request, err := http.NewRequest(http.MethodPut, server.URL+"/api/internal/v1/llm/relay-leases/"+id, bytes.NewBufferString(payload))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		response, err := server.Client().Do(request)
		require.NoError(t, err)
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		if index == len(payloads) {
			require.Equal(t, http.StatusOK, response.StatusCode, "所有拒绝项须有完整合法请求对照")
			continue
		}
		require.Equal(t, http.StatusBadRequest, response.StatusCode, "拒绝项%d", index)
		var failure dto.InternalErrorResponse
		require.NoError(t, common.Unmarshal(body, &failure))
		assert.Equal(t, "invalid_request", failure.Code)
		var count int64
		require.NoError(t, db.Model(&model.Token{}).Where("external_lease_id = ?", id).Count(&count).Error)
		assert.Zero(t, count, "格式拒绝不得写Token")
	}
}

func putNodeLease(t *testing.T, client *http.Client, serverURL, id string, limits []string) (int, dto.LlmRelayLeaseView) {
	t.Helper()
	payload, err := common.Marshal(map[string]any{"newapi_user_id": 771001, "credential_generation": 1, "model_group": "default", "model_limits": limits,
		"expires_time": time.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339), "purpose": "generic", "accounting_mode": "legacy"})
	require.NoError(t, err)
	request, err := http.NewRequest(http.MethodPut, serverURL+"/api/internal/v1/llm/relay-leases/"+id, bytes.NewReader(payload))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	var view dto.LlmRelayLeaseView
	if response.StatusCode == http.StatusOK {
		require.NoError(t, common.Unmarshal(body, &view))
	}
	return response.StatusCode, view
}

// 节点令牌按「空间 + 节点」一把：不按用途分路由，对话、搜索、抓取、语音与音色建删查都放行到消费者。
func TestNodeTokenReachesEveryOpenedEndpoint(t *testing.T) {
	_, engine, server := setupOrdinaryCallHTTP(t)
	visited := 0
	consumer := func(c *gin.Context) { visited++; c.Status(http.StatusNoContent) }
	routes := []struct{ method, path, body string }{
		{http.MethodPost, "/v1/chat/completions", `{"model":"gpt-4o","messages":[]}`},
		{http.MethodPost, "/v1/tools/search", `{"query":"唤星","count":3}`},
		{http.MethodPost, "/v1/tools/fetch", `{"url":"https://example.com/"}`},
		{http.MethodPost, "/v1/audio/speech", `{"model":"tts-1","input":"test","voice":"alloy"}`},
		{http.MethodPost, "/v1/audio/voices", `{"model":"tts-1","kind":"design"}`},
		{http.MethodGet, "/v1/audio/voices", ""},
		{http.MethodGet, "/v1/audio/voices/resource", ""},
		{http.MethodGet, "/v1/audio/voices/resource/preview", ""},
		{http.MethodDelete, "/v1/audio/voices/resource", ""},
	}
	for _, route := range routes {
		engine.Handle(route.method, route.path, middleware.TokenAuth(), consumer)
	}
	status, lease := putNodeLease(t, server.Client(), server.URL, "node.one-token", []string{})
	require.Equal(t, http.StatusOK, status)
	for _, route := range routes {
		request, err := http.NewRequest(route.method, server.URL+route.path, bytes.NewBufferString(route.body))
		require.NoError(t, err)
		request.Header.Set("Authorization", "Bearer "+lease.RelayToken)
		request.Header.Set("Content-Type", "application/json")
		response, err := server.Client().Do(request)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		assert.Equal(t, http.StatusNoContent, response.StatusCode, route.method+" "+route.path)
	}
	assert.Equal(t, len(routes), visited)
	for _, limits := range [][]string{{"tts-1"}, {"gpt-4o", "tts-1"}} {
		status, _ = putNodeLease(t, server.Client(), server.URL, "node.narrowed", limits)
		assert.Equal(t, http.StatusBadRequest, status, "generic 租约不得再按模型收窄")
	}
}

// 组外模型即使 pin 了支持它的渠道也不能派发：令牌唯一的收窄是模型组。
func TestPinnedChannelCannotBypassModelGroup(t *testing.T) {
	_, engine, server := setupOrdinaryCallHTTP(t)
	visited := false
	engine.POST("/v1/audio/speech", middleware.TokenAuth(), func(c *gin.Context) {
		service.GetChannelConstraints(c).AddPin(dto.ChannelPin{ChannelId: 771001, Source: dto.PinSourceOriginTask, Rank: dto.PinRankOriginTask, RetryMode: dto.PinRetrySingleAttempt})
	}, middleware.Distribute(), func(c *gin.Context) { visited = true; c.Status(http.StatusNoContent) })
	status, lease := putNodeLease(t, server.Client(), server.URL, "node.pinned", []string{})
	require.Equal(t, http.StatusOK, status)
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/audio/speech", bytes.NewBufferString(`{"model":"whisper-1","input":"test","voice":"alloy"}`))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+lease.RelayToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, http.StatusForbidden, response.StatusCode)
	assert.False(t, visited, "pin不得跳过模型组")
}
