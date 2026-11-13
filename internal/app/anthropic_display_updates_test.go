package app

import (
	"compress/gzip"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"

	"github.com/tidwall/gjson"
)

/**
 * @brief 验证模型重定向后的显式后缀与动态等级以实际转换输入为准。
 * @param t 当前测试上下文。
 */
func TestProxy_AnthropicClaude52UsesCurrentEffort(t *testing.T) {
	for _, modelName := range []string{"claude-fable-5-2", "claude-mythos-5-2"} {
		for _, client := range []protocol.Protocol{protocol.OpenAI, protocol.Codex} {
			for _, mode := range []string{"max", "xhigh", "dynamic"} {
				t.Run(modelName+"/"+string(client)+"/"+mode, func(t *testing.T) {
					business := newReasoningAutoBusiness(t)
					clientModel, wantEffort := "gpt-5.5", mode
					if mode == "dynamic" {
						clientModel, wantEffort = "gpt-5.6-sol", "xhigh"
					}
					channel := testChannel{name: "claude52", upstreamProtocol: "anthropic", protocolTransformMode: "local",
						modelEntries: []model.ModelEntry{{Model: clientModel, RedirectModel: modelName}}}
					var env *proxyTestEnv
					var judge *reasoningAutoJudgeFixture
					requestedModel := clientModel + "(" + mode + ")"
					if mode == "dynamic" {
						judge = newReasoningAutoJudge(t, wantEffort)
						env = setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{channel}, []string{business.server.URL})
						requestedModel = clientModel
					} else {
						env = setupProxyTestEnv(t, []testChannel{channel}, map[int]string{0: business.server.URL})
					}
					path := "/v1/chat/completions"
					body := map[string]any{"model": requestedModel, "reasoning_effort": "low",
						"messages": []any{map[string]any{"role": "user", "content": "Choose effort for this task."}}}
					if client == protocol.Codex {
						path = "/v1/responses"
						body = map[string]any{"model": requestedModel, "reasoning": map[string]any{"effort": "low"},
							"input": []any{map[string]any{"role": "user", "content": "Choose effort for this task."}}}
					}
					response := doProxyRequest(t, env.engine, path, body, nil)
					if response.Code != http.StatusOK {
						t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
					}
					requests := business.snapshot()
					if len(requests) != 1 {
						t.Fatalf("upstream requests=%d, want 1", len(requests))
					}
					if got := gjson.GetBytes(requests[0], "output_config.effort").String(); got != wantEffort {
						t.Fatalf("effort=%q, want %q: %s", got, wantEffort, requests[0])
					}
					if gjson.GetBytes(requests[0], "model").String() != modelName || gjson.GetBytes(requests[0], "thinking.type").String() != "adaptive" {
						t.Fatalf("actual model or adaptive thinking lost: %s", requests[0])
					}
					if judge != nil && len(judge.snapshot()) != 1 {
						t.Fatalf("dynamic decision calls=%d, want 1", len(judge.snapshot()))
					}
				})
			}
		}
	}
}

/**
 * @brief 验证重定向到 Sonnet/Haiku 5.5 的 none/xhigh 后缀经过真实代理后保留模型语义。
 * @param t 当前测试上下文。
 */
