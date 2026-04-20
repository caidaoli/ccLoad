package app

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
)

func TestZstdMiddleware_RemovesContentLengthForCompressedResponses(t *testing.T) {
	t.Parallel()

	r := gin.New()
	web := r.Group("/web", ZstdMiddleware())
	web.GET("/app.js", func(c *gin.Context) {
		c.Data(http.StatusOK, "application/javascript; charset=utf-8", []byte("console.log('x')"))
	})

	req := newRequest(http.MethodGet, "/web/app.js", nil)
	req.Header.Set("Accept-Encoding", "zstd")

	w := serveHTTP(t, r, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d", w.Code, http.StatusOK)
	}
	if got := w.Header().Get("Content-Encoding"); got != "zstd" {
		t.Fatalf("Content-Encoding=%q, want %q", got, "zstd")
	}
	if got := w.Header().Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length=%q, want empty for compressed response", got)
	}
}

func TestZstdMiddleware_ResponseBodyIsValidZstd(t *testing.T) {
	t.Parallel()

	const payload = "hello zstd world"
	r := gin.New()
	r.Group("/web", ZstdMiddleware()).GET("/f.js", func(c *gin.Context) {
		c.Data(http.StatusOK, "application/javascript", []byte(payload))
	})

	req := newRequest(http.MethodGet, "/web/f.js", nil)
	req.Header.Set("Accept-Encoding", "zstd")

	w := serveHTTP(t, r, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", w.Code)
	}

	dec, err := zstd.NewReader(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatalf("zstd.NewReader: %v", err)
	}
	defer dec.Close()

	got, err := io.ReadAll(dec)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("decompressed=%q, want %q", got, payload)
	}
}

func TestZstdMiddleware_NoZstdWhenNotAccepted(t *testing.T) {
	t.Parallel()

	r := gin.New()
	r.Group("/web", ZstdMiddleware()).GET("/f.js", func(c *gin.Context) {
		c.Data(http.StatusOK, "application/javascript", []byte("plain"))
	})

	req := newRequest(http.MethodGet, "/web/f.js", nil)
	// no Accept-Encoding: zstd
	w := serveHTTP(t, r, req)
	if got := w.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding=%q, want empty", got)
	}
}

func TestZstdMiddleware_SkipsAlreadyCompressedExtensions(t *testing.T) {
	t.Parallel()

	r := gin.New()
	r.Group("/web", ZstdMiddleware()).GET("/img.png", func(c *gin.Context) {
		c.Data(http.StatusOK, "image/png", []byte{0x89, 0x50, 0x4e, 0x47})
	})

	req := newRequest(http.MethodGet, "/web/img.png", nil)
	req.Header.Set("Accept-Encoding", "zstd")
	w := serveHTTP(t, r, req)
	if got := w.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding=%q, want empty for png", got)
	}
}

// TestZstdMiddleware_FlushForwards 验证 Flush() 会透传到底层 ResponseWriter，
// 保证 HTTP/2 与 QUIC 下流式响应帧完整、不触发协议错误。
func TestZstdMiddleware_FlushForwards(t *testing.T) {
	t.Parallel()

	r := gin.New()
	r.Group("/web", ZstdMiddleware()).GET("/stream", func(c *gin.Context) {
		c.Writer.Header().Set("Content-Type", "text/plain")
		_, _ = c.Writer.Write([]byte("chunk1"))
		c.Writer.Flush()
		_, _ = c.Writer.Write([]byte("chunk2"))
		c.Writer.Flush()
	})

	req := newRequest(http.MethodGet, "/web/stream", nil)
	req.Header.Set("Accept-Encoding", "zstd")
	w := serveHTTP(t, r, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", w.Code)
	}
	if !w.Flushed {
		t.Fatalf("expected underlying ResponseRecorder to be flushed at least once")
	}
	if got := w.Header().Get("Content-Encoding"); got != "zstd" {
		t.Fatalf("Content-Encoding=%q, want zstd", got)
	}
}

// TestZstdMiddleware_Skip204NoBody 验证 204 状态响应不带 Content-Encoding 且 body 为空。
func TestZstdMiddleware_Skip204NoBody(t *testing.T) {
	t.Parallel()

	r := gin.New()
	r.Group("/web", ZstdMiddleware()).GET("/nc", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	req := newRequest(http.MethodGet, "/web/nc", nil)
	req.Header.Set("Accept-Encoding", "zstd")
	w := serveHTTP(t, r, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status=%d, want 204", w.Code)
	}
	if got := w.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding=%q, want empty for 204", got)
	}
	if w.Body.Len() != 0 {
		t.Fatalf("body len=%d, want 0 for 204", w.Body.Len())
	}
}

