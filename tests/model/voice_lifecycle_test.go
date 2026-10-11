package modeltests

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/migrations"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestVoiceLifecycleRealDatabases(t *testing.T) {
	dialects := []string{"sqlite"}
	if os.Getenv("VC7_SCHEMA_MATRIX") == "1" {
		dialects = append(dialects, "postgres", "mysql")
	}
	previousDB, previousType := model.DB, common.MainDatabaseType()
	t.Cleanup(func() { model.DB = previousDB; common.SetMainDatabaseType(previousType) })
	for _, dialect := range dialects {
		t.Run(dialect, func(t *testing.T) {
			var connection gorm.Dialector
			switch dialect {
			case "sqlite":
				connection = sqlite.Open(t.TempDir() + "/voice-lifecycle.db")
				common.SetMainDatabaseType(common.DatabaseTypeSQLite)
			case "postgres":
				connection = postgres.Open("host=127.0.0.1 port=15433 user=hasn_dev password=hasn_dev dbname=vc7_newapi_voice sslmode=disable")
				common.SetMainDatabaseType(common.DatabaseTypePostgreSQL)
			case "mysql":
				connection = mysql.Open("root@tcp(127.0.0.1:3396)/vc7_newapi_voice?charset=utf8mb4&parseTime=True&loc=UTC")
				common.SetMainDatabaseType(common.DatabaseTypeMySQL)
			}
			db, err := gorm.Open(connection, &gorm.Config{})
			require.NoError(t, err)
			pool, err := db.DB()
			require.NoError(t, err)
			pool.SetMaxOpenConns(2)
			if dialect == "sqlite" {
				pool.SetMaxOpenConns(1)
			} else {
				var database string
				query := "SELECT DATABASE()"
				if dialect == "postgres" {
					query = "SELECT current_database()"
				}
				require.NoError(t, db.Raw(query).Scan(&database).Error)
				require.Equal(t, "vc7_newapi_voice", database)
			}
			require.NoError(t, db.AutoMigrate(&model.Token{}))
			require.NoError(t, migrations.Apply(db))
			model.DB = db
			require.NoError(t, db.Where("user_id IN ?", []int{772001, 772002}).Delete(&model.VoiceProfile{}).Error)
			t.Cleanup(func() {
				require.NoError(t, db.Where("user_id IN ?", []int{772001, 772002}).Delete(&model.VoiceProfile{}).Error)
				require.NoError(t, pool.Close())
			})
			runVoiceLifecycleContract(t, db)
		})
	}
}

