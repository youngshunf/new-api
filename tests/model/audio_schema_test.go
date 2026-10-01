package modeltests

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/migrations"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/clickhouse"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestAudioSchemaCannotBeCreatedByLegacyAutoMigrate(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/baseline.db"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Token{}))
	assert.False(t, db.Migrator().HasColumn("tokens", "purpose"), "新用途列必须由正式SQLrunner创建，不由上游AutoMigrate代建")
	assert.False(t, db.Migrator().HasTable("audio_request_settlements"))
}

func TestAudioSchemaRealDatabaseMatrix(t *testing.T) {
	if os.Getenv("SOTC12_SCHEMA_MATRIX") != "1" {
		t.Skip("独立三库验证由SOTC12_SCHEMA_MATRIX启用")
	}
	for _, dialect := range []string{"sqlite", "postgres", "mysql"} {
		t.Run(dialect, func(t *testing.T) {
			var connection gorm.Dialector
			switch dialect {
			case "sqlite":
				connection = sqlite.Open(t.TempDir() + "/matrix.db")
			case "postgres":
				connection = postgres.Open("host=127.0.0.1 port=15433 user=tk_sotc12_s1a password=tk_sotc12_s1a_local dbname=tk_sotc12_s1a sslmode=disable")
			case "mysql":
				connection = mysql.Open("tk_sotc12:tk_sotc12_local@tcp(127.0.0.1:13312)/tk_sotc12_s1a?charset=utf8mb4&parseTime=True&loc=UTC")
			}
			db, err := gorm.Open(connection, &gorm.Config{})
			require.NoError(t, err)
			var version string
			query := "SELECT version()"
			if dialect == "sqlite" {
				query = "SELECT sqlite_version()"
			}
			require.NoError(t, db.Raw(query).Scan(&version).Error)
			t.Logf("真实%s版本：%s", dialect, version)
			if dialect != "sqlite" {
				var database string
				query := "SELECT DATABASE()"
				if dialect == "postgres" {
					query = "SELECT current_database()"
				}
				require.NoError(t, db.Raw(query).Scan(&database).Error)
				require.Equal(t, "tk_sotc12_s1a", database, "禁止触及共享开发业务库")
			}
			for _, stage := range []string{"fresh", "upgrade"} {
				t.Run(stage, func(t *testing.T) {
					require.NoError(t, db.Migrator().DropTable("audio_request_settlements", "schema_migrations", "tokens"))
					// 旧schema只从fixed884f基线构建，不能拿本tree新model假装旧发布。
					require.NoError(t, db.AutoMigrate(&token884fBaseline{}))
					if stage == "upgrade" {
						require.NoError(t, db.Table("tokens").Create(map[string]any{"id": 120001, "user_id": 120002, "key": "tk_old_schema_token", "name": "原token", "remain_quota": 71}).Error)
					}
					require.NoError(t, migrations.Apply(db))
					require.NoError(t, migrations.Apply(db))
					require.NoError(t, migrations.Verify(db))
					previousType := common.MainDatabaseType()
					common.SetMainDatabaseType(common.DatabaseType(dialect))
					if dialect == "postgres" {
						common.SetMainDatabaseType(common.DatabaseTypePostgreSQL)
					}
					stamp := time.Date(2026, 10, 1, 8, 0, 0, 123000000, time.UTC)
					entity := model.AudioRequestSettlement{
						AudioRequestSettlementId: "01998dd2-0000-7000-8000-000000000001", CreatedTime: model.DatabaseTime{Time: stamp}, UpdatedTime: model.DatabaseTime{Time: stamp}, Revision: 1,
						ExternalLeaseId: "tk.audio.receipt", GatewayRequestId: "tk.request.1", TokenId: 120020, UserId: 120002, CredentialGeneration: 1, RelayMode: 24, ModelName: "tts", FundingPreference: "wallet_only", BillingStatus: "pending", DispatchState: "unknown",
					}
					require.NoError(t, db.Create(&entity).Error)
					var loaded model.AudioRequestSettlement
					require.NoError(t, db.Where("audio_request_settlement_id = ?", entity.AudioRequestSettlementId).First(&loaded).Error)
					assert.True(t, loaded.CreatedTime.Equal(stamp), "真实三库公共时间逐毫秒同值")
					assert.Nil(t, loaded.CompletedTime)
					assert.Nil(t, loaded.TargetQuota)
					duplicate := entity
					duplicate.AudioRequestSettlementId = "01998dd2-0000-7000-8000-000000000002"
					assert.Error(t, db.Create(&duplicate).Error, "查询关联键不得重复登记音频请求")
					zero := int64(0)
					entity.TargetQuota = &zero
					require.NoError(t, db.Save(&entity).Error)
					require.NoError(t, db.Where("audio_request_settlement_id = ?", entity.AudioRequestSettlementId).First(&loaded).Error)
					require.NotNil(t, loaded.TargetQuota)
					assert.Equal(t, int64(0), *loaded.TargetQuota, "NULL与确定零费不得混同")
					common.SetMainDatabaseType(previousType)
					if stage == "upgrade" {
						var remaining int
						require.NoError(t, db.Table("tokens").Where("id = ?", 120001).Pluck("remain_quota", &remaining).Error)
						assert.Equal(t, 71, remaining)
					}
					var nullable []any = []any{nil, nil}
					for i, external := range nullable {
						require.NoError(t, db.Table("tokens").Create(map[string]any{"id": 120010 + i, "user_id": 120002, "key": fmt.Sprintf("tk_generic_%d", i), "external_lease_id": external}).Error)
					}
					require.NoError(t, db.Table("tokens").Create(map[string]any{"id": 120020, "user_id": 120002, "key": "tk_audio_1", "external_lease_id": "tk.audio.unique"}).Error)
					assert.Error(t, db.Table("tokens").Create(map[string]any{"id": 120021, "user_id": 120002, "key": "tk_audio_2", "external_lease_id": "tk.audio.unique"}).Error)
					require.NoError(t, db.Table("tokens").Where("id = ?", 120020).Update("deleted_at", time.Now().UTC()).Error)
					assert.Error(t, db.Table("tokens").Create(map[string]any{"id": 120022, "user_id": 120003, "key": "tk_audio_deleted_reuse", "external_lease_id": "tk.audio.unique"}).Error, "软删外部grain仍保唯一占位")
					// 摘要不一致必须明确启动失败，而不是把已应用文件当新migration忽略。
					require.NoError(t, db.Table("schema_migrations").Where("migration_id = ?", "V001__音频同步账务底座.sql").Update("checksum", "altered").Error)
					assert.Error(t, migrations.Verify(db))
					assert.Error(t, migrations.Apply(db), "master也不得覆盖已登记摘要")
				})
			}
			if dialect == "mysql" {
				t.Run("implicit_ddl_recovery", func(t *testing.T) {
					require.NoError(t, db.Migrator().DropTable("audio_request_settlements", "schema_migrations", "tokens"))
					require.NoError(t, db.AutoMigrate(&token884fBaseline{}))
					// 真实模拟已提交第一列而账本尚未完成，不声称MySQL可整文件回滚。
					require.NoError(t, db.Exec("ALTER TABLE tokens ADD COLUMN purpose varchar(32) NOT NULL DEFAULT 'generic'").Error)
					require.NoError(t, migrations.Apply(db))
					require.NoError(t, migrations.Verify(db))
				})
			}
			// 留存最终目标schema供真实DB modelgen；摘要损坏反例后重建隔离schema，不留假通过。
			require.NoError(t, db.Migrator().DropTable("audio_request_settlements", "schema_migrations", "tokens"))
			require.NoError(t, db.AutoMigrate(&model.Token{}))
			require.NoError(t, migrations.Apply(db))
		})
	}
}

