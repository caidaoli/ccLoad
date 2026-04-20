package app

import (
	"bytes"
	"io"
	"net/http"
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
