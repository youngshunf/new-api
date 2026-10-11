package modeltests

import (
	"net/http"
	"os"
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

// 模型组写入（options 行锁 + 事务）与 lease 改组（只读映射列的 Table 更新）在真实三库上的行为。
func TestLlmModelGroupRealDatabaseMatrix(t *testing.T) {
	if os.Getenv("SOTC12_SCHEMA_MATRIX") != "1" {
		t.Skip("独立三库验证由SOTC12_SCHEMA_MATRIX启用")
	}
	previousDB, previousType, previousRedis := model.DB, common.MainDatabaseType(), common.RedisEnabled
	common.RedisEnabled = false
	common.OptionMapRWMutex.Lock()
	if common.OptionMap == nil {
		common.OptionMap = map[string]string{}
	}
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		model.DB = previousDB
		common.SetMainDatabaseType(previousType)
		common.RedisEnabled = previousRedis
	})
	for _, dialect := range []string{"sqlite", "postgres", "mysql"} {
		t.Run(dialect, func(t *testing.T) {
			var connection gorm.Dialector
			databaseType := common.DatabaseTypeSQLite
			switch dialect {
			case "sqlite":
				connection = sqlite.Open(t.TempDir() + "/model-group.db")
			case "postgres":
				connection = postgres.Open("host=127.0.0.1 port=15433 user=tk_sotc12_s1a password=tk_sotc12_s1a_local dbname=tk_sotc12_s1a sslmode=disable")
				databaseType = common.DatabaseTypePostgreSQL
			case "mysql":
				connection = mysql.Open("tk_sotc12:tk_sotc12_local@tcp(127.0.0.1:13312)/tk_sotc12_s1a?charset=utf8mb4&parseTime=True&loc=UTC")
				databaseType = common.DatabaseTypeMySQL
			}
			db, err := gorm.Open(connection, &gorm.Config{})
			require.NoError(t, err)
			require.NoError(t, db.Migrator().DropTable("audio_request_settlements", "schema_migrations", "tokens", "options"))
			require.NoError(t, db.AutoMigrate(&model.Token{}, &model.User{}, &model.Option{}))
			require.NoError(t, migrations.Apply(db))
			model.DB = db
			common.SetMainDatabaseType(databaseType)
			require.NoError(t, db.Where("id = ?", 140001).Delete(&model.User{}).Error)
			require.NoError(t, db.Create(&model.User{Id: 140001, Username: "tk-model-group", AffCode: "tk-model-group", Status: common.UserStatusEnabled}).Error)

			stored, putErr := model.PutLlmModelGroup("default", 2, []string{"gpt-4o", "claude-4"})
			require.Nil(t, putErr)
			assert.Equal(t, []string{"claude-4", "gpt-4o"}, stored.Models)
			_, putErr = model.PutLlmModelGroup("default", 2, []string{"gpt-4o", "claude-4"})
			require.Nil(t, putErr, "同世代同集合幂等")
			_, putErr = model.PutLlmModelGroup("default", 1, []string{"gpt-4o"})
			require.NotNil(t, putErr)
			assert.Equal(t, http.StatusConflict, putErr.HTTPStatus)
			assert.Equal(t, model.LlmModelGroupErrorStaleRevision, putErr.Code)
			_, putErr = model.PutLlmModelGroup("default", 3, []string{"gpt-4o"})
			require.Nil(t, putErr)
			var option model.Option
			require.NoError(t, db.Where(&model.Option{Key: model.HasnLlmModelGroupsOptionKey}).First(&option).Error)
			assert.JSONEq(t, `{"default":{"publication_revision":3,"models":["gpt-4o"]}}`, option.Value)
			assert.Equal(t, map[string]bool{"gpt-4o": true}, model.LlmModelGroupModelSet("default"))

			spec := model.RelayLeaseSpec{
				ExternalLeaseId: "tk.model-group." + dialect, NewApiUserId: 140001, CredentialGeneration: 1,
				ModelGroup: "default", ModelLimits: []string{}, ExpiredTime: time.Now().Add(time.Hour).Unix(),
				Purpose: "generic", AccountingMode: "legacy",
			}
			issued, leaseErr := model.UpsertRelayLease(spec)
			require.Nil(t, leaseErr)
			spec.ModelGroup = "pro"
			spec.CredentialGeneration++
			_, leaseErr = model.UpsertRelayLease(spec)
			require.Nil(t, leaseErr)
			var group string
			require.NoError(t, db.Table("tokens").Where("id = ?", issued.Token.Id).Pluck("model_group", &group).Error)
			assert.Equal(t, "pro", group, "改组必须真实落库，而不是被只读映射静默丢弃")

			require.NoError(t, model.UpdateOption(model.HasnLlmModelGroupsOptionKey, ""))
		})
	}
}