func TestFormalMigrationCreatesRequiredAudioSchema(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/baseline.db"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Token{}))
	require.Error(t, migrations.Verify(db), "未正式迁移时只读启动门必须拒绝")
	require.NoError(t, migrations.Apply(db))
	require.NoError(t, migrations.Apply(db), "第二次启动必须幂等")
	require.NoError(t, migrations.Verify(db))
	assert.True(t, db.Migrator().HasColumn("tokens", "purpose"), "正式迁移后应有持久用途，不允许只内存或自报模式")
	assert.True(t, db.Migrator().HasTable("audio_request_settlements"), "table-first真实表必须在生成业务model前存在")
	require.NoError(t, db.Exec("DROP INDEX idx_tokens_external_lease_id").Error)
	require.NoError(t, db.Exec("CREATE INDEX idx_tokens_external_lease_id ON tokens(external_lease_id)").Error)
	assert.Error(t, migrations.Verify(db), "同名普通索引不得冒充唯一外部绑定")
}

func TestAudioFormalStartupTwice(t *testing.T) {
	if os.Getenv("SOTC12_SCHEMA_MATRIX") != "1" {
		t.Skip("真实三库启动专项")
	}
	previousDB, previousMaster, previousSQLite := model.DB, common.IsMasterNode, common.SQLitePath
	previousType := common.MainDatabaseType()
	t.Cleanup(func() {
		model.DB, common.IsMasterNode, common.SQLitePath = previousDB, previousMaster, previousSQLite
		common.SetMainDatabaseType(previousType)
	})
	for _, dialect := range []string{"sqlite", "postgres", "mysql"} {
		t.Run(dialect, func(t *testing.T) {
			common.IsMasterNode = true
			switch dialect {
			case "sqlite":
				common.SQLitePath = t.TempDir() + "/startup.db"
				t.Setenv("SQL_DSN", "local")
			case "postgres":
				t.Setenv("SQL_DSN", "postgres://tk_sotc12_s1a:tk_sotc12_s1a_local@127.0.0.1:15433/tk_sotc12_s1a_startup?sslmode=disable")
			case "mysql":
				t.Setenv("SQL_DSN", "tk_sotc12:tk_sotc12_local@tcp(127.0.0.1:13312)/tk_sotc12_s1a_startup?charset=utf8mb4&parseTime=True&loc=UTC")
			}
			require.NoError(t, model.InitDB(), "实际master首次启动")
			first, err := model.DB.DB()
			require.NoError(t, err)
			require.NoError(t, first.Close())
			require.NoError(t, model.InitDB(), "实际master二次启动")
			require.NoError(t, migrations.Verify(model.DB))
			second, err := model.DB.DB()
			require.NoError(t, err)
			require.NoError(t, second.Close())
			common.IsMasterNode = false
			require.NoError(t, model.InitDB(), "非master不得代建，只核正式目标schema")
			third, err := model.DB.DB()
			require.NoError(t, err)
			require.NoError(t, third.Close())
		})
	}
}

