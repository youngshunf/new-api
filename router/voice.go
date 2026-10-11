package router

import (
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/gin-gonic/gin"
)

// relayV1继承普通TokenAuth和限流，材料取件仅由短期单次令牌授权。
func SetVoiceRouter(engine *gin.Engine, relayV1 *gin.RouterGroup) {
	voices := relayV1.Group("/audio/voices", middleware.PrepareVoiceResource())
	voices.POST("", controller.CreateVoice)
	voices.GET("", controller.QueryVoice)
	voices.GET("/:voice_ref", controller.QueryVoice)
	voices.GET("/:voice_ref/preview", controller.VoicePreview)
	voices.DELETE("/:voice_ref", controller.DeleteVoice)
	engine.GET("/v1/audio/voice-materials/:token", controller.VoiceMaterial)
}