// TestZstdMiddleware_SkipHEADRequest 验证 HEAD 请求不会被包装，
// 响应 body 保持原始字节且不带 Content-Encoding。
func TestZstdMiddleware_SkipHEADRequest(t *testing.T) {
	t.Parallel()

	const payload = "hello"
	r := gin.New()
	grp := r.Group("/web", ZstdMiddleware())
	grp.GET("/doc", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/plain", []byte(payload))
	})
	grp.HEAD("/doc", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/plain", []byte(payload))
	})

	req := newRequest(http.MethodHead, "/web/doc", nil)
	req.Header.Set("Accept-Encoding", "zstd")
	w := serveHTTP(t, r, req)

	if got := w.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding=%q, want empty for HEAD", got)
	}
	// httptest.ResponseRecorder 不会像真实 http server 那样自动丢弃 HEAD body，
	// 只断言 body 未被 zstd 编码（即保留明文），确认中间件已跳过包装。
	if got := w.Body.String(); got != payload {
		t.Fatalf("HEAD body=%q, want %q (raw, no zstd)", got, payload)
	}
}

// TestZstdMiddleware_SkipWhenContentEncodingPreset 验证已预设非 zstd 的 Content-Encoding 时不重复编码。
func TestZstdMiddleware_SkipWhenContentEncodingPreset(t *testing.T) {
	t.Parallel()

	const payload = "preset gzip body"
	r := gin.New()
	r.Group("/web", ZstdMiddleware()).GET("/g", func(c *gin.Context) {
		c.Header("Content-Encoding", "gzip")
		c.Data(http.StatusOK, "application/octet-stream", []byte(payload))
	})

	req := newRequest(http.MethodGet, "/web/g", nil)
	req.Header.Set("Accept-Encoding", "zstd")
	w := serveHTTP(t, r, req)

	if got := w.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding=%q, want gzip preserved", got)
	}
	if got := w.Body.String(); got != payload {
		t.Fatalf("body=%q, want %q (must not be re-encoded)", got, payload)
	}
}

// TestZstdMiddleware_PanicReleasesEncoder 验证 handler panic 后 encoder 仍被归还池，
// 下次请求能正确压缩而非失败或触发竞态。
func TestZstdMiddleware_PanicReleasesEncoder(t *testing.T) {
	t.Parallel()

	r := gin.New()
	r.Use(gin.Recovery())
	grp := r.Group("/web", ZstdMiddleware())
	grp.GET("/boom", func(c *gin.Context) {
		panic("boom")
	})
	grp.GET("/ok", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/plain", []byte("after-panic"))
	})

	reqPanic := newRequest(http.MethodGet, "/web/boom", nil)
	reqPanic.Header.Set("Accept-Encoding", "zstd")
	wPanic := serveHTTP(t, r, reqPanic)
	if wPanic.Code != http.StatusInternalServerError {
		t.Fatalf("panic request status=%d, want 500", wPanic.Code)
	}

	reqOk := newRequest(http.MethodGet, "/web/ok", nil)
	reqOk.Header.Set("Accept-Encoding", "zstd")
	wOk := serveHTTP(t, r, reqOk)
	if wOk.Code != http.StatusOK {
		t.Fatalf("post-panic status=%d, want 200", wOk.Code)
	}
	if got := wOk.Header().Get("Content-Encoding"); got != "zstd" {
		t.Fatalf("post-panic Content-Encoding=%q, want zstd", got)
	}
	dec, err := zstd.NewReader(bytes.NewReader(wOk.Body.Bytes()))
	if err != nil {
		t.Fatalf("zstd.NewReader: %v", err)
	}
	defer dec.Close()
	body, err := io.ReadAll(dec)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	if string(body) != "after-panic" {
		t.Fatalf("decompressed=%q, want after-panic", body)
	}
}

