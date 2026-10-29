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

func TestRelayLeasePersistsAcrossCredentialRotation(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/lease.db"), &gorm.Config{})
	require.NoError(t, err)
	pool, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pool.Close()) })
	require.NoError(t, db.AutoMigrate(&model.Token{}))
	assert.False(t, db.Migrator().HasColumn("tokens", "model_group"), "模型组列必须由正式SQL拥有")
	require.NoError(t, migrations.Apply(db))
	runRelayLeasePersistenceContract(t, db)
	require.NoError(t, db.Exec("ALTER TABLE tokens RENAME COLUMN model_group TO lost_model_group").Error)
	require.Error(t, migrations.Verify(db), "账本在场不能掩盖模型组列损坏")
}

func runRelayLeasePersistenceContract(t *testing.T, db *gorm.DB) {
	t.Helper()
	previousDB, previousRedis := model.DB, common.RedisEnabled
	model.DB, common.RedisEnabled = db, false
	t.Cleanup(func() { model.DB, common.RedisEnabled = previousDB, previousRedis })
	require.NoError(t, db.AutoMigrate(&model.User{}))
	require.NoError(t, db.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.User{Id: 777001, Username: "vc7-lease", AffCode: "vc7-lease", Status: common.UserStatusEnabled}).Error)
	spec := model.RelayLeaseSpec{ExternalLeaseId: "vc7.lease.persistence", NewApiUserId: 777001, CredentialGeneration: 1, ModelGroup: "default",
		ModelLimits: []string{}, ExpiredTime: time.Now().Add(15 * time.Minute).Unix(), Purpose: "generic", AccountingMode: "legacy"}
	issued, leaseErr := model.UpsertRelayLease(spec)
	require.Nil(t, leaseErr)
	replayed, leaseErr := model.UpsertRelayLease(spec)
	require.Nil(t, leaseErr, "同代完整同规格必须返回原Key")
	assert.Equal(t, issued.Token.Key, replayed.Token.Key)
	spec.CredentialGeneration++
	rotated, leaseErr := model.UpsertRelayLease(spec)
	require.Nil(t, leaseErr)
	assert.True(t, rotated.Rotated)
	assert.NotEqual(t, issued.Token.Key, rotated.Token.Key)
	stored, err := model.GetTokenById(rotated.Token.Id)
	require.NoError(t, err)
	assert.Equal(t, "default", stored.ModelGroup)
	assert.Error(t, stored.Update(), "普通Token编辑不得改写内部租约")
	audio := spec
	audio.Purpose, audio.AccountingMode, audio.ModelLimits, audio.CredentialGeneration = "audio.tts", "synchronous", []string{"tts-1"}, 3
	_, leaseErr = model.UpsertRelayLease(audio)
	require.NotNil(t, leaseErr)
	assert.Equal(t, http.StatusConflict, leaseErr.HTTPStatus, "既有租约的用途不能改")
	assert.Equal(t, model.RelayLeaseErrorConflict, leaseErr.Code)
	stored, err = model.GetTokenById(rotated.Token.Id)
	require.NoError(t, err)
	assert.Equal(t, rotated.Token.Key, stored.Key)
	for _, field := range []string{"model_group", "expired_time"} {
		changed := spec
		switch field {
		case "model_group":
			changed.ModelGroup = "pro"
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
	for _, generation := range []int64{1, 3, 999} {
		spec.CredentialGeneration = generation
		_, leaseErr = model.UpsertRelayLease(spec)
		require.NotNil(t, leaseErr, "撤销是lease身份终态，任何代号不得复活")
		assert.Equal(t, http.StatusConflict, leaseErr.HTTPStatus)
		assert.Equal(t, model.RelayLeaseErrorConflict, leaseErr.Code)
	}
	var revoked model.Token
	require.NoError(t, db.Unscoped().First(&revoked, rotated.Token.Id).Error)
	assert.True(t, revoked.DeletedAt.Valid)
	assert.Equal(t, rotated.Token.Key, revoked.Key)
	assert.Equal(t, rotated.Token.Name, revoked.Name)
	_, revokeErr = model.RevokeRelayLease(spec.ExternalLeaseId)
	require.Nil(t, revokeErr)
}
