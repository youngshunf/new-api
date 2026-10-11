package controllertests_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/router"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestManagedRuntimeCannotAccessVoiceResources(t *testing.T) {
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
	var createCalls, ttsCalls atomic.Int32
	// 真实TCP端点仅计量完整收件后断连，不生成任何供应商业务回执。
	wire := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/services/audio/tts/customization":
			createCalls.Add(1)
		case "/api/v1/services/audio/tts/SpeechSynthesizer":
			ttsCalls.Add(1)
		default:
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
		}
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
	require.NoError(t, model.UpdateOption(model.HasnLlmModelGroupsOptionKey, `{"default":{"publication_revision":2,"models":["cosyvoice-v3.5-plus"]}}`))
	previousPrice := ratio_setting.ModelPrice2JSONString()
	t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(previousPrice)) })
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"cosyvoice-v3.5-plus":0.001}`))
	lease, leaseErr := model.UpsertRelayLease(model.RelayLeaseSpec{ExternalLeaseId: "vc7.runtime-voice-boundary", NewApiUserId: 771001, CredentialGeneration: 1, ModelGroup: "default", ModelLimits: []string{}, ExpiredTime: time.Now().Add(15 * time.Minute).Unix(), Purpose: "generic", AccountingMode: "legacy"})
	require.Nil(t, leaseErr)
	runtimeKey := model.RelayTokenPlaintext(lease.Token)
	ordinary := model.Token{UserId: 771001, Key: strings.Repeat("a", 48), Status: common.TokenStatusEnabled, UnlimitedQuota: true, ExpiredTime: -1, Group: "default"}
	require.NoError(t, ordinary.Insert())
	ordinaryKey := model.RelayTokenPlaintext(&ordinary)
	// unknown数据库夹具用于证明拒绝发生在读档案和媒体之前，不冒充供应商验证。
	profile, _, err := model.BeginVoiceCreation(context.Background(), model.VoiceCreationSpec{UserId: 771001, CreatorTokenId: ordinary.Id, ChannelId: 771001, IdempotencyKey: "existing-voice", RequestDigest: strings.Repeat("d", 64), Kind: "clone", Language: "zh", TargetModel: "cosyvoice-v3.5-plus"})
	require.NoError(t, err)
	preview, err := os.ReadFile("../fixtures/voice-preview.wav")
	require.NoError(t, err)
	require.NoError(t, store.SavePreview(profile.VoiceRef, bytes.NewReader(preview)))
	var voiceReads atomic.Int32
	require.NoError(t, model.DB.Callback().Query().Before("gorm:query").Register("vc7:count_voice_reads", func(tx *gorm.DB) {
		if tx.Statement.Table == model.TableNameVoiceProfile {
			voiceReads.Add(1)
		}
	}))
	t.Cleanup(func() { require.NoError(t, model.DB.Callback().Query().Remove("vc7:count_voice_reads")) })
	invoke := func(method, path, bearer, key, body string) (int, []byte) {
		request, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
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
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/audio/voices", body},
		{http.MethodGet, "/v1/audio/voices?idempotency_key=existing-voice", ""},
		{http.MethodGet, "/v1/audio/voices/" + profile.VoiceRef, ""},
		{http.MethodGet, "/v1/audio/voices/" + profile.VoiceRef + "/preview", ""},
		{http.MethodDelete, "/v1/audio/voices/" + profile.VoiceRef, ""},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			status, data := invoke(tc.method, tc.path, runtimeKey, "blocked-runtime-create", tc.body)
			assert.Equal(t, http.StatusForbidden, status, string(data))
			assert.Contains(t, string(data), "relay lease operation does not allow this endpoint")
			assert.Zero(t, voiceReads.Load(), "必须在读取音色档案之前拒绝")
			assert.Zero(t, createCalls.Load(), "不得调用供应商")
			var profiles []model.VoiceProfile
			require.NoError(t, db.Find(&profiles).Error)
			assert.Equal(t, []model.VoiceProfile{*profile}, profiles, "不得占位、删档或改revision")
			assert.True(t, store.HasPreview(profile.VoiceRef), "不得回收现有试听")
		})
	}
	status, data := invoke(http.MethodPost, "/v1/audio/voices", runtimeKey, "invalid-runtime-body", "{")
	assert.Equal(t, http.StatusForbidden, status, string(data))
	assert.Zero(t, voiceReads.Load())
	status, data = invoke(http.MethodGet, "/v1/models", runtimeKey, "", "")
	require.Equal(t, http.StatusOK, status, string(data))
	assert.Contains(t, string(data), "cosyvoice-v3.5-plus")
	status, data = invoke(http.MethodPost, "/v1/audio/speech", runtimeKey, "", `{"model":"cosyvoice-v3.5-plus","input":"验证普通Runtime推理仍能派发。","voice":"protocol-measurement","response_format":"wav"}`)
	assert.GreaterOrEqual(t, status, 400, string(data))
	assert.NotEqual(t, http.StatusForbidden, status, string(data))
	assert.Equal(t, int32(1), ttsCalls.Load(), "普通Runtime推理必须到达真实TCP端点")

	// 传统API Key仍能从公开入口创建、恢复、试听和请求删除。
	status, data = invoke(http.MethodPost, "/v1/audio/voices", ordinaryKey, "ordinary-public-create", body)
	require.Equal(t, http.StatusOK, status, string(data))
	var created dto.VoiceResource
	require.NoError(t, common.Unmarshal(data, &created))
	assert.Equal(t, model.VoiceStateUnknown, created.State)
	assert.Equal(t, int32(1), createCalls.Load())
	require.NoError(t, store.SavePreview(created.VoiceRef, bytes.NewReader(preview)))
	for _, path := range []string{"/v1/audio/voices?idempotency_key=ordinary-public-create", "/v1/audio/voices/" + created.VoiceRef, "/v1/audio/voices/" + created.VoiceRef + "/preview"} {
		status, data = invoke(http.MethodGet, path, ordinaryKey, "", "")
		require.Equal(t, http.StatusOK, status, string(data))
		if strings.HasSuffix(path, "/preview") {
			assert.Equal(t, preview, data)
		}
	}
	status, data = invoke(http.MethodDelete, "/v1/audio/voices/"+created.VoiceRef, ordinaryKey, "", "")
	require.Equal(t, http.StatusOK, status, string(data))
	require.NoError(t, common.Unmarshal(data, &created))
	assert.True(t, created.DeletePending)
	assert.Equal(t, model.VoiceStateUnknown, created.State, "无供应商ID时不得伪造delete ACK")
	assert.Equal(t, int32(1), createCalls.Load())
	assert.Positive(t, voiceReads.Load(), "读取计数器必须真实观测传统Key读档案，避免零读断言空转")
}
