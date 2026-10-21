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
	return performInternalCreditUsageRequest("/daily", query)
}

// performInternalCreditUsageRequest 走真实 gin 路由打日统计（suffix="/daily"）或逐笔流水（suffix=""）。
func performInternalCreditUsageRequest(suffix string, query string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/internal/v1/credit-usage/:newapi_user_id", GetCreditUsage)
	router.GET("/api/internal/v1/credit-usage/:newapi_user_id/daily", GetCreditUsageDaily)
	req := httptest.NewRequest(http.MethodGet, "/api/internal/v1/credit-usage/7801"+suffix+query, nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	return recorder
}

// start/end 给了却不是合法的非负整数秒，是调用方的确定性错误：必须 400 invalid_request，
// 不能静默当成「不限」去扫全量（那是假 fallback，会把错误范围的统计当真数据交出去）。
func TestInternalCreditUsageRoutesRejectMalformedRangeBounds(t *testing.T) {
	for _, route := range []struct{ name, suffix, extra string }{
		{name: "daily", suffix: "/daily", extra: "&timezone=UTC"},
		{name: "list", suffix: "", extra: ""},
	} {
		for _, bound := range []string{"start=abc", "end=-1", "start=1.5", "end=1e9", "start=%201"} {
			t.Run(route.name+"/"+bound, func(t *testing.T) {
				recorder := performInternalCreditUsageRequest(route.suffix, "?"+bound+route.extra)
				require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
				var body dto.InternalErrorResponse
				require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &body))
				assert.Equal(t, model.CreditErrorInvalidRequest, body.Code)
				assert.False(t, body.Retryable)
			})
		}
	}
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
