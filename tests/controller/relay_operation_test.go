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
	"github.com/QuantumNous/new-api/controller"
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
	for _, extra := range []string{`,"purpose":"generic"}`, `,"pur\u0070ose":"generic"}`, `,"operation":null}`, `,"unknown":true}`} {
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

func putOperationLease(t *testing.T, client *http.Client, serverURL, id string, operation any, present bool, limits []string, generation int64) (int, dto.LlmRelayLeaseView) {
	t.Helper()
	requestBody := map[string]any{"newapi_user_id": 771001, "credential_generation": generation, "model_group": "default", "model_limits": limits,
		"expires_time": time.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339), "purpose": "generic", "accounting_mode": "legacy"}
	if present {
		requestBody["operation"] = operation
	}
	payload, err := common.Marshal(requestBody)
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

func TestOrdinaryOperationLeaseRejectsCrossPurposeBeforeDispatch(t *testing.T) {
	_, engine, server := setupOrdinaryCallHTTP(t)
	visited := 0
	consumer := func(c *gin.Context) { visited++; c.Status(http.StatusNoContent) }
	engine.POST("/v1/audio/speech", middleware.TokenAuth(), middleware.Distribute(), consumer)
	engine.POST("/v1/audio/voices", middleware.TokenAuth(), consumer)
	engine.GET("/v1/audio/voices", middleware.TokenAuth(), consumer)
	engine.GET("/v1/audio/voices/:voice_ref", middleware.TokenAuth(), consumer)
	engine.GET("/v1/audio/voices/:voice_ref/preview", middleware.TokenAuth(), consumer)
	engine.DELETE("/v1/audio/voices/:voice_ref", middleware.TokenAuth(), consumer)
	engine.GET("/v1/unregistered", middleware.TokenAuth(), consumer)
	status, tts := putOperationLease(t, server.Client(), server.URL, "vc7.operation.tts", "audio.tts", true, []string{"tts-1"}, 1)
	require.Equal(t, http.StatusOK, status)
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{http.MethodPost, "/v1/audio/speech", `{"model":"tts-1","input":"test","voice":"alloy"}`, http.StatusNoContent},
		{http.MethodPost, "/v1/audio/voices", `{"model":"tts-1","kind":"design","operation":"audio.voice_create"}`, http.StatusForbidden},
		{http.MethodGet, "/v1/audio/voices?idempotency_key=key", "", http.StatusForbidden},
		{http.MethodGet, "/v1/audio/voices/resource", "", http.StatusForbidden},
		{http.MethodGet, "/v1/audio/voices/resource/preview", "", http.StatusForbidden},
		{http.MethodDelete, "/v1/audio/voices/resource", "", http.StatusForbidden},
		{http.MethodGet, "/v1/unregistered", "", http.StatusForbidden},
	} {
		request, err := http.NewRequest(tc.method, server.URL+tc.path, bytes.NewBufferString(tc.body))
		require.NoError(t, err)
		request.Header.Set("Authorization", "Bearer "+tts.RelayToken)
		request.Header.Set("Content-Type", "application/json")
		response, err := server.Client().Do(request)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		assert.Equal(t, tc.status, response.StatusCode, tc.method+" "+tc.path)
	}
	assert.Equal(t, 1, visited, "错误用途不得到达资源消费者")
	status, _ = putOperationLease(t, server.Client(), server.URL, "vc7.operation.tts", "audio.voice_create", true, []string{"tts-1"}, 2)
	assert.Equal(t, http.StatusConflict, status, "换代也不能改变用途")
	status, _ = putOperationLease(t, server.Client(), server.URL, "vc7.operation.tts", nil, false, []string{}, 2)
	assert.Equal(t, http.StatusConflict, status, "用途不可清空为Runtime")
	for _, value := range []any{nil, "", "audio.unknown"} {
		status, _ = putOperationLease(t, server.Client(), server.URL, "vc7.operation.invalid", value, true, []string{"tts-1"}, 1)
		assert.Equal(t, http.StatusBadRequest, status)
	}
	status, _ = putOperationLease(t, server.Client(), server.URL, "vc7.operation.missing", nil, false, []string{"tts-1"}, 1)
	assert.Equal(t, http.StatusBadRequest, status)
	status, _ = putOperationLease(t, server.Client(), server.URL, "vc7.operation.no-model", "audio.tts", true, []string{}, 1)
	assert.Equal(t, http.StatusBadRequest, status)
	status, runtime := putOperationLease(t, server.Client(), server.URL, "vc7.operation.runtime", nil, false, []string{}, 1)
	require.Equal(t, http.StatusOK, status)
	status, _ = putOperationLease(t, server.Client(), server.URL, "vc7.operation.runtime", "audio.tts", true, []string{"tts-1"}, 2)
	assert.Equal(t, http.StatusConflict, status, "Runtime缺席用途也不能改成call")
	// 使用真实Runtime令牌进入原普通Token编辑控制器，不能通过省略operation清空内部租约。
	engine.PUT("/api/token/", middleware.TokenAuth(), controller.UpdateToken)
	payload, err := common.Marshal(map[string]any{"id": tts.NewApiTokenId, "name": "cleared", "model_limits_enabled": false})
	require.NoError(t, err)
	request, err := http.NewRequest(http.MethodPut, server.URL+"/api/token/", bytes.NewReader(payload))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+runtime.RelayToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	var edited struct {
		Success bool `json:"success"`
	}
	require.NoError(t, common.Unmarshal(body, &edited))
	assert.False(t, edited.Success)
	stored, err := model.GetTokenById(tts.NewApiTokenId)
	require.NoError(t, err)
	assert.Equal(t, "audio.tts", stored.Operation)
	assert.Equal(t, []string{"tts-1"}, stored.GetModelLimits())
}

