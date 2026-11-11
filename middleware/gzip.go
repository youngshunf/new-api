package middleware

import (
	"compress/gzip"
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/retrycontrol"
	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
)

// encodedRequestBody保留输入超限错误，避免解码器把它折叠成截断或格式错误。
type encodedRequestBody struct {
	io.ReadCloser
	limitErr error
}

func (body *encodedRequestBody) Read(p []byte) (int, error) {
	n, err := body.ReadCloser.Read(p)
	if common.IsRequestBodyTooLargeError(err) {
		body.limitErr = err
	}
	return n, err
}

type readCloser struct {
	io.Reader
	input   *encodedRequestBody
	closeFn func() error
}

func (rc *readCloser) Read(p []byte) (int, error) {
	n, err := rc.Reader.Read(p)
	if rc.input.limitErr != nil {
		return n, rc.input.limitErr
	}
	return n, err
}

func (rc *readCloser) Close() error {
	if rc.closeFn != nil {
		return rc.closeFn()
	}
	return nil
}

func DecompressRequestMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if decompressRequest(c) {
			c.Next()
		}
	}
}

// decompressRequest只安装解码，不推进处理链，让认证预算能在业务读体前完成暂存。
func decompressRequest(c *gin.Context) bool {
	if c.Request.Body == nil || c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead {
		return true
	}
	maxBytes := common.RequestBodyLimitBytes(c)
	// 压缩输入与解压输出分别计数，未知长度不能绕过。
	origBody := &encodedRequestBody{ReadCloser: http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)}
	wrapMaxBytes := func(body io.ReadCloser) io.ReadCloser {
		return http.MaxBytesReader(c.Writer, body, maxBytes)
	}
	decompressed := false
	switch c.GetHeader("Content-Encoding") {
	case "gzip":
		gzipReader, err := gzip.NewReader(origBody)
		if err != nil {
			_ = origBody.Close()
			if common.IsRequestBodyTooLargeError(err) {
				retrycontrol.WriteHeaders(c, retrycontrol.New(retrycontrol.ReasonRequestInvalid, retrycontrol.DispatchStateNotDispatched))
			}
			c.AbortWithStatus(invalidRequestStatus(err))
			return false
		}
		c.Request.Body = wrapMaxBytes(&readCloser{
			Reader: gzipReader,
			input:  origBody,
			closeFn: func() error {
				_ = gzipReader.Close()
				return origBody.Close()
			},
		})
		decompressed = true
	case "br":
		c.Request.Body = wrapMaxBytes(&readCloser{Reader: brotli.NewReader(origBody), input: origBody, closeFn: origBody.Close})
		decompressed = true
	case "zstd":
		// 解压窗口独立封顶8MiB；关闭异步预读，认证与大请求占用均在首次读体前完成。
		reader, err := zstd.NewReader(origBody,
			zstd.WithDecoderMaxWindow(8<<20),
			zstd.WithDecoderMaxMemory(uint64(maxBytes)),
			zstd.WithDecoderConcurrency(1),
		)
		if err != nil {
			_ = origBody.Close()
			c.AbortWithStatus(http.StatusBadRequest)
			return false
		}
		c.Request.Body = wrapMaxBytes(&readCloser{
			Reader: reader,
			input:  origBody,
			closeFn: func() error {
				reader.Close()
				return origBody.Close()
			},
		})
		decompressed = true
	default:
		c.Request.Body = origBody
	}
	if decompressed {
		c.Request.Header.Del("Content-Encoding")
		c.Request.Header.Del("Content-Length")
		c.Request.ContentLength = -1
	}
	return true
}
