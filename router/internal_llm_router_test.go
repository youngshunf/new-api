package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 内部通道的分权是**按 group 分**的，不是靠一句注释。
//
// ⚠️ 本文件是 router 包里唯一会真正发起内部通道请求的测试。
// middleware 的凭据装载带 sync.Once，一个测试进程只装载一次，所以凭据必须在这里一次配齐，
// 并且所有断言都在同一个用例里跑完；拆成多个用例会让第二个用例起就不再真正读配置。

func internalServiceTestToken(seed string) string {
	return seed + strings.Repeat("x", 40)
}

func callInternalRoute(t *testing.T, engine *gin.Engine, method string, path string, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(""))
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	return recorder
}

func internalErrorCode(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var failure dto.InternalErrorResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &failure))
	return failure.Code
}

// LLM 接口挂在自己的 group 上：credit 凭据打不进来，llm 凭据也打不进 credit。
//
// 复用 creditRouter 的话，下面两条 401 会当场变成「放行」——那正是 S1-A 分权改造要消除的形态。
func TestInternalRouterKeepsLlmAndCreditInSeparateScopedGroups(t *testing.T) {
	credentials := internalServiceTestCredentials(t)
	creditToken := credentials["credit"]
	llmToken := credentials["llm"]

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	require.NotPanics(t, func() { SetInternalRouter(engine) })

	registered := map[string]bool{}
	for _, route := range engine.Routes() {
		registered[route.Method+" "+route.Path] = true
	}
	for _, expected := range []string{
		"GET /api/internal/v1/llm/model-inventory",
		"PUT /api/internal/v1/llm/relay-leases/:external_lease_id",
		"DELETE /api/internal/v1/llm/relay-leases/:external_lease_id",
		"GET /api/internal/v1/credit-accounts/:newapi_user_id",
	} {
		assert.True(t, registered[expected], "路由 %s 没有注册", expected)
	}

	const llmProbe = "/api/internal/v1/llm/model-inventory"
	const creditProbe = "/api/internal/v1/credit-accounts/7"

	// 无凭据：两条 group 都拒。
	assert.Equal(t, http.StatusUnauthorized, callInternalRoute(t, engine, http.MethodGet, llmProbe, "").Code)
	assert.Equal(t, http.StatusUnauthorized, callInternalRoute(t, engine, http.MethodGet, creditProbe, "").Code)

	// 跨 scope：状态码与错误码都必须是同一条 401，不泄露该凭据在别处是否有效。
	creditOnLlm := callInternalRoute(t, engine, http.MethodGet, llmProbe, creditToken)
	assert.Equal(t, http.StatusUnauthorized, creditOnLlm.Code, "credit 凭据打进了 llm 接口，说明两者共用了同一个 group")
	assert.Equal(t, "invalid_service_credential", internalErrorCode(t, creditOnLlm))

	llmOnCredit := callInternalRoute(t, engine, http.MethodGet, creditProbe, llmToken)
	assert.Equal(t, http.StatusUnauthorized, llmOnCredit.Code, "llm 凭据打进了 credit 接口")
	assert.Equal(t, "invalid_service_credential", internalErrorCode(t, llmOnCredit))

	// 正向：llm 凭据确实能过 llm group 的鉴权。
	// 探针刻意用一个非法的 external_lease_id——handler 在碰数据库之前就会 400，
	// 于是「400 而不是 401」这件事本身就证明鉴权放行了，而这条断言不需要任何数据库。
	passed := callInternalRoute(t, engine, http.MethodPut,
		"/api/internal/v1/llm/relay-leases/NOT_A_LEASE_ID", llmToken)
	assert.Equal(t, http.StatusBadRequest, passed.Code, "llm 凭据没能通过 llm group 的鉴权")
	assert.Equal(t, "invalid_request", internalErrorCode(t, passed))
}
