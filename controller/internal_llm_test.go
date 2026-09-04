package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// ---------------------------------------------------------------------------
// 库存快照：source_revision 必须是「内容的函数」
// ---------------------------------------------------------------------------

func samplePricing() []model.Pricing {
	cacheRatio := 0.25
	return []model.Pricing{
		{
			ModelName:              "gpt-4o",
			EnableGroup:            []string{"vip", "default"},
			SupportedEndpointTypes: []constant.EndpointType{constant.EndpointTypeOpenAI},
			VendorID:               2,
			Description:            "旗舰对话模型",
			Tags:                   "chat",
			QuotaType:              0,
			ModelRatio:             2.5,
			CompletionRatio:        3,
			CacheRatio:             &cacheRatio,
		},
		{
			ModelName:              "dall-e-3",
			EnableGroup:            []string{"default"},
			SupportedEndpointTypes: []constant.EndpointType{constant.EndpointTypeImageGeneration},
			VendorID:               1,
			QuotaType:              1,
			ModelPrice:             0.04,
			BillingMode:            "tiered_expr",
			BillingExpr:            "size == '1024x1024' ? 1 : 2",
		},
	}
}

func sampleVendors() []model.PricingVendor {
	return []model.PricingVendor{
		{ID: 2, Name: "OpenAI", Description: "官方"},
		{ID: 1, Name: "Azure"},
	}
}

func sampleEndpoints() map[string]common.EndpointInfo {
	return map[string]common.EndpointInfo{
		"openai":           {Path: "/v1/chat/completions", Method: "POST"},
		"image_generation": {Path: "/v1/images/generations", Method: "POST"},
	}
}

func buildSampleInventory(t *testing.T, pricing []model.Pricing, vendors []model.PricingVendor,
	endpoints map[string]common.EndpointInfo, groupRatio map[string]float64) *dto.LlmModelInventory {
	t.Helper()
	inventory, err := buildModelInventory(pricing, vendors, endpoints, groupRatio, time.Unix(1700000000, 0))
	require.NoError(t, err)
	return inventory
}

// 同一份库存，无论上游以什么顺序交出来，都必须算出同一个 source_revision。
//
// 这不是排版问题：分组来自集合、端点来自 map，迭代顺序天然不稳定。若摘要跟着顺序变，
// Reconciler 会把每一次轮询都判成「库存变了」，于是每轮都重写整张 model_inventory。
func TestModelInventoryRevisionIgnoresUpstreamOrdering(t *testing.T) {
	first := buildSampleInventory(t, samplePricing(), sampleVendors(), sampleEndpoints(),
		map[string]float64{"default": 1, "vip": 0.8})

	shuffled := samplePricing()
	shuffled[0], shuffled[1] = shuffled[1], shuffled[0]
	shuffled[1].EnableGroup = []string{"default", "vip"} // 同一个集合，另一种迭代顺序
	shuffledVendors := []model.PricingVendor{{ID: 1, Name: "Azure"}, {ID: 2, Name: "OpenAI", Description: "官方"}}
	second := buildSampleInventory(t, shuffled, shuffledVendors, sampleEndpoints(),
		map[string]float64{"vip": 0.8, "default": 1})

	assert.Equal(t, first.SourceRevision, second.SourceRevision)
	require.Len(t, first.Models, 2)
	assert.Equal(t, []string{"dall-e-3", "gpt-4o"}, []string{first.Models[0].ModelName, first.Models[1].ModelName})
	assert.Equal(t, []string{"default", "vip"}, first.Models[1].EnableGroups)
}

// measured_at 不进摘要：库存没变就必须拿到同一个 revision，哪怕两次读取相隔很久。
func TestModelInventoryRevisionExcludesMeasuredAt(t *testing.T) {
	early, err := buildModelInventory(samplePricing(), sampleVendors(), sampleEndpoints(), nil, time.Unix(1700000000, 0))
	require.NoError(t, err)
	late, err := buildModelInventory(samplePricing(), sampleVendors(), sampleEndpoints(), nil, time.Unix(1800000000, 0))
	require.NoError(t, err)

	assert.Equal(t, early.SourceRevision, late.SourceRevision)
	assert.NotEqual(t, early.MeasuredAt, late.MeasuredAt)
	assert.Equal(t, "2023-11-14T22:13:20Z", early.MeasuredAt)
}

