package common

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"strings"

	"github.com/QuantumNous/new-api/constant"
	"github.com/gin-gonic/gin"
)

const RelayBodyBudgetKey = "relay_body_budget"
const relayLargeBodyKey = "relay_large_body_held"
const RelayMemoryBodyBytes int64 = 8 << 20

var ErrLargeRequestBusy = errors.New("large_request_busy")

// 单份占用覆盖正文解码、插件重建与响应全程，不随某一份存储关闭而释放。
var relayLargeBodies = make(chan struct{}, 1)

// RelayRequestBodyLimit严格解析实际MIME，畸形multipart不能获得较大额度。
func RelayRequestBodyLimit(contentType string) (int64, error) {
	if strings.TrimSpace(contentType) == "" {
		return 256 << 20, nil
	}
	mediaType, parameters, err := mime.ParseMediaType(contentType)
	if err != nil {
		return 0, err
	}
	if mediaType != "multipart/form-data" {
		return 256 << 20, nil
	}
	writer := multipart.NewWriter(io.Discard)
	if err := writer.SetBoundary(parameters["boundary"]); err != nil {
		return 0, err
	}
	return 1 << 30, nil
}

// RequestBodyLimitBytes只消费服务器安装的LLM预算，其余请求保持旧上限。
func RequestBodyLimitBytes(c *gin.Context) int64 {
	if value, ok := c.Get(RelayBodyBudgetKey); ok {
		if limit, ok := value.(int64); ok && limit > 0 {
			return limit
		}
	}
	maxMB := constant.MaxRequestBodyMB
	if maxMB <= 0 {
		maxMB = 128
	}
	return int64(maxMB) << 20
}

// AcquireRelayLargeBody在任何大正文读取前取得进程共用占用；重复获取不另占位。
func AcquireRelayLargeBody(c *gin.Context) error {
	if c.GetBool(relayLargeBodyKey) {
		return nil
	}
	select {
	case relayLargeBodies <- struct{}{}:
		c.Set(relayLargeBodyKey, true)
		return nil
	default:
		return ErrLargeRequestBusy
	}
}

// ReleaseRelayLargeBody仅由请求最外层清理调用，存储替换不能提前放位。
func ReleaseRelayLargeBody(c *gin.Context) {
	if c.GetBool(relayLargeBodyKey) {
		c.Set(relayLargeBodyKey, false)
		<-relayLargeBodies
	}
}

// createRelayBodyStorage不信任小Content-Length；超过有界前缀就升级到强制磁盘。
func createRelayBodyStorage(c *gin.Context, maxBytes int64) (BodyStorage, error) {
	reader := c.Request.Body
	if c.Request.ContentLength < 0 || c.Request.ContentLength > RelayMemoryBodyBytes || c.GetBool(relayLargeBodyKey) {
		if err := AcquireRelayLargeBody(c); err != nil {
			return nil, err
		}
		return newDiskStorageFromReader(reader, maxBytes, GetDiskCachePath())
	}
	prefix, err := io.ReadAll(io.LimitReader(reader, min(maxBytes, RelayMemoryBodyBytes)+1))
	if err != nil {
		return nil, err
	}
	if int64(len(prefix)) > maxBytes {
		return nil, ErrRequestBodyTooLarge
	}
	if int64(len(prefix)) <= RelayMemoryBodyBytes {
		return newMemoryStorage(prefix), nil
	}
	if err := AcquireRelayLargeBody(c); err != nil {
		return nil, err
	}
	return newDiskStorageFromReader(io.MultiReader(bytes.NewReader(prefix), reader), maxBytes, GetDiskCachePath())
}
