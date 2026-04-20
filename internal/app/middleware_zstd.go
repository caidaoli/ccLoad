package app

import (
	"bufio"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
)

// zstdEncoderPool 复用 zstd encoder 避免频繁分配。
var zstdEncoderPool = sync.Pool{
	New: func() any {
		enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
		return enc
	},
}

// zstdResponseWriter 包装 gin.ResponseWriter，按需启用 zstd 压缩。
// encoder 采用 lazy 策略：仅首次实际 Write 时 Reset 并挂接，
// 以便 204/304/HEAD/已压缩类型等路径直接旁路而不触发终止帧写入。
type zstdResponseWriter struct {
	gin.ResponseWriter
	encoder *zstd.Encoder
	bypass  bool
	started bool
}

// Unwrap 暴露底层 writer，供 http.ResponseController 等工具使用。
func (w *zstdResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *zstdResponseWriter) markBypass() {
	// Content-Encoding: zstd 仅在 beginCompression 中设置；bypass 路径始终在此之前返回，
	// 因此这里无需清理头，避免误删 handler 预设的其他编码值。
	w.bypass = true
}

func (w *zstdResponseWriter) beginCompression() {
	if w.started {
		return
	}
	h := w.Header()
	h.Set("Content-Encoding", "zstd")
	addVaryAcceptEncoding(h)
	h.Del("Content-Length")
	w.encoder.Reset(w.ResponseWriter)
	w.started = true
}

func (w *zstdResponseWriter) Write(data []byte) (int, error) {
	if w.bypass {
		return w.ResponseWriter.Write(data)
	}
	if !w.started {
		if shouldBypassResponse(w.Header()) {
			w.markBypass()
			return w.ResponseWriter.Write(data)
		}
		w.beginCompression()
	}
	return w.encoder.Write(data)
}

func (w *zstdResponseWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

func (w *zstdResponseWriter) WriteHeader(code int) {
	if !w.bypass {
		// 204/304 必须无 body（RFC 7230 §3.3.3）
		if code == http.StatusNoContent || code == http.StatusNotModified {
			w.markBypass()
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *zstdResponseWriter) WriteHeaderNow() {
	w.ResponseWriter.WriteHeaderNow()
}

func (w *zstdResponseWriter) Flush() {
	if w.bypass {
		w.ResponseWriter.Flush()
		return
	}
	if w.started {
		_ = w.encoder.Flush()
	}
	w.ResponseWriter.Flush()
}

// Hijack 在接管连接前刷新 encoder 并移除 Content-Encoding 头，
// 防止升级后的字节流被下游视为 zstd 数据。
func (w *zstdResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if w.started {
		_ = w.encoder.Flush()
	}
	w.Header().Del("Content-Encoding")
	return w.ResponseWriter.Hijack()
}

// skipExtensions URL 扩展名对应的响应通常已压缩，无需重复压缩。
var skipExtensions = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".ico": true, ".webp": true, ".woff": true, ".woff2": true, ".eot": true,
}

// alreadyCompressedTypes Content-Type 表示响应体已为压缩格式或纯二进制，
// 跳过 zstd 以避免无效 CPU 开销。
var alreadyCompressedTypes = map[string]struct{}{
	"application/zip":              {},
	"application/gzip":             {},
	"application/x-gzip":           {},
	"application/zstd":             {},
	"application/x-zstd":           {},
	"application/x-bzip2":          {},
	"application/x-7z-compressed":  {},
	"application/x-tar":            {},
	"application/x-rar-compressed": {},
	"application/octet-stream":     {},
}

// shouldBypassResponse 根据当前响应头决定是否绕过 zstd 压缩。
func shouldBypassResponse(h http.Header) bool {
	if ce := h.Get("Content-Encoding"); ce != "" && !strings.EqualFold(ce, "zstd") {
		return true
	}
	ct := h.Get("Content-Type")
	if ct == "" {
		return false
	}
	if idx := strings.Index(ct, ";"); idx >= 0 {
		ct = ct[:idx]
	}
	ct = strings.ToLower(strings.TrimSpace(ct))
	if ct == "" {
		return false
	}
	if strings.HasPrefix(ct, "image/") ||
		strings.HasPrefix(ct, "video/") ||
		strings.HasPrefix(ct, "audio/") {
		return true
	}
	_, ok := alreadyCompressedTypes[ct]
	return ok
}

// acceptsZstd 按 token 解析 Accept-Encoding 头，识别 zstd 支持并处理 q=0 显式拒绝。
func acceptsZstd(header string) bool {
	if header == "" {
		return false
	}
	for part := range strings.SplitSeq(header, ",") {
		tok := strings.TrimSpace(part)
		if tok == "" {
			continue
		}
		name, params, _ := strings.Cut(tok, ";")
		if !strings.EqualFold(strings.TrimSpace(name), "zstd") {
			continue
		}
		rejected := false
		for p := range strings.SplitSeq(params, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
			if !ok {
				continue
			}
			if strings.EqualFold(strings.TrimSpace(k), "q") {
				qv := strings.TrimSpace(v)
				if qv == "0" || qv == "0.0" || qv == "0.00" || qv == "0.000" {
					rejected = true
				}
			}
		}
		return !rejected
	}
	return false
}

// addVaryAcceptEncoding 向 Vary 追加 Accept-Encoding，已存在则忽略，避免覆盖下游头。
func addVaryAcceptEncoding(h http.Header) {
	for _, v := range h.Values("Vary") {
		for field := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(field), "Accept-Encoding") {
				return
			}
		}
	}
	h.Add("Vary", "Accept-Encoding")
}

// ZstdMiddleware 返回 gin 中间件，对支持 zstd 的客户端启用响应压缩。
func ZstdMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method == http.MethodHead {
			c.Next()
			return
		}

		if !acceptsZstd(c.GetHeader("Accept-Encoding")) {
			c.Next()
			return
		}

		reqPath := c.Request.URL.Path
		if dot := strings.LastIndex(reqPath, "."); dot >= 0 {
			if skipExtensions[reqPath[dot:]] {
				c.Next()
				return
			}
		}

		if ce := c.Writer.Header().Get("Content-Encoding"); ce != "" && !strings.EqualFold(ce, "zstd") {
			c.Next()
			return
		}

		enc := zstdEncoderPool.Get().(*zstd.Encoder)
		w := &zstdResponseWriter{
			ResponseWriter: c.Writer,
			encoder:        enc,
		}
		c.Writer = w

		defer func() {
			if w.started && !w.bypass {
				_ = enc.Close()
			}
			zstdEncoderPool.Put(enc)
		}()

		c.Next()
	}
}