func TestPinnedChannelCannotBypassOrdinaryCallModelLimit(t *testing.T) {
	_, engine, server := setupOrdinaryCallHTTP(t)
	visited := false
	engine.POST("/v1/audio/speech", middleware.TokenAuth(), func(c *gin.Context) {
		service.GetChannelConstraints(c).AddPin(dto.ChannelPin{ChannelId: 771001, Source: dto.PinSourceOriginTask, Rank: dto.PinRankOriginTask, RetryMode: dto.PinRetrySingleAttempt})
	}, middleware.Distribute(), func(c *gin.Context) { visited = true; c.Status(http.StatusNoContent) })
	status, lease := putOperationLease(t, server.Client(), server.URL, "vc7.operation.pinned", "audio.tts", true, []string{"tts-1"}, 1)
	require.Equal(t, http.StatusOK, status)
	stored, err := model.GetTokenById(lease.NewApiTokenId)
	require.NoError(t, err)
	assert.Equal(t, []string{"tts-1"}, stored.GetModelLimits())
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/audio/speech", bytes.NewBufferString(`{"model":"gpt-4o","input":"test","voice":"alloy"}`))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+lease.RelayToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, http.StatusForbidden, response.StatusCode)
	assert.False(t, visited, "pin不得跳过组与单模型交集")
}

// web.search 租约只到达 /v1/tools/search；抓取、对话等其它路由在消费者之前拒绝。
func TestWebSearchLeaseOnlyReachesToolsSearch(t *testing.T) {
	_, engine, server := setupOrdinaryCallHTTP(t)
	visited := 0
	consumer := func(c *gin.Context) { visited++; c.Status(http.StatusNoContent) }
	engine.POST("/v1/tools/search", middleware.TokenAuth(), consumer)
	engine.POST("/v1/tools/fetch", middleware.TokenAuth(), consumer)
	engine.POST("/v1/chat/completions", middleware.TokenAuth(), consumer)
	status, lease := putOperationLease(t, server.Client(), server.URL, "web.search.lease", "web.search", true, []string{"deepseek-search"}, 1)
	require.Equal(t, http.StatusOK, status, "web.search 必须是可换发的普通调用用途")
	for _, tc := range []struct {
		path, body string
		status     int
	}{
		{"/v1/tools/search", `{"query":"唤星","count":3}`, http.StatusNoContent},
		{"/v1/tools/fetch", `{"url":"https://example.com/"}`, http.StatusForbidden},
		{"/v1/chat/completions", `{"model":"deepseek-search","messages":[]}`, http.StatusForbidden},
	} {
		request, err := http.NewRequest(http.MethodPost, server.URL+tc.path, bytes.NewBufferString(tc.body))
		require.NoError(t, err)
		request.Header.Set("Authorization", "Bearer "+lease.RelayToken)
		request.Header.Set("Content-Type", "application/json")
		response, err := server.Client().Do(request)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		assert.Equal(t, tc.status, response.StatusCode, tc.path)
	}
	assert.Equal(t, 1, visited, "错误用途不得到达资源消费者")
}
