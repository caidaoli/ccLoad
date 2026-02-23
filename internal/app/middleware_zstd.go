package app

import (
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
)

// zstdEncoderPool 复用 zstd encoder 避免频繁分配
var zstdEncoderPool = sync.Pool{
	New: func() any {
		enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
		return enc
	},
}

// zstdResponseWriter 包装 gin.ResponseWriter，透明压缩响应体
type zstdResponseWriter struct {
	gin.ResponseWriter
	encoder *zstd.Encoder
}

func (w *zstdResponseWriter) Write(data []byte) (int, error) {
	return w.encoder.Write(data)
}

func (w *zstdResponseWriter) WriteString(s string) (int, error) {
	return w.encoder.Write([]byte(s))
}

// skipExtensions 已压缩的文件类型，不需要再压缩
var skipExtensions = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".ico": true, ".webp": true, ".woff": true, ".woff2": true, ".eot": true,
}

// ZstdMiddleware 返回 gin 中间件，对支持 zstd 的客户端启用 zstd 压缩
func ZstdMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 检查客户端是否支持 zstd
		if !strings.Contains(c.GetHeader("Accept-Encoding"), "zstd") {
			c.Next()
			return
		}

		// 跳过已压缩的文件类型
		reqPath := c.Request.URL.Path
		if dot := strings.LastIndex(reqPath, "."); dot >= 0 {
			if skipExtensions[reqPath[dot:]] {
				c.Next()
				return
			}
		}

		enc := zstdEncoderPool.Get().(*zstd.Encoder)
		enc.Reset(c.Writer)

		c.Header("Content-Encoding", "zstd")
		c.Header("Vary", "Accept-Encoding")
		// 压缩后长度未知，移除可能被提前设置的 Content-Length
		c.Writer.Header().Del("Content-Length")

		w := &zstdResponseWriter{ResponseWriter: c.Writer, encoder: enc}
		c.Writer = w

		c.Next()

		// flush 并归还 encoder
		enc.Close()
		zstdEncoderPool.Put(enc)
	}
}
