package modeltests

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestVoiceFormalMigrationCreatesResourceTable(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/voices.db"), &gorm.Config{})
	require.NoError(t, err)
	connection, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, connection.Close()) })
	require.NoError(t, db.AutoMigrate(&model.Token{}))
	require.NoError(t, migrations.Apply(db))
	require.True(t, db.Migrator().HasTable("voice_profiles"), "音色资源必须由正式迁移建立")
	require.NoError(t, migrations.Apply(db), "二次执行不得重复DDL")
	require.NoError(t, migrations.Verify(db))
	require.NoError(t, db.Exec("ALTER TABLE voice_profiles DROP COLUMN failure_reason").Error)
	require.Error(t, migrations.Verify(db), "迁移账本存在也必须核真实表结构")
	require.Error(t, migrations.Apply(db), "master不得把损坏目标表假装启动成功")
}

// 矩阵只连接固定任务隔离库；fresh、旧版本升级、二次启动和唯一约束都使用真实数据库。
func TestVoiceSchemaRealDatabaseMatrix(t *testing.T) {
	if os.Getenv("VC7_SCHEMA_MATRIX") != "1" {
		t.Skip("三方言专项由VC7_SCHEMA_MATRIX启用")
	}
	previousType := common.MainDatabaseType()
	previousDB := model.DB
	t.Cleanup(func() { common.SetMainDatabaseType(previousType); model.DB = previousDB })
	for _, dialect := range []string{"sqlite", "postgres", "mysql"} {
		t.Run(dialect, func(t *testing.T) {
			var connection gorm.Dialector
			switch dialect {
			case "sqlite":
				connection = sqlite.Open(t.TempDir() + "/voice-matrix.db")
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
			t.Cleanup(func() { require.NoError(t, pool.Close()) })
			var version string
			query := "SELECT version()"
			if dialect == "sqlite" {
				query = "SELECT sqlite_version()"
			} else {
				var database string
				query := "SELECT DATABASE()"
				if dialect == "postgres" {
					query = "SELECT current_database()"
				}
				require.NoError(t, db.Raw(query).Scan(&database).Error)
				require.Equal(t, "vc7_newapi_voice", database, "禁止清理任何既有业务库")
			}
			require.NoError(t, db.Raw(query).Scan(&version).Error)
			t.Logf("真实%s版本：%s", dialect, version)
			for _, stage := range []string{"fresh", "upgrade", "upgrade_v003"} {
				t.Run(stage, func(t *testing.T) {
					require.NoError(t, db.Migrator().DropTable("voice_profiles", "audio_request_settlements", "schema_migrations", "tokens"))
					require.NoError(t, db.AutoMigrate(&model.Token{}))
					if stage != "fresh" {
						applyVC7PreviousRelease(t, db, dialect)
						if stage == "upgrade_v003" {
							applyVC7PreviousMigration(t, db, dialect, "V003__远端音色资源.sql")
						}
						require.NoError(t, db.Table("tokens").Create(map[string]any{"id": 77001, "user_id": 77002, "key": "vc7-existing-token", "model_group": "default", "remain_quota": 73}).Error)
					}
					require.NoError(t, migrations.Apply(db))
					require.NoError(t, migrations.Apply(db))
					require.NoError(t, migrations.Verify(db))
					runRelayOperationPersistenceContract(t, db)
					if stage != "fresh" {
						var remaining int
						require.NoError(t, db.Table("tokens").Where("id = ?", 77001).Pluck("remain_quota", &remaining).Error)
						assert.Equal(t, 73, remaining)
						var operation string
						require.NoError(t, db.Table("tokens").Where("id = ?", 77001).Pluck("operation", &operation).Error)
						assert.Empty(t, operation, "旧Runtime升级后仍省略用途")
					}
					stamp := model.DatabaseTime{Time: time.Date(2026, 10, 11, 1, 2, 3, 456000000, time.UTC)}
					row := map[string]any{"voice_ref": "01998dd2-0000-7000-8000-000000007701", "created_time": stamp, "updated_time": stamp, "revision": 1,
						"user_id": 77002, "creator_token_id": 77003, "idempotency_key_digest": strings.Repeat("a", 64), "request_digest": strings.Repeat("b", 64),
						"kind": "design", "language": "zh", "target_model": "cosyvoice-v3.5-plus", "channel_id": 77004, "state": "unknown", "retention_policy": "provider_unused_one_year"}
					require.NoError(t, db.Table("voice_profiles").Create(maps.Clone(row)).Error)
					var loaded model.VoiceProfile
					require.NoError(t, db.Where("voice_ref = ?", row["voice_ref"]).First(&loaded).Error)
					assert.True(t, loaded.CreatedTime.Equal(stamp.Time), "生成model的公共时间在真实三库逐毫秒同值")
					assert.Nil(t, loaded.ProviderVoiceId)
					assert.Nil(t, loaded.DeleteRequestedTime)
					assert.Nil(t, loaded.DeletedTime)
					row["voice_ref"] = "01998dd2-0000-7000-8000-000000007702"
					assert.Error(t, db.Table("voice_profiles").Create(maps.Clone(row)).Error, "同账号幂等键不得重复占位")
					row["user_id"] = 77005
					require.NoError(t, db.Table("voice_profiles").Create(maps.Clone(row)).Error, "不同账号独立占用相同幂等键")
					require.NoError(t, db.Migrator().DropIndex("voice_profiles", "idx_voice_creation_key"))
					require.NoError(t, db.Exec("CREATE INDEX idx_voice_creation_key ON voice_profiles(user_id, idempotency_key_digest)").Error)
					assert.Error(t, migrations.Verify(db), "同名普通索引不能冒充幂等唯一绑定")
				})
			}
			// 为table-first modelgen保留有效schema；该隔离库最终由任务出口清理。
			require.NoError(t, db.Migrator().DropTable("voice_profiles", "audio_request_settlements", "schema_migrations", "tokens"))
			require.NoError(t, db.AutoMigrate(&model.Token{}))
			require.NoError(t, migrations.Apply(db))
		})
	}
}

// hasn@92a1488ae 的正式迁移只有V001/V002；不使用新表model伪装旧发布基线。
func applyVC7PreviousRelease(t *testing.T, db *gorm.DB, dialect string) {
	t.Helper()
	for _, name := range []string{"V001__音频同步账务底座.sql", "V002__令牌模型组.sql"} {
		applyVC7PreviousMigration(t, db, dialect, name)
	}
}

func applyVC7PreviousMigration(t *testing.T, db *gorm.DB, dialect, name string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "migrations", dialect, name))
	require.NoError(t, err)
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			lines = append(lines, line)
		}
	}
	for _, statement := range strings.Split(strings.Join(lines, "\n"), ";") {
		if strings.TrimSpace(statement) != "" {
			require.NoError(t, db.Exec(statement).Error)
		}
	}
	require.NoError(t, db.Table("schema_migrations").Create(map[string]any{"migration_id": name, "checksum": fmt.Sprintf("%x", sha256.Sum256(data)), "created_time": model.DatabaseTime{Time: time.Now().UTC()}}).Error)
}
