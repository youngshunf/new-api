package middleware

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"

	"github.com/gin-gonic/gin"
)

// 内部服务面鉴权与限流。
//
// /api/internal/v1 是 Cloud → NewAPI 的服务到服务通道：
//   - 只接受 Cloud 服务凭据（Bearer service token），不接受浏览器 cookie / Owner JWT；
//   - 不挂 CORS，浏览器无法直接调用；
//   - 凭据未配置时整条通道拒绝服务（fail closed），不会退化成匿名可写。
//
// # 凭据按 scope 分权，不是一把钥匙开整组
//
// 改造前这条通道只有一个扁平凭据集合，并且在**组级**鉴权：任何一枚凭据通过，就能打通
// /api/internal/v1 下的全部接口。往这一组里新增一类接口（LLM 库存与 Relay lease），等于把既有
// credit 凭据的权限一并扩大到新接口，反之亦然——而两类接口的爆炸半径完全不同：credit 凭据能
// 动钱，LLM 凭据能签发「可以调模型」的 Relay Token。
//
// 现在每一枚凭据都必须声明自己属于哪个 scope，每条路由按自己需要的 scope 判权。
// **没有「默认全通」这条路**：credit 凭据打 LLM 接口一定 401，反之亦然。

// ServiceScope 是内部通道的权限域。闭集——新增一个 scope 必须同时新增它的路由与凭据装载，
// 不存在「未声明 scope 的路由」这种状态。
type ServiceScope string

const (
	// ScopeCredit 是积分与用量接口：能动钱。
	ScopeCredit ServiceScope = "credit"
	// ScopeLLM 是 LLM 库存与 Relay lease 接口：能签发可调模型的 Token。
	ScopeLLM ServiceScope = "llm"
	// ScopeAccount 是 Cloud 按 workspace 开通 NewAPI 账户的接口。
	ScopeAccount ServiceScope = "account"
)

// knownServiceScopes 是配置解析时的白名单。配置里出现名单外的 scope 会被跳过并告警，
// 而不是当作某个已知 scope 处理——猜错的方向恰好是放大权限。
var knownServiceScopes = map[ServiceScope]bool{
	ScopeCredit:  true,
	ScopeLLM:     true,
	ScopeAccount: true,
}

// InternalServiceTokensEnv 按 scope 声明服务凭据，形如 `credit:<token>,llm:<token>`；
// 同一个 scope 可以配多枚以便轮换。
const InternalServiceTokensEnv = "INTERNAL_SERVICE_TOKENS"

// LegacyInternalCreditTokensEnv 是改造前的扁平凭据变量，**只映射到 credit scope**。
//
// 它必须继续被读取：这个变量配在部署环境里、不在版本库中，直接停读会让正在跑的 credit 通道
// 当场全部 401。但它绝不会因为「是历史凭据」而获得新 scope——迁移期的存量凭据只能是 credit。
const LegacyInternalCreditTokensEnv = "INTERNAL_CREDIT_SERVICE_TOKENS"

// minServiceTokenLength 是凭据的最小长度。过短的凭据等于没有凭据，直接不装载，
// 避免误以为已开启鉴权。
const minServiceTokenLength = 32

// internalServiceIdentityKey 是通过鉴权后写入上下文的服务身份（凭据指纹前缀）。
const internalServiceIdentityKey = "internal_service_identity"

// internalServiceScopeKey 是本次请求判权所用的 scope，用于日志与限流分桶。
const internalServiceScopeKey = "internal_service_scope"

const invalidServiceCredentialMessage = "invalid internal service credential"

var (
	internalServiceTokensOnce sync.Once
	internalServiceTokens     map[ServiceScope][]string
)

// parseInternalServiceTokens 把两个环境变量解析成「scope → 凭据集合」。
//
// 刻意做成纯函数：装载点有 sync.Once，测试改不动它；把判据放在纯函数里，
// scope 隔离这条不变量才能被真正逐条证伪。
func parseInternalServiceTokens(scoped string, legacyCredit string) map[ServiceScope][]string {
	tokens := make(map[ServiceScope][]string)

	for _, entry := range strings.Split(scoped, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, token, found := strings.Cut(entry, ":")
		if !found {
			common.SysLog("ignored an internal service credential without a scope prefix (want `<scope>:<token>`)")
			continue
		}
		scope := ServiceScope(strings.TrimSpace(name))
		if !knownServiceScopes[scope] {
			common.SysLog("ignored an internal service credential with unknown scope: " + string(scope))
			continue
		}
		token = strings.TrimSpace(token)
		if len(token) < minServiceTokenLength {
			if token != "" {
				common.SysLog("ignored an internal service token shorter than " +
					strconv.Itoa(minServiceTokenLength) + " chars for scope " + string(scope))
			}
			continue
		}
		tokens[scope] = append(tokens[scope], token)
	}

	for _, token := range strings.Split(legacyCredit, ",") {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		if len(token) < minServiceTokenLength {
			common.SysLog("ignored a legacy internal credit service token shorter than " +
				strconv.Itoa(minServiceTokenLength) + " chars")
			continue
		}
		tokens[ScopeCredit] = append(tokens[ScopeCredit], token)
	}

	warnOnSharedServiceTokens(tokens)
	return tokens
}

