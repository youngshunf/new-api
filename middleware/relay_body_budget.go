package middleware

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/retrycontrol"
	"github.com/gin-gonic/gin"
)

// AuthenticatedRelayBodyBudget必须排在既有认证之后、任何解压和业务读体之前。
func AuthenticatedRelayBodyBudget() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body == nil || c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead {
			c.Next()
			return
		}
		limit, err := common.RelayRequestBodyLimit(c.GetHeader("Content-Type"))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_content_type", "type": "invalid_request_error", "message": "invalid Content-Type"}})
			return
		}
		c.Set(common.RelayBodyBudgetKey, limit)
		if c.Request.ContentLength < 0 || c.Request.ContentLength > common.RelayMemoryBodyBytes || strings.TrimSpace(c.GetHeader("Content-Encoding")) != "" {
			if err := common.AcquireRelayLargeBody(c); err != nil {
				c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": gin.H{"code": "large_request_busy", "type": "rate_limit_error", "message": "large request processing is busy"}})
				return
			}
		}
		if !decompressRequest(c) {
			return
		}
		// 认证后的唯一预接收点也覆盖自行读体的Adapter，伪小长度不能绕过占用。
		storage, err := common.GetBodyStorage(c)
		if err != nil {
			if errors.Is(err, common.ErrLargeRequestBusy) {
				c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": gin.H{"code": "large_request_busy", "type": "rate_limit_error", "message": "large request processing is busy"}})
			} else if common.IsRequestBodyTooLargeError(err) {
				retrycontrol.WriteHeaders(c, retrycontrol.New(retrycontrol.ReasonRequestInvalid, retrycontrol.DispatchStateNotDispatched))
				c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": gin.H{"code": "request_body_too_large", "type": "invalid_request_error", "message": "request body exceeds limit"}})
			} else {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "request_body_unavailable", "type": "invalid_request_error", "message": "request body could not be received"}})
			}
			return
		}
		c.Request.Body = io.NopCloser(storage)
		c.Request.ContentLength = storage.Size()
		c.Next()
	}
}
