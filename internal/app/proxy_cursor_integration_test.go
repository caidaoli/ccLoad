package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"ccLoad/internal/cursorauth"
	"ccLoad/internal/model"
)

func TestProxy_CursorOAuthUsesCLIInsteadOfHTTP(t *testing.T) {
	t.Parallel()

	upstreamHits := 0
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits++
		w.WriteHeader(http.StatusTeapot)
	}))
	defer upstream.Close()

	credentialJSON, err := (&cursorauth.Credential{AccessToken: "tok", Email: "user@example.com"}).JSON()
	if err != nil {
		t.Fatalf("encode cursor credential: %v", err)
	}
	env := setupProxyTestEnv(t, []testChannel{{
		name: "cursor-cli", upstreamProtocol: "anthropic", models: "claude-sonnet-5",
		authType: model.AuthTypeCursorOAuth, oauthCredential: credentialJSON,
	}}, map[int]string{0: upstream.URL})
	runner := &fakeCursorRunner{text: "ok from cli", usage: &cursorauth.Usage{
		InputTokens: 11, OutputTokens: 7, CacheReadTokens: 5, CacheWriteTokens: 3,
		TotalTokens: 26, ReasoningTokens: 2,
	}}
	env.server.cursorRunner = runner

	response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model":      "claude-sonnet-5",
		"max_tokens": 16,
		"messages":   []any{map[string]any{"role": "user", "content": "hello"}},
	}, map[string]string{"anthropic-version": "2023-06-01"})
	if response.Code != http.StatusOK {
		t.Fatalf("proxy status = %d body = %s", response.Code, response.Body.String())
	}
	if upstreamHits != 0 {
		t.Fatalf("Cursor OAuth must not HTTP-forward, hits = %d", upstreamHits)
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	content, _ := payload["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("body = %s", response.Body.String())
	}
	block, _ := content[0].(map[string]any)
	if block["text"] != "ok from cli" {
		t.Fatalf("text = %#v", block["text"])
	}
	if runner.model != "claude-sonnet-5" {
		t.Fatalf("model = %q", runner.model)
	}
	entry := waitForProxyLog(t, env, "claude-sonnet-5")
	if entry.InputTokens != 11 || entry.OutputTokens != 7 || entry.CacheReadInputTokens != 5 ||
		entry.CacheCreationInputTokens != 3 || entry.Cache5mInputTokens != 3 || entry.ReasoningTokens != 2 {
		t.Fatalf("logged usage = in:%d out:%d cache_read:%d cache_write:%d cache_5m:%d reasoning:%d",
			entry.InputTokens, entry.OutputTokens, entry.CacheReadInputTokens,
			entry.CacheCreationInputTokens, entry.Cache5mInputTokens, entry.ReasoningTokens)
	}
}

func TestProxy_CursorOAuthSupportsResponses(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		name := "non-stream"
		if streaming {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			upstreamHits := 0
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamHits++
				w.WriteHeader(http.StatusTeapot)
			}))
			defer upstream.Close()

			credentialJSON, err := (&cursorauth.Credential{
				APIKey: "cursor-user-api-key", AccessToken: "cursor-access-token", Email: "user@example.com",
			}).JSON()
			if err != nil {
				t.Fatalf("encode cursor credential: %v", err)
			}
			env := setupProxyTestEnv(t, []testChannel{{
				name: "cursor-responses", upstreamProtocol: "openai", models: "composer-2.5",
				authType: model.AuthTypeCursorOAuth, oauthCredential: credentialJSON,
			}}, map[int]string{0: upstream.URL})
			if streaming {
				env.server.configService.cache["debug_log_enabled"] = &model.SystemSetting{
					Key: "debug_log_enabled", Value: "true",
				}
			}
			runner := &fakeCursorRunner{text: "MacBook M5 answer", usage: &cursorauth.Usage{
				InputTokens: 11, OutputTokens: 7, CacheReadTokens: 5, CacheWriteTokens: 3,
			}}
			env.server.cursorRunner = runner

			response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
				"model": "composer-2.5",
				"input": []any{
					map[string]any{"role": "system", "content": "使用中文回复"},
					map[string]any{"role": "user", "content": []any{
						map[string]any{"type": "input_text", "text": "macbook m5有几款\n\n"},
					}},
				},
				"temperature": "[undefined]", "top_p": "[undefined]",
				"max_output_tokens": "[undefined]", "instructions": "[undefined]",
				"tools": "[undefined]", "tool_choice": "[undefined]", "stream": streaming,
			}, nil)
			if response.Code != http.StatusOK {
				t.Fatalf("proxy status = %d body = %s", response.Code, response.Body.String())
			}
			if upstreamHits != 0 {
				t.Fatalf("Cursor OAuth must not HTTP-forward, hits = %d", upstreamHits)
			}
			if !strings.Contains(runner.prompt, "使用中文回复") || !strings.Contains(runner.prompt, "macbook m5有几款") {
				t.Fatalf("translated prompt lost Responses messages: %q", runner.prompt)
			}
			if !strings.Contains(runner.prompt, "[undefined]") {
				t.Fatalf("caller-provided prompt content was rewritten: %q", runner.prompt)
			}
			if streaming {
				entry := waitForProxyLog(t, env, "composer-2.5")
				debugLog, err := env.store.GetDebugLogByLogID(context.Background(), entry.ID)
				if err != nil || debugLog == nil {
					t.Fatalf("load Cursor Responses debug log: debug=%+v err=%v", debugLog, err)
				}
				if !debugLog.ProtocolTransformed || !bytes.Equal(debugLog.TranslatedRespBody, response.Body.Bytes()) {
					t.Fatalf("translated debug response must match client body:\ndebug=%s\nclient=%s",
						debugLog.TranslatedRespBody, response.Body.Bytes())
				}
			}

			if streaming {
				var delta, completed bool
				for _, block := range bytes.Split(response.Body.Bytes(), []byte("\n\n")) {
					eventType, data := parseSSEEventChunk(block)
					payload, ok := decodeSSEPayload(data)
					if !ok {
						continue
					}
					switch eventType {
					case "response.output_text.delta":
						delta = payload["delta"] == "MacBook M5 answer"
					case "response.completed":
						completed = true
					}
				}
				if !delta || !completed {
					t.Fatalf("invalid Responses stream: %s", response.Body.String())
				}
				return
			}

			var payload struct {
				Object string `json:"object"`
				Output []struct {
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"output"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode Responses body: %v body=%s", err, response.Body.String())
			}
			if payload.Object != "response" || len(payload.Output) == 0 ||
				len(payload.Output[0].Content) == 0 || payload.Output[0].Content[0].Text != "MacBook M5 answer" {
				t.Fatalf("invalid Responses body: %s", response.Body.String())
			}
		})
	}
}
