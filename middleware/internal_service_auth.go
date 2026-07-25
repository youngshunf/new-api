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

// InternalServiceTokenEnv 是服务凭据的环境变量名，支持逗号分隔多枚以便轮换。
const InternalServiceTokenEnv = "INTERNAL_CREDIT_SERVICE_TOKENS"

// internalServiceIdentityKey 是通过鉴权后写入上下文的服务身份（凭据指纹前缀）。
const internalServiceIdentityKey = "internal_service_identity"

var (
	internalServiceTokensOnce sync.Once
	internalServiceTokens     []string
)

func loadInternalServiceTokens() []string {
	internalServiceTokensOnce.Do(func() {
		raw := strings.TrimSpace(os.Getenv(InternalServiceTokenEnv))
		if raw == "" {
			common.SysLog("internal credit API disabled: " + InternalServiceTokenEnv + " is not configured")
			return
		}
		for _, part := range strings.Split(raw, ",") {
			token := strings.TrimSpace(part)
			// 过短的凭据等于没有凭据，直接不装载，避免误以为已开启鉴权。
			if len(token) >= 32 {
				internalServiceTokens = append(internalServiceTokens, token)
				continue
			}
			if token != "" {
				common.SysLog("ignored an internal credit service token shorter than 32 chars")
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

// AbortWithCreditError 用内部 API 的统一错误体中止请求。
func AbortWithCreditError(c *gin.Context, status int, code string, message string, retryable bool) {
	c.AbortWithStatusJSON(status, dto.CreditErrorResponse{
		Code:      code,
		Message:   message,
		TraceId:   c.GetString(common.RequestIdKey),
		Retryable: retryable,
	})
}

// InternalServiceAuth 校验 Cloud 服务凭据。
func InternalServiceAuth() func(c *gin.Context) {
	return func(c *gin.Context) {
		tokens := loadInternalServiceTokens()
		if len(tokens) == 0 {
			AbortWithCreditError(c, http.StatusUnauthorized, "invalid_service_credential",
				"internal credit API is not configured on this deployment", false)
			return
		}
		header := strings.TrimSpace(c.GetHeader("Authorization"))
		presented := ""
		if strings.HasPrefix(header, "Bearer ") {
			presented = strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
		}
		if presented == "" {
			AbortWithCreditError(c, http.StatusUnauthorized, "invalid_service_credential",
				"a service bearer token is required", false)
			return
		}
		for _, token := range tokens {
			if subtle.ConstantTimeCompare([]byte(token), []byte(presented)) == 1 {
				c.Set(internalServiceIdentityKey, serviceTokenFingerprint(token))
				c.Next()
				return
			}
		}
		AbortWithCreditError(c, http.StatusUnauthorized, "invalid_service_credential",
			"service bearer token is not recognized", false)
	}
}

var internalServiceRateLimiter common.InMemoryRateLimiter

// InternalServiceRateLimit 按调用方服务身份限流。
//
// 默认 300 次 / 3 秒 ≈ 100 RPS、突发 300，与内部契约建议一致；
// 可用 INTERNAL_CREDIT_RATE_LIMIT_NUM / INTERNAL_CREDIT_RATE_LIMIT_DURATION 调整。
// 这是单进程内存限流：多副本部署时每副本各自计数，容量按副本数放大。
func InternalServiceRateLimit() func(c *gin.Context) {
	maxRequestNum := 300
	if raw := strings.TrimSpace(os.Getenv("INTERNAL_CREDIT_RATE_LIMIT_NUM")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			maxRequestNum = parsed
		}
	}
	duration := int64(3)
	if raw := strings.TrimSpace(os.Getenv("INTERNAL_CREDIT_RATE_LIMIT_DURATION")); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed > 0 {
			duration = parsed
		}
	}
	internalServiceRateLimiter.Init(time.Duration(duration*3) * time.Second)
	return func(c *gin.Context) {
		identity := c.GetString(internalServiceIdentityKey)
		if identity == "" {
			identity = c.ClientIP()
		}
		if !internalServiceRateLimiter.Request("internal-credit:"+identity, maxRequestNum, duration) {
			c.Header("Retry-After", strconv.FormatInt(duration, 10))
			AbortWithCreditError(c, http.StatusTooManyRequests, "too_many_requests",
				"internal credit API rate limit exceeded", true)
			return
		}
		c.Next()
	}
}