// 每一条被平台消费的事实，改动之后 source_revision 必须变。
//
// 这条用例真正防的是「字段没被搬进出参」：某个计费事实忘了映射，它对应的那一行当场变红，
// 因为改了它却算出同一个摘要。
func TestModelInventoryRevisionTracksEveryPublishedFact(t *testing.T) {
	baseline := buildSampleInventory(t, samplePricing(), sampleVendors(), sampleEndpoints(),
		map[string]float64{"default": 1})

	cases := []struct {
		name   string
		mutate func(pricing []model.Pricing, vendors []model.PricingVendor, endpoints map[string]common.EndpointInfo, groupRatio map[string]float64)
	}{
		{"model_name", func(p []model.Pricing, _ []model.PricingVendor, _ map[string]common.EndpointInfo, _ map[string]float64) {
			p[0].ModelName = "gpt-4o-mini"
		}},
		{"enable_groups", func(p []model.Pricing, _ []model.PricingVendor, _ map[string]common.EndpointInfo, _ map[string]float64) {
			p[0].EnableGroup = []string{"default"}
		}},
		{"supported_endpoint_types", func(p []model.Pricing, _ []model.PricingVendor, _ map[string]common.EndpointInfo, _ map[string]float64) {
			p[0].SupportedEndpointTypes = []constant.EndpointType{constant.EndpointTypeOpenAIResponse}
		}},
		{"vendor_id", func(p []model.Pricing, _ []model.PricingVendor, _ map[string]common.EndpointInfo, _ map[string]float64) {
			p[0].VendorID = 9
		}},
		{"description", func(p []model.Pricing, _ []model.PricingVendor, _ map[string]common.EndpointInfo, _ map[string]float64) {
			p[0].Description = "改过的描述"
		}},
		{"tags", func(p []model.Pricing, _ []model.PricingVendor, _ map[string]common.EndpointInfo, _ map[string]float64) {
			p[0].Tags = "chat,vision"
		}},
		{"quota_type", func(p []model.Pricing, _ []model.PricingVendor, _ map[string]common.EndpointInfo, _ map[string]float64) {
			p[0].QuotaType = 1
		}},
		{"model_ratio", func(p []model.Pricing, _ []model.PricingVendor, _ map[string]common.EndpointInfo, _ map[string]float64) {
			p[0].ModelRatio = 5
		}},
		{"completion_ratio", func(p []model.Pricing, _ []model.PricingVendor, _ map[string]common.EndpointInfo, _ map[string]float64) {
			p[0].CompletionRatio = 4
		}},
		{"cache_ratio", func(p []model.Pricing, _ []model.PricingVendor, _ map[string]common.EndpointInfo, _ map[string]float64) {
			changed := 0.5
			p[0].CacheRatio = &changed
		}},
		{"create_cache_ratio", func(p []model.Pricing, _ []model.PricingVendor, _ map[string]common.EndpointInfo, _ map[string]float64) {
			changed := 1.25
			p[0].CreateCacheRatio = &changed
		}},
		{"image_ratio", func(p []model.Pricing, _ []model.PricingVendor, _ map[string]common.EndpointInfo, _ map[string]float64) {
			changed := 2.0
			p[0].ImageRatio = &changed
		}},
		{"audio_ratio", func(p []model.Pricing, _ []model.PricingVendor, _ map[string]common.EndpointInfo, _ map[string]float64) {
			changed := 3.0
			p[0].AudioRatio = &changed
		}},
		{"audio_completion_ratio", func(p []model.Pricing, _ []model.PricingVendor, _ map[string]common.EndpointInfo, _ map[string]float64) {
			changed := 4.0
			p[0].AudioCompletionRatio = &changed
		}},
		{"model_price", func(p []model.Pricing, _ []model.PricingVendor, _ map[string]common.EndpointInfo, _ map[string]float64) {
			p[1].ModelPrice = 0.08
		}},
		{"billing_mode", func(p []model.Pricing, _ []model.PricingVendor, _ map[string]common.EndpointInfo, _ map[string]float64) {
			p[1].BillingMode = ""
		}},
		{"billing_expr", func(p []model.Pricing, _ []model.PricingVendor, _ map[string]common.EndpointInfo, _ map[string]float64) {
			p[1].BillingExpr = "1"
		}},
		{"vendor_name", func(_ []model.Pricing, v []model.PricingVendor, _ map[string]common.EndpointInfo, _ map[string]float64) {
			v[0].Name = "OpenAI 改名"
		}},
		{"vendor_description", func(_ []model.Pricing, v []model.PricingVendor, _ map[string]common.EndpointInfo, _ map[string]float64) {
			v[0].Description = "改过的供应商说明"
		}},
		{"supported_endpoint", func(_ []model.Pricing, _ []model.PricingVendor, e map[string]common.EndpointInfo, _ map[string]float64) {
			e["openai"] = common.EndpointInfo{Path: "/v2/chat/completions", Method: "POST"}
		}},
		{"group_ratio", func(_ []model.Pricing, _ []model.PricingVendor, _ map[string]common.EndpointInfo, g map[string]float64) {
			g["default"] = 1.5
		}},
		{"model_removed", func(p []model.Pricing, _ []model.PricingVendor, _ map[string]common.EndpointInfo, _ map[string]float64) {
			p[1] = p[0]
		}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			pricing, vendors := samplePricing(), sampleVendors()
			endpoints, groupRatio := sampleEndpoints(), map[string]float64{"default": 1}
			testCase.mutate(pricing, vendors, endpoints, groupRatio)
			mutated := buildSampleInventory(t, pricing, vendors, endpoints, groupRatio)
			assert.NotEqual(t, baseline.SourceRevision, mutated.SourceRevision,
				"改了 %s 却算出同一个 source_revision，说明这个事实没有进入快照", testCase.name)
		})
	}
}