// TestZstdMiddleware_VaryAppends 验证 Vary 头会追加 Accept-Encoding，而不是覆盖下游已设置的值。
func TestZstdMiddleware_VaryAppends(t *testing.T) {
	t.Parallel()

	r := gin.New()
	r.Group("/web", ZstdMiddleware()).GET("/v", func(c *gin.Context) {
		c.Writer.Header().Add("Vary", "Cookie")
		c.Data(http.StatusOK, "text/plain", []byte("vary-body"))
	})

	req := newRequest(http.MethodGet, "/web/v", nil)
	req.Header.Set("Accept-Encoding", "zstd")
	w := serveHTTP(t, r, req)

	hasCookie := false
	hasAE := false
	for _, v := range w.Header().Values("Vary") {
		for field := range strings.SplitSeq(v, ",") {
			f := strings.TrimSpace(field)
			if strings.EqualFold(f, "Cookie") {
				hasCookie = true
			}
			if strings.EqualFold(f, "Accept-Encoding") {
				hasAE = true
			}
		}
	}
	if !hasCookie || !hasAE {
		t.Fatalf("Vary=%v, want both Cookie and Accept-Encoding", w.Header().Values("Vary"))
	}
}

// TestZstdMiddleware_AcceptEncodingQ0Rejected 验证 q=0 的显式拒绝会阻止 zstd 启用。
func TestZstdMiddleware_AcceptEncodingQ0Rejected(t *testing.T) {
	t.Parallel()

	r := gin.New()
	r.Group("/web", ZstdMiddleware()).GET("/q", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/plain", []byte("plain"))
	})

	req := newRequest(http.MethodGet, "/web/q", nil)
	req.Header.Set("Accept-Encoding", "zstd;q=0, gzip")
	w := serveHTTP(t, r, req)

	if got := w.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding=%q, want empty when q=0", got)
	}
	if got := w.Body.String(); got != "plain" {
		t.Fatalf("body=%q, want plain", got)
	}
}

// TestZstdMiddleware_SkipAlreadyCompressedContentType 验证已压缩的 Content-Type 不再被 zstd 编码。
func TestZstdMiddleware_SkipAlreadyCompressedContentType(t *testing.T) {
	t.Parallel()

	r := gin.New()
	r.Group("/web", ZstdMiddleware()).GET("/zip", func(c *gin.Context) {
		c.Data(http.StatusOK, "application/zip", []byte{0x50, 0x4b, 0x03, 0x04})
	})

	req := newRequest(http.MethodGet, "/web/zip", nil)
	req.Header.Set("Accept-Encoding", "zstd")
	w := serveHTTP(t, r, req)

	if got := w.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding=%q, want empty for application/zip", got)
	}
	expected := []byte{0x50, 0x4b, 0x03, 0x04}
	if !bytes.Equal(w.Body.Bytes(), expected) {
		t.Fatalf("body=%x, want %x (raw zip bytes)", w.Body.Bytes(), expected)
	}
}

// TestZstdMiddleware_LargeResponseChunked 验证多次分块写入（>64KiB）后解压内容完整。
func TestZstdMiddleware_LargeResponseChunked(t *testing.T) {
	t.Parallel()

	// 构造 4 × 32KiB = 128KiB 数据，确保跨过 zstd 内部缓冲边界
	const chunkSize = 32 * 1024
	const chunks = 4
	const total = chunkSize * chunks
	expected := make([]byte, 0, total)
	chunk := make([]byte, chunkSize)
	for i := range chunk {
		chunk[i] = byte(i % 251)
	}
	for range chunks {
		expected = append(expected, chunk...)
	}

	r := gin.New()
	r.Group("/web", ZstdMiddleware()).GET("/big", func(c *gin.Context) {
		c.Writer.Header().Set("Content-Type", "text/plain")
		c.Writer.WriteHeader(http.StatusOK)
		for i := range chunks {
			if _, err := c.Writer.Write(chunk); err != nil {
				t.Errorf("chunk %d write err: %v", i, err)
				return
			}
		}
	})

	req := newRequest(http.MethodGet, "/web/big", nil)
	req.Header.Set("Accept-Encoding", "zstd")
	w := serveHTTP(t, r, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", w.Code)
	}
	dec, err := zstd.NewReader(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatalf("zstd.NewReader: %v", err)
	}
	defer dec.Close()
	got, err := io.ReadAll(dec)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	if !bytes.Equal(got, expected) {
		t.Fatalf("decompressed len=%d, want %d; first mismatch at byte %d", len(got), len(expected), firstDiff(got, expected))
	}
}

func firstDiff(a, b []byte) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}
	if len(a) != len(b) {
		return n
	}
	return -1
}
