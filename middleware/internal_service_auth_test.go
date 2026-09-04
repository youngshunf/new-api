package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 长度合法（≥ minServiceTokenLength）的测试凭据。
func testToken(seed string) string {
	return seed + strings.Repeat("x", minServiceTokenLength)
}

// runScopedAuth 用注入的凭据表跑一次鉴权，返回响应与「是否放行到 handler」。
//
// 刻意不走 InternalServiceAuth：那条路读环境变量且带 sync.Once，一个进程里只装载一次，
// 用它写多用例测试会互相污染，且第二个用例起就不再真正判定配置。
func runScopedAuth(t *testing.T, scope ServiceScope, tokens map[ServiceScope][]string, authorization string) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	reached := false
	router := gin.New()
	router.GET("/probe", newInternalServiceAuth(scope, tokens), func(c *gin.Context) {
		reached = true
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	return recorder, reached
}

// 本次改造的核心不变量：一个 scope 的凭据在任何**别的** scope 上都必须被拒。
//
// 用例遍历 knownServiceScopes 本身，而不是一份手抄的 scope 列表——手抄的那份漏掉谁，
// 谁就永远没有被判过，而测试照样全绿。新增 scope 会自动进入这张两两矩阵。
func TestCredentialOfOneScopeIsRejectedOnEveryOtherScope(t *testing.T) {
	require.Greater(t, len(knownServiceScopes), 1, "至少要有两个 scope，否则这条不变量无从证伪")

	for issued := range knownServiceScopes {
		for target := range knownServiceScopes {
			if issued == target {
				continue
			}
			t.Run(string(issued)+"_token_on_"+string(target)+"_route", func(t *testing.T) {
				token := testToken(string(issued))
				tokens := map[ServiceScope][]string{issued: {token}, target: {testToken("other")}}

				recorder, reached := runScopedAuth(t, target, tokens, "Bearer "+token)

				assert.False(t, reached, "%s 的凭据不得打通 %s 的路由", issued, target)
				assert.Equal(t, http.StatusUnauthorized, recorder.Code)
			})
		}
	}
}

// 某个 scope 没有配凭据时必须整条拒绝，不得回落到别的 scope 已配的凭据。
func TestScopeWithoutCredentialFailsClosed(t *testing.T) {
	creditToken := testToken("credit")
	tokens := map[ServiceScope][]string{ScopeCredit: {creditToken}}

	recorder, reached := runScopedAuth(t, ScopeLLM, tokens, "Bearer "+creditToken)

	assert.False(t, reached)
	assert.Equal(t, http.StatusUnauthorized, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "llm")
}

func TestValidCredentialPassesAndRecordsIdentityAndScope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	token := testToken("credit")

	var identity, scope string
	router := gin.New()
	router.GET("/probe", newInternalServiceAuth(ScopeCredit, map[ServiceScope][]string{ScopeCredit: {token}}),
		func(c *gin.Context) {
			identity = c.GetString(internalServiceIdentityKey)
			scope = c.GetString(internalServiceScopeKey)
			c.Status(http.StatusNoContent)
		})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusNoContent, recorder.Code)
	assert.Equal(t, serviceTokenFingerprint(token), identity)
	assert.Equal(t, string(ScopeCredit), scope)
	assert.NotContains(t, identity, token, "身份是指纹，不得是凭据本身")
}

func TestMalformedAuthorizationIsRejected(t *testing.T) {
	token := testToken("credit")
	tokens := map[ServiceScope][]string{ScopeCredit: {token}}

	for name, header := range map[string]string{
		"缺 Authorization": "",
		"没有 Bearer 前缀":    token,
		"Bearer 后为空":      "Bearer ",
		"用了 Basic":        "Basic " + token,
		"凭据错":             "Bearer " + testToken("wrong"),
	} {
		t.Run(name, func(t *testing.T) {
			recorder, reached := runScopedAuth(t, ScopeCredit, tokens, header)
			assert.False(t, reached)
			assert.Equal(t, http.StatusUnauthorized, recorder.Code)
		})
	}
}

func TestErrorBodyKeepsWireFieldNames(t *testing.T) {
	recorder, _ := runScopedAuth(t, ScopeCredit, map[ServiceScope][]string{}, "")

	var body map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	for _, field := range []string{"code", "message", "trace_id", "retryable"} {
		assert.Contains(t, body, field, "wire 字段名是契约，改 Go 类型名不得动它")
	}
}

// ── 配置解析 ────────────────────────────────────────────────────────────────

func TestLegacyEnvBindsToCreditOnlyNeverToOtherScopes(t *testing.T) {
	legacy := testToken("legacy")

	tokens := parseInternalServiceTokens("", legacy)

	assert.Equal(t, []string{legacy}, tokens[ScopeCredit])
	for scope := range knownServiceScopes {
		if scope == ScopeCredit {
			continue
		}
		assert.Empty(t, tokens[scope], "存量 credit 凭据不得凭「历史」获得 %s", scope)
	}
}

func TestScopedEnvParsesEveryDeclaredScope(t *testing.T) {
	var entries []string
	expected := map[ServiceScope]string{}
	for scope := range knownServiceScopes {
		token := testToken(string(scope))
		expected[scope] = token
		entries = append(entries, string(scope)+":"+token)
	}

	tokens := parseInternalServiceTokens(strings.Join(entries, ","), "")

	for scope, token := range expected {
		assert.Equal(t, []string{token}, tokens[scope])
	}
}

func TestRejectedConfigEntries(t *testing.T) {
	cases := map[string]string{
		"没有 scope 前缀":  testToken("bare"),
		"scope 不认识":    "billing:" + testToken("unknown"),
		"凭据过短":         "credit:short",
		"scope 有了但凭据空": "credit:",
	}
	for name, entry := range cases {
		t.Run(name, func(t *testing.T) {
			tokens := parseInternalServiceTokens(entry, "")
			assert.Empty(t, tokens, "非法配置项必须被丢弃，而不是降级成某个已知 scope")
		})
	}
}

func TestScopedAndLegacyCreditCredentialsCoexist(t *testing.T) {
	scoped := testToken("scoped")
	legacy := testToken("legacy")

	tokens := parseInternalServiceTokens("credit:"+scoped, legacy)

	assert.ElementsMatch(t, []string{scoped, legacy}, tokens[ScopeCredit],
		"迁移期两种来源要并存，否则切换配置的那一刻会断服")
}

func TestSameTokenInTwoScopesIsParsedButFlagged(t *testing.T) {
	shared := testToken("shared")

	tokens := parseInternalServiceTokens("credit:"+shared+",llm:"+shared, "")

	// 配置说了算，但它在配置层把两个 scope 又合并回一个——warnOnSharedServiceTokens 负责喊出来。
	assert.Equal(t, []string{shared}, tokens[ScopeCredit])
	assert.Equal(t, []string{shared}, tokens[ScopeLLM])
}
