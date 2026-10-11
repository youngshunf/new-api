package controllertests_test

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/router"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVoiceHTTPBoundTTSSendsNativeVoiceOnlyOnce(t *testing.T) {
	db, _, _ := setupOrdinaryCallHTTP(t)
	engine := gin.New()
	router.SetRelayRouter(engine)
	server := httptest.NewServer(engine)
	t.Cleanup(server.Close)
	require.NoError(t, db.AutoMigrate(&model.UserSubscription{}, &model.SubscriptionPreConsumeRecord{}, &model.WalletPreConsumeRecord{}))
	service.InitHttpClient()
	var calls atomic.Int32
	var outbound atomic.Value
	wire := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		data, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		outbound.Store(data)
		assert.Equal(t, "/api/v1/services/audio/tts/SpeechSynthesizer", r.URL.Path)
		conn, _, err := w.(http.Hijacker).Hijack()
		if assert.NoError(t, err) {
			_ = conn.Close()
		}
	}))
	t.Cleanup(wire.Close)
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", 771001).Updates(map[string]any{"type": constant.ChannelTypeAli, "models": "cosyvoice-v3.5-plus", "key": "local-protocol-measurement", "base_url": wire.URL}).Error)
	require.NoError(t, db.Where("channel_id = ?", 771001).Delete(&model.Ability{}).Error)
	require.NoError(t, db.Create(&model.Ability{Group: "default", Model: "cosyvoice-v3.5-plus", ChannelId: 771001, Enabled: true}).Error)
	require.NoError(t, model.UpdateOption(model.HasnLlmModelGroupsOptionKey, `{"default":{"publication_revision":2,"models":["cosyvoice-v3.5-plus"]}}`))
	previousPrice := ratio_setting.ModelPrice2JSONString()
	t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(previousPrice)) })
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"cosyvoice-v3.5-plus":0}`))
	lease, leaseErr := model.UpsertRelayLease(model.RelayLeaseSpec{ExternalLeaseId: "vc7.voice-wire", NewApiUserId: 771001, CredentialGeneration: 1, ModelGroup: "default", ModelLimits: []string{"cosyvoice-v3.5-plus"}, ExpiredTime: time.Now().Add(15 * time.Minute).Unix(), Purpose: "generic", AccountingMode: "legacy", Operation: "audio.tts"})
	require.Nil(t, leaseErr)
	// 真实数据库状态夹具只用于消费边界，不冒充供应商ready取证。
	profile, _, err := model.BeginVoiceCreation(context.Background(), model.VoiceCreationSpec{UserId: 771001, CreatorTokenId: lease.Token.Id, ChannelId: 771001, IdempotencyKey: "tts-wire-state", RequestDigest: strings.Repeat("c", 64), Kind: "clone", Language: "zh", TargetModel: "cosyvoice-v3.5-plus"})
	require.NoError(t, err)
	_, err = model.RecordVoiceProvider(context.Background(), 771001, profile.VoiceRef, "transport-measurement-voice")
	require.NoError(t, err)
	_, err = model.RecordVoiceStatus(context.Background(), 771001, profile.VoiceRef, "transport-measurement-voice", "cosyvoice-v3.5-plus", "OK")
	require.NoError(t, err)
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/audio/speech", strings.NewReader(`{"model":"cosyvoice-v3.5-plus","input":"这段文本验证原渠道音色绑定。","voice":"`+profile.VoiceRef+`","response_format":"wav"}`))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+model.RelayTokenPlaintext(lease.Token))
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.GreaterOrEqual(t, response.StatusCode, 400, string(data))
	require.Equal(t, int32(1), calls.Load(), string(data))
	var body struct {
		Input struct {
			Voice string `json:"voice"`
		} `json:"input"`
	}
	require.NoError(t, common.Unmarshal(outbound.Load().([]byte), &body))
	assert.Equal(t, "transport-measurement-voice", body.Input.Voice)
	assert.NotEqual(t, profile.VoiceRef, body.Input.Voice, "私有voice_ref不能直接外发供应商")
}

func TestVoiceHTTPUnknownRecoveryNeverRecreates(t *testing.T) {
	db, _, _ := setupOrdinaryCallHTTP(t)
	engine := gin.New()
	router.SetRelayRouter(engine)
	server := httptest.NewServer(engine)
	t.Cleanup(server.Close)
	require.NoError(t, db.AutoMigrate(&model.UserSubscription{}, &model.SubscriptionPreConsumeRecord{}, &model.WalletPreConsumeRecord{}))
	previousMedia := service.VoiceMedia
	store, err := service.NewVoiceMediaStore(t.TempDir())
	require.NoError(t, err)
	service.VoiceMedia = store
	t.Cleanup(func() { service.VoiceMedia = previousMedia })
	service.InitHttpClient()
	var calls atomic.Int32
	// 真实HTTP收完整请求后断连，不产生供应商业务回执。
	wire := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/services/audio/tts/customization", r.URL.Path)
		_, err := io.Copy(io.Discard, r.Body)
		assert.NoError(t, err)
		conn, _, err := w.(http.Hijacker).Hijack()
		if assert.NoError(t, err) {
			_ = conn.Close()
		}
	}))
	t.Cleanup(wire.Close)
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", 771001).Updates(map[string]any{"type": constant.ChannelTypeAli, "models": "cosyvoice-v3.5-plus", "key": "local-protocol-measurement", "base_url": wire.URL}).Error)
	require.NoError(t, db.Where("channel_id = ?", 771001).Delete(&model.Ability{}).Error)
	require.NoError(t, db.Create(&model.Ability{Group: "default", Model: "cosyvoice-v3.5-plus", ChannelId: 771001, Enabled: true}).Error)
	require.NoError(t, model.UpdateOption(model.HasnLlmModelGroupsOptionKey, `{"default":{"publication_revision":2,"models":["cosyvoice-v3.5-plus","tts-1"]}}`))
	issue := func(id, operation, modelName string, userID int) string {
		issued, leaseErr := model.UpsertRelayLease(model.RelayLeaseSpec{ExternalLeaseId: id, NewApiUserId: userID, CredentialGeneration: 1, ModelGroup: "default",
			ModelLimits: []string{modelName}, ExpiredTime: time.Now().Add(15 * time.Minute).Unix(), Purpose: "generic", AccountingMode: "legacy", Operation: operation})
		require.Nil(t, leaseErr)
		return model.RelayTokenPlaintext(issued.Token)
	}
	createKey := issue("vc7.voice-create", "audio.voice_create", "cosyvoice-v3.5-plus", 771001)
	queryKey := issue("vc7.voice-query", "audio.voice_query", "cosyvoice-v3.5-plus", 771001)
	deleteKey := issue("vc7.voice-delete", "audio.voice_delete", "cosyvoice-v3.5-plus", 771001)
	ttsKey := issue("vc7.voice-tts", "audio.tts", "cosyvoice-v3.5-plus", 771001)
	t.Setenv("VOICE_PUBLIC_ADDRESS", "")
	for _, invalid := range []string{"missing-file", "duplicate-kind", "truncated", "missing-public-origin"} {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		for _, field := range []struct{ name, value string }{{"kind", "clone"}, {"model", "cosyvoice-v3.5-plus"}, {"language", "zh"}} {
			require.NoError(t, writer.WriteField(field.name, field.value))
		}
		if invalid == "duplicate-kind" {
			require.NoError(t, writer.WriteField("kind", "clone"))
		}
		if invalid != "missing-file" {
			part, err := writer.CreateFormFile("file", "reference.wav")
			require.NoError(t, err)
			wav, err := os.ReadFile("../fixtures/voice-preview.wav")
			require.NoError(t, err)
			_, err = part.Write(wav)
			require.NoError(t, err)
		}
		require.NoError(t, writer.Close())
		payload := body.Bytes()
		if invalid == "truncated" {
			payload = payload[:len(payload)-20]
		}
		request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/audio/voices", bytes.NewReader(payload))
		require.NoError(t, err)
		request.Header.Set("Content-Type", writer.FormDataContentType())
		request.Header.Set("Authorization", "Bearer "+createKey)
		request.Header.Set("Idempotency-Key", "invalid-clone-"+invalid)
		response, err := server.Client().Do(request)
		require.NoError(t, err)
		data, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		expected := http.StatusBadRequest
		if invalid == "missing-public-origin" {
			expected = http.StatusServiceUnavailable
		}
		assert.Equal(t, expected, response.StatusCode, invalid+": "+string(data))
	}
	var beforeCreation int64
	require.NoError(t, db.Model(&model.VoiceProfile{}).Count(&beforeCreation).Error)
	assert.Zero(t, beforeCreation, "非法克隆正文和缺失可信取件地址不能占用幂等键")
	assert.Zero(t, calls.Load(), "非法克隆不能向真实HTTP测量端点派发")
	invoke := func(method, path, bearer, key, body string) (int, []byte) {
		request, err := http.NewRequest(method, server.URL+path, bytes.NewBufferString(body))
		require.NoError(t, err)
		request.Header.Set("Authorization", "Bearer "+bearer)
		request.Header.Set("Content-Type", "application/json")
		if key != "" {
			request.Header.Set("Idempotency-Key", key)
		}
		response, err := server.Client().Do(request)
		require.NoError(t, err)
		data, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		return response.StatusCode, data
	}
	body := `{"kind":"design","model":"cosyvoice-v3.5-plus","language":"zh","description":"沉稳温柔的成年女性声音","preview_text":"这是一段长度符合要求的音色设计试听文本。"}`
	status, _ := invoke(http.MethodPost, "/v1/audio/voices", ttsKey, "wrong-operation", body)
	assert.Equal(t, http.StatusForbidden, status, "生产路由上的普通TTS租约不能建档")
	status, data := invoke(http.MethodPost, "/v1/audio/voices", createKey, "stable-create", body)
	require.Equal(t, http.StatusOK, status, string(data))
	var created dto.VoiceResource
	require.NoError(t, common.Unmarshal(data, &created))
	require.True(t, model.ValidVoiceRef(created.VoiceRef))
	assert.Equal(t, "unknown", created.State)
	require.NotNil(t, created.FailureReason)
	assert.Equal(t, "provider_response_unknown", *created.FailureReason)
	assert.False(t, created.PreviewAvailable)
	assert.Equal(t, int32(1), calls.Load())
	preview, err := os.ReadFile("../fixtures/voice-preview.wav")
	require.NoError(t, err)
	require.NoError(t, store.SavePreview(created.VoiceRef, bytes.NewReader(preview)))
	status, data = invoke(http.MethodGet, "/v1/audio/voices/"+created.VoiceRef+"/preview", queryKey, "", "")
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, preview, data, "试听HTTP直接返回原始WAV字节")
	require.NoError(t, store.RemovePreview(created.VoiceRef))
	material, err := store.SaveMaterial(bytes.NewReader(preview))
	require.NoError(t, err)
	response, err := server.Client().Get(server.URL + "/v1/audio/voice-materials/" + material)
	require.NoError(t, err)
	materialBytes, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, http.StatusOK, response.StatusCode)
	assert.Equal(t, preview, materialBytes)
	response, err = server.Client().Get(server.URL + "/v1/audio/voice-materials/" + material)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, http.StatusNotFound, response.StatusCode, "公开取件不能二次消费")
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", 771001).Update("status", 2).Error)
	status, data = invoke(http.MethodPost, "/v1/audio/voices", createKey, "stable-create", body)
	require.Equal(t, http.StatusOK, status, string(data))
	var replay dto.VoiceResource
	require.NoError(t, common.Unmarshal(data, &replay))
	assert.Equal(t, created, replay, "离线渠道也能恢复同一unknown，无重建")
	status, _ = invoke(http.MethodPost, "/v1/audio/voices", createKey, "stable-create", `{"kind":"design","model":"cosyvoice-v3.5-plus","language":"zh","description":"另一声音","preview_text":"这是一段长度符合要求的音色设计试听文本。"}`)
	assert.Equal(t, http.StatusConflict, status)
	status, data = invoke(http.MethodGet, "/v1/audio/voices?idempotency_key=stable-create", queryKey, "", "")
	require.Equal(t, http.StatusOK, status, string(data))
	require.NoError(t, common.Unmarshal(data, &replay))
	assert.Equal(t, created.VoiceRef, replay.VoiceRef)
	status, _ = invoke(http.MethodGet, "/v1/audio/voices/"+created.VoiceRef+"/preview", queryKey, "", "")
	assert.Equal(t, http.StatusNotFound, status)
	status, _ = invoke(http.MethodPost, "/v1/audio/speech", ttsKey, "", `{"model":"cosyvoice-v3.5-plus","input":"你好","voice":"`+created.VoiceRef+`"}`)
	assert.Equal(t, http.StatusBadRequest, status, "unknown不能合成或回落默认声音")
	status, data = invoke(http.MethodDelete, "/v1/audio/voices/"+created.VoiceRef, deleteKey, "", "")
	require.Equal(t, http.StatusOK, status, string(data))
	require.NoError(t, common.Unmarshal(data, &replay))
	assert.True(t, replay.DeletePending)
	assert.Equal(t, "unknown", replay.State, "无供应商ID不能编造delete ACK")
	assert.Equal(t, int32(1), calls.Load())
	outside := issue("vc7.voice-outside", "audio.voice_query", "tts-1", 771001)
	status, _ = invoke(http.MethodGet, "/v1/audio/voices/"+created.VoiceRef, outside, "", "")
	assert.Equal(t, http.StatusForbidden, status)
	require.NoError(t, db.Create(&model.User{Id: 771002, Username: "vc7-other", AffCode: "vc7-other", Group: "default", Quota: 1000, Status: common.UserStatusEnabled}).Error)
	other := issue("vc7.voice-other", "audio.voice_query", "cosyvoice-v3.5-plus", 771002)
	status, _ = invoke(http.MethodGet, "/v1/audio/voices/"+created.VoiceRef, other, "", "")
	assert.Equal(t, http.StatusNotFound, status, "稳定账号归属不能由新token替代")
	// 按次价显式配置必须被现BillingSession执行，不能以供应商免费静默忽略。
	previousPrice := ratio_setting.ModelPrice2JSONString()
	t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(previousPrice)) })
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"voice-enrollment":100}`))
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", 771001).Update("status", 1).Error)
	status, _ = invoke(http.MethodPost, "/v1/audio/voices", createKey, "paid-create", body)
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, int32(1), calls.Load(), "不足额度必须在真实派发前拒绝")
	var count int64
	require.NoError(t, db.Model(&model.VoiceProfile{}).Count(&count).Error)
	assert.Equal(t, int64(1), count)
	// 真实数据库终局夹具验证消费行为，不宣称已取得供应商拒绝回执。
	_, err = model.RecordVoiceFailure(context.Background(), 771001, created.VoiceRef, "provider_rejected", true)
	require.NoError(t, err)
	status, data = invoke(http.MethodPost, "/v1/audio/voices", createKey, "stable-create", body)
	require.Equal(t, http.StatusOK, status, string(data))
	require.NoError(t, common.Unmarshal(data, &replay))
	assert.Equal(t, "failed", replay.State)
	assert.Equal(t, created.VoiceRef, replay.VoiceRef)
	assert.Equal(t, int32(1), calls.Load(), "已终局失败的同键请求也不得再次创建")
}

