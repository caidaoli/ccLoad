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
		{"Codex Alpha Search", "/v1/alpha/search", protocol.Codex},
		{"Codex Direct Alpha Search", "/backend-api/codex/alpha/search", protocol.Codex},
		{"OpenAI Chat", "/v1/chat/completions", protocol.OpenAI},
		{"OpenAI Completions", "/v1/completions", protocol.OpenAI},
		{"OpenAI Embeddings", "/v1/embeddings", protocol.OpenAI},
		{"OpenAI Images Generations", "/v1/images/generations", protocol.OpenAI},
		{"OpenAI Images Edits", "/v1/images/edits", protocol.OpenAI},
		{"OpenAI Images Variations", "/v1/images/variations", protocol.OpenAI},
		{"Gemini Stream", "/v1beta/models/gemini-pro:streamGenerateContent", protocol.Gemini},
		{"Gemini Generate", "/v1beta/models/gemini-2.5-flash:generateContent", protocol.Gemini},
		{"Gemini Models Special Route", "/v1beta/models", ""},
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

func TestNormalizeCodexClientPath_AlphaSearchAliases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path string
		want string
	}{
		{"/v1/alpha/search", "/v1/alpha/search"},
		{"/v1/codex/alpha/search", "/v1/alpha/search"},
		{"/backend-api/codex/alpha/search", "/v1/alpha/search"},
		{"/v1/codex/responses", "/v1/responses"},
		{"/backend-api/codex/responses", "/v1/responses"},
	}
	for _, tc := range tests {
		if got := normalizeCodexClientPath(tc.path); got != tc.want {
			t.Fatalf("normalizeCodexClientPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
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

func TestDashboardProxyMetadataRestoresCanonicalProxyPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(captureDashboardProxyMetadata())
	r.POST("/dashboard/v1/*path", func(c *gin.Context) {
		clientProtocol, clientPath := clientRequestMetadata(c)
		if clientProtocol != protocol.Anthropic {
			t.Fatalf("clientProtocol = %s, want %s", clientProtocol, protocol.Anthropic)
		}
		if clientPath != "/v1/messages" || c.Request.URL.Path != "/v1/messages" {
			t.Fatalf("paths = (%q,%q), want canonical /v1/messages", clientPath, c.Request.URL.Path)
		}
		c.Status(http.StatusNoContent)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/dashboard/v1/messages", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNoContent)
	}
}

func TestRequestHasNonTextContent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		protocol protocol.Protocol
		body     string
		want     bool
	}{
		// Anthropic：content 数组的 type tag，含 tool_result 嵌套（Claude Code 截图最常见位置）。
		{name: "anthropic_image_block", protocol: protocol.Anthropic, body: `{"model":"claude-4","messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBOR"}}]}]}`, want: true},
		{name: "anthropic_document_block", protocol: protocol.Anthropic, body: `{"model":"claude-4","messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"AAAA"}}]}]}`, want: true},
		{name: "anthropic_tool_result_nested_image", protocol: protocol.Anthropic, body: `{"model":"claude-4","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"screenshot"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}]}`, want: true},
		{name: "anthropic_tool_result_nested_text_only", protocol: protocol.Anthropic, body: `{"model":"claude-4","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"file not found"}]}]}]}`, want: false},
		{name: "anthropic_tool_use_not_multimodal", protocol: protocol.Anthropic, body: `{"model":"claude-4","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/x"}}]}]}`, want: false},
		{name: "anthropic_text_mentions_image_word", protocol: protocol.Anthropic, body: `{"model":"claude-4","messages":[{"role":"user","content":"look at this image please"}]}`, want: false},

		// OpenAI：content 可以是数组、单个对象或字符串。
		{name: "openai_image_url", protocol: protocol.OpenAI, body: `{"model":"gpt","messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`, want: true},
		{name: "openai_image_url_single_object_content", protocol: protocol.OpenAI, body: `{"model":"gpt","messages":[{"role":"user","content":{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}}]}`, want: true},
		{name: "openai_video_url", protocol: protocol.OpenAI, body: `{"model":"gpt","messages":[{"role":"user","content":[{"type":"video_url","video_url":{"url":"https://example.com/a.mp4"}}]}]}`, want: true},
		{name: "openai_input_audio", protocol: protocol.OpenAI, body: `{"model":"gpt","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}]}]}`, want: true},
		{name: "openai_file", protocol: protocol.OpenAI, body: `{"model":"gpt","messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"file-123"}}]}]}`, want: true},
		{name: "openai_text_string_content", protocol: protocol.OpenAI, body: `{"model":"gpt","messages":[{"role":"user","content":"hello"}]}`, want: false},
		{name: "openai_text_parts", protocol: protocol.OpenAI, body: `{"model":"gpt","messages":[{"role":"user","content":[{"type":"text","text":"no image here"}]}]}`, want: false},

		// Codex：input 数组的 content type tag；input 本身也可能是字符串。
		{name: "codex_input_image", protocol: protocol.Codex, body: `{"model":"gpt","input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.com/a.png"}]}]}`, want: true},
		{name: "codex_input_file", protocol: protocol.Codex, body: `{"model":"gpt","input":[{"role":"user","content":[{"type":"input_file","file_id":"file-123"}]}]}`, want: true},
		{name: "codex_input_string", protocol: protocol.Codex, body: `{"model":"gpt","input":"just text"}`, want: false},
		{name: "codex_input_text_mentions_file", protocol: protocol.Codex, body: `{"model":"gpt","input":[{"role":"user","content":[{"type":"input_text","text":"read that file for me"}]}]}`, want: false},

		// Gemini：part 没有 type tag，只能按字段存在性判定；camelCase 与 snake_case 双写。
		{name: "gemini_inline_data_camel", protocol: protocol.Gemini, body: `{"contents":[{"parts":[{"text":"describe"},{"inlineData":{"mimeType":"image/png","data":"AAAA"}}]}]}`, want: true},
		{name: "gemini_inline_data_snake", protocol: protocol.Gemini, body: `{"contents":[{"parts":[{"inline_data":{"mimeType":"image/png","data":"AAAA"}}]}]}`, want: true},
		{name: "gemini_file_data_camel", protocol: protocol.Gemini, body: `{"contents":[{"parts":[{"fileData":{"mimeType":"application/pdf","fileUri":"https://example.com/a.pdf"}}]}]}`, want: true},
		{name: "gemini_file_data_snake", protocol: protocol.Gemini, body: `{"contents":[{"parts":[{"file_data":{"mimeType":"application/pdf","fileUri":"https://example.com/a.pdf"}}]}]}`, want: true},
		{name: "gemini_text_only", protocol: protocol.Gemini, body: `{"contents":[{"parts":[{"text":"hello"}]}]}`, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := requestHasNonTextContent(tt.protocol, []byte(tt.body)); got != tt.want {
				t.Fatalf("requestHasNonTextContent(%s, %s) = %v, want %v", tt.protocol, tt.body, got, tt.want)
			}
		})
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
