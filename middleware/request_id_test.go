package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestIdPublishesStandardResponseHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(RequestId())
	router.GET("/request-id", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/request-id", nil))

	oneAPIRequestID := response.Header().Get(common.RequestIdKey)
	require.NotEmpty(t, oneAPIRequestID)
	assert.Equal(t, oneAPIRequestID, response.Header().Get("X-Request-Id"))
}