// 库存是**完整**库存：不按可用分组裁剪，也不丢掉当前登录用户看不到的模型。
// /api/pricing 那条要裁剪，因为它回答「这个人能看到什么」；本接口回答「这台机器上有什么」。
func TestModelInventoryKeepsGroupsThatNoSingleUserCouldSee(t *testing.T) {
	inventory := buildSampleInventory(t, samplePricing(), sampleVendors(), sampleEndpoints(), nil)

	require.Len(t, inventory.Models, 2)
	byName := map[string]dto.LlmModelInventoryEntry{}
	for _, entry := range inventory.Models {
		byName[entry.ModelName] = entry
	}
	assert.Equal(t, []string{"default", "vip"}, byName["gpt-4o"].EnableGroups)
	assert.Equal(t, []string{"default"}, byName["dall-e-3"].EnableGroups)
	assert.Equal(t, map[string]float64{}, inventory.GroupRatio)
}

// pricing 是 model 包的共享缓存，就地排序会改到别人的数据。
func TestModelInventoryDoesNotMutateSharedPricingSlices(t *testing.T) {
	pricing := samplePricing()
	sharedGroups := pricing[0].EnableGroup
	endpoints := sampleEndpoints()

	buildSampleInventory(t, pricing, sampleVendors(), endpoints, nil)

	assert.Equal(t, []string{"vip", "default"}, sharedGroups, "共享的 EnableGroup 被就地排序了")
	assert.Equal(t, "gpt-4o", pricing[0].ModelName, "共享的 pricing 切片被就地重排了")
	assert.Len(t, endpoints, 2)
}

// ---------------------------------------------------------------------------
// Relay lease：幂等签发与撤销
// ---------------------------------------------------------------------------

const (
	leaseTestAccountId      = 7
	leaseTestOtherAccountId = 8
	leaseTestId             = "01j9lease.demo-a"
)

func setupRelayLeaseTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousRedis := common.RedisEnabled
	previousMain, previousLog := common.MainDatabaseType(), common.LogDatabaseType()
	gin.SetMode(gin.TestMode)
	common.RedisEnabled = false
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)

	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	model.DB, model.LOG_DB = db, db
	require.NoError(t, db.AutoMigrate(&model.Token{}, &model.User{}))
	// aff_code 上有唯一索引，两个账户必须给不同的值，否则第二条种子数据插不进去。
	require.NoError(t, db.Create(&model.User{
		Id: leaseTestAccountId, Username: "workspace-a", AffCode: "aff-a", Status: common.UserStatusEnabled,
	}).Error)
	require.NoError(t, db.Create(&model.User{
		Id: leaseTestOtherAccountId, Username: "workspace-b", AffCode: "aff-b", Status: common.UserStatusEnabled,
	}).Error)

	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.RedisEnabled = previousRedis
		common.SetDatabaseTypes(previousMain, previousLog)
		if sqlDB, dbErr := db.DB(); dbErr == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

