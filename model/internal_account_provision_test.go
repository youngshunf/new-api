package model

import (
	"fmt"
	"net/http"
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

	first, err := ProvisionInternalAccount("workspace-provision-1", nil)
	require.Nil(t, err)
	second, err := ProvisionInternalAccount("workspace-provision-1", nil)
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

	first, err := ProvisionInternalAccount("opaque-key", nil)
	require.Nil(t, err)
	second, err := ProvisionInternalAccount(" opaque-key ", nil)
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

	userId, err := ProvisionInternalAccount("workspace-provision-user", nil)
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
			userId, err := ProvisionInternalAccount("workspace-provision-concurrent", nil)
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
			_, err := ProvisionInternalAccount(key, nil)
			require.NotNil(t, err)
			assert.Equal(t, InternalAccountErrorInvalidRequest, err.Code)
		})
	}
}

func TestGetInternalAccountStatus(t *testing.T) {
	truncateTables(t)
	userId, err := ProvisionInternalAccount("workspace-provision-status", nil)
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
		// 删表让下次运行仍从「表不存在」开始验证建表路径。
		_ = db.Migrator().DropTable(&InternalAccountProvision{})
	})

	const attempts = 100
	results := make(chan int, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			userId, err := ProvisionInternalAccount(provisionKey, nil)
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

// legacyInternalAccountProvision 是加 workspace 两列之前的表形，用于验证升级路径。
type legacyInternalAccountProvision struct {
	Id           int    `gorm:"primaryKey"`
	ProvisionKey string `gorm:"type:varchar(64);uniqueIndex;not null"`
	UserId       int    `gorm:"column:user_id;index;not null"`
	CreatedTime  int64  `gorm:"autoCreateTime;column:created_time"`
}

func (legacyInternalAccountProvision) TableName() string { return "internal_account_provisions" }

// exerciseInternalAccountWorkspaceUpgrade 用旧表形建表并写入旧户，再按新模型迁移两次，
// 核对两列可空新增、旧数据与唯一约束保留、旧户重放带值补写、同空间可多账户。
func exerciseInternalAccountWorkspaceUpgrade(t *testing.T, db *gorm.DB, keyPrefix string) {
	t.Helper()
	previousDB := DB
	DB = db
	t.Cleanup(func() { DB = previousDB })

	require.NoError(t, db.AutoMigrate(&User{}))
	require.False(t, db.Migrator().HasTable("internal_account_provisions"))
	require.NoError(t, db.AutoMigrate(&legacyInternalAccountProvision{}))
	stamp := time.Now().UnixNano()
	legacyKey := fmt.Sprintf("%s-legacy-%d", keyPrefix, stamp)
	legacyUser := User{Username: fmt.Sprintf("iapup-%d", stamp%1_000_000_000), DisplayName: "Cloud Account", Role: common.RoleCommonUser,
		Status: common.UserStatusEnabled, Group: "default", AffCode: fmt.Sprintf("iapup-%d", stamp)}
	require.NoError(t, db.Create(&legacyUser).Error)
	t.Cleanup(func() {
		var provisions []InternalAccountProvision
		if err := db.Where("provision_key LIKE ?", legacyKey+"%").Find(&provisions).Error; err == nil {
			for _, provision := range provisions {
				_ = db.Unscoped().Where("id = ?", provision.UserId).Delete(&User{}).Error
			}
		}
		_ = db.Unscoped().Where("id = ?", legacyUser.Id).Delete(&User{}).Error
		_ = db.Migrator().DropTable("internal_account_provisions")
	})
	require.NoError(t, db.Create(&legacyInternalAccountProvision{ProvisionKey: legacyKey, UserId: legacyUser.Id}).Error)

	for range 2 {
		require.NoError(t, db.AutoMigrate(&User{}, &InternalAccountProvision{}))
	}
	assert.True(t, db.Migrator().HasColumn(&InternalAccountProvision{}, "workspace_key"))
	assert.True(t, db.Migrator().HasColumn(&InternalAccountProvision{}, "workspace_kind"))
	assert.True(t, db.Migrator().HasIndex(&InternalAccountProvision{}, "ProvisionKey"))

	var migrated InternalAccountProvision
	require.NoError(t, db.Where("provision_key = ?", legacyKey).First(&migrated).Error)
	assert.Equal(t, legacyUser.Id, migrated.UserId)
	assert.Nil(t, migrated.WorkspaceKey)
	assert.Nil(t, migrated.WorkspaceKind)

	duplicate := db.Create(&InternalAccountProvision{ProvisionKey: legacyKey, UserId: legacyUser.Id})
	assert.Error(t, duplicate.Error, "provision_key 唯一约束升级后仍在")

	workspace := &InternalAccountWorkspace{Key: "workspace_" + keyPrefix, Kind: InternalAccountWorkspaceKindEnterprise}
	backfilled, provisionErr := ProvisionInternalAccount(legacyKey, workspace)
	require.Nil(t, provisionErr)
	assert.Equal(t, legacyUser.Id, backfilled)
	secondKey := legacyKey + "-seat2"
	second, provisionErr := ProvisionInternalAccount(secondKey, workspace)
	require.Nil(t, provisionErr)
	assert.NotEqual(t, legacyUser.Id, second)
	_, conflictErr := ProvisionInternalAccount(legacyKey, &InternalAccountWorkspace{Key: "workspace_other", Kind: "personal"})
	require.NotNil(t, conflictErr)
	assert.Equal(t, InternalAccountErrorIdempotencyConflict, conflictErr.Code)

	var rows []InternalAccountProvision
	require.NoError(t, db.Where("workspace_key = ?", workspace.Key).Order("id").Find(&rows).Error)
	require.Len(t, rows, 2)
	assert.Equal(t, "enterprise", *rows[0].WorkspaceKind)
	assert.Equal(t, "enterprise", *rows[1].WorkspaceKind)
}

func TestInternalAccountWorkspaceUpgradeSQLite(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/internal-account-upgrade.db"), &gorm.Config{})
	require.NoError(t, err)
	exerciseInternalAccountWorkspaceUpgrade(t, db, "sqlite-upgrade")
}

func TestInternalAccountWorkspaceUpgradeMySQL(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN is not configured")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	exerciseInternalAccountWorkspaceUpgrade(t, db, "mysql-upgrade")
}

func TestInternalAccountWorkspaceUpgradePostgreSQL(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not configured")
	}
	db, err := gorm.Open(postgres.New(postgres.Config{DSN: dsn, PreferSimpleProtocol: true}), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	exerciseInternalAccountWorkspaceUpgrade(t, db, "postgres-upgrade")
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
	t.Cleanup(func() { _ = db.Unscoped().Where("username = ?", username).Delete(&User{}).Error })
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

func loadInternalAccountProvision(t *testing.T, provisionKey string) InternalAccountProvision {
	t.Helper()
	var provision InternalAccountProvision
	require.NoError(t, DB.Where("provision_key = ?", provisionKey).First(&provision).Error)
	return provision
}

func TestProvisionInternalAccountRecordsWorkspaceOnFirstProvisionAndReplays(t *testing.T) {
	truncateTables(t)
	workspace := &InternalAccountWorkspace{Key: "workspace_ent-001", Kind: InternalAccountWorkspaceKindEnterprise}

	first, err := ProvisionInternalAccount("seat-provision-1", workspace)
	require.Nil(t, err)
	stored := loadInternalAccountProvision(t, "seat-provision-1")
	require.NotNil(t, stored.WorkspaceKey)
	require.NotNil(t, stored.WorkspaceKind)
	assert.Equal(t, "workspace_ent-001", *stored.WorkspaceKey)
	assert.Equal(t, "enterprise", *stored.WorkspaceKind)

	sameReplay, err := ProvisionInternalAccount("seat-provision-1", &InternalAccountWorkspace{Key: "workspace_ent-001", Kind: "enterprise"})
	require.Nil(t, err)
	assert.Equal(t, first, sameReplay)

	bareReplay, err := ProvisionInternalAccount("seat-provision-1", nil)
	require.Nil(t, err, "未声明空间的旧 core 重放不判冲突")
	assert.Equal(t, first, bareReplay)

	for _, other := range []InternalAccountWorkspace{
		{Key: "workspace_ent-002", Kind: "enterprise"},
		{Key: "workspace_ent-001", Kind: "personal"},
	} {
		_, conflictErr := ProvisionInternalAccount("seat-provision-1", &other)
		require.NotNil(t, conflictErr)
		assert.Equal(t, http.StatusConflict, conflictErr.HTTPStatus)
		assert.Equal(t, InternalAccountErrorIdempotencyConflict, conflictErr.Code)
	}
	unchanged := loadInternalAccountProvision(t, "seat-provision-1")
	assert.Equal(t, "workspace_ent-001", *unchanged.WorkspaceKey)
	assert.Equal(t, "enterprise", *unchanged.WorkspaceKind)
}

func TestProvisionInternalAccountWithoutWorkspaceStoresNullAndBackfillsOnce(t *testing.T) {
	truncateTables(t)

	legacy, err := ProvisionInternalAccount("legacy-core-provision", nil)
	require.Nil(t, err)
	stored := loadInternalAccountProvision(t, "legacy-core-provision")
	assert.Nil(t, stored.WorkspaceKey)
	assert.Nil(t, stored.WorkspaceKind)

	backfilled, err := ProvisionInternalAccount("legacy-core-provision", &InternalAccountWorkspace{Key: "workspace_personal_9", Kind: "personal"})
	require.Nil(t, err)
	assert.Equal(t, legacy, backfilled)
	stored = loadInternalAccountProvision(t, "legacy-core-provision")
	require.NotNil(t, stored.WorkspaceKey)
	assert.Equal(t, "workspace_personal_9", *stored.WorkspaceKey)
	assert.Equal(t, "personal", *stored.WorkspaceKind)

	_, conflictErr := ProvisionInternalAccount("legacy-core-provision", &InternalAccountWorkspace{Key: "workspace_personal_8", Kind: "personal"})
	require.NotNil(t, conflictErr, "补写只发生一次，之后不同值判冲突")
	assert.Equal(t, InternalAccountErrorIdempotencyConflict, conflictErr.Code)
}

func TestProvisionInternalAccountAllowsManyAccountsPerWorkspace(t *testing.T) {
	truncateTables(t)
	workspace := &InternalAccountWorkspace{Key: "workspace_ent-seats", Kind: "enterprise"}

	seatA, err := ProvisionInternalAccount("seat-a", workspace)
	require.Nil(t, err)
	seatB, err := ProvisionInternalAccount("seat-b", workspace)
	require.Nil(t, err)

	assert.NotEqual(t, seatA, seatB)
	var count int64
	require.NoError(t, DB.Model(&InternalAccountProvision{}).Where("workspace_key = ?", "workspace_ent-seats").Count(&count).Error)
	assert.Equal(t, int64(2), count)
}

func TestValidateInternalAccountWorkspace(t *testing.T) {
	ptr := func(value string) *string { return &value }
	cases := []struct {
		name    string
		key     *string
		kind    *string
		want    *InternalAccountWorkspace
		invalid bool
	}{
		{name: "both absent", want: nil},
		{name: "personal", key: ptr("workspace_abc"), kind: ptr("personal"), want: &InternalAccountWorkspace{Key: "workspace_abc", Kind: "personal"}},
		{name: "enterprise max length", key: ptr("workspace_" + strings.Repeat("a", 96)), kind: ptr("enterprise"), want: &InternalAccountWorkspace{Key: "workspace_" + strings.Repeat("a", 96), Kind: "enterprise"}},
		{name: "key only", key: ptr("workspace_abc"), invalid: true},
		{name: "kind only", kind: ptr("personal"), invalid: true},
		{name: "kind not allowed", key: ptr("workspace_abc"), kind: ptr("team"), invalid: true},
		{name: "kind empty", key: ptr("workspace_abc"), kind: ptr(""), invalid: true},
		{name: "key missing prefix", key: ptr("abc123"), kind: ptr("personal"), invalid: true},
		{name: "key suffix too short", key: ptr("workspace_ab"), kind: ptr("personal"), invalid: true},
		{name: "key suffix too long", key: ptr("workspace_" + strings.Repeat("a", 97)), kind: ptr("personal"), invalid: true},
		{name: "key illegal char", key: ptr("workspace_ab.c"), kind: ptr("personal"), invalid: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateInternalAccountWorkspace(tc.key, tc.kind)
			if tc.invalid {
				require.NotNil(t, err)
				assert.Equal(t, http.StatusBadRequest, err.HTTPStatus)
				assert.Equal(t, InternalAccountErrorInvalidRequest, err.Code)
				return
			}
			require.Nil(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
