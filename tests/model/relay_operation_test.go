package modeltests

import (
	"net/http"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/migrations"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestRelayOperationPersistsAcrossCredentialRotation(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/operation.db"), &gorm.Config{})
	require.NoError(t, err)
	pool, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pool.Close()) })
	require.NoError(t, db.AutoMigrate(&model.Token{}))
	assert.False(t, db.Migrator().HasColumn("tokens", "operation"), "用途列必须由正式SQL拥有")
	require.NoError(t, migrations.Apply(db))
	runRelayOperationPersistenceContract(t, db)
	for _, statement := range []string{"ALTER TABLE tokens RENAME COLUMN operation TO lost_operation"} {
		require.NoError(t, db.Exec(statement).Error)
		require.Error(t, migrations.Verify(db), "账本在场不能掩盖用途列损坏")
	}
}

func runRelayOperationPersistenceContract(t *testing.T, db *gorm.DB) {
	t.Helper()
	previousDB, previousRedis := model.DB, common.RedisEnabled
	model.DB, common.RedisEnabled = db, false
	t.Cleanup(func() { model.DB, common.RedisEnabled = previousDB, previousRedis })
	require.NoError(t, db.AutoMigrate(&model.User{}))
	require.NoError(t, db.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.User{Id: 777001, Username: "vc7-operation", AffCode: "vc7-operation", Status: common.UserStatusEnabled}).Error)
	spec := model.RelayLeaseSpec{ExternalLeaseId: "vc7.operation.persistence", NewApiUserId: 777001, CredentialGeneration: 1, ModelGroup: "default",
		ModelLimits: []string{"tts-1"}, ExpiredTime: time.Now().Add(15 * time.Minute).Unix(), Purpose: "generic", AccountingMode: "legacy", Operation: "audio.tts"}
	issued, leaseErr := model.UpsertRelayLease(spec)
	require.Nil(t, leaseErr)
	assert.Equal(t, spec.Operation, issued.Token.Operation)
	replayed, leaseErr := model.UpsertRelayLease(spec)
	require.Nil(t, leaseErr, "同代完整同规格必须返回原Key")
	assert.Equal(t, issued.Token.Key, replayed.Token.Key)
	spec.CredentialGeneration++
	rotated, leaseErr := model.UpsertRelayLease(spec)
	require.Nil(t, leaseErr)
	assert.True(t, rotated.Rotated)
	assert.NotEqual(t, issued.Token.Key, rotated.Token.Key)
	assert.Equal(t, spec.Operation, rotated.Token.Operation)
	stored, err := model.GetTokenById(rotated.Token.Id)
	require.NoError(t, err)
	assert.Equal(t, spec.Operation, stored.Operation)
	assert.Error(t, stored.Update(), "普通Token编辑不得清空内部租约用途")
	spec.Operation = "audio.voice_create"
	spec.CredentialGeneration++
	_, leaseErr = model.UpsertRelayLease(spec)
	require.NotNil(t, leaseErr)
	assert.Equal(t, http.StatusConflict, leaseErr.HTTPStatus)
	assert.Equal(t, model.RelayLeaseErrorConflict, leaseErr.Code)
	spec.Operation, spec.ModelLimits = "", []string{}
	_, leaseErr = model.UpsertRelayLease(spec)
	require.NotNil(t, leaseErr)
	assert.Equal(t, http.StatusConflict, leaseErr.HTTPStatus)
	stored, err = model.GetTokenById(rotated.Token.Id)
	require.NoError(t, err)
	assert.Equal(t, "audio.tts", stored.Operation)
	assert.Equal(t, rotated.Token.Key, stored.Key)
	spec.Operation, spec.ModelLimits, spec.CredentialGeneration = "audio.tts", []string{"tts-1"}, 2
	for _, field := range []string{"model_group", "model_limits", "expired_time"} {
		changed := spec
		switch field {
		case "model_group":
			changed.ModelGroup = "pro"
		case "model_limits":
			changed.ModelLimits = []string{"other"}
		case "expired_time":
			changed.ExpiredTime++
		}
		_, conflict := model.UpsertRelayLease(changed)
		require.NotNil(t, conflict)
		assert.Equal(t, "lease_conflict", conflict.Code)
	}

	_, revokeErr := model.RevokeRelayLease(spec.ExternalLeaseId)
	require.Nil(t, revokeErr)
	_, leaseErr = model.UpsertRelayLease(spec)
	require.NotNil(t, leaseErr, "撤销同代不可被旧PUT复活")
	assert.Equal(t, "lease_conflict", leaseErr.Code)
	spec.CredentialGeneration++
	renewed, leaseErr := model.UpsertRelayLease(spec)
	require.Nil(t, leaseErr, "同用途更高代可重新激活原Token行")
	assert.Equal(t, rotated.Token.Id, renewed.Token.Id)
	assert.NotEqual(t, rotated.Token.Key, renewed.Token.Key)
	assert.False(t, renewed.Token.DeletedAt.Valid)
	_, revokeErr = model.RevokeRelayLease(spec.ExternalLeaseId)
	require.Nil(t, revokeErr)
}
