package app

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"ccLoad/internal/anthropicauth"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/protocol/builtin"

	"github.com/tidwall/gjson"
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

// finalizeAnthropicClaudeCodeMessagesBody 按 body/header 自动识别调用方 wire 后最终化，
// 与 forwardOnce 的调用方式一致。
func finalizeAnthropicClaudeCodeMessagesBody(body []byte, cfg *model.Config, apiKey string, headers http.Header, target *url.URL) ([]byte, error) {
	body, _, err := finalizeAnthropicClaudeCodeMessagesBodyForCaller(
		body, cfg, apiKey, headers, target, classifyAnthropicCallerWire(body, headers))
	return body, err
}

type anthropicGoldenCase struct {
	name    string
	body    string
	oauth   bool // true 走 OAuth 凭证，false 走 API Key 合成身份
	apiKey  string
	headers http.Header
	// thirdParty 走非第一方 origin；模拟 billing 在两条路径上都省略 CCH。
	thirdParty bool
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
			name:       "minimal_api_key_third_party",
			body:       `{"model":"claude-sonnet-4-5","max_tokens":1024,"messages":[{"role":"user","content":"hello"}]}`,
			apiKey:     anthropicGoldenAPIKey,
			headers:    caller,
			thirdParty: true,
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
		{
			// 下游 Claude Code 指向 ccLoad 时看到的是非第一方 base URL，native gate
			// 省略 cch。身份信号仍然齐全，必须整体直通：客户端自管的 cache_control
			// 断点一个都不能动，否则 prompt cache 会被网关重写打散。
			name: "native_claude_code_no_cch_passthrough",
			body: fmt.Sprintf(`{"model":"claude-sonnet-4-5-20250929","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.220.abc; cc_entrypoint=cli;","cache_control":{"type":"ephemeral"}}],"metadata":{"user_id":"%s"},"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}}]}],"max_tokens":1024,"temperature":0.7}`,
				nativeUserID),
			apiKey:     anthropicGoldenAPIKey,
			headers:    anthropicGoldenNativeHeaders(),
			thirdParty: true,
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

func anthropicGoldenTarget(testCase anthropicGoldenCase) *url.URL {
	if testCase.thirdParty {
		return anthropicThirdPartyTestURL
	}
	return anthropicOfficialTestURL
}

// anthropicGoldenOutputs 跑完整语料，返回 name → 输出字节。
// finalize 与 normalize 两个入口都记，因为它们是 proxy_forward 实际调用的两个边界。
func anthropicGoldenOutputs(t *testing.T) map[string]string {
	t.Helper()
	outputs := make(map[string]string)
	for _, testCase := range anthropicGoldenCases(t) {
		cfg := anthropicGoldenConfig(t, testCase)
		finalized, err := finalizeAnthropicClaudeCodeMessagesBody(
			[]byte(testCase.body), cfg, testCase.apiKey, testCase.headers, anthropicGoldenTarget(testCase),
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

func TestAnthropicWireBillingFingerprintUTF16(t *testing.T) {
	// Samples come from Node.js Buffer.from([4, 7, 20].map(i => text[i] ?? '0').join('')).
	// Keep UTF-16 sampling independent of Go; only the release version changes the digest.
	// The salt is the fixed Claude Code billing protocol value.
	const salt = "59cf53e54c78"
	cases := []struct {
		name   string
		text   string
		sample string
	}{
		{name: "ascii", text: "abcdefghijklmnopqrstu", sample: "ehu"},
		{name: "empty", text: "", sample: "000"},
		{name: "short_ascii", text: "hello", sample: "o00"},
		{name: "short_chinese", text: "你好世界", sample: "000"},
		{name: "chinese", text: "天地玄黄宇宙洪荒日月盈昃辰宿列张寒来暑往秋收冬藏", sample: "宇荒秋"},
		{name: "emoji", text: "😀😁😂😃😄😅😆😇😈😉😊", sample: "😃\uFFFD"},
		{name: "joined_surrogate_pair", text: "abcd😀😁xxxxxxxxxxxxZ", sample: "😁Z"},
		{name: "lone_high_surrogate", text: "abcd😀xxxxxxxxxxxxxxZ", sample: "\uFFFDxZ"},
		{name: "lone_low_surrogate", text: "abc😀xxxxxxxxxxxxxxxZ", sample: "\uFFFDxZ"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			for _, blocks := range []bool{false, true} {
				var content any = testCase.text
				if blocks {
					content = []any{map[string]any{"type": "text", "text": testCase.text}}
				}
				body, err := json.Marshal(map[string]any{
					"model": "claude-sonnet-4-5", "max_tokens": 1024,
					"messages": []any{map[string]any{"role": "user", "content": content}},
				})
				if err != nil {
					t.Fatal(err)
				}
				headers := anthropicGoldenCallerHeaders()
				headers.Set("User-Agent", "claude-cli/"+anthropicCLIVersion+" (external, cli)")
				finalized, err := finalizeAnthropicClaudeCodeMessagesBody(body,
					&model.Config{AuthType: model.AuthTypeAPIKey}, anthropicGoldenAPIKey,
					headers, anthropicOfficialTestURL)
				if err != nil {
					t.Fatal(err)
				}
				digest := sha256.Sum256([]byte(salt + testCase.sample + anthropicCLIVersion))
				fingerprint := fmt.Sprintf("%x", digest)[:3]
				want := "x-anthropic-billing-header: cc_version=" + anthropicCLIVersion + "." + fingerprint + "; cc_entrypoint=cli;"
				if got := gjson.GetBytes(finalized, "system.0.text").String(); got != want {
					t.Errorf("blocks=%t: billing header = %q, want %q", blocks, got, want)
				}
			}
		})
	}
}

/**
 * @brief 验证原生 Fable/Mythos 5.2 在正文改写前拒绝不支持的参数。
 * @param t 当前测试上下文。
 */
func TestAnthropicClaude52NativeRequestContract(t *testing.T) {
	for _, modelName := range []string{"claude-fable-5-2", "anthropic/claude-mythos-5.2-20261101"} {
		for _, testCase := range []struct {
			name    string
			fields  string
			wantErr bool
		}{
			{name: "implicit adaptive"},
			{name: "adaptive", fields: `,"thinking":{"type":"adaptive"},"output_config":{"effort":"max"}`},
			{name: "disabled", fields: `,"thinking":{"type":"disabled"}`, wantErr: true},
			{name: "manual", fields: `,"thinking":{"type":"enabled","budget_tokens":8192}`, wantErr: true},
			{name: "between tools", fields: `,"thinking":{"type":"between_tools"}`, wantErr: true},
			{name: "auto tool", fields: `,"tool_choice":{"type":"auto"}`},
			{name: "no tool", fields: `,"tool_choice":{"type":"none"}`},
			{name: "any tool", fields: `,"tool_choice":{"type":"any"}`, wantErr: true},
			{name: "named tool", fields: `,"tool_choice":{"type":"tool","name":"lookup"}`, wantErr: true},
			{name: "required tool", fields: `,"tool_choice":"required"`, wantErr: true},
			{name: "default temperature", fields: `,"temperature":1`},
			{name: "default top p", fields: `,"top_p":0.99`},
			{name: "temperature", fields: `,"temperature":0.5`, wantErr: true},
			{name: "temperature type", fields: `,"temperature":"1"`, wantErr: true},
			{name: "top p", fields: `,"top_p":1`, wantErr: true},
			{name: "top k", fields: `,"top_k":50`, wantErr: true},
			{name: "both sampling defaults", fields: `,"temperature":1,"top_p":0.99`, wantErr: true},
		} {
			t.Run(modelName+"/"+testCase.name, func(t *testing.T) {
				body := []byte(`{"model":"client-alias","messages":[{"role":"user","content":"hello"}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}]` + testCase.fields + `}`)
				_, _, err := (&Server{}).prepareTranslatedUpstreamBody(&model.Config{Name: "anthropic"},
					protocol.Anthropic, "/v1/messages", modelName, body, body, anthropicGoldenAPIKey,
					anthropicGoldenCallerHeaders(), false, anthropicOfficialTestURL, false, true)
				if (err != nil) != testCase.wantErr {
					t.Fatalf("error=%v, wantErr=%t", err, testCase.wantErr)
				}
				if err != nil {
					if _, ok := errors.AsType[*requestValidationError](err); !ok {
						t.Fatalf("error=%T %v, want requestValidationError", err, err)
					}
				}
			})
		}
	}
}

/**
 * @brief 验证 Sonnet/Haiku 5.5 的最低思考模式、参数限制及模拟后的语义保留。
 * @param t 当前测试上下文。
 */
func TestAnthropicClaude55ThinkingRequestContract(t *testing.T) {
	for _, modelName := range []string{"claude-sonnet-5-5", "anthropic/claude-sonnet-5.5-20261001", "claude-haiku-5-5", "anthropic/claude-haiku-5.5-20261001"} {
		isSonnet := anthropicClaude55Family(anthropicCanonicalModelName(modelName)) == "claude-sonnet-5-5"
		minimumType := "disabled"
		defaultEffort := "medium"
		if isSonnet {
			minimumType = "between_tools"
			defaultEffort = "high"
		}
		for _, testCase := range []struct {
			name      string
			fields    string
			sonnetErr bool
			haikuErr  bool
		}{
			{name: "implicit adaptive"},
			{name: "adaptive max", fields: `,"thinking":{"type":"adaptive"},"output_config":{"effort":"max"}`},
			{name: "manual", fields: `,"thinking":{"type":"enabled","budget_tokens":8192}`, sonnetErr: true, haikuErr: true},
			{name: "disabled", fields: `,"thinking":{"type":"disabled"}`, sonnetErr: true},
			{name: "disabled high", fields: `,"thinking":{"type":"disabled"},"output_config":{"effort":"high"}`, sonnetErr: true},
			{name: "disabled max", fields: `,"thinking":{"type":"disabled"},"output_config":{"effort":"max"}`, sonnetErr: true, haikuErr: true},
			{name: "disabled display", fields: `,"thinking":{"type":"disabled","display":"omitted"}`, sonnetErr: true, haikuErr: true},
			{name: "disabled budget", fields: `,"thinking":{"type":"disabled","budget_tokens":0}`, sonnetErr: true, haikuErr: true},
			{name: "disabled binding", fields: `,"thinking":{"type":"disabled","block_binding":{}}`, sonnetErr: true, haikuErr: true},
			{name: "between tools", fields: `,"thinking":{"type":"between_tools"}`, haikuErr: true},
			{name: "between tools low", fields: `,"thinking":{"type":"between_tools"},"output_config":{"effort":"low"}`, haikuErr: true},
			{name: "between tools xhigh", fields: `,"thinking":{"type":"between_tools"},"output_config":{"effort":"xhigh"}`, sonnetErr: true, haikuErr: true},
			{name: "between tools display", fields: `,"thinking":{"type":"between_tools","display":"omitted"}`, sonnetErr: true, haikuErr: true},
			{name: "between tools budget", fields: `,"thinking":{"type":"between_tools","budget_tokens":0}`, sonnetErr: true, haikuErr: true},
			{name: "between tools binding", fields: `,"thinking":{"type":"between_tools","block_binding":{}}`, sonnetErr: true, haikuErr: true},
			{name: "forced tool", fields: `,"tool_choice":{"type":"any"}`, sonnetErr: true},
			{name: "default temperature", fields: `,"temperature":1`},
			{name: "default top p", fields: `,"top_p":0.99`},
			{name: "temperature", fields: `,"temperature":0.5`, sonnetErr: true, haikuErr: true},
			{name: "top p one", fields: `,"top_p":1`, sonnetErr: true, haikuErr: true},
			{name: "top p type", fields: `,"top_p":"0.99"`, sonnetErr: true, haikuErr: true},
			{name: "top k", fields: `,"top_k":0`, sonnetErr: true, haikuErr: true},
			{name: "both sampling defaults", fields: `,"temperature":1,"top_p":0.99`, sonnetErr: true, haikuErr: true},
		} {
			t.Run(modelName+"/"+testCase.name, func(t *testing.T) {
				body := []byte(`{"model":"` + modelName + `","messages":[{"role":"user","content":"hello"}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}]` + testCase.fields + `}`)
				got, _, err := (&Server{}).prepareTranslatedUpstreamBody(&model.Config{Name: "anthropic"},
					protocol.Anthropic, "/v1/messages", modelName, body, body, anthropicGoldenAPIKey,
					anthropicGoldenCallerHeaders(), false, anthropicOfficialTestURL, false, true)
				wantErr := testCase.haikuErr
				if isSonnet {
					wantErr = testCase.sonnetErr
				}
				if (err != nil) != wantErr {
					t.Fatalf("error=%v, wantErr=%t", err, wantErr)
				}
				if err != nil {
					if _, ok := errors.AsType[*requestValidationError](err); !ok {
						t.Fatalf("error=%T %v, want requestValidationError", err, err)
					}
					return
				}
				if mode := gjson.GetBytes(body, "thinking.type").String(); mode != "" && gjson.GetBytes(got, "thinking.type").String() != mode {
					t.Fatalf("caller thinking mode changed: %s", got)
				}
				if effort := gjson.GetBytes(body, "output_config.effort").String(); effort != "" && gjson.GetBytes(got, "output_config.effort").String() != effort {
					t.Fatalf("caller output effort changed: %s", got)
				}
			})
		}
		for _, testCase := range []struct {
			name    string
			effort  string
			message string
			wantErr bool
		}{
			{name: "same default", message: defaultEffort},
			{name: "different from default", message: "low", wantErr: true},
			{name: "same explicit", effort: "low", message: "low"},
			{name: "changed explicit", effort: "medium", message: "high", wantErr: true},
			{name: "xhigh", effort: "xhigh", wantErr: true},
			{name: "max", effort: "max", wantErr: true},
		} {
			t.Run(modelName+"/minimum mode "+testCase.name, func(t *testing.T) {
				body := []byte(`{"model":"` + modelName + `","thinking":{"type":"` + minimumType + `"},"messages":[{"role":"user","content":"hello"}]}`)
				if testCase.effort != "" {
					body = setJSONValue(body, "output_config.effort", testCase.effort)
				}
				if testCase.message != "" {
					body = setJSONValue(body, "messages.1", map[string]any{"role": "system", "content": "continue", "output_config": map[string]any{"effort": testCase.message}})
				}
				_, _, err := (&Server{}).prepareTranslatedUpstreamBody(&model.Config{Name: "anthropic"},
					protocol.Anthropic, "/v1/messages", modelName, body, body, anthropicGoldenAPIKey,
					anthropicGoldenCallerHeaders(), false, anthropicOfficialTestURL, false, true)
				if (err != nil) != testCase.wantErr {
					t.Fatalf("error=%v, wantErr=%t", err, testCase.wantErr)
				}
			})
		}
	}
}

/**
 * @brief 验证 Chat/Responses 请求在 Sonnet/Haiku 5.5 保留明确等级及关闭思考的模型差异。
 * @param t 当前测试上下文。
 */
func TestAnthropicClaude55ConvertedThinkingContract(t *testing.T) {
	registry := protocol.NewRegistry()
	builtin.Register(registry)
	for _, modelName := range []string{"claude-sonnet-5-5", "claude-haiku-5-5"} {
		for _, client := range []protocol.Protocol{protocol.OpenAI, protocol.Codex} {
			for _, effort := range []string{"none", "low", "medium", "high", "xhigh", "max"} {
				t.Run(modelName+"/"+string(client)+"/"+effort, func(t *testing.T) {
					raw := `{"model":"client-alias","temperature":0.5,"top_p":0.7`
					if client == protocol.OpenAI {
						raw += `,"messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"tool_choice":"required","reasoning_effort":"` + effort + `"}`
					} else {
						raw += `,"input":"hello","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"tool_choice":"required","reasoning":{"effort":"` + effort + `"}}`
					}
					source := []byte(raw)
					translated, err := registry.TranslateRequest(client, protocol.Anthropic, modelName, source, false)
					if err != nil {
						t.Fatal(err)
					}
					got, _, err := (&Server{}).prepareTranslatedUpstreamBody(&model.Config{Name: "anthropic"},
						protocol.Anthropic, "/v1/messages", modelName, translated, source, anthropicGoldenAPIKey,
						anthropicGoldenCallerHeaders(), false, anthropicOfficialTestURL, false, false)
					if err != nil {
						t.Fatal(err)
					}
					wantMode, wantEffort, wantChoice := "adaptive", effort, "any"
					if modelName == "claude-sonnet-5-5" {
						wantChoice = "auto"
					}
					if effort == "none" {
						wantMode, wantEffort = "disabled", ""
						if modelName == "claude-sonnet-5-5" {
							wantMode = "between_tools"
						}
					}
					if gjson.GetBytes(got, "thinking.type").String() != wantMode || gjson.GetBytes(got, "output_config.effort").String() != wantEffort || gjson.GetBytes(got, "tool_choice.type").String() != wantChoice {
						t.Fatalf("thinking mode, effort or tool choice changed: want=%s/%s/%s body=%s", wantMode, wantEffort, wantChoice, got)
					}
					for _, field := range []string{"thinking.budget_tokens", "temperature", "top_p", "top_k"} {
						if gjson.GetBytes(got, field).Exists() {
							t.Fatalf("unsupported %s survived: %s", field, got)
						}
					}
				})
			}
		}
	}
}

/**
 * @brief 验证真实 Chat/Responses 转换后保留 5.2 的五档等级，并先于渠道规则调整不兼容参数。
 * @param t 当前测试上下文。
 */
func TestAnthropicClaude52ConvertedRequestContract(t *testing.T) {
	registry := protocol.NewRegistry()
	builtin.Register(registry)
	for _, modelName := range []string{"claude-fable-5-2", "claude-mythos-5-2"} {
		for _, client := range []protocol.Protocol{protocol.OpenAI, protocol.Codex} {
			for _, effort := range []string{"", "none", "auto", "low", "medium", "high", "xhigh", "max"} {
				t.Run(modelName+"/"+string(client)+"/"+effort, func(t *testing.T) {
					var raw string
					if client == protocol.OpenAI {
						raw = `{"model":"client-alias","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"tool_choice":{"type":"function","function":{"name":"lookup"}},"temperature":0.5,"top_p":0.7`
						if effort != "" {
							raw += `,"reasoning_effort":"` + effort + `"`
						}
					} else {
						raw = `{"model":"client-alias","input":"hello","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"lookup"},"temperature":0.5,"top_p":0.7`
						if effort != "" {
							raw += `,"reasoning":{"effort":"` + effort + `"}`
						}
					}
					source := []byte(raw + `}`)
					translated, err := registry.TranslateRequest(client, protocol.Anthropic, modelName, source, false)
					if err != nil {
						t.Fatal(err)
					}
					translated = preserveAnthropicConvertedEffort(translated, source, modelName)
					got, _, err := (&Server{}).prepareTranslatedUpstreamBody(&model.Config{Name: "anthropic"},
						protocol.Anthropic, "/v1/messages", modelName, translated, source, anthropicGoldenAPIKey,
						anthropicGoldenCallerHeaders(), false, anthropicOfficialTestURL, false, false)
					if err != nil {
						t.Fatal(err)
					}
					wantEffort := effort
					if effort == "none" || effort == "auto" {
						wantEffort = ""
					}
					if gotEffort := gjson.GetBytes(got, "output_config.effort").String(); gotEffort != wantEffort {
						t.Fatalf("effort=%q, want=%q: %s", gotEffort, wantEffort, got)
					}
					if thinking := gjson.GetBytes(got, "thinking.type").String(); (effort != "" && thinking != "adaptive") || (effort == "" && thinking != "") {
						t.Fatalf("unexpected thinking mode: %s", got)
					}
					if gjson.GetBytes(got, "thinking.budget_tokens").Exists() || gjson.GetBytes(got, "tool_choice.type").String() != "auto" || gjson.GetBytes(got, "tool_choice.name").Exists() {
						t.Fatalf("unsupported thinking budget or forced tool survived: %s", got)
					}
					for _, field := range []string{"temperature", "top_p", "top_k"} {
						if gjson.GetBytes(got, field).Exists() {
							t.Fatalf("unsupported %s survived: %s", field, got)
						}
					}
				})
			}
		}
	}
	for _, testCase := range []struct {
		name       string
		fields     string
		rules      *model.CustomRequestRules
		wantEffort string
		wantChoice string
	}{
		{name: "legacy budget", fields: `,"thinking":{"type":"enabled","budget_tokens":8192}`, wantEffort: "medium", wantChoice: "auto"},
		{name: "explicit effort", fields: `,"thinking":{"type":"enabled","budget_tokens":8192},"output_config":{"effort":"max"}`, wantEffort: "max", wantChoice: "auto"},
		{name: "channel rules", fields: `,"thinking":{"type":"enabled","budget_tokens":8192}`,
			rules: &model.CustomRequestRules{Body: []model.CustomBodyRule{
				{Action: model.RuleActionOverride, Path: "output_config.effort", Value: json.RawMessage(`"low"`)},
				{Action: model.RuleActionOverride, Path: "tool_choice", Value: json.RawMessage(`{"type":"none"}`)},
			}}, wantEffort: "low", wantChoice: "none"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			body := []byte(`{"model":"claude-mythos-5-2","messages":[{"role":"user","content":"hello"}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"lookup","disable_parallel_tool_use":true},"top_k":50` + testCase.fields + `}`)
			got, _, err := (&Server{}).prepareTranslatedUpstreamBody(&model.Config{Name: "anthropic", CustomRequestRules: testCase.rules},
				protocol.Anthropic, "/v1/messages", "claude-mythos-5-2", body, []byte(`{}`), anthropicGoldenAPIKey,
				anthropicGoldenCallerHeaders(), false, anthropicOfficialTestURL, false, false)
			if err != nil {
				t.Fatal(err)
			}
			if gjson.GetBytes(got, "output_config.effort").String() != testCase.wantEffort || gjson.GetBytes(got, "tool_choice.type").String() != testCase.wantChoice ||
				gjson.GetBytes(got, "thinking.type").String() != "adaptive" || gjson.GetBytes(got, "thinking.budget_tokens").Exists() {
				t.Fatalf("converted request or channel priority changed: %s", got)
			}
			if testCase.rules == nil && !gjson.GetBytes(got, "tool_choice.disable_parallel_tool_use").Bool() {
				t.Fatalf("parallel-tool preference lost: %s", got)
			}
		})
	}
}
