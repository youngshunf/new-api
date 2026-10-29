package controllertests_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/migrations"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 真实 HTTP 签发后由原 TokenAuth 和模型列表消费，证明名单限制确实落库并与组求交。
func setupOrdinaryCallHTTP(t *testing.T) (*gorm.DB, *gin.Engine, *httptest.Server) {
	t.Helper()
	require.NoError(t, i18n.Init())
	previousDB, previousLog := model.DB, model.LOG_DB
	previousMain, previousLogType := common.MainDatabaseType(), common.LogDatabaseType()
	previousRedis, previousMemory := common.RedisEnabled, common.MemoryCacheEnabled
	previousMaster, previousPath := common.IsMasterNode, common.SQLitePath
	previousSelfUse, previousGin := operation_setting.SelfUseModeEnabled, gin.Mode()
	common.RedisEnabled, common.MemoryCacheEnabled = false, false
	common.IsMasterNode = false
	common.SQLitePath = filepath.Join(t.TempDir(), "ordinary-call.db")
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	operation_setting.SelfUseModeEnabled = true
	gin.SetMode(gin.TestMode)
	t.Setenv("SQL_DSN", "local")
	t.Setenv("LOG_SQL_DSN", "")
	db, err := gorm.Open(sqlite.Open(common.SQLitePath), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Token{}, &model.User{}, &model.Option{}, &model.Channel{}, &model.Ability{}, &model.Model{}, &model.Vendor{}))
	require.NoError(t, migrations.Apply(db))
	require.NoError(t, model.InitDB())
	model.LOG_DB = model.DB
	t.Cleanup(func() {
		connection, closeErr := model.DB.DB()
		require.NoError(t, closeErr)
		require.NoError(t, connection.Close())
		connection, closeErr = db.DB()
		require.NoError(t, closeErr)
		require.NoError(t, connection.Close())
		model.DB, model.LOG_DB = previousDB, previousLog
		common.SetDatabaseTypes(previousMain, previousLogType)
		common.RedisEnabled, common.MemoryCacheEnabled = previousRedis, previousMemory
		common.IsMasterNode, common.SQLitePath = previousMaster, previousPath
		operation_setting.SelfUseModeEnabled = previousSelfUse
		gin.SetMode(previousGin)
	})
	common.OptionMapRWMutex.Lock()
	if common.OptionMap == nil {
		common.OptionMap = map[string]string{}
	}
	previousGroups := common.OptionMap[model.HasnLlmModelGroupsOptionKey]
	common.OptionMapRWMutex.Unlock()
	require.NoError(t, model.UpdateOption(model.HasnLlmModelGroupsOptionKey, `{"default":{"publication_revision":1,"models":["gpt-4o","tts-1"]}}`))
	t.Cleanup(func() { require.NoError(t, model.UpdateOption(model.HasnLlmModelGroupsOptionKey, previousGroups)) })
	require.NoError(t, db.Create(&model.User{Id: 771001, Username: "vc7-call", AffCode: "vc7-call", Group: "default", Quota: 1000000, Status: common.UserStatusEnabled}).Error)
	require.NoError(t, db.Create(&model.Channel{Id: 771001, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Group: "default", Models: "gpt-4o,tts-1,whisper-1"}).Error)
	for _, name := range []string{"gpt-4o", "tts-1", "whisper-1"} {
		require.NoError(t, db.Create(&model.Ability{Group: "default", Model: name, ChannelId: 771001, Enabled: true}).Error)
	}
	engine := gin.New()
	engine.PUT("/api/internal/v1/llm/relay-leases/:external_lease_id", controller.PutInternalRelayLease)
	engine.GET("/v1/models", middleware.TokenAuth(), func(c *gin.Context) { controller.ListModels(c, constant.ChannelTypeOpenAI) })
	server := httptest.NewServer(engine)
	t.Cleanup(server.Close)
	return db, engine, server
}

// 节点令牌只绑模型组：/v1/models 恰好列出组内模型，任何名单收窄都在签发时拒绝。
func TestNodeLeaseListsExactlyItsModelGroup(t *testing.T) {
	db, _, server := setupOrdinaryCallHTTP(t)
	for _, tc := range []struct {
		name   string
		limits []string
		status int
		models []string
	}{
		{name: "node", limits: []string{}, status: http.StatusOK, models: []string{"gpt-4o", "tts-1"}},
		{name: "single-model", limits: []string{"tts-1"}, status: http.StatusBadRequest},
		{name: "outside-group", limits: []string{"whisper-1"}, status: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, marshalErr := common.Marshal(dto.LlmRelayLeaseRequest{NewApiUserId: 771001, CredentialGeneration: 1,
				ModelGroup: "default", ModelLimits: tc.limits, ExpiresTime: time.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339), Purpose: "generic", AccountingMode: "legacy"})
			require.NoError(t, marshalErr)
			request, requestErr := http.NewRequest(http.MethodPut, server.URL+"/api/internal/v1/llm/relay-leases/vc7."+tc.name, bytes.NewReader(payload))
			require.NoError(t, requestErr)
			request.Header.Set("Content-Type", "application/json")
			response, requestErr := server.Client().Do(request)
			require.NoError(t, requestErr)
			body, readErr := io.ReadAll(response.Body)
			require.NoError(t, readErr)
			require.NoError(t, response.Body.Close())
			require.Equal(t, tc.status, response.StatusCode, string(body))
			if tc.status != http.StatusOK {
				var count int64
				require.NoError(t, db.Model(&model.Token{}).Where("external_lease_id = ?", "vc7."+tc.name).Count(&count).Error)
				assert.Zero(t, count)
				return
			}
			var lease dto.LlmRelayLeaseView
			require.NoError(t, common.Unmarshal(body, &lease))
			stored, lookupErr := model.GetTokenById(lease.NewApiTokenId)
			require.NoError(t, lookupErr)
			assert.False(t, stored.ModelLimitsEnabled)
			assert.Empty(t, stored.GetModelLimits())
			request, requestErr = http.NewRequest(http.MethodGet, server.URL+"/v1/models", nil)
			require.NoError(t, requestErr)
			request.Header.Set("Authorization", "Bearer "+lease.RelayToken)
			response, requestErr = server.Client().Do(request)
			require.NoError(t, requestErr)
			body, readErr = io.ReadAll(response.Body)
			require.NoError(t, readErr)
			require.NoError(t, response.Body.Close())
			require.Equal(t, http.StatusOK, response.StatusCode, string(body))
			var listed struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			require.NoError(t, common.Unmarshal(body, &listed))
			names := make([]string, 0, len(listed.Data))
			for _, item := range listed.Data {
				names = append(names, item.ID)
			}
			assert.ElementsMatch(t, tc.models, names)
		})
	}
}