func newRelayLeaseEngine() *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.PUT("/api/internal/v1/llm/relay-leases/:external_lease_id", PutInternalRelayLease)
	engine.DELETE("/api/internal/v1/llm/relay-leases/:external_lease_id", DeleteInternalRelayLease)
	return engine
}

func callRelayLease(t *testing.T, engine *gin.Engine, method string, leaseId string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, "/api/internal/v1/llm/relay-leases/"+leaseId, reader)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	return recorder
}

func leaseRequestBody(t *testing.T, accountId int, generation int64, models []string, expiresTime time.Time) string {
	t.Helper()
	payload, err := common.Marshal(dto.LlmRelayLeaseRequest{
		NewApiUserId:         accountId,
		CredentialGeneration: generation,
		ModelLimits:          models,
		ExpiresTime:          expiresTime.UTC().Format(time.RFC3339),
	})
	require.NoError(t, err)
	return string(payload)
}

func decodeLeaseView(t *testing.T, recorder *httptest.ResponseRecorder) dto.LlmRelayLeaseView {
	t.Helper()
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var view dto.LlmRelayLeaseView
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &view))
	return view
}

func decodeInternalError(t *testing.T, recorder *httptest.ResponseRecorder) dto.InternalErrorResponse {
	t.Helper()
	var failure dto.InternalErrorResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &failure))
	return failure
}

func countLeaseTokens(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var count int64
	require.NoError(t, db.Model(&model.Token{}).Count(&count).Error)
	return count
}

func TestPutRelayLeaseIssuesUnderTheGivenAccount(t *testing.T) {
	db := setupRelayLeaseTestDB(t)
	engine := newRelayLeaseEngine()
	expires := time.Now().Add(2 * time.Hour).Truncate(time.Second)

	recorder := callRelayLease(t, engine, http.MethodPut, leaseTestId,
		leaseRequestBody(t, leaseTestAccountId, 1, []string{"gpt-4o", "dall-e-3"}, expires))
	view := decodeLeaseView(t, recorder)

	assert.True(t, view.Created)
	assert.False(t, view.Rotated)
	assert.Equal(t, leaseTestId, view.ExternalLeaseId)
	assert.Equal(t, leaseTestAccountId, view.NewApiUserId)
	assert.Equal(t, int64(1), view.CredentialGeneration)
	assert.Equal(t, []string{"gpt-4o", "dall-e-3"}, view.ModelLimits)
	assert.Equal(t, expires.UTC().Format(time.RFC3339), view.ExpiresTime)
	assert.True(t, strings.HasPrefix(view.RelayToken, "sk-"))
	assert.True(t, strings.HasPrefix(view.RelayToken, view.TokenPrefix))
	assert.Len(t, view.TokenPrefix, 12)
	assert.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))

	stored, err := model.GetTokenById(view.NewApiTokenId)
	require.NoError(t, err)
	assert.Equal(t, leaseTestAccountId, stored.UserId)
	assert.Equal(t, "hasn-lease:"+leaseTestId+":g1", stored.Name)
	assert.Equal(t, "sk-"+stored.Key, view.RelayToken)
	assert.True(t, stored.ModelLimitsEnabled)
	assert.Equal(t, "gpt-4o,dall-e-3", stored.ModelLimits)
	// lease 不建立第二套积分余额：额度从发起空间的账户扣（设计 §10.1）。
	assert.True(t, stored.UnlimitedQuota)
	assert.Equal(t, expires.Unix(), stored.ExpiredTime)
	assert.Equal(t, int64(1), countLeaseTokens(t, db))
}

