package modeltests

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/migrations"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestSynchronousAudioTokenCannotDispatchOrUseLegacyQuota(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/managed.db"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Token{}))
	require.NoError(t, migrations.Apply(db))
	previousDB, previousRedis, previousBatch := model.DB, common.RedisEnabled, common.BatchUpdateEnabled
	model.DB, common.RedisEnabled, common.BatchUpdateEnabled = db, false, true
	t.Cleanup(func() {
		model.DB, common.RedisEnabled, common.BatchUpdateEnabled = previousDB, previousRedis, previousBatch
	})
	for index, purpose := range []string{"audio.transcribe", "audio.translate", "audio.tts"} {
		t.Run(purpose, func(t *testing.T) {
			id := 130001 + index
			key := purpose + "-managed-key"
			require.NoError(t, db.Table("tokens").Create(map[string]any{"id": id, "user_id": 130000, "key": key, "status": common.TokenStatusEnabled, "expired_time": common.GetTimestamp() + 600, "remain_quota": 100, "purpose": purpose, "accounting_mode": "synchronous", "external_lease_id": purpose + ".lease"}).Error)
			var token model.Token
			require.NoError(t, db.First(&token, id).Error)
			assert.ErrorIs(t, model.CheckTokenAccountingMode(&token), model.ErrAudioAccountingUnavailable)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", nil)
			assert.ErrorIs(t, middleware.SetupContextForToken(c, &token), model.ErrAudioAccountingUnavailable)
			reserved, err := model.TryReserveTokenQuota(id, key, 20, false)
			assert.False(t, reserved)
			assert.ErrorIs(t, err, model.ErrAudioAccountingUnavailable)
			assert.ErrorIs(t, model.IncreaseTokenQuota(id, key, 20), model.ErrAudioAccountingUnavailable)
			assert.ErrorIs(t, model.DecreaseTokenQuota(id, key, 20), model.ErrAudioAccountingUnavailable)
			token.Purpose, token.AccountingMode = "generic", "legacy"
			assert.Error(t, token.Update(), "调用者伪造generic也不得扩大已持久受管资格")
			assert.Error(t, token.SelectUpdate())
			var remaining int
			require.NoError(t, db.Table("tokens").Where("id = ?", id).Pluck("remain_quota", &remaining).Error)
			assert.Equal(t, 100, remaining)
		})
	}
	assert.Error(t, (&model.Token{Purpose: "audio.tts", AccountingMode: "synchronous"}).Insert(), "普通创建入口不得铸受管令牌")
}

func TestTokenAccountingClosedSet(t *testing.T) {
	lease := "lease.audio"
	for _, tc := range []struct {
		name, purpose, mode string
		external            *string
		want                error
	}{
		{name: "legacy_zero"},
		{name: "legacy_explicit", purpose: "generic", mode: "legacy"},
		{name: "mode_without_purpose", purpose: "generic", mode: "synchronous", want: model.ErrTokenInvalid},
		{name: "unknown_mode", purpose: "generic", mode: "changed", want: model.ErrTokenInvalid},
		{name: "unknown_purpose", purpose: "unknown", mode: "synchronous", external: &lease, want: model.ErrTokenInvalid},
		{name: "audio_missing_external", purpose: "audio.tts", mode: "synchronous", want: model.ErrTokenInvalid},
		{name: "audio_legacy", purpose: "audio.tts", mode: "legacy", external: &lease, want: model.ErrTokenInvalid},
		{name: "audio_managed", purpose: "audio.tts", mode: "synchronous", external: &lease, want: model.ErrAudioAccountingUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := model.CheckTokenAccountingMode(&model.Token{Purpose: tc.purpose, AccountingMode: tc.mode, ExternalLeaseId: tc.external})
			assert.True(t, errors.Is(err, tc.want), "闭集组合必须给确定结果")
		})
	}
}