func runVoiceLifecycleContract(t *testing.T, db *gorm.DB) {
	t.Helper()
	ctx := context.Background()
	spec := model.VoiceCreationSpec{UserId: 772001, CreatorTokenId: 773001, ChannelId: 774001, IdempotencyKey: "original-key",
		RequestDigest: strings.Repeat("a", 64), Kind: "design", Language: "zh", TargetModel: "cosyvoice-v3.5-plus"}
	original, dispatch, err := model.BeginVoiceCreation(ctx, spec)
	require.NoError(t, err)
	require.True(t, dispatch)
	assert.True(t, model.ValidVoiceRef(original.VoiceRef))
	assert.False(t, model.ValidVoiceRef(strings.ToUpper(original.VoiceRef)))
	assert.False(t, model.ValidVoiceRef("cosyvoice-provider-id"))
	assert.Equal(t, model.VoiceStateUnknown, original.State)
	assert.Nil(t, original.ProviderVoiceId)
	assert.Equal(t, model.VoiceRetention, original.RetentionPolicy)
	stored, err := model.FindVoiceByIdempotencyKey(ctx, spec.UserId, spec.IdempotencyKey)
	require.NoError(t, err)
	assert.Equal(t, original.VoiceRef, stored.VoiceRef, "派发资格返回前记录已持久化")
	// 租约换代只改变调用审计Token，稳定账号仍能恢复同一资源，不能占用第二次派发。
	spec.CreatorTokenId++
	replayed, dispatch, err := model.BeginVoiceCreation(ctx, spec)
	require.NoError(t, err)
	assert.False(t, dispatch)
	assert.Equal(t, original.VoiceRef, replayed.VoiceRef)
	assert.Equal(t, original.CreatorTokenId, replayed.CreatorTokenId)
	spec.RequestDigest = strings.Repeat("b", 64)
	_, dispatch, err = model.BeginVoiceCreation(ctx, spec)
	assert.ErrorIs(t, err, model.ErrVoiceIdempotencyConflict)
	assert.False(t, dispatch)
	spec.RequestDigest = strings.Repeat("a", 64)
	_, err = model.RecordVoiceFailure(ctx, spec.UserId, original.VoiceRef, "provider_response_lost", false)
	require.NoError(t, err)
	replayed, dispatch, err = model.BeginVoiceCreation(ctx, spec)
	require.NoError(t, err)
	assert.Equal(t, model.VoiceStateUnknown, replayed.State)
	assert.False(t, dispatch, "unknown不能重建")
	_, err = model.FindVoiceProfile(ctx, 772002, original.VoiceRef)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
	_, err = model.FindVoiceByIdempotencyKey(ctx, 772002, spec.IdempotencyKey)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
	spec.UserId = 772002
	other, dispatch, err := model.BeginVoiceCreation(ctx, spec)
	require.NoError(t, err)
	assert.True(t, dispatch)
	assert.NotEqual(t, original.VoiceRef, other.VoiceRef)
	spec.UserId = 772001
	pending, dispatch, err := model.BeginVoiceDelete(ctx, spec.UserId, original.VoiceRef)
	require.NoError(t, err)
	assert.False(t, dispatch, "供应商ID丢失时不派发猜测删除")
	assert.Equal(t, model.VoiceStateUnknown, pending.State)
	assert.NotNil(t, pending.DeleteRequestedTime)
	assert.Nil(t, pending.DeletedTime)
	_, err = model.ConfirmVoiceDeletion(ctx, spec.UserId, original.VoiceRef)
	assert.ErrorIs(t, err, model.ErrVoiceInvalidState)
	_, err = model.RecordVoiceStatus(ctx, spec.UserId, original.VoiceRef, "missing-id", spec.TargetModel, "OK")
	assert.ErrorIs(t, err, model.ErrVoiceInvalidState, "无供应商ID的unknown不能猜成ready")
	spec.IdempotencyKey = "ready-key"
	ready, _, err := model.BeginVoiceCreation(ctx, spec)
	require.NoError(t, err)
	ready, err = model.RecordVoiceProvider(ctx, spec.UserId, ready.VoiceRef, "supplier-profile-id")
	require.NoError(t, err)
	assert.Equal(t, model.VoiceStateDeploying, ready.State)
	_, err = model.ValidateVoiceSynthesis(ctx, spec.UserId, ready.VoiceRef, spec.TargetModel)
	assert.ErrorIs(t, err, model.ErrVoiceUnavailable)
	_, err = model.RecordVoiceStatus(ctx, spec.UserId, ready.VoiceRef, *ready.ProviderVoiceId, spec.TargetModel, "new-status")
	assert.ErrorIs(t, err, model.ErrVoiceInvalidState)
	ready, err = model.RecordVoiceStatus(ctx, spec.UserId, ready.VoiceRef, *ready.ProviderVoiceId, spec.TargetModel, "OK")
	require.NoError(t, err)
	assert.Equal(t, model.VoiceStateReady, ready.State)
	_, err = model.ValidateVoiceSynthesis(ctx, spec.UserId, ready.VoiceRef, "tts-1")
	assert.ErrorIs(t, err, model.ErrVoiceUnavailable)
	synthesis, err := model.ValidateVoiceSynthesis(ctx, spec.UserId, ready.VoiceRef, spec.TargetModel)
	require.NoError(t, err)
	assert.Equal(t, int64(spec.ChannelId), synthesis.ChannelId)
	_, err = model.ConfirmVoiceDeletion(ctx, spec.UserId, ready.VoiceRef)
	assert.ErrorIs(t, err, model.ErrVoiceInvalidState, "尚未派发删除时不能确认")
	pending, dispatch, err = model.BeginVoiceDelete(ctx, spec.UserId, ready.VoiceRef)
	require.NoError(t, err)
	assert.True(t, dispatch)
	assert.Equal(t, model.VoiceStateReady, pending.State, "删除回执前保留原状态并标pending")
	assert.Nil(t, pending.DeletedTime)
	_, dispatch, err = model.BeginVoiceDelete(ctx, spec.UserId, ready.VoiceRef)
	require.NoError(t, err)
	assert.False(t, dispatch, "删除超时不得再次派发")
	_, err = model.ValidateVoiceSynthesis(ctx, spec.UserId, ready.VoiceRef, spec.TargetModel)
	assert.ErrorIs(t, err, model.ErrVoiceUnavailable, "删除请求先封禁合成")
	deleted, err := model.ConfirmVoiceDeletion(ctx, spec.UserId, ready.VoiceRef)
	require.NoError(t, err)
	assert.Equal(t, model.VoiceStateDeleted, deleted.State)
	assert.NotNil(t, deleted.DeletedTime)
	_, err = model.RecordVoiceStatus(ctx, spec.UserId, ready.VoiceRef, *ready.ProviderVoiceId, spec.TargetModel, "OK")
	assert.ErrorIs(t, err, model.ErrVoiceInvalidState, "查询不可复活已删除资源")
	_, dispatch, err = model.BeginVoiceCreation(ctx, spec)
	require.NoError(t, err)
	assert.False(t, dispatch)
	spec.IdempotencyKey = "concurrent-key"
	type result struct {
		profile  *model.VoiceProfile
		dispatch bool
		err      error
	}
	results := make(chan result, 8)
	for range 8 {
		go func() {
			profile, dispatch, err := model.BeginVoiceCreation(ctx, spec)
			results <- result{profile: profile, dispatch: dispatch, err: err}
		}()
	}
	dispatches, ref := 0, ""
	for range 8 {
		item := <-results
		require.NoError(t, item.err)
		if ref == "" {
			ref = item.profile.VoiceRef
		}
		assert.Equal(t, ref, item.profile.VoiceRef)
		if item.dispatch {
			dispatches++
		}
	}
	assert.Equal(t, 1, dispatches, "数据库唯一键必须只给一个并发调用创建资格")
}
