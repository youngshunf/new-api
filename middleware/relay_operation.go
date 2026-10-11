package middleware

import (
	"errors"
	"net/http"
	"slices"

	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

// operationRoutes 只认Gin已匹配的受信路由及HTTP方法；body不能选择或扩大用途。
var operationRoutes = map[string][]string{
	"chat.completions":   {"POST /v1/chat/completions"},
	"vision":             {"POST /v1/chat/completions"},
	"responses":          {"POST /v1/responses"},
	"messages":           {"POST /v1/messages"},
	"embedding":          {"POST /v1/embeddings", "POST /v1/engines/:model/embeddings"},
	"rerank":             {"POST /v1/rerank"},
	"image.generate":     {"POST /v1/images/generations"},
	"image.edit":         {"POST /v1/images/edits", "POST /v1/edits"},
	"audio.transcribe":   {"POST /v1/audio/transcriptions"},
	"audio.translate":    {"POST /v1/audio/translations"},
	"audio.tts":          {"POST /v1/audio/speech"},
	"audio.voice_create": {"POST /v1/audio/voices"},
	"audio.voice_query":  {"GET /v1/audio/voices", "GET /v1/audio/voices/:voice_ref", "GET /v1/audio/voices/:voice_ref/preview"},
	"audio.voice_delete": {"DELETE /v1/audio/voices/:voice_ref"},
	"video.generate":     {"POST /v1/videos", "POST /v1/video/generations", "GET /v1/videos/:task_id", "GET /v1/videos/:task_id/content", "GET /v1/video/generations/:task_id"},
}

func authorizeTokenOperation(c *gin.Context, token *model.Token) error {
	route := c.Request.Method + " " + c.FullPath()
	allowed := token.Operation == ""
	if token.Operation == "" && token.ExternalLeaseId != nil && *token.ExternalLeaseId != "" {
		// 受管Runtime仅授权普通推理；持久音色必须另取带用途的call租约。
		for _, operation := range []string{"audio.voice_create", "audio.voice_query", "audio.voice_delete"} {
			if slices.Contains(operationRoutes[operation], route) {
				allowed = false
				break
			}
		}
	}
	if token.Operation != "" {
		allowed = route == "GET /v1/models" || route == "GET /v1/models/:model" || slices.Contains(operationRoutes[token.Operation], route)
	}
	if allowed {
		return nil
	}
	err := errors.New("relay lease operation does not allow this endpoint")
	abortWithOpenAiMessage(c, http.StatusForbidden, err.Error())
	return err
}