func TestVoiceInventoryRequiresExplicitEndpointMetadata(t *testing.T) {
	db, engine, server := setupOrdinaryCallHTTP(t)
	require.NoError(t, db.AutoMigrate(&model.LlmInventoryState{}))
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", 771001).Update("type", constant.ChannelTypeAli).Error)
	require.NoError(t, db.Create(&model.Ability{Group: "default", Model: "cosyvoice-v3.5-plus", ChannelId: 771001, Enabled: true}).Error)
	engine.GET("/api/internal/v1/llm/model-inventory", controller.GetInternalModelInventory)
	defer model.InvalidatePricingCache()
	for _, explicit := range []bool{false, true} {
		if explicit {
			require.NoError(t, db.Create(&model.Model{ModelName: "cosyvoice-v3.5-plus", Endpoints: `{"hasn_audio_voice":{"path":"/v1/audio/voices","method":"POST"},"openai_audio_speech":{"path":"/v1/audio/speech","method":"POST"}}`}).Error)
		}
		model.InvalidatePricingCache()
		response, err := server.Client().Get(server.URL + "/api/internal/v1/llm/model-inventory")
		require.NoError(t, err)
		data, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		require.Equal(t, http.StatusOK, response.StatusCode, string(data))
		var inventory dto.LlmModelInventory
		require.NoError(t, common.Unmarshal(data, &inventory))
		found := false
		for _, entry := range inventory.Models {
			if entry.ModelName != "cosyvoice-v3.5-plus" {
				continue
			}
			found = true
			if explicit {
				assert.Contains(t, entry.SupportedEndpointTypes, "hasn_audio_voice")
				assert.Contains(t, entry.SupportedEndpointTypes, "openai_audio_speech")
			} else {
				assert.NotContains(t, entry.SupportedEndpointTypes, "hasn_audio_voice")
				assert.NotContains(t, entry.SupportedEndpointTypes, "openai_audio_speech")
			}
		}
		assert.True(t, found)
	}
}
