package middleware

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// BodyStorageCleanup 请求体存储清理中间件
// 在请求处理完成后自动清理磁盘/内存缓存
func BodyStorageCleanup() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer common.ReleaseRelayLargeBody(c)
		defer func() {
			if c.Request.MultipartForm != nil {
				if err := c.Request.MultipartForm.RemoveAll(); err != nil {
					logger.LogWarn(c, common.LogText("request_body subsystem=cleanup event=multipart_cleanup_failed"))
				}
			}
			// 请求结束后清理所有解析表单与存储，日志只记稳定事件，不输出正文或临时路径。
			if err := common.CleanupBodyStorage(c); err != nil {
				logger.LogWarn(c, common.LogText("request_body subsystem=cleanup event=body_cleanup_failed"))
			}

			// 清理文件缓存（URL 下载的文件等）
			service.CleanupFileSources(c)
		}()
		c.Next()
	}
}
