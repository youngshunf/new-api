package model

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestProvisionInternalAccountIsIdempotentPerProvisionKey(t *testing.T) {
	truncateTables(t)

	first, err := ProvisionInternalAccount("workspace-provision-1")
	require.Nil(t, err)
	second, err := ProvisionInternalAccount("workspace-provision-1")
	require.Nil(t, err)

	assert.Equal(t, first, second)
	var provisionCount int64
	require.NoError(t, DB.Model(&InternalAccountProvision{}).Where("provision_key = ?", "workspace-provision-1").Count(&provisionCount).Error)
	assert.Equal(t, int64(1), provisionCount)
	var userCount int64
	require.NoError(t, DB.Model(&User{}).Count(&userCount).Error)
	assert.Equal(t, int64(1), userCount)
}

func TestProvisionKeyIsOpaqueAndNotTrimmed(t *testing.T) {
	truncateTables(t)

	first, err := ProvisionInternalAccount("opaque-key")
	require.Nil(t, err)
	second, err := ProvisionInternalAccount(" opaque-key ")
	require.Nil(t, err)

	assert.NotEqual(t, first, second, "provision_key 是不透明 wire 字面量，不能 trim 后合并")
	var provisionCount int64
	require.NoError(t, DB.Model(&InternalAccountProvision{}).Count(&provisionCount).Error)
	assert.Equal(t, int64(2), provisionCount)
}

func TestProvisionKeyDerivedIdentifiersStayWithinLimitsAndCarryEnoughEntropy(t *testing.T) {
	firstUsername := internalAccountUsername("workspace-a")
	secondUsername := internalAccountUsername("workspace-b")
	firstAffCode := internalAccountAffCode("workspace-a")
	secondAffCode := internalAccountAffCode("workspace-b")

	assert.NotEqual(t, firstUsername, secondUsername)
	assert.NotEqual(t, firstAffCode, secondAffCode)
	assert.Len(t, firstUsername, UserNameMaxLength)
	assert.Len(t, firstAffCode, 32)
	for _, value := range []string{firstUsername, firstAffCode} {
		assert.NotContains(t, value, "/")
		assert.NotContains(t, value, "+")
		assert.NotContains(t, value, "=")
	}
}

func TestProvisionInternalAccountCreatesNonLoginDefaultUser(t *testing.T) {
	truncateTables(t)

	userId, err := ProvisionInternalAccount("workspace-provision-user")
	require.Nil(t, err)

	var user User
	require.NoError(t, DB.Where("id = ?", userId).First(&user).Error)
	assert.Equal(t, common.UserStatusEnabled, user.Status)
	assert.Equal(t, common.RoleCommonUser, user.Role)
	assert.Equal(t, "default", user.Group)
	assert.Empty(t, user.Password)
	assert.Empty(t, user.Email)
	assert.Zero(t, user.Quota)
	assert.NotEmpty(t, user.AffCode)

	login := User{Username: user.Username, Password: "any-password"}
	assert.ErrorIs(t, login.ValidateAndFill(), ErrInvalidCredentials)
}

func TestConcurrentProvisionInternalAccountCreatesOneMappingAndOneUser(t *testing.T) {
	truncateTables(t)

	const attempts = 100
	results := make(chan int, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			userId, err := ProvisionInternalAccount("workspace-provision-concurrent")
			if err == nil {
				results <- userId
			}
		}()
	}
	wg.Wait()
	close(results)

	var userIds []int
	for userId := range results {
		userIds = append(userIds, userId)
	}
	require.Len(t, userIds, attempts)
	for _, userId := range userIds {
		assert.Equal(t, userIds[0], userId)
	}
	var provisionCount int64
	require.NoError(t, DB.Model(&InternalAccountProvision{}).Where("provision_key = ?", "workspace-provision-concurrent").Count(&provisionCount).Error)
	assert.Equal(t, int64(1), provisionCount)
	var userCount int64
	require.NoError(t, DB.Model(&User{}).Count(&userCount).Error)
	assert.Equal(t, int64(1), userCount)
}

func TestProvisionInternalAccountRejectsInvalidProvisionKey(t *testing.T) {
	for _, key := range []string{"", strings.Repeat("x", 65)} {
		t.Run("invalid", func(t *testing.T) {
			_, err := ProvisionInternalAccount(key)
			require.NotNil(t, err)
			assert.Equal(t, InternalAccountErrorInvalidRequest, err.Code)
		})
	}
}

