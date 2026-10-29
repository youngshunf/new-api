package controller

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupInternalAccountControllerDB(t *testing.T) {
	t.Helper()
	previousDB := model.DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.InternalAccountProvision{}))
	model.DB = db
	t.Cleanup(func() { model.DB = previousDB })
}

func performInternalAccountRequest(body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/internal/v1/accounts", PostInternalAccount)
	req := httptest.NewRequest(http.MethodPost, "/api/internal/v1/accounts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	return recorder
}

func TestPostInternalAccountRejectsStrictBodyViolations(t *testing.T) {
	cases := []string{
		`{}`,
		`{"provision_key":""}`,
		`{"provision_key":"` + strings.Repeat("x", 65) + `"}`,
		`{"provision_key":"ok","payload_hash":"nope"}`,
		`{"provision_key":"ok"} {"provision_key":"again"}`,
		`[]`,
		`{"provision_key":"ok","workspace_key":"workspace_abc"}`,
		`{"provision_key":"ok","workspace_kind":"personal"}`,
		`{"provision_key":"ok","workspace_key":"workspace_abc","workspace_kind":"team"}`,
		`{"provision_key":"ok","workspace_key":"ws_abc","workspace_kind":"personal"}`,
		`{"provision_key":"ok","workspace_key":null,"workspace_kind":"personal"}`,
		`{"provision_key":"ok","workspace_key":7,"workspace_kind":"personal"}`,
	}
	for _, body := range cases {
		t.Run(body, func(t *testing.T) {
			recorder := performInternalAccountRequest(body)
			assert.Equal(t, http.StatusBadRequest, recorder.Code)
			assert.Contains(t, recorder.Body.String(), model.InternalAccountErrorInvalidRequest)
		})
	}
}

func TestPostInternalAccountReturnsNewAPIUserID(t *testing.T) {
	setupInternalAccountControllerDB(t)
	recorder := performInternalAccountRequest(`{"provision_key":"controller-provision"}`)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var first map[string]int
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &first))
	require.Positive(t, first["newapi_user_id"])

	replay := performInternalAccountRequest(`{"provision_key":"controller-provision"}`)
	require.Equal(t, http.StatusOK, replay.Code, replay.Body.String())
	var second map[string]int
	require.NoError(t, common.Unmarshal(replay.Body.Bytes(), &second))
	assert.Equal(t, first["newapi_user_id"], second["newapi_user_id"])
}

func TestGetInternalAccountReturnsExactStatusBodyAndNotFound(t *testing.T) {
	setupInternalAccountControllerDB(t)
	userId, err := model.ProvisionInternalAccount("controller-get-provision", nil)
	require.Nil(t, err)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/internal/v1/accounts/:newapi_user_id", GetInternalAccount)

	foundReq := httptest.NewRequest(http.MethodGet, "/api/internal/v1/accounts/"+strconv.Itoa(userId), nil)
	found := httptest.NewRecorder()
	router.ServeHTTP(found, foundReq)
	require.Equal(t, http.StatusOK, found.Code, found.Body.String())
	var body map[string]int
	require.NoError(t, common.Unmarshal(found.Body.Bytes(), &body))
	assert.Equal(t, map[string]int{"status": common.UserStatusEnabled}, body)

	missingReq := httptest.NewRequest(http.MethodGet, "/api/internal/v1/accounts/999999", nil)
	missing := httptest.NewRecorder()
	router.ServeHTTP(missing, missingReq)
	assert.Equal(t, http.StatusNotFound, missing.Code)
	assert.Contains(t, missing.Body.String(), model.InternalAccountErrorUserNotFound)
}

func TestPostInternalAccountRecordsWorkspaceAndRejectsConflictingReplay(t *testing.T) {
	setupInternalAccountControllerDB(t)
	body := `{"provision_key":"controller-seat","workspace_key":"workspace_ent_42","workspace_kind":"enterprise"}`
	first := performInternalAccountRequest(body)
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	var created map[string]int
	require.NoError(t, common.Unmarshal(first.Body.Bytes(), &created))

	var stored model.InternalAccountProvision
	require.NoError(t, model.DB.Where("provision_key = ?", "controller-seat").First(&stored).Error)
	require.NotNil(t, stored.WorkspaceKey)
	assert.Equal(t, "workspace_ent_42", *stored.WorkspaceKey)
	assert.Equal(t, "enterprise", *stored.WorkspaceKind)
	assert.Equal(t, created["newapi_user_id"], stored.UserId)

	replay := performInternalAccountRequest(body)
	require.Equal(t, http.StatusOK, replay.Code, replay.Body.String())
	var replayed map[string]int
	require.NoError(t, common.Unmarshal(replay.Body.Bytes(), &replayed))
	assert.Equal(t, created["newapi_user_id"], replayed["newapi_user_id"])

	conflict := performInternalAccountRequest(`{"provision_key":"controller-seat","workspace_key":"workspace_ent_43","workspace_kind":"enterprise"}`)
	assert.Equal(t, http.StatusConflict, conflict.Code)
	assert.Contains(t, conflict.Body.String(), `"code":"idempotency_conflict"`)
}
