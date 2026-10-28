package middleware

import (
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
)

const RouteTagKey = "route_tag"

func RouteTag(tag string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(RouteTagKey, tag)
		c.Next()
	}
}

func SetUpLogger(server *gin.Engine) {
	server.Use(redactTaskArtifactAccessQuery())
	server.Use(gin.LoggerWithFormatter(func(param gin.LogFormatterParams) string {
		var requestID string
		if param.Keys != nil {
			requestID, _ = param.Keys[common.RequestIdKey].(string)
		}
		tag, _ := param.Keys[RouteTagKey].(string)
		if tag == "" {
			tag = "web"
		}
		path := param.Path
		pathOnly, _, _ := strings.Cut(path, "?")
		// 单次材料令牌是bearer凭据；错误路径与查询也仅输出稳定路由名。
		// 只修改日志副本，不能改取件所需的URL、查询或Param。
		const materialRoute = "/v1/audio/voice-materials"
		switch {
		case strings.EqualFold(pathOnly, materialRoute) || strings.HasPrefix(strings.ToLower(pathOnly), materialRoute+"/"):
			path = materialRoute + "/:token"
		// OAuth callbacks carry one-time codes and state in the query string.
		// Redact the log value only; the handler still needs the original query.
		case strings.HasPrefix(path, "/api/oauth/") || strings.HasPrefix(path, "/oauth/"):
			path = pathOnly
		case param.Request.URL.RawQuery != "":
			query := param.Request.URL.Query().Encode()
			path = param.Request.URL.EscapedPath() + relaycommon.SanitizeURLForLog("?"+query)
		}
		return fmt.Sprintf("[GIN] %s | %s | %s | %3d | %13v | %15s | %7s %s\n",
			param.TimeStamp.Format("2006/01/02 - 15:04:05"),
			tag,
			requestID,
			param.StatusCode,
			param.Latency,
			param.ClientIP,
			param.Method,
			path,
		)
	}))
}
