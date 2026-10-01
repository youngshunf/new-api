package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const audioReceiptLeaseID = "lease.0199a123-4567-7000-8000-123456789abc"

func setupAudioReceiptControllerDB(t *testing.T) *gorm.DB {
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
	require.NoError(t, db.AutoMigrate(&model.AudioRequestSettlement{}))
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

func newAudioReceiptEngine() *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/api/internal/v1/llm/relay-leases/:external_lease_id/audio-request-settlements/:gateway_request_id", GetInternalAudioRequestSettlement)
	return engine
}

func callAudioReceipt(t *testing.T, engine *gin.Engine, method, leaseID, requestID string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, "/api/internal/v1/llm/relay-leases/"+leaseID+"/audio-request-settlements/"+requestID, nil)
	engine.ServeHTTP(recorder, request)
	return recorder
}

func decodeAudioReceipt(t *testing.T, recorder *httptest.ResponseRecorder) dto.AudioRequestSettlementResult {
	t.Helper()
	var result dto.AudioRequestSettlementResult
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &result))
	return result
}

func TestGetInternalAudioRequestSettlementReturnsOnlySafePendingFields(t *testing.T) {
	db := setupAudioReceiptControllerDB(t)
	receipt := &model.AudioRequestSettlement{
		AudioRequestSettlementId: "0199a123-4567-7000-8000-123456789abd",
		CreatedTime:              model.DatabaseTime{Time: time.Now().UTC()},
		UpdatedTime:              model.DatabaseTime{Time: time.Now().UTC()},
		Revision:                 1,
		ExternalLeaseId:          audioReceiptLeaseID,
		GatewayRequestId:         "20261001000000sotc12request001",
		TokenId:                  99,
		UserId:                   7,
		CredentialGeneration:     3,
		RelayMode:                1,
		ModelName:                "provider-secret-model",
		PreConsumedQuota:         12345,
		FundingPreference:        "wallet_only",
		BillingStatus:            model.AudioBillingStatusPending,
		DispatchState:            model.AudioSettlementDispatchUnknown,
	}
	require.NoError(t, db.Create(receipt).Error)

	recorder := callAudioReceipt(t, newAudioReceiptEngine(), http.MethodGet, audioReceiptLeaseID, receipt.GatewayRequestId)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	result := decodeAudioReceipt(t, recorder)
	assert.Equal(t, audioReceiptLeaseID, result.ExternalLeaseId)
	assert.Equal(t, receipt.GatewayRequestId, result.GatewayRequestId)
	assert.Equal(t, receipt.AudioRequestSettlementId, *result.AudioRequestSettlementId)
	assert.Equal(t, model.AudioBillingStatusPending, result.BillingStatus)
	assert.Equal(t, model.AudioSettlementDispatchUnknown, result.DispatchState)
	assert.Equal(t, int64(1), *result.Revision)
	assert.Nil(t, result.CompletedTime)
	assert.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))

	var wire map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &wire))
	assert.NotContains(t, wire, "newapi_user_id")
	assert.NotContains(t, wire, "token_id")
	assert.NotContains(t, wire, "model_name")
	assert.NotContains(t, wire, "pre_consumed_quota")
	assert.NotContains(t, wire, "provider")
}

func TestGetInternalAudioRequestSettlementUsesUnifiedUnknownForMissingReceipt(t *testing.T) {
	setupAudioReceiptControllerDB(t)

	recorder := callAudioReceipt(t, newAudioReceiptEngine(), http.MethodGet, audioReceiptLeaseID, "RequestMissing001")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	result := decodeAudioReceipt(t, recorder)
	assert.Equal(t, audioReceiptLeaseID, result.ExternalLeaseId)
	assert.Equal(t, "RequestMissing001", result.GatewayRequestId)
	assert.Nil(t, result.AudioRequestSettlementId)
	assert.Equal(t, "unknown", result.BillingStatus)
	assert.Equal(t, "unknown", result.DispatchState)
	assert.Nil(t, result.Revision)
	assert.Nil(t, result.CompletedTime)
}

func TestGetInternalAudioRequestSettlementRejectsInvalidPathAndQuery(t *testing.T) {
	setupAudioReceiptControllerDB(t)
	engine := newAudioReceiptEngine()

	invalidLease := httptest.NewRequest(http.MethodGet, "/api/internal/v1/llm/relay-leases/LEASE/audio-request-settlements/Request001", nil)
	invalidLeaseRecorder := httptest.NewRecorder()
	engine.ServeHTTP(invalidLeaseRecorder, invalidLease)
	assert.Equal(t, http.StatusBadRequest, invalidLeaseRecorder.Code)

	invalidRequest := httptest.NewRequest(http.MethodGet, "/api/internal/v1/llm/relay-leases/"+audioReceiptLeaseID+"/audio-request-settlements/Request_001", nil)
	invalidRequestRecorder := httptest.NewRecorder()
	engine.ServeHTTP(invalidRequestRecorder, invalidRequest)
	assert.Equal(t, http.StatusBadRequest, invalidRequestRecorder.Code)

	withQuery := httptest.NewRequest(http.MethodGet, "/api/internal/v1/llm/relay-leases/"+audioReceiptLeaseID+"/audio-request-settlements/Request001?retry=true", nil)
	withQueryRecorder := httptest.NewRecorder()
	engine.ServeHTTP(withQueryRecorder, withQuery)
	assert.Equal(t, http.StatusBadRequest, withQueryRecorder.Code)
}