func TestProxy_AnthropicClaude55ThinkingSuffix(t *testing.T) {
	for _, modelName := range []string{"claude-sonnet-5-5", "claude-haiku-5-5"} {
		for _, client := range []protocol.Protocol{protocol.Anthropic, protocol.OpenAI, protocol.Codex} {
			for _, suffix := range []string{"none", "xhigh"} {
				t.Run(modelName+"/"+string(client)+"/"+suffix, func(t *testing.T) {
					business := newReasoningAutoBusiness(t)
					channel := testChannel{name: "claude55", upstreamProtocol: "anthropic", protocolTransformMode: "local",
						modelEntries: []model.ModelEntry{{Model: "gpt-5.5", RedirectModel: modelName}}}
					env := setupProxyTestEnv(t, []testChannel{channel}, map[int]string{0: business.server.URL})
					path := "/v1/messages"
					requestedModel := "gpt-5.5(" + suffix + ")"
					body := map[string]any{"model": requestedModel, "messages": []any{map[string]any{"role": "user", "content": "hello"}},
						"thinking": map[string]any{"type": "adaptive", "display": "summarized"}, "output_config": map[string]any{"effort": "low"}}
					switch client {
					case protocol.OpenAI:
						path = "/v1/chat/completions"
						body = map[string]any{"model": requestedModel, "messages": []any{map[string]any{"role": "user", "content": "hello"}}, "reasoning_effort": "low"}
					case protocol.Codex:
						path = "/v1/responses"
						body = map[string]any{"model": requestedModel, "input": "hello", "reasoning": map[string]any{"effort": "low"}}
					}
					response := doProxyRequest(t, env.engine, path, body, nil)
					if response.Code != http.StatusOK {
						t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
					}
					requests := business.snapshot()
					if len(requests) != 1 {
						t.Fatalf("upstream requests=%d, want 1", len(requests))
					}
					wantMode := "disabled"
					if modelName == "claude-sonnet-5-5" {
						wantMode = "between_tools"
					}
					if suffix == "xhigh" {
						wantMode = "adaptive"
						if gjson.GetBytes(requests[0], "output_config.effort").String() != "xhigh" {
							t.Fatalf("xhigh suffix changed: %s", requests[0])
						}
					}
					if gjson.GetBytes(requests[0], "model").String() != modelName || gjson.GetBytes(requests[0], "thinking.type").String() != wantMode {
						t.Fatalf("actual model or minimum thinking mode lost: %s", requests[0])
					}
					for _, field := range []string{"thinking.display", "thinking.budget_tokens", "thinking.block_binding", "output_config.effort"} {
						if suffix == "none" && gjson.GetBytes(requests[0], field).Exists() {
							t.Fatalf("none suffix retained %s: %s", field, requests[0])
						}
					}
				})
			}
		}
	}
}

