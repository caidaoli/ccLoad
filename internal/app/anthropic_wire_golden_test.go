package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"ccLoad/internal/anthropicauth"
	"ccLoad/internal/model"
)

// anthropic wire 的字节级金标准。
//
// 这一族改写是全产品对字节最敏感的路径：上游按 body 形态做 Claude Code 指纹识别，
// 任何一个字节的差异都是可检测的。语义断言（"包含 cache_control"、"temperature 存在"）
// 挡不住键序、空白、注入位置的漂移，所以重构这一族之前先把**完整输出字节**钉死。
//
// 更新金标准：UPDATE_ANTHROPIC_GOLDEN=1 go test -tags sonic ./internal/app -run TestAnthropicWireGolden
// 每一处 diff 都必须先解释清楚是修正还是回归，再决定要不要接受。
const anthropicGoldenPath = "testdata/anthropic_wire_golden.json"

type anthropicGoldenCase struct {
	name    string
	body    string
	oauth   bool // true 走 OAuth 凭证，false 走 API Key 合成身份
	apiKey  string
	headers http.Header
}

// 固定凭证：金标准要求可复现，身份不能随机。
const (
	anthropicGoldenAccountUUID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	anthropicGoldenSessionID   = "11111111-2222-4333-8444-555555555555"
	anthropicGoldenAPIKey      = "sk-ant-golden-fixture-key"
)

