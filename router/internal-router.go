package router

import (
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"

	"github.com/gin-gonic/gin"
)

// SetInternalRouter 挂载 Cloud → NewAPI 的服务到服务内部通道。
//
// 它刻意独立于 /api：不挂 CORS、不挂 session、不接受 Owner cookie 或 JWT，
// 只认服务凭据。版本前缀固定 /api/internal/v1，语义变更必须走新前缀。
//
// ⚠️ **鉴权按 scope 分组，不在组级挂一把通用钥匙。** 每个 scope 一个 gin group，
// 各自 Use 自己那个 scope 的鉴权与限流；新增一类接口时必须**新建它自己的 group**，
// 挂进已有 group 等于把已有凭据的权限静默扩大到新接口（改造前正是这个形态）。
func SetInternalRouter(router *gin.Engine) {
	creditRouter := router.Group("/api/internal/v1")
	creditRouter.Use(middleware.RouteTag("internal"))
	creditRouter.Use(middleware.InternalServiceAuth(middleware.ScopeCredit))
	creditRouter.Use(middleware.InternalServiceRateLimit(middleware.ScopeCredit))
	{
		creditRouter.PUT("/credit-operations/:event_id", controller.PutCreditOperation)
		creditRouter.GET("/credit-operations/:event_id", controller.GetCreditOperation)
		creditRouter.GET("/credit-accounts/:newapi_user_id", controller.GetCreditAccount)
		creditRouter.GET("/credit-usage/:newapi_user_id", controller.GetCreditUsage)
		creditRouter.GET("/credit-usage/:newapi_user_id/daily", controller.GetCreditUsageDaily)
		// 仅供 doc94 R1 一次性存量 rebase 使用；迁移完成后连同工具一并删除。
		creditRouter.GET("/credit-consumption/:newapi_user_id", controller.GetCreditConsumptionSummary)
	}

	accountRouter := router.Group("/api/internal/v1")
	accountRouter.Use(middleware.RouteTag("internal"))
	accountRouter.Use(middleware.InternalServiceAuth(middleware.ScopeAccount))
	accountRouter.Use(middleware.InternalServiceRateLimit(middleware.ScopeAccount))
	{
		accountRouter.POST("/accounts", controller.PostInternalAccount)
		accountRouter.GET("/accounts/:newapi_user_id", controller.GetInternalAccount)
	}

	// LLM scope 的 group 在 S1-B/S1-C 落地库存与 Relay lease 接口时建立。
	// 它必须是**独立 group**，不得复用上面已有的 scope group。
}