func TestPutRelayLeaseReplayReturnsTheSameToken(t *testing.T) {
	db := setupRelayLeaseTestDB(t)
	engine := newRelayLeaseEngine()
	expires := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	body := leaseRequestBody(t, leaseTestAccountId, 1, []string{"gpt-4o"}, expires)

	first := decodeLeaseView(t, callRelayLease(t, engine, http.MethodPut, leaseTestId, body))
	second := decodeLeaseView(t, callRelayLease(t, engine, http.MethodPut, leaseTestId, body))

	assert.Equal(t, first.NewApiTokenId, second.NewApiTokenId)
	assert.Equal(t, first.RelayToken, second.RelayToken)
	assert.False(t, second.Created)
	assert.False(t, second.Rotated)
	assert.Equal(t, int64(1), countLeaseTokens(t, db), "同一个幂等键必须只对应一枚 Token")
}

// 同键、不同 body：白名单与期限被就地更新，Token 本身不变——PUT 是 upsert，不是「只创建一次」。
func TestPutRelayLeaseSameKeyDifferentBodyUpdatesInPlace(t *testing.T) {
	db := setupRelayLeaseTestDB(t)
	engine := newRelayLeaseEngine()
	firstExpires := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	secondExpires := time.Now().Add(48 * time.Hour).Truncate(time.Second)

	first := decodeLeaseView(t, callRelayLease(t, engine, http.MethodPut, leaseTestId,
		leaseRequestBody(t, leaseTestAccountId, 1, []string{"gpt-4o"}, firstExpires)))
	second := decodeLeaseView(t, callRelayLease(t, engine, http.MethodPut, leaseTestId,
		leaseRequestBody(t, leaseTestAccountId, 1, []string{"claude-4", "gpt-4o"}, secondExpires)))

	assert.Equal(t, first.NewApiTokenId, second.NewApiTokenId)
	assert.Equal(t, first.RelayToken, second.RelayToken, "同代重投不换 Key")
	assert.False(t, second.Created)
	assert.False(t, second.Rotated)
	assert.Equal(t, []string{"claude-4", "gpt-4o"}, second.ModelLimits)

	stored, err := model.GetTokenById(second.NewApiTokenId)
	require.NoError(t, err)
	assert.Equal(t, "claude-4,gpt-4o", stored.ModelLimits)
	assert.Equal(t, secondExpires.Unix(), stored.ExpiredTime)
	assert.Equal(t, int64(1), countLeaseTokens(t, db))
}

// 代号更大才轮换：换 Key、旧明文当场作废，Token 行与幂等键都不变。
func TestPutRelayLeaseRotatesOnHigherGeneration(t *testing.T) {
	db := setupRelayLeaseTestDB(t)
	engine := newRelayLeaseEngine()
	expires := time.Now().Add(2 * time.Hour).Truncate(time.Second)

	first := decodeLeaseView(t, callRelayLease(t, engine, http.MethodPut, leaseTestId,
		leaseRequestBody(t, leaseTestAccountId, 1, []string{"gpt-4o"}, expires)))
	rotated := decodeLeaseView(t, callRelayLease(t, engine, http.MethodPut, leaseTestId,
		leaseRequestBody(t, leaseTestAccountId, 2, []string{"gpt-4o"}, expires)))

	assert.True(t, rotated.Rotated)
	assert.False(t, rotated.Created)
	assert.Equal(t, first.NewApiTokenId, rotated.NewApiTokenId)
	assert.Equal(t, int64(2), rotated.CredentialGeneration)
	assert.NotEqual(t, first.RelayToken, rotated.RelayToken)

	stored, err := model.GetTokenById(rotated.NewApiTokenId)
	require.NoError(t, err)
	assert.Equal(t, "hasn-lease:"+leaseTestId+":g2", stored.Name)

	_, err = model.GetTokenByKey(strings.TrimPrefix(first.RelayToken, "sk-"), true)
	assert.Error(t, err, "轮换之后旧明文必须当场作废")
	assert.Equal(t, int64(1), countLeaseTokens(t, db))
}