func TestGetInternalAccountStatus(t *testing.T) {
	truncateTables(t)
	userId, err := ProvisionInternalAccount("workspace-provision-status")
	require.Nil(t, err)

	status, getErr := GetInternalAccountStatus(userId)
	require.Nil(t, getErr)
	assert.Equal(t, common.UserStatusEnabled, status)

	_, missingErr := GetInternalAccountStatus(999999)
	require.NotNil(t, missingErr)
	assert.Equal(t, InternalAccountErrorUserNotFound, missingErr.Code)
}

func exerciseInternalAccountMigrationAndConcurrency(t *testing.T, db *gorm.DB, keyPrefix string) {
	t.Helper()
	previousDB := DB
	DB = db
	t.Cleanup(func() { DB = previousDB })

	require.NoError(t, db.AutoMigrate(&User{}))
	var baselineUsers int64
	require.NoError(t, db.Model(&User{}).Count(&baselineUsers).Error)
	for range 2 {
		require.NoError(t, db.AutoMigrate(&User{}, &InternalAccountProvision{}))
	}

	provisionKey := fmt.Sprintf("%s-%d", keyPrefix, time.Now().UnixNano())
	t.Cleanup(func() {
		var provision InternalAccountProvision
		if err := db.Where("provision_key = ?", provisionKey).Limit(1).Find(&provision).Error; err == nil && provision.UserId > 0 {
			_ = db.Unscoped().Where("id = ?", provision.UserId).Delete(&User{}).Error
		}
		_ = db.Where("provision_key = ?", provisionKey).Delete(&InternalAccountProvision{}).Error
	})

	const attempts = 100
	results := make(chan int, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			userId, err := ProvisionInternalAccount(provisionKey)
			if err == nil {
				results <- userId
			}
		}()
	}
	wg.Wait()
	close(results)

	var userIds []int
	for userId := range results {
		userIds = append(userIds, userId)
	}
	require.Len(t, userIds, attempts)
	for _, userId := range userIds {
		assert.Equal(t, userIds[0], userId)
	}
	var provisionCount int64
	require.NoError(t, db.Model(&InternalAccountProvision{}).Where("provision_key = ?", provisionKey).Count(&provisionCount).Error)
	assert.Equal(t, int64(1), provisionCount)
	var provision InternalAccountProvision
	require.NoError(t, db.Where("provision_key = ?", provisionKey).First(&provision).Error)
	assert.Equal(t, userIds[0], provision.UserId)

	var userCount int64
	require.NoError(t, db.Model(&User{}).Count(&userCount).Error)
	assert.Equal(t, baselineUsers+1, userCount)
	var provisionedUser User
	require.NoError(t, db.Where("id = ?", provision.UserId).First(&provisionedUser).Error)
	assert.Equal(t, internalAccountUsername(provisionKey), provisionedUser.Username)
}

func TestInternalAccountMigrationAndConcurrencySQLite(t *testing.T) {
	dbPath := t.TempDir() + "/internal-account.db?_pragma=busy_timeout(30000)&_pragma=journal_mode(WAL)&_txlock=immediate"
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(8)
	require.NoError(t, db.AutoMigrate(&User{}))
	require.False(t, db.Migrator().HasTable(&InternalAccountProvision{}))
	seedLegacyInternalAccountUser(t, db, "legacy-account-user", "legacy-account-aff")
	exerciseInternalAccountMigrationAndConcurrency(t, db, "sqlite-internal-account")
}

func seedLegacyInternalAccountUser(t *testing.T, db *gorm.DB, username string, affCode string) {
	t.Helper()
	require.NoError(t, db.Create(&User{
		Username:    username,
		Password:    "legacy-password",
		DisplayName: "Legacy User",
		Role:        common.RoleCommonUser,
		Status:      common.UserStatusEnabled,
		Group:       "default",
		AffCode:     affCode,
	}).Error)
}

func TestInternalAccountMigrationAndConcurrencyMySQL(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN is not configured")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	require.NoError(t, db.AutoMigrate(&User{}))
	require.False(t, db.Migrator().HasTable(&InternalAccountProvision{}))
	seedLegacyInternalAccountUser(t, db, "legacy-mysql-user", "legacy-mysql-aff")
	exerciseInternalAccountMigrationAndConcurrency(t, db, "mysql-internal-account")
}

func TestInternalAccountMigrationAndConcurrencyPostgreSQL(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not configured")
	}
	db, err := gorm.Open(postgres.New(postgres.Config{
		DSN:                  dsn,
		PreferSimpleProtocol: true,
	}), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	require.NoError(t, db.AutoMigrate(&User{}))
	require.False(t, db.Migrator().HasTable(&InternalAccountProvision{}))
	seedLegacyInternalAccountUser(t, db, "legacy-pg-user", "legacy-pg-aff")
	exerciseInternalAccountMigrationAndConcurrency(t, db, "postgres-internal-account")
}
