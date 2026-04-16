package protocol_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"ccLoad/internal/protocol"
	"ccLoad/internal/protocol/builtin"
)

func TestRegistryCodexToolNameRoundTrip(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	longName := "a_very_long_tool_name_that_exceeds_sixty_four_characters_limit_here_test"

	t.Run("openai stream restores original tool name", func(t *testing.T) {
		rawReq := []byte(fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"%s","arguments":"{}"}}]}],"tools":[{"type":"function","function":{"name":"%s","parameters":{"type":"object"}}}]}`, longName, longName))
		translatedReq, err := reg.TranslateRequest(protocol.OpenAI, protocol.Codex, "gpt-5-codex", rawReq, true)
		if err != nil {
			t.Fatalf("TranslateRequest failed: %v", err)
		}
		shortName := mustCodexShortToolName(t, translatedReq)
		if shortName == longName || len(shortName) > 64 {
			t.Fatalf("expected shortened codex tool name, got %q", shortName)
		}

		resp := []byte(fmt.Sprintf("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"%s\",\"arguments\":{\"q\":\"go\"}}}\n\n", shortName))
		out, err := reg.TranslateResponseStream(context.Background(), protocol.Codex, protocol.OpenAI, "gpt-4o", rawReq, translatedReq, resp, new(any))
		if err != nil {
			t.Fatalf("TranslateResponseStream failed: %v", err)
		}
		if len(out) == 0 || !strings.Contains(string(out[0]), longName) {
			t.Fatalf("expected restored original tool name, got %#v", out)
		}
	})

	t.Run("gemini nonstream restores original tool name", func(t *testing.T) {
		rawReq := []byte(fmt.Sprintf(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"%s","args":{"q":"go"}}}]}],"tools":[{"functionDeclarations":[{"name":"%s","parameters":{"type":"object"}}]}]}`, longName, longName))
		translatedReq, err := reg.TranslateRequest(protocol.Gemini, protocol.Codex, "gpt-5-codex", rawReq, false)
		if err != nil {
			t.Fatalf("TranslateRequest failed: %v", err)
		}
		shortName := mustCodexShortToolName(t, translatedReq)
		resp := []byte(fmt.Sprintf(`{"id":"resp_1","object":"response","status":"completed","model":"gpt-5-codex","output":[{"type":"function_call","call_id":"call_1","name":"%s","arguments":{"q":"go"}}]}`, shortName))
		out, err := reg.TranslateResponseNonStream(context.Background(), protocol.Codex, protocol.Gemini, "gemini-2.5-pro", rawReq, translatedReq, resp)
		if err != nil {
			t.Fatalf("TranslateResponseNonStream failed: %v", err)
		}
		if !strings.Contains(string(out), longName) {
			t.Fatalf("expected restored original tool name, got %s", out)
		}
	})

	t.Run("anthropic nonstream restores original tool name", func(t *testing.T) {
		rawReq := []byte(fmt.Sprintf(`{"model":"claude-3-5-sonnet","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"%s","input":{"q":"go"}}]}],"tools":[{"name":"%s","input_schema":{"type":"object"}}]}`, longName, longName))
		translatedReq, err := reg.TranslateRequest(protocol.Anthropic, protocol.Codex, "gpt-5-codex", rawReq, false)
		if err != nil {
			t.Fatalf("TranslateRequest failed: %v", err)
		}
		shortName := mustCodexShortToolName(t, translatedReq)
		resp := []byte(fmt.Sprintf(`{"id":"resp_1","object":"response","status":"completed","model":"gpt-5-codex","output":[{"type":"function_call","call_id":"call_1","name":"%s","arguments":{"q":"go"}}]}`, shortName))
		out, err := reg.TranslateResponseNonStream(context.Background(), protocol.Codex, protocol.Anthropic, "claude-3-5-sonnet", rawReq, translatedReq, resp)
		if err != nil {
			t.Fatalf("TranslateResponseNonStream failed: %v", err)
		}
		if !strings.Contains(string(out), longName) {
			t.Fatalf("expected restored original tool name, got %s", out)
		}
	})
}

func mustCodexShortToolName(t *testing.T, translatedReq []byte) string {
	t.Helper()

	var payload struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(translatedReq, &payload); err != nil {
		t.Fatalf("unmarshal codex request: %v", err)
	}
	if len(payload.Tools) == 0 || payload.Tools[0].Name == "" {
		t.Fatalf("expected codex tools in translated request, got %s", translatedReq)
	}
	return payload.Tools[0].Name
}