func TestPutRelayLeaseRejectsStaleGeneration(t *testing.T) {
	setupRelayLeaseTestDB(t)
	engine := newRelayLeaseEngine()
	expires := time.Now().Add(2 * time.Hour).Truncate(time.Second)

	current := decodeLeaseView(t, callRelayLease(t, engine, http.MethodPut, leaseTestId,
		leaseRequestBody(t, leaseTestAccountId, 3, []string{"gpt-4o"}, expires)))
	recorder := callRelayLease(t, engine, http.MethodPut, leaseTestId,
		leaseRequestBody(t, leaseTestAccountId, 2, []string{"gpt-4o"}, expires))

	require.Equal(t, http.StatusConflict, recorder.Code)
	assert.Equal(t, model.RelayLeaseErrorStaleGeneration, decodeInternalError(t, recorder).Code)
	assert.False(t, decodeInternalError(t, recorder).Retryable)

	stored, err := model.GetTokenById(current.NewApiTokenId)
	require.NoError(t, err)
	assert.Equal(t, "sk-"+stored.Key, current.RelayToken, "过期请求不得把已经轮换过的凭据倒回去")
}

// lease 换账户等于换扣费主体，必须是冲突而不是静默再签一枚（设计 §10.1）。
func TestPutRelayLeaseRejectsAnotherAccount(t *testing.T) {
	db := setupRelayLeaseTestDB(t)
	engine := newRelayLeaseEngine()
	expires := time.Now().Add(2 * time.Hour).Truncate(time.Second)

	decodeLeaseView(t, callRelayLease(t, engine, http.MethodPut, leaseTestId,
		leaseRequestBody(t, leaseTestAccountId, 1, []string{"gpt-4o"}, expires)))
	recorder := callRelayLease(t, engine, http.MethodPut, leaseTestId,
		leaseRequestBody(t, leaseTestOtherAccountId, 1, []string{"gpt-4o"}, expires))

	require.Equal(t, http.StatusConflict, recorder.Code)
	assert.Equal(t, model.RelayLeaseErrorAccountMismatch, decodeInternalError(t, recorder).Code)
	assert.Equal(t, int64(1), countLeaseTokens(t, db))
}

// 这条通道绝不开账户：账户不存在就 404，且不留下任何痕迹（设计 §15.1）。
func TestPutRelayLeaseNeverProvisionsTheAccount(t *testing.T) {
	db := setupRelayLeaseTestDB(t)
	engine := newRelayLeaseEngine()

	recorder := callRelayLease(t, engine, http.MethodPut, leaseTestId,
		leaseRequestBody(t, 4242, 1, []string{"gpt-4o"}, time.Now().Add(time.Hour)))

	require.Equal(t, http.StatusNotFound, recorder.Code)
	assert.Equal(t, model.RelayLeaseErrorAccountNotFound, decodeInternalError(t, recorder).Code)
	assert.Equal(t, int64(0), countLeaseTokens(t, db))

	var users int64
	require.NoError(t, db.Model(&model.User{}).Count(&users).Error)
	assert.Equal(t, int64(2), users, "账户必须由 account 通道创建，这里一个都不能多")
}

