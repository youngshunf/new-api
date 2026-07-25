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
func SetInternalRouter(router *gin.Engine) {
	internalRouter := router.Group("/api/internal/v1")
	internalRouter.Use(middleware.RouteTag("internal"))
	internalRouter.Use(middleware.InternalServiceAuth())
	internalRouter.Use(middleware.InternalServiceRateLimit())
	{
		internalRouter.PUT("/credit-operations/:event_id", controller.PutCreditOperation)
		internalRouter.GET("/credit-operations/:event_id", controller.GetCreditOperation)
		internalRouter.GET("/credit-accounts/:newapi_user_id", controller.GetCreditAccount)
		internalRouter.GET("/credit-usage/:newapi_user_id", controller.GetCreditUsage)
		internalRouter.GET("/credit-usage/:newapi_user_id/daily", controller.GetCreditUsageDaily)
		// 仅供 doc94 R1 一次性存量 rebase 使用；迁移完成后连同工具一并删除。
		internalRouter.GET("/credit-consumption/:newapi_user_id", controller.GetCreditConsumptionSummary)
	}
}