// warnOnSharedServiceTokens 对「同一枚凭据配给多个 scope」告警。
//
// 这不是洁癖：把同一个字符串同时写进 credit 与 llm，在配置上就把两个 scope 重新合并成一个，
// 而代码这一侧看起来仍然是分权的——那正是本次改造要消除的形态，只是搬去了配置里。
func warnOnSharedServiceTokens(tokens map[ServiceScope][]string) {
	seen := make(map[string][]ServiceScope)
	for scope, list := range tokens {
		for _, token := range list {
			seen[token] = append(seen[token], scope)
		}
	}
	for token, scopes := range seen {
		if len(scopes) > 1 {
			common.SysLog("internal service credential " + serviceTokenFingerprint(token) +
				" is configured for multiple scopes; that re-merges scopes in configuration")
		}
	}
}

func loadInternalServiceTokens() map[ServiceScope][]string {
	internalServiceTokensOnce.Do(func() {
		internalServiceTokens = parseInternalServiceTokens(
			os.Getenv(InternalServiceTokensEnv),
			os.Getenv(LegacyInternalCreditTokensEnv),
		)
		if len(internalServiceTokens) == 0 {
			common.SysLog("internal service API disabled: neither " + InternalServiceTokensEnv +
				" nor " + LegacyInternalCreditTokensEnv + " is configured")
			return
		}
		for scope := range knownServiceScopes {
			if len(internalServiceTokens[scope]) == 0 {
				common.SysLog("internal service scope " + string(scope) +
					" has no credential; its routes will reject every request")
			}
		}
	})
	return internalServiceTokens
}

// serviceTokenFingerprint 是凭据的短指纹，用于审计日志区分调用方而不泄露凭据本身。
func serviceTokenFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:12]
}

// AbortWithInternalServiceError 用内部通道的统一错误体中止请求。
func AbortWithInternalServiceError(c *gin.Context, status int, code string, message string, retryable bool) {
	c.AbortWithStatusJSON(status, dto.InternalErrorResponse{
		Code:      code,
		Message:   message,
		TraceId:   c.GetString(common.RequestIdKey),
		Retryable: retryable,
	})
}

// InternalServiceAuth 校验 Cloud 服务凭据，并要求它属于 scope。
func InternalServiceAuth(scope ServiceScope) gin.HandlerFunc {
	return func(c *gin.Context) {
		newInternalServiceAuth(scope, loadInternalServiceTokens())(c)
	}
}

func newInternalServiceAuth(scope ServiceScope, tokens map[ServiceScope][]string) gin.HandlerFunc {
	return func(c *gin.Context) {
		allowed := tokens[scope]
		if len(allowed) == 0 {
			// fail closed：本 scope 没有凭据就整条拒绝，绝不回落到别的 scope 的凭据。
			AbortWithInternalServiceError(c, http.StatusUnauthorized, "invalid_service_credential",
				invalidServiceCredentialMessage, false)
			return
		}
		presented := bearerCredential(c)
		if presented == "" {
			AbortWithInternalServiceError(c, http.StatusUnauthorized, "invalid_service_credential",
				invalidServiceCredentialMessage, false)
			return
		}
		for _, token := range allowed {
			if subtle.ConstantTimeCompare([]byte(token), []byte(presented)) == 1 {
				c.Set(internalServiceIdentityKey, serviceTokenFingerprint(token))
				c.Set(internalServiceScopeKey, string(scope))
				c.Next()
				return
			}
		}
		AbortWithInternalServiceError(c, http.StatusUnauthorized, "invalid_service_credential",
			invalidServiceCredentialMessage, false)
	}
}

func bearerCredential(c *gin.Context) string {
	header := strings.TrimSpace(c.GetHeader("Authorization"))
	if !strings.HasPrefix(header, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
}

var internalServiceRateLimiter common.InMemoryRateLimiter

// InternalServiceRateLimit 按 scope + 调用方服务身份限流。
//
// 默认 300 次 / 3 秒 ≈ 100 RPS、突发 300，与内部契约建议一致；可用
// INTERNAL_SERVICE_RATE_LIMIT_NUM / INTERNAL_SERVICE_RATE_LIMIT_DURATION 调整
// （沿用旧的 INTERNAL_CREDIT_RATE_LIMIT_* 作为兜底，避免部署侧调参在改名后失效）。
// 这是单进程内存限流：多副本部署时每副本各自计数，容量按副本数放大。
//
// 分桶键带 scope：一个 scope 被打满不应该把另一个 scope 也限住。
func InternalServiceRateLimit(scope ServiceScope) gin.HandlerFunc {
	maxRequestNum := intFromEnv("INTERNAL_SERVICE_RATE_LIMIT_NUM", "INTERNAL_CREDIT_RATE_LIMIT_NUM", 300)
	duration := int64(intFromEnv("INTERNAL_SERVICE_RATE_LIMIT_DURATION", "INTERNAL_CREDIT_RATE_LIMIT_DURATION", 3))
	internalServiceRateLimiter.Init(time.Duration(duration*3) * time.Second)
	return func(c *gin.Context) {
		identity := c.GetString(internalServiceIdentityKey)
		if identity == "" {
			identity = c.ClientIP()
		}
		if !internalServiceRateLimiter.Request("internal:"+string(scope)+":"+identity, maxRequestNum, duration) {
			c.Header("Retry-After", strconv.FormatInt(duration, 10))
			AbortWithInternalServiceError(c, http.StatusTooManyRequests, "too_many_requests",
				"internal API rate limit exceeded", true)
			return
		}
		c.Next()
	}
}

func intFromEnv(name string, legacyName string, fallback int) int {
	for _, key := range []string{name, legacyName} {
		raw := strings.TrimSpace(os.Getenv(key))
		if raw == "" {
			continue
		}
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			return parsed
		}
	}
	return fallback
}