func TestPutRelayLeaseRejectsInvalidRequests(t *testing.T) {
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	cases := []struct {
		name    string
		leaseId string
		body    string
	}{
		{"uppercase lease id", "01J9LEASE", `{"newapi_user_id":7,"credential_generation":1,"model_limits":["gpt-4o"],"expires_time":"` + future + `"}`},
		{"lease id with underscore", "lease_a", `{"newapi_user_id":7,"credential_generation":1,"model_limits":["gpt-4o"],"expires_time":"` + future + `"}`},
		{"lease id with percent", "lease%25a", `{"newapi_user_id":7,"credential_generation":1,"model_limits":["gpt-4o"],"expires_time":"` + future + `"}`},
		{"lease id starting with dot", ".lease", `{"newapi_user_id":7,"credential_generation":1,"model_limits":["gpt-4o"],"expires_time":"` + future + `"}`},
		{"unknown field", leaseTestId, `{"newapi_user_id":7,"credential_generation":1,"model_limits":["gpt-4o"],"expires_time":"` + future + `","rotate":true}`},
		{"idempotency key duplicated in body", leaseTestId, `{"newapi_user_id":7,"credential_generation":1,"model_limits":["gpt-4o"],"expires_time":"` + future + `","external_lease_id":"x"}`},
		{"zero account", leaseTestId, `{"newapi_user_id":0,"credential_generation":1,"model_limits":["gpt-4o"],"expires_time":"` + future + `"}`},
		{"zero generation", leaseTestId, `{"newapi_user_id":7,"credential_generation":0,"model_limits":["gpt-4o"],"expires_time":"` + future + `"}`},
		{"empty model limits", leaseTestId, `{"newapi_user_id":7,"credential_generation":1,"model_limits":[],"expires_time":"` + future + `"}`},
		{"missing model limits", leaseTestId, `{"newapi_user_id":7,"credential_generation":1,"expires_time":"` + future + `"}`},
		{"model name with comma", leaseTestId, `{"newapi_user_id":7,"credential_generation":1,"model_limits":["gpt-4o,dall-e-3"],"expires_time":"` + future + `"}`},
		{"duplicate model name", leaseTestId, `{"newapi_user_id":7,"credential_generation":1,"model_limits":["gpt-4o","gpt-4o"],"expires_time":"` + future + `"}`},
		{"model name with whitespace", leaseTestId, `{"newapi_user_id":7,"credential_generation":1,"model_limits":[" gpt-4o"],"expires_time":"` + future + `"}`},
		{"expires time in the past", leaseTestId, `{"newapi_user_id":7,"credential_generation":1,"model_limits":["gpt-4o"],"expires_time":"2020-01-01T00:00:00Z"}`},
		{"expires time not rfc3339", leaseTestId, `{"newapi_user_id":7,"credential_generation":1,"model_limits":["gpt-4o"],"expires_time":"永远"}`},
		{"missing expires time", leaseTestId, `{"newapi_user_id":7,"credential_generation":1,"model_limits":["gpt-4o"]}`},
		{"body is not a json object", leaseTestId, `[]`},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			db := setupRelayLeaseTestDB(t)
			engine := newRelayLeaseEngine()

			recorder := callRelayLease(t, engine, http.MethodPut, testCase.leaseId, testCase.body)

			require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
			assert.Equal(t, model.RelayLeaseErrorInvalidRequest, decodeInternalError(t, recorder).Code)
			assert.Equal(t, int64(0), countLeaseTokens(t, db), "被拒的请求不得留下 Token")
		})
	}
}

// 名字长得像 lease、但不是本通道签发的 Token，绝不能被当成 lease 拿去改写。
func TestPutRelayLeaseIgnoresLookalikeTokenNames(t *testing.T) {
	db := setupRelayLeaseTestDB(t)
	engine := newRelayLeaseEngine()
	lookalike := &model.Token{
		UserId:       leaseTestOtherAccountId,
		Name:         "hasn-lease:" + leaseTestId + ":gnot-a-number",
		Key:          strings.Repeat("k", 48),
		Status:       common.TokenStatusEnabled,
		CreatedTime:  1,
		AccessedTime: 1,
		ExpiredTime:  -1,
	}
	require.NoError(t, db.Create(lookalike).Error)

	view := decodeLeaseView(t, callRelayLease(t, engine, http.MethodPut, leaseTestId,
		leaseRequestBody(t, leaseTestAccountId, 1, []string{"gpt-4o"}, time.Now().Add(time.Hour))))

	assert.True(t, view.Created)
	assert.NotEqual(t, lookalike.Id, view.NewApiTokenId)

	untouched, err := model.GetTokenById(lookalike.Id)
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("k", 48), untouched.Key)
	assert.Equal(t, leaseTestOtherAccountId, untouched.UserId)
}

func TestDeleteRelayLeaseIsIdempotent(t *testing.T) {
	setupRelayLeaseTestDB(t)
	engine := newRelayLeaseEngine()

	issued := decodeLeaseView(t, callRelayLease(t, engine, http.MethodPut, leaseTestId,
		leaseRequestBody(t, leaseTestAccountId, 1, []string{"gpt-4o"}, time.Now().Add(time.Hour))))

	firstDelete := callRelayLease(t, engine, http.MethodDelete, leaseTestId, "")
	require.Equal(t, http.StatusOK, firstDelete.Code)
	var revocation dto.LlmRelayLeaseRevocation
	require.NoError(t, common.Unmarshal(firstDelete.Body.Bytes(), &revocation))
	assert.True(t, revocation.Revoked)
	require.NotNil(t, revocation.NewApiTokenId)
	assert.Equal(t, issued.NewApiTokenId, *revocation.NewApiTokenId)
	assert.Equal(t, "no-store", firstDelete.Header().Get("Cache-Control"))

	_, err := model.GetTokenByKey(strings.TrimPrefix(issued.RelayToken, "sk-"), true)
	assert.Error(t, err, "撤销之后明文必须失效")

	secondDelete := callRelayLease(t, engine, http.MethodDelete, leaseTestId, "")
	require.Equal(t, http.StatusOK, secondDelete.Code)
	require.NoError(t, common.Unmarshal(secondDelete.Body.Bytes(), &revocation))
	assert.False(t, revocation.Revoked, "重复撤销是 200 且 revoked=false，不是错误")
	assert.Nil(t, revocation.NewApiTokenId)
}