func anthropicGoldenCredentialJSON(t *testing.T) string {
	t.Helper()
	credentialJSON, err := (&anthropicauth.Credential{
		Type: anthropicauth.ChannelType, AccessToken: "access", RefreshToken: "refresh",
		Expired: "2030-01-01T00:00:00Z", AccountUUID: anthropicGoldenAccountUUID,
	}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	return credentialJSON
}

// anthropicGoldenCallerHeaders 是第三方调用方（非原生 Claude Code）的最小头部：
// 不带 CLI UA，因此既不会命中 Haiku 辅助请求分支，也不会命中原生直通分支。
func anthropicGoldenCallerHeaders() http.Header {
	return http.Header{
		"Content-Type":             {"application/json"},
		"X-Claude-Code-Session-Id": {anthropicGoldenSessionID},
	}
}

func anthropicGoldenNativeHeaders() http.Header {
	return http.Header{
		"User-Agent":               {"claude-cli/" + anthropicCLIVersion + " (external, cli)"},
		"X-App":                    {"cli"},
		"Anthropic-Beta":           {"claude-code-20250219"},
		"X-Claude-Code-Session-Id": {anthropicGoldenSessionID},
	}
}

func anthropicGoldenCases(t *testing.T) []anthropicGoldenCase {
	t.Helper()
	credential, err := anthropicauth.ParseCredential([]byte(anthropicGoldenCredentialJSON(t)))
	if err != nil {
		t.Fatal(err)
	}
	nativeUserID := fmt.Sprintf(`{\"device_id\":\"%s\",\"account_uuid\":\"%s\",\"session_id\":\"%s\"}`,
		credential.DeviceID, anthropicGoldenAccountUUID, anthropicGoldenSessionID)

	caller := anthropicGoldenCallerHeaders()
	return []anthropicGoldenCase{
		{
			name:    "minimal",
			body:    `{"model":"claude-sonnet-4-5","max_tokens":1024,"messages":[{"role":"user","content":"hello"}]}`,
			oauth:   true,
			headers: caller,
		},
		{
			name:    "minimal_api_key",
			body:    `{"model":"claude-sonnet-4-5","max_tokens":1024,"messages":[{"role":"user","content":"hello"}]}`,
			apiKey:  anthropicGoldenAPIKey,
			headers: caller,
		},
		{
			name:    "system_string_and_tools",
			body:    `{"model":"claude-opus-4-5","max_tokens":2048,"system":"be terse","tools":[{"name":"a","description":"da","input_schema":{"type":"object"}},{"name":"b","description":"db","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":[{"type":"text","text":"q1"}]},{"role":"assistant","content":[{"type":"text","text":"a1"}]},{"role":"user","content":[{"type":"text","text":"q2"}]}]}`,
			oauth:   true,
			headers: caller,
		},
		{
			name:    "caller_cache_control_5m",
			body:    `{"model":"claude-sonnet-4-5","max_tokens":1024,"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`,
			oauth:   true,
			headers: caller,
		},
		{
			name:    "caller_cache_control_1h",
			body:    `{"model":"claude-sonnet-4-5","max_tokens":1024,"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`,
			oauth:   true,
			headers: caller,
		},
		{
			name:    "cache_control_scope_key_order",
			body:    `{"model":"claude-sonnet-4-5","max_tokens":1024,"system":[{"type":"text","text":"sys","cache_control":{"scope":"x","ttl":"1h","type":"ephemeral"}}],"messages":[{"role":"user","content":"hi"}]}`,
			oauth:   true,
			headers: caller,
		},
		{
			name:    "cache_control_over_limit",
			body:    `{"model":"claude-sonnet-4-5","max_tokens":1024,"tools":[{"name":"t1","cache_control":{"type":"ephemeral"}},{"name":"t2","cache_control":{"type":"ephemeral"}}],"system":[{"type":"text","text":"s1","cache_control":{"type":"ephemeral"}},{"type":"text","text":"s2","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"m1","cache_control":{"type":"ephemeral"}}]},{"role":"assistant","content":[{"type":"text","text":"m2","cache_control":{"type":"ephemeral"}}]},{"role":"user","content":[{"type":"text","text":"m3","cache_control":{"type":"ephemeral"}}]}]}`,
			oauth:   true,
			headers: caller,
		},
		{
			name:    "thinking_adaptive_with_budget",
			body:    `{"model":"claude-sonnet-4-5","max_tokens":8192,"thinking":{"type":"adaptive","budget_tokens":12000},"messages":[{"role":"user","content":"think"}]}`,
			oauth:   true,
			headers: caller,
		},
		{
			name:    "thinking_auto",
			body:    `{"model":"claude-sonnet-4-5","max_tokens":8192,"thinking":{"type":"auto"},"messages":[{"role":"user","content":"think"}]}`,
			oauth:   true,
			headers: caller,
		},
		{
			name:    "thinking_disabled",
			body:    `{"model":"claude-sonnet-4-5","max_tokens":8192,"thinking":{"type":"disabled"},"output_config":{"effort":"high"},"messages":[{"role":"user","content":"think"}]}`,
			oauth:   true,
			headers: caller,
		},
		{
			name:    "forced_tool_choice_strips_thinking",
			body:    `{"model":"claude-sonnet-4-5","max_tokens":8192,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"tool_choice":{"type":"tool","name":"a"},"tools":[{"name":"a","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"go"}]}`,
			oauth:   true,
			headers: caller,
		},
		{
			name:    "sampling_knobs_removed",
			body:    `{"model":"claude-sonnet-4-5","max_tokens":1024,"temperature":0.3,"top_p":0.9,"top_k":40,"thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"hi"}]}`,
			oauth:   true,
			headers: caller,
		},
		{
			name:    "empty_text_blocks_and_web_search",
			body:    `{"model":"claude-sonnet-4-5","max_tokens":1024,"tools":[{"type":"web_search_20250305","name":"web_search","allowed_domains":[],"blocked_domains":[]}],"messages":[{"role":"user","content":[{"type":"text","text":"   "},{"type":"text","text":"real"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":[{"type":"text","text":""},{"type":"text","text":"kept"}]}]}]}`,
			oauth:   true,
			headers: caller,
		},
		{
			name:    "trailing_system_role_message",
			body:    `{"model":"claude-sonnet-4-5","max_tokens":1024,"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"system","content":"tail reminder"}]}`,
			oauth:   true,
			headers: caller,
		},
		{
			name:    "assistant_thinking_tail_not_eligible",
			body:    `{"model":"claude-sonnet-4-5","max_tokens":1024,"messages":[{"role":"user","content":[{"type":"text","text":"q"}]},{"role":"assistant","content":[{"type":"thinking","thinking":"t"}]}]}`,
			oauth:   true,
			headers: caller,
		},
		{
			name:    "context_management_auto_injected",
			body:    `{"model":"claude-sonnet-4-5","max_tokens":8192,"thinking":{"type":"enabled","budget_tokens":4096},"messages":[{"role":"user","content":"hi"}]}`,
			oauth:   true,
			headers: caller,
		},
		{
			name:    "oauth_model_alias_normalized",
			body:    `{"model":"claude-haiku-4-5","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`,
			oauth:   true,
			headers: caller,
		},
		{
			name:    "big_integer_literals_preserved",
			body:    `{"model":"claude-sonnet-4-5","max_tokens":9007199254740993,"metadata":{"trace":12345678901234567890},"messages":[{"role":"user","content":"hi"}]}`,
			oauth:   true,
			headers: caller,
		},
		{
			name: "native_claude_code_passthrough",
			body: fmt.Sprintf(`{"model":"claude-sonnet-4-5-20250929","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.220.abc; cc_entrypoint=cli; cch=308a9;"}],"metadata":{"user_id":"%s"},"messages":[{"role":"user","content":"hello"}],"max_tokens":1024}`,
				nativeUserID),
			oauth:   true,
			headers: anthropicGoldenNativeHeaders(),
		},
	}
}

func anthropicGoldenConfig(t *testing.T, testCase anthropicGoldenCase) *model.Config {
	t.Helper()
	if testCase.oauth {
		return &model.Config{
			AuthType:        model.AuthTypeAnthropicOAuth,
			OAuthCredential: anthropicGoldenCredentialJSON(t),
		}
	}
	return &model.Config{AuthType: model.AuthTypeAPIKey}
}

// anthropicGoldenOutputs 跑完整语料，返回 name → 输出字节。
// finalize 与 normalize 两个入口都记，因为它们是 proxy_forward 实际调用的两个边界。
func anthropicGoldenOutputs(t *testing.T) map[string]string {
	t.Helper()
	outputs := make(map[string]string)
	for _, testCase := range anthropicGoldenCases(t) {
		cfg := anthropicGoldenConfig(t, testCase)
		finalized, err := finalizeAnthropicClaudeCodeMessagesBody(
			[]byte(testCase.body), cfg, testCase.apiKey, testCase.headers,
		)
		if err != nil {
			outputs["finalize/"+testCase.name] = "ERROR: " + err.Error()
		} else {
			outputs["finalize/"+testCase.name] = string(finalized)
		}

		normalized, err := normalizeAnthropicMessagesBody([]byte(testCase.body))
		if err != nil {
			outputs["normalize/"+testCase.name] = "ERROR: " + err.Error()
		} else {
			outputs["normalize/"+testCase.name] = string(normalized)
		}
	}
	return outputs
}

func TestAnthropicWireGolden(t *testing.T) {
	outputs := anthropicGoldenOutputs(t)

	if os.Getenv("UPDATE_ANTHROPIC_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(anthropicGoldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.MarshalIndent(outputs, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(anthropicGoldenPath, append(encoded, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("golden updated: %d entries", len(outputs))
		return
	}

	raw, err := os.ReadFile(anthropicGoldenPath)
	if err != nil {
		t.Fatalf("read golden (run with UPDATE_ANTHROPIC_GOLDEN=1 to create): %v", err)
	}
	var want map[string]string
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, len(outputs))
	for name := range outputs {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		expected, exists := want[name]
		if !exists {
			t.Errorf("%s: missing from golden\n got: %s", name, outputs[name])
			continue
		}
		if outputs[name] != expected {
			t.Errorf("%s: bytes changed\n got:  %s\n want: %s", name, outputs[name], expected)
		}
	}
	for name := range want {
		if _, exists := outputs[name]; !exists {
			t.Errorf("%s: present in golden but no longer produced", name)
		}
	}
}
