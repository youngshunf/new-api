package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func performCreditUsageDailyRequest(query string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/internal/v1/credit-usage/:newapi_user_id/daily", GetCreditUsageDaily)
	req := httptest.NewRequest(http.MethodGet, "/api/internal/v1/credit-usage/7801/daily"+query, nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	return recorder
}

// 统一时间设计 §4.4 冻结基线：timezone 必填；旧 tz_offset_minutes 已删除、不留兼容，
// 只传它等同于缺 timezone。非法时区名同样是 400 invalid_request（不可重试），而不是 503。
func TestGetCreditUsageDailyRejectsMissingOrUnknownTimezone(t *testing.T) {
	for _, query := range []string{"", "?timezone=", "?tz_offset_minutes=480", "?timezone=Mars%2FOlympus_Mons"} {
		t.Run(query, func(t *testing.T) {
			recorder := performCreditUsageDailyRequest(query)
			require.Equal(t, http.StatusBadRequest, recorder.Code)
			var body dto.InternalErrorResponse
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &body))
			assert.Equal(t, model.CreditErrorInvalidRequest, body.Code)
			assert.False(t, body.Retryable)
		})
	}
}