func TestAnthropicClaudeCodeMimicBetasIncludesThinkingDisplayUpdates(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"claude-opus-5-5","thinking":{"type":"adaptive","display":"updates"},"messages":[{"role":"user","content":"hi"}]}`)
	betas := anthropicClaudeCodeMimicBetas(body, true)
	if !strings.Contains(betas, "thinking-display-updates-2026-08-18") {
		t.Fatalf("OAuth betas missing thinking-display-updates: %s", betas)
	}
	plain := []byte(`{"model":"claude-opus-5-5","thinking":{"type":"adaptive","display":"omitted"},"messages":[{"role":"user","content":"hi"}]}`)
	if strings.Contains(anthropicClaudeCodeMimicBetas(plain, true), "thinking-display-updates-2026-08-18") {
		t.Fatal("thinking-display-updates declared without display=updates")
	}
}

func TestSanitizeAnthropicBodyForBetaTokensStripsUpdatesDisplayWithoutBeta(t *testing.T) {
	t.Parallel()
	body := []byte(`{"thinking":{"type":"adaptive","display":"updates"},"messages":[{"role":"user","content":"hi"}]}`)
	got := sanitizeAnthropicBodyForBetaTokens(body, "claude-code-20250219")
	if gjson.GetBytes(got, "thinking.display").Exists() {
		t.Fatalf("updates display survived without beta: %s", got)
	}
	if gjson.GetBytes(got, "thinking.type").String() != "adaptive" {
		t.Fatalf("adaptive thinking lost: %s", got)
	}
	kept := sanitizeAnthropicBodyForBetaTokens(body, "claude-code-20250219,thinking-display-updates-2026-08-18")
	if gjson.GetBytes(kept, "thinking.display").String() != "updates" {
		t.Fatalf("updates display dropped despite beta: %s", kept)
	}
}

func TestIsAnthropicThinkingBlockErrorSkipsDisplayExtraInputs(t *testing.T) {
	t.Parallel()
	if isAnthropicThinkingBlockError("thinking.display: Extra inputs are not permitted") {
		t.Fatal("thinking.display Extra inputs must not trigger thinking downgrade")
	}
	if !isAnthropicThinkingBlockError("thinking blocks are not supported") {
		t.Fatal("real thinking-unsupported errors must still match")
	}
}

/**
 * @brief 验证真实 HTTP 代理出站的自动缓存、签名绑定、能力 beta 与 UA 请求字段。
 * @param t 当前测试上下文。
 */
func TestProxy_AnthropicClaudeWireCompatibility(t *testing.T) {
	for _, scenario := range []string{
		"cache_1h", "cache_5m", "cache_limit", "cache_native",
		"binding", "binding_missing_thinking", "binding_disabled", "binding_rule_disabled", "binding_explicit", "binding_forced_tool",
		"binding_foreign", "binding_native", "binding_mythos", "binding_mythos_native", "binding_third_party", "binding_removed",
		"timing_thread", "timing_message", "timing_haiku", "timing_removed", "timing_native",
		"ua_old", "ua_current", "ua_incomplete",
	} {
		t.Run(scenario, func(t *testing.T) {
			modelName := "claude-fable-5-2"
			switch scenario {
			case "timing_haiku":
				modelName = "claude-haiku-5-5"
			case "binding_disabled", "binding_forced_tool":
				modelName = "claude-fable-5-1"
			case "binding_mythos", "binding_mythos_native":
				modelName = "claude-mythos-5-2"
			}
			channel := testChannel{name: "anthropic-wire", upstreamProtocol: "anthropic", models: modelName,
				authType: model.AuthTypeAnthropicOAuth, oauthCredential: anthropicProxyTestCredential(t, "wire-test-token")}
			upstreamURL := "https://api.anthropic.com"
			if scenario == "binding_third_party" {
				channel.authType, channel.oauthCredential, channel.apiKey = model.AuthTypeAPIKey, "", "wire-test-key"
				upstreamURL = "https://relay.example.com"
			}
			if scenario == "binding_removed" || scenario == "timing_removed" {
				remove := []string{"thinking-binding-controls-2026-08-01"}
				if scenario == "timing_removed" {
					remove = []string{"timing-2026-09-09", "message-threads-2026-08-12"}
				}
				channel.customRequestRules = &model.CustomRequestRules{}
				for _, beta := range remove {
					channel.customRequestRules.Headers = append(channel.customRequestRules.Headers,
						model.CustomHeaderRule{Action: model.RuleActionRemove, Name: "Anthropic-Beta", Value: beta})
				}
			}
			if scenario == "binding_rule_disabled" {
				channel.customRequestRules = &model.CustomRequestRules{Body: []model.CustomBodyRule{
					{Action: model.RuleActionOverride, Path: "thinking", Value: []byte(`{"type":"disabled"}`)},
				}}
			}
			body := map[string]any{"model": modelName, "max_tokens": 64, "system": "caller instructions",
				"messages": []any{map[string]any{"role": "user", "content": "hello"}}}
			headers := map[string]string{"Anthropic-Version": "2023-06-01"}
			if strings.HasPrefix(scenario, "cache_") {
				cache := map[string]any{"type": "ephemeral", "scope": "global"}
				if scenario != "cache_5m" {
					cache["ttl"] = "1h"
				}
				body["cache_control"] = cache
				if scenario == "cache_limit" || scenario == "cache_native" {
					body["tools"] = []any{map[string]any{"name": "Read", "input_schema": map[string]any{"type": "object"}, "cache_control": cache}}
					body["system"] = []any{map[string]any{"type": "text", "text": "caller instructions", "cache_control": cache}}
					body["messages"] = []any{map[string]any{"role": "user", "content": []any{
						map[string]any{"type": "text", "text": "first", "cache_control": cache},
						map[string]any{"type": "text", "text": "second", "cache_control": cache},
						map[string]any{"type": "text", "text": "last"},
					}}}
				}
			}
			if strings.HasPrefix(scenario, "binding") {
				signature := anthropicNativeCAQS()
				if scenario == "binding_foreign" {
					signature = "skip_thought_signature_validator"
				}
				body["thinking"] = map[string]any{"type": "adaptive"}
				body["messages"] = []any{map[string]any{"role": "user", "content": "hello"},
					map[string]any{"role": "assistant", "content": []any{
						map[string]any{"type": "thinking", "thinking": "plan", "signature": signature},
						map[string]any{"type": "text", "text": "ok"},
					}}, map[string]any{"role": "user", "content": "continue"}}
				switch scenario {
				case "binding_missing_thinking":
					delete(body, "thinking")
				case "binding_disabled":
					body["thinking"] = map[string]any{"type": "disabled"}
				case "binding_explicit":
					body["thinking"] = map[string]any{"type": "adaptive", "block_binding": map[string]any{"prefix_mismatch_behavior": "error"}}
				case "binding_forced_tool":
					body["tools"] = []any{map[string]any{"name": "Read", "input_schema": map[string]any{"type": "object"}}}
					body["tool_choice"] = map[string]any{"type": "tool", "name": "Read"}
				}
			}
			if strings.HasPrefix(scenario, "timing_") {
				body["output_config"] = map[string]any{"effort": "medium", "timing": map[string]any{"type": "enabled"}}
				body["thread"] = map[string]any{"type": "create"}
				if scenario == "timing_message" || scenario == "timing_removed" {
					body["messages"] = []any{map[string]any{"role": "user", "content": "hello"},
						map[string]any{"role": "system", "content": "new turn",
							"output_config": map[string]any{"effort": "low", "timing": map[string]any{"type": "enabled"}}}}
				}
			}
			native := strings.HasPrefix(scenario, "ua_") || scenario == "cache_native" || scenario == "binding_native" || scenario == "binding_mythos_native" || scenario == "timing_native"
			if native {
				body["metadata"] = map[string]any{"user_id": `{"device_id":"native-device","session_id":"e03895ad-8b34-4a84-bbf6-002e8909b17b"}`}
				headers["User-Agent"] = "claude-cli/" + anthropicCLIVersion + " (external, cli, agent-sdk/0.2.133, workload/cron, request-phase/background)"
				headers["X-Stainless-Lang"], headers["X-Stainless-Runtime"] = "js", "node"
				headers["X-Stainless-Package-Version"], headers["X-Stainless-Runtime-Version"] = "0.128.0", "v26.12.1"
				switch scenario {
				case "ua_old":
					headers["User-Agent"] = "claude-cli/2.1.312 (external, cli, agent-sdk/0.2.133, workload/cron, request-phase/background)"
				case "ua_incomplete":
					delete(headers, "X-Stainless-Package-Version")
				}
				if scenario == "timing_native" {
					headers["Anthropic-Beta"] = "oauth-2025-04-20,per-turn-control-2026-07-01,timing-2026-09-09,message-threads-2026-08-12"
				}
			}
			env := setupProxyTestEnv(t, []testChannel{channel}, map[int]string{0: upstreamURL})
			var sent []byte
			var sentHeaders http.Header
			env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				reader := r.Body
				if r.Header.Get("Content-Encoding") == "gzip" {
					decoded, err := gzip.NewReader(r.Body)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = decoded.Close() }()
					reader = decoded
				}
				var err error
				sent, err = io.ReadAll(reader)
				if err != nil {
					t.Fatal(err)
				}
				sentHeaders = r.Header.Clone()
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
					Body: io.NopCloser(strings.NewReader(`{"id":"msg_wire","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"` + modelName + `","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))}, nil
			})}
			response := doProxyRequest(t, env.engine, "/v1/messages", body, headers)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			betas := strings.Split(headerValueFold(sentHeaders, "Anthropic-Beta"), ",")
			for index, beta := range betas {
				betas[index] = strings.TrimSpace(beta)
			}
			hasBeta := func(beta string) bool {
				return slices.Contains(betas, beta)
			}
			switch {
			case strings.HasPrefix(scenario, "cache_"):
				if gjson.GetBytes(sent, "cache_control").Exists() != native {
					t.Fatalf("top-level cache preservation=%t body=%s", native, sent)
				}
				caches := gjson.GetBytes(sent, "@dig:cache_control").Array()
				if len(caches) > 4 || ((scenario == "cache_limit" || native) && len(caches) != 4) {
					t.Fatalf("cache breakpoint count=%d body=%s", len(caches), sent)
				}
				for _, cache := range caches {
					wantTTL := "1h"
					if scenario == "cache_5m" {
						wantTTL = ""
					}
					if cache.Get("ttl").String() != wantTTL {
						t.Fatalf("cache window changed: %s", sent)
					}
				}
				if !native {
					messages := gjson.GetBytes(sent, "messages").Array()
					last := messages[len(messages)-1].Get("content").Array()
					if last[len(last)-1].Get("cache_control.scope").String() != "global" {
						t.Fatalf("automatic cache scope lost: %s", sent)
					}
				} else if gjson.GetBytes(sent, "cache_control.scope").String() != "global" {
					t.Fatalf("native automatic cache scope lost: %s", sent)
				}
			case strings.HasPrefix(scenario, "binding"):
				want := ""
				switch scenario {
				case "binding", "binding_missing_thinking", "binding_mythos":
					want = "drop_block"
				case "binding_explicit":
					want = "error"
				}
				if got := gjson.GetBytes(sent, "thinking.block_binding.prefix_mismatch_behavior").String(); got != want {
					t.Fatalf("binding=%q want=%q body=%s", got, want, sent)
				}
				if want != "" && gjson.GetBytes(sent, "thinking.type").String() != "adaptive" {
					t.Fatalf("adaptive thinking lost: %s", sent)
				}
				if (scenario == "binding_disabled" || scenario == "binding_rule_disabled" || scenario == "binding_forced_tool") && gjson.GetBytes(sent, "thinking").Exists() {
					t.Fatalf("explicit thinking disable was reopened: %s", sent)
				}
			case strings.HasPrefix(scenario, "timing_"):
				wantTiming := scenario != "timing_haiku" && scenario != "timing_removed"
				if hasBeta("timing-2026-09-09") != wantTiming || gjson.GetBytes(sent, "output_config.timing").Exists() != wantTiming {
					t.Fatalf("timing header/body mismatch: betas=%v body=%s", betas, sent)
				}
				if !hasBeta("per-turn-control-2026-07-01") || hasBeta("message-threads-2026-08-12") != (scenario != "timing_removed") ||
					gjson.GetBytes(sent, "thread").Exists() != (scenario != "timing_removed") {
					t.Fatalf("per-turn/thread capabilities mismatch: betas=%v body=%s", betas, sent)
				}
				if scenario == "timing_message" || scenario == "timing_removed" {
					messages := gjson.GetBytes(sent, "messages").Array()
					config := messages[len(messages)-1].Get("output_config")
					if config.Get("effort").String() != "low" || config.Get("timing").Exists() != wantTiming {
						t.Fatalf("message output_config changed incorrectly: %s", sent)
					}
				}
			case strings.HasPrefix(scenario, "ua_"):
				wantUA := headers["User-Agent"]
				if scenario != "ua_current" {
					wantUA = "claude-cli/" + anthropicCLIVersion + " (external, cli, workload/cron, request-phase/background)"
				}
				if got := headerValueFold(sentHeaders, "User-Agent"); got != wantUA {
					t.Fatalf("UA=%q want=%q", got, wantUA)
				}
				if headerValueFold(sentHeaders, "X-Stainless-Package-Version") != anthropicStainlessPackageVersion ||
					headerValueFold(sentHeaders, "X-Stainless-Runtime-Version") != anthropicStainlessRuntimeVersion ||
					gjson.GetBytes(sent, "system").String() != "caller instructions" {
					t.Fatalf("software tuple/native prefix changed: headers=%v body=%s", sentHeaders, sent)
				}
			}
		})
	}
}
