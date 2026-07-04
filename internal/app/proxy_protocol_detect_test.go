package app

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"ccLoad/internal/protocol"

	"github.com/gin-gonic/gin"
)

func TestClientRequestMetadataDetectsClientProtocolFromPath(t *testing.T) {
	testCases := []struct {
		name string
		path string
		want protocol.Protocol
	}{
		{"Claude Messages", "/v1/messages", protocol.Anthropic},
		{"Claude Count Tokens", "/v1/messages/count_tokens", protocol.Anthropic},
		{"Codex Responses", "/v1/responses", protocol.Codex},
		{"OpenAI Chat", "/v1/chat/completions", protocol.OpenAI},
		{"OpenAI Completions", "/v1/completions", protocol.OpenAI},
		{"OpenAI Embeddings", "/v1/embeddings", protocol.OpenAI},
		{"OpenAI Images Generations", "/v1/images/generations", protocol.OpenAI},
		{"OpenAI Images Edits", "/v1/images/edits", protocol.OpenAI},
		{"OpenAI Images Variations", "/v1/images/variations", protocol.OpenAI},
		{"Gemini Stream", "/v1beta/models/gemini-pro:streamGenerateContent", protocol.Gemini},
		{"Gemini Generate", "/v1beta/models/gemini-2.5-flash:generateContent", protocol.Gemini},
		{"Gemini Models", "/v1beta/models", protocol.Gemini},
		{"Unknown Path", "/unknown/path", ""},
		{"Empty Path", "", ""},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			target := tc.path
			if target == "" {
				target = "/"
			}
			req := httptest.NewRequest(http.MethodPost, target, nil)
			req.URL.Path = tc.path
			c, _ := newTestContext(t, req)

			got, gotPath := clientRequestMetadata(c)
			if got != tc.want {
				t.Fatalf("clientProtocol = %q, want %q", got, tc.want)
			}
			if gotPath != tc.path {
				t.Fatalf("requestPath = %q, want %q", gotPath, tc.path)
			}
		})
	}
}

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

func TestValidateClientBodyMatchesProtocol_AllowsClaudeMessagesWithSystemRole(t *testing.T) {
	body := []byte(`{
		"model":"kiro-opus",
		"messages":[
			{
				"role":"user",
				"content":[
					{"type":"text","text":"list file","cache_control":{"type":"ephemeral"}}
				]
			},
			{
				"role":"system",
				"content":"SessionStart:startup hook success"
			}
		],
		"system":[
			{"type":"text","text":"You are Claude Code","cache_control":{"type":"ephemeral"}}
		],
		"max_tokens":32000,
		"thinking":{"type":"adaptive","display":"summarized"},
		"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]},
		"output_config":{"effort":"high"},
		"stream":true
	}`)

	if err := validateClientBodyMatchesProtocol(protocol.Anthropic, body); err != nil {
		t.Fatalf("expected Claude Messages body to pass validation, got %v", err)
	}
}