func TestFormalMigrationRejectsWrongPartialSchema(t *testing.T) {
	for _, malformed := range []string{
		"ALTER TABLE tokens ADD COLUMN purpose INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE tokens ADD COLUMN purpose TEXT NULL",
	} {
		t.Run(malformed, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(t.TempDir()+"/wrong.db"), &gorm.Config{})
			require.NoError(t, err)
			require.NoError(t, db.AutoMigrate(&token884fBaseline{}))
			require.NoError(t, db.Exec(malformed).Error)
			assert.Error(t, migrations.Apply(db), "部分DDL形状不符必须阻启动，不伪装恢复完成")
			assert.Error(t, migrations.Verify(db))
		})
	}
}

func TestFormalMigrationRejectsUnsupportedDialect(t *testing.T) {
	base, err := gorm.Open(sqlite.Open(t.TempDir()+"/unsupported.db"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := base.DB()
	require.NoError(t, err)
	defer sqlDB.Close()

	db, err := gorm.Open(clickhouse.New(clickhouse.Config{
		Conn:                      sqlDB,
		SkipInitializeWithVersion: true,
	}), &gorm.Config{})
	require.NoError(t, err)

	err = migrations.Apply(db)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "不支持迁移方言")
}

func TestFormalMigrationRejectsIncompleteLedger(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/incomplete-ledger.db"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&token884fBaseline{}))
	require.NoError(t, db.Exec(`CREATE TABLE schema_migrations (
		migration_id TEXT PRIMARY KEY NOT NULL,
		checksum TEXT NOT NULL,
		created_time INTEGER NOT NULL
	)`).Error)

	err = migrations.Verify(db)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "未完成")
}

func TestFormalMigrationRejectsUniqueIndexWithWrongColumns(t *testing.T) {
	tests := []struct {
		name       string
		indexName  string
		table      string
		wrongIndex string
		want       string
	}{
		{
			name:       "token external lease index",
			indexName:  "idx_tokens_external_lease_id",
			table:      "tokens",
			wrongIndex: "purpose",
			want:       "唯一索引列错误",
		},
		{
			name:       "settlement lookup index",
			indexName:  "idx_audio_settlement_lookup",
			table:      "audio_request_settlements",
			wrongIndex: "external_lease_id,model_name",
			want:       "唯一索引列错误",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(t.TempDir()+"/wrong-index.db"), &gorm.Config{})
			require.NoError(t, err)
			require.NoError(t, db.AutoMigrate(&token884fBaseline{}))
			require.NoError(t, migrations.Apply(db))
			require.NoError(t, db.Exec("DROP INDEX "+tc.indexName).Error)
			require.NoError(t, db.Exec("CREATE UNIQUE INDEX "+tc.indexName+" ON "+tc.table+"("+tc.wrongIndex+")").Error)

			err = migrations.Verify(db)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestFormalMigrationDoesNotRecordLedgerAfterSchemaFailure(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/failed-ledger.db"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&token884fBaseline{}))
	require.NoError(t, db.Exec("ALTER TABLE tokens ADD COLUMN purpose INTEGER NOT NULL DEFAULT 0").Error)

	err = migrations.Apply(db)
	require.Error(t, err)

	var count int64
	if db.Migrator().HasTable("schema_migrations") {
		require.NoError(t, db.Table("schema_migrations").Count(&count).Error)
	}
	assert.Zero(t, count, "schema形状失败时不得登记已完成迁移")
}
