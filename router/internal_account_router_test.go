package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestInternalAccountRoutesUseAccountScope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	credentials := internalServiceTestCredentials(t)
	creditToken := credentials["credit"]
	accountToken := credentials["account"]
	router := gin.New()
	SetInternalRouter(router)

	req := httptest.NewRequest(http.MethodPost, "/api/internal/v1/accounts", nil)
	req.Header.Set("Authorization", "Bearer "+creditToken)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusUnauthorized, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "invalid_service_credential")
	assert.NotContains(t, recorder.Body.String(), "account")

	validScopeReq := httptest.NewRequest(http.MethodGet, "/api/internal/v1/accounts/not-an-id", nil)
	validScopeReq.Header.Set("Authorization", "Bearer "+accountToken)
	validScope := httptest.NewRecorder()
	router.ServeHTTP(validScope, validScopeReq)

	assert.Equal(t, http.StatusBadRequest, validScope.Code)
	assert.Contains(t, validScope.Body.String(), "invalid_request")
}

func testRouterServiceToken(seed string) string {
	return seed + "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
}