func TestDeleteRelayLeaseRejectsInvalidLeaseId(t *testing.T) {
	setupRelayLeaseTestDB(t)
	engine := newRelayLeaseEngine()

	recorder := callRelayLease(t, engine, http.MethodDelete, "LEASE_A", "")

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	assert.Equal(t, model.RelayLeaseErrorInvalidRequest, decodeInternalError(t, recorder).Code)
}

// ---------------------------------------------------------------------------
// 库存端点：复用 /api/pricing 的聚合，并拒绝交出空快照
// ---------------------------------------------------------------------------

func setupModelInventoryTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousRedis := common.RedisEnabled
	previousMain, previousLog := common.MainDatabaseType(), common.LogDatabaseType()
	gin.SetMode(gin.TestMode)
	common.RedisEnabled = false
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)

	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	model.DB, model.LOG_DB = db, db
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Channel{}, &model.Ability{}, &model.Model{}, &model.Vendor{}))
	// pricingMap 是 model 包的进程级缓存，前后都要清，免得和同包别的用例互相看见对方的库存。
	model.InvalidatePricingCache()

	t.Cleanup(func() {
		model.InvalidatePricingCache()
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.RedisEnabled = previousRedis
		common.SetDatabaseTypes(previousMain, previousLog)
		if sqlDB, dbErr := db.DB(); dbErr == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

func callModelInventory(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	engine := gin.New()
	engine.GET("/api/internal/v1/llm/model-inventory", GetInternalModelInventory)
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/internal/v1/llm/model-inventory", nil))
	return recorder
}

// 空库存不是一份合法快照：Reconciler 的规则是「一次完整同步成功才允许 upsert 和标记 missing」，
// 把空数组当 200 交出去，它会把全部模型标成 missing（设计 §5.1）。
func TestGetModelInventoryRefusesToPublishAnEmptySnapshot(t *testing.T) {
	setupModelInventoryTestDB(t)

	recorder := callModelInventory(t)

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code, recorder.Body.String())
	failure := decodeInternalError(t, recorder)
	assert.Equal(t, "inventory_unavailable", failure.Code)
	assert.True(t, failure.Retryable, "空库存多半是一次读取故障，平台应当重试而不是照单执行")
}

// 库存事实只有一个产生方：这一条走的就是 /api/pricing 那套聚合（abilities × channels × models）。
func TestGetModelInventoryReusesThePricingAggregation(t *testing.T) {
	db := setupModelInventoryTestDB(t)
	require.NoError(t, db.Create(&model.Channel{
		Id: 1, Type: constant.ChannelTypeOpenAI, Key: "key-1", Status: common.ChannelStatusEnabled,
		Name: "ch-1", Group: "default", Models: "gpt-4o-test",
	}).Error)
	require.NoError(t, db.Create(&model.Ability{
		Group: "vip", Model: "gpt-4o-test", ChannelId: 1, Enabled: true,
	}).Error)
	require.NoError(t, db.Create(&model.Ability{
		Group: "default", Model: "gpt-4o-test", ChannelId: 1, Enabled: true,
	}).Error)

	recorder := callModelInventory(t)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var inventory dto.LlmModelInventory
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &inventory))
	require.Len(t, inventory.Models, 1)
	entry := inventory.Models[0]
	assert.Equal(t, "gpt-4o-test", entry.ModelName)
	assert.Equal(t, []string{"default", "vip"}, entry.EnableGroups)
	assert.Contains(t, entry.SupportedEndpointTypes, "openai")
	assert.Len(t, inventory.SourceRevision, 64, "source_revision 是 sha256 的十六进制")
	_, err := time.Parse(time.RFC3339, inventory.MeasuredAt)
	assert.NoError(t, err)
}
