package app

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"ccLoad/internal/protocol"

	"github.com/gin-gonic/gin"
)

func TestClientRequestMetadataFallbackDoesNotUseBodyShapeAsProtocol(t *testing.T) {
	body := []byte(`{
		"model":"mimo-v2.5",
		"messages":[{"role":"user","content":"hello"}],
		"response_format":{"type":"json_object"},
		"stream_options":{"include_usage":true},
		"prompt_cache_key":"cache-key-1"
	}`)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c, _ := newTestContext(t, req)

	clientProtocol, requestPath := clientRequestMetadata(c)
	if clientProtocol != protocol.Anthropic {
		t.Fatalf("clientProtocol = %s, want %s", clientProtocol, protocol.Anthropic)
	}
	if requestPath != "/v1/messages" {
		t.Fatalf("requestPath = %q, want /v1/messages", requestPath)
	}
}

func TestClientRequestMetadataUsesCapturedIngressValues(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(captureClientRequestMetadata())
	r.POST("/v1/chat/completions", func(c *gin.Context) {
		c.Request.URL.Path = "/v1/messages"

		clientProtocol, clientPath := clientRequestMetadata(c)
		if clientProtocol != protocol.OpenAI {
			t.Fatalf("clientProtocol = %s, want %s", clientProtocol, protocol.OpenAI)
		}
		if clientPath != "/v1/chat/completions" {
			t.Fatalf("clientPath = %q, want /v1/chat/completions", clientPath)
		}
		c.Status(http.StatusNoContent)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusNoContent, w.Body.String())
	}
}
