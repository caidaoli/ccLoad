package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"ccLoad/internal/model"
	"ccLoad/internal/util"
	"ccLoad/internal/zedauth"

	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

type reasoningAutoJudgeRequest struct {
	Conversation   string `json:"conversation"`
	PreviousEffort string `json:"previous_effort"`
}

type reasoningAutoJudgeFixture struct {
	server   *testHTTPServer
	mu       sync.Mutex
	requests []reasoningAutoJudgeRequest
	respond  func(http.ResponseWriter, *http.Request, int)
}

func newReasoningAutoJudge(t testing.TB, efforts ...string) *reasoningAutoJudgeFixture {
	t.Helper()
	judge := &reasoningAutoJudgeFixture{}
	judge.respond = func(w http.ResponseWriter, _ *http.Request, index int) {
		if index >= len(efforts) {
			t.Errorf("unexpected decision request %d; expected %d", index+1, len(efforts))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "judge", "answers": map[string]any{
				"effort": map[string]any{"type": "choice", "choice": efforts[index]},
			}, "usage": map[string]any{"input_tokens": 1000, "output_tokens": 100},
		})
	}
	judge.server = newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer sk-judge" {
			t.Errorf("unexpected decision request: %s %s, authorization=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body struct {
			Model string `json:"model"`
			State string `json:"state"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode decision request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var request reasoningAutoJudgeRequest
		if err := json.Unmarshal([]byte(body.State), &request); err != nil {
			t.Errorf("decode decision state: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if body.Model != "judge" || !utf8.ValidString(request.Conversation) {
			t.Errorf("invalid decision state: model=%q, valid UTF-8=%v", body.Model, utf8.ValidString(request.Conversation))
		}
		judge.mu.Lock()
		index := len(judge.requests)
		judge.requests = append(judge.requests, request)
		judge.mu.Unlock()
		judge.respond(w, r, index)
	}))
	t.Cleanup(judge.server.Close)
	return judge
}

func (j *reasoningAutoJudgeFixture) snapshot() []reasoningAutoJudgeRequest {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.requests)
}

type reasoningAutoBusinessFixture struct {
	server   *testHTTPServer
	mu       sync.Mutex
	requests [][]byte
}

func newReasoningAutoBusiness(t testing.TB) *reasoningAutoBusinessFixture {
	t.Helper()
	business := &reasoningAutoBusinessFixture{}
	business.server = newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || !json.Valid(body) {
			t.Errorf("invalid business request: body=%s err=%v", body, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		business.mu.Lock()
		business.requests = append(business.requests, body)
		index := len(business.requests)
		business.mu.Unlock()
		if r.URL.Path == "/v1/responses" {
			w.Header().Set("Content-Type", "text/event-stream")
			event := reasoningAutoCompletedEvent(fmt.Sprintf("resp-auto-%d", index), "business answer")
			encoded, _ := json.Marshal(event)
			_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", encoded)
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected business path=%q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chat-auto","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"business answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	}))
	t.Cleanup(business.server.Close)
	return business
}

func (b *reasoningAutoBusinessFixture) snapshot() [][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.requests)
}

func reasoningAutoCompletedEvent(id, text string) map[string]any {
	return map[string]any{
		"type": "response.completed", "response": map[string]any{
			"id": id, "status": "completed", "output": []any{map[string]any{
				"type": "message", "role": "assistant", "content": []any{map[string]any{
					"type": "output_text", "text": text,
				}},
			}}, "usage": map[string]any{"input_tokens": 3, "output_tokens": 2, "total_tokens": 5},
		},
	}
}

func setupReasoningAutoTestEnv(t testing.TB, judgeURL string, channels []testChannel, urls []string) *proxyTestEnv {
	t.Helper()
	if len(channels) != len(urls) {
		t.Fatal("business channel and URL counts differ")
	}
	allChannels := append([]testChannel{{
		name: "reasoning-judge", upstreamProtocol: "system_one", models: "judge", apiKey: "sk-judge",
	}}, channels...)
	upstreamURLs := map[int]string{0: judgeURL}
	for i, url := range urls {
		upstreamURLs[i+1] = url
	}
	policy, err := json.Marshal(model.ReasoningAutoConfig{
		Enabled: true, DecisionChannelID: 1, DecisionModel: "judge",
		BusinessModels: []string{"gpt-5.6-sol", "gpt-5.6-luna"}, TimeoutMS: 2000,
	})
	if err != nil {
		t.Fatal(err)
	}
	return setupProxyTestEnvWithSettings(t, allChannels, upstreamURLs, map[string]string{
		"reasoning_auto": string(policy), "cooldown_fallback_enabled": "false",
	})
}

func reasoningAutoChatBody(content string) map[string]any {
	return map[string]any{"model": "gpt-5.6-sol", "messages": []any{
		map[string]any{"role": "user", "content": content},
	}}
}

func reasoningAutoToolBody(outputs ...string) map[string]any {
	messages := []any{map[string]any{"role": "user", "content": "Complete the original migration without losing concurrent writes."}}
	calls := make([]any, len(outputs))
	for i := range outputs {
		calls[i] = map[string]any{"id": fmt.Sprintf("call-%d", i), "type": "function", "function": map[string]any{"name": "migration_step", "arguments": "{}"}}
	}
	messages = append(messages, map[string]any{"role": "assistant", "tool_calls": calls})
	for i, output := range outputs {
		messages = append(messages, map[string]any{"role": "tool", "tool_call_id": fmt.Sprintf("call-%d", i), "content": output})
	}
	return map[string]any{"model": "gpt-5.6-sol", "messages": messages}
}

func reasoningAutoRequest(t testing.TB, env *proxyTestEnv, path string, body any, headers map[string]string) {
	t.Helper()
	response := doProxyRequest(t, env.engine, path, body, headers)
	if response.Code != http.StatusOK {
		t.Fatalf("business request status=%d body=%s", response.Code, response.Body.String())
	}
}

func assertReasoningAutoEfforts(t testing.TB, requests [][]byte, want ...string) {
	t.Helper()
	got := make([]string, len(requests))
	for i, request := range requests {
		got[i] = gjson.GetBytes(request, "reasoning_effort").String()
		if got[i] == "" {
			got[i] = gjson.GetBytes(request, "reasoning.effort").String()
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("upstream reasoning efforts=%v, want %v", got, want)
	}
}

func TestReasoningAutoUpgradeAndConfirmedDowngrade(t *testing.T) {
	t.Parallel()
	judge := newReasoningAutoJudge(t, "low", "xhigh", "low", "medium")
	business := newReasoningAutoBusiness(t)
	env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
		name: "business", upstreamProtocol: "openai", models: "gpt-5.6-sol",
	}}, []string{business.server.URL})
	headers := map[string]string{"Session-Id": "changing-task"}
	messages := []any{map[string]any{"role": "user", "content": "What is 2+2?"}}
	for _, next := range []string{"", "Design a lock-free migration with rollback and concurrent writers.", "继续", "The design is settled; finish the mechanical work and report the result."} {
		if next != "" {
			messages = append(messages, map[string]any{"role": "user", "content": next})
		}
		reasoningAutoRequest(t, env, "/v1/chat/completions", map[string]any{"model": "gpt-5.6-sol", "messages": messages}, headers)
	}
	assertReasoningAutoEfforts(t, business.snapshot(), "low", "xhigh", "xhigh", "medium")
	decisions := judge.snapshot()
	if len(decisions) != 4 {
		t.Fatalf("decision calls=%d, want 4", len(decisions))
	}
	previous := []string{decisions[0].PreviousEffort, decisions[1].PreviousEffort, decisions[2].PreviousEffort, decisions[3].PreviousEffort}
	if !slices.Equal(previous, []string{"", "low", "xhigh", "xhigh"}) {
		t.Fatalf("previous efforts=%v", previous)
	}
	if !strings.Contains(decisions[2].Conversation, "lock-free migration") || !strings.Contains(decisions[2].Conversation, "继续") || !strings.Contains(decisions[3].Conversation, "finish the mechanical work") {
		t.Fatalf("decision lost the original task or latest instruction: %q", decisions[3].Conversation)
	}
}

func TestReasoningAutoToolContinuationAndParallelFailure(t *testing.T) {
	t.Parallel()
	judge := newReasoningAutoJudge(t, "high", "xhigh", "high", "xhigh")
	business := newReasoningAutoBusiness(t)
	env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
		name: "tools", upstreamProtocol: "openai", models: "gpt-5.6-sol",
	}}, []string{business.server.URL})
	headers := map[string]string{"Session-Id": "tool-chain"}
	reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoChatBody("Plan a migration with concurrent writes."), headers)
	for turn := range 3 {
		reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoToolBody(fmt.Sprintf("completed mechanical step %d", turn)), headers)
		want := 1
		if turn == 2 {
			want = 2
		}
		if got := len(judge.snapshot()); got != want {
			t.Fatalf("after tool continuation %d decision calls=%d, want %d", turn+1, got, want)
		}
	}
	// The failing result is earlier in the same parallel batch, before a successful result.
	reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoToolBody("write failed: conflicting concurrent migration", "read completed successfully"), headers)
	assertReasoningAutoEfforts(t, business.snapshot(), "high", "high", "high", "xhigh", "xhigh")
	if got := len(judge.snapshot()); got != 3 {
		t.Fatalf("parallel tool failure did not trigger early reevaluation: calls=%d", got)
	}
	reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoToolBody("new requirement: preserve availability during rollback"), headers)
	assertReasoningAutoEfforts(t, business.snapshot(), "high", "high", "high", "xhigh", "xhigh", "xhigh")
	if got := len(judge.snapshot()); got != 4 {
		t.Fatalf("new tool constraint did not trigger early reevaluation: calls=%d", got)
	}
}

func TestReasoningAutoNullToolContentKeepsReevaluation(t *testing.T) {
	t.Parallel()
	judge := newReasoningAutoJudge(t, "low", "high", "xhigh")
	business := newReasoningAutoBusiness(t)
	env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
		name: "null-tool-content", upstreamProtocol: "openai", models: "gpt-5.6-sol",
	}}, []string{business.server.URL})
	headers := map[string]string{"Session-Id": "null-tool-content"}
	body := reasoningAutoToolBody("completed")
	messages := body["messages"].([]any)
	messages[1].(map[string]any)["content"] = nil
	reasoningAutoRequest(t, env, "/v1/chat/completions", body, headers)
	messages[2].(map[string]any)["content"] = "migration failed: conflicting writes"
	reasoningAutoRequest(t, env, "/v1/chat/completions", body, headers)
	body["messages"] = append(messages, map[string]any{"role": "user", "content": "Redesign the migration to preserve all concurrent writes."})
	reasoningAutoRequest(t, env, "/v1/chat/completions", body, headers)
	assertReasoningAutoEfforts(t, business.snapshot(), "low", "high", "xhigh")
	decisions := judge.snapshot()
	if len(decisions) != 3 {
		t.Fatalf("decision requests=%d, want 3", len(decisions))
	}
	if !strings.Contains(decisions[1].Conversation, "migration_step") || !strings.Contains(decisions[1].Conversation, "conflicting writes") {
		t.Fatalf("decision lost tool call or failure context: %s", decisions[1].Conversation)
	}
}

// Array content is judged by its first text line, like string content: a
// marker deep in a dump is not a failure, a failing status line is.
func TestReasoningAutoArrayToolContentUsesFirstLine(t *testing.T) {
	t.Parallel()
	judge := newReasoningAutoJudge(t, "low", "high")
	business := newReasoningAutoBusiness(t)
	env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
		name: "array-tool-content", upstreamProtocol: "openai", models: "gpt-5.6-sol",
	}}, []string{business.server.URL})
	headers := map[string]string{"Session-Id": "array-tool-content"}
	reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoChatBody("Run the migration."), headers)
	body := reasoningAutoToolBody("")
	messages := body["messages"].([]any)
	tool := messages[2].(map[string]any)
	tool["content"] = []any{map[string]any{"type": "text", "text": "read migrate.log\n2026-10-10 step 3 failed: retried"}}
	reasoningAutoRequest(t, env, "/v1/chat/completions", body, headers)
	tool["content"] = []any{map[string]any{"type": "text", "text": "migration failed: conflicting writes\nstack trace"}}
	reasoningAutoRequest(t, env, "/v1/chat/completions", body, headers)
	assertReasoningAutoEfforts(t, business.snapshot(), "low", "low", "high")
}

func TestReasoningAutoCodexOriginalTaskSkipsInjectedContext(t *testing.T) {
	t.Parallel()
	judge := newReasoningAutoJudge(t, "high")
	business := newReasoningAutoBusiness(t)
	env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
		name: "codex-injected", upstreamProtocol: "codex", models: "gpt-5.6-sol",
	}}, []string{business.server.URL})
	message := func(role, kind, text string) map[string]any {
		return map[string]any{"type": "message", "role": role, "content": []any{map[string]any{"type": kind, "text": text}}}
	}
	reasoningAutoRequest(t, env, "/v1/responses", map[string]any{"model": "gpt-5.6-sol", "stream": true, "input": []any{
		message("user", "input_text", "# AGENTS.md instructions for /repo\n\nUse gofmt."),
		message("user", "input_text", "<environment_context>\n  <cwd>/repo</cwd>\n</environment_context>"),
		message("user", "input_text", "Migrate the billing ledger without downtime."),
		message("assistant", "output_text", strings.Repeat("progress ", 4*1024)),
		message("user", "input_text", "continue"),
	}}, map[string]string{"Session-Id": "codex-injected"})
	decisions := judge.snapshot()
	if len(decisions) != 1 || !strings.HasPrefix(decisions[0].Conversation, "original task: Migrate the billing ledger") {
		t.Fatalf("truncated decision lost the real original task: %+v", decisions)
	}
}

func TestReasoningAutoResponsesSummaryDoesNotSpecifyEffort(t *testing.T) {
	t.Parallel()
	for _, transport := range []string{"http", "websocket"} {
		t.Run(transport, func(t *testing.T) {
			t.Parallel()
			judge := newReasoningAutoJudge(t, "high", "high")
			business := newReasoningAutoBusiness(t)
			env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
				name: "summary", upstreamProtocol: "codex", models: "gpt-5.6-sol",
			}}, []string{business.server.URL})
			var conn *websocket.Conn
			if transport == "websocket" {
				conn = dialResponsesWebsocketWithSessionID(t, env.engine, "summary")
				if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			for _, explicit := range []bool{false, true} {
				reasoning := map[string]any{"summary": "auto"}
				if explicit {
					reasoning["effort"] = "low"
				}
				body := map[string]any{"model": "gpt-5.6-sol", "input": []any{map[string]any{"role": "user", "content": "Design a concurrent migration."}}, "reasoning": reasoning}
				if conn != nil {
					body["type"] = "response.create"
					if err := conn.WriteJSON(body); err != nil {
						t.Fatal(err)
					}
					readWebsocketUntilType(t, conn, "response.completed")
				} else {
					reasoningAutoRequest(t, env, "/v1/responses", body, map[string]string{"Session-Id": "summary"})
				}
			}
			if got := len(judge.snapshot()); got != 2 {
				t.Fatalf("decision calls=%d, want 2", got)
			}
			requests := business.snapshot()
			assertReasoningAutoEfforts(t, requests, "high", "high")
			for _, body := range requests {
				if got := gjson.GetBytes(body, "reasoning.summary").String(); got != "auto" {
					t.Fatalf("summary=%q, want auto", got)
				}
			}
		})
	}
}

func TestReasoningAutoRequestEffortPolicy(t *testing.T) {
	t.Parallel()
	for _, transport := range []string{"chat", "responses", "websocket"} {
		t.Run(transport, func(t *testing.T) {
			t.Parallel()
			for _, policy := range []struct {
				name, suffix, bodyEffort, decision, upstreamEffort string
				decisionCalls                                      int
			}{
				{name: "body_low", bodyEffort: "low", decision: "high", upstreamEffort: "high", decisionCalls: 1},
				{name: "auto_body_high", suffix: "(auto)", bodyEffort: "high", decision: "low", upstreamEffort: "low", decisionCalls: 1},
				{name: "auto_case_whitespace", suffix: "( AuTo )", bodyEffort: "high", decision: "low", upstreamEffort: "low", decisionCalls: 1},
				{name: "auto_numeric", suffix: "(-1)", bodyEffort: "high", decision: "low", upstreamEffort: "low", decisionCalls: 1},
				{name: "low_suffix", suffix: "(low)", bodyEffort: "high", upstreamEffort: "low"},
				{name: "none_suffix", suffix: "(none)", bodyEffort: "high", upstreamEffort: "none"},
				{name: "budget_suffix", suffix: "(8192)", bodyEffort: "high", upstreamEffort: "medium"},
			} {
				t.Run(policy.name, func(t *testing.T) {
					t.Parallel()
					var efforts []string
					if policy.decisionCalls != 0 {
						efforts = append(efforts, policy.decision)
					}
					judge := newReasoningAutoJudge(t, efforts...)
					business := newReasoningAutoBusiness(t)
					upstreamProtocol := "codex"
					if transport == "chat" {
						upstreamProtocol = "openai"
					}
					env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
						name: "effort-policy", upstreamProtocol: upstreamProtocol, models: "gpt-5.6-sol",
					}}, []string{business.server.URL})
					body := map[string]any{"model": "gpt-5.6-sol" + policy.suffix}
					if transport == "chat" {
						body["messages"] = []any{map[string]any{"role": "user", "content": "Choose effort for this task."}}
						body["reasoning_effort"] = policy.bodyEffort
						reasoningAutoRequest(t, env, "/v1/chat/completions", body, nil)
					} else {
						body["input"] = []any{map[string]any{"role": "user", "content": "Choose effort for this task."}}
						body["reasoning"] = map[string]any{"effort": policy.bodyEffort, "summary": "auto"}
						if transport == "websocket" {
							conn := dialResponsesWebsocketWithSessionID(t, env.engine, "effort-policy")
							if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
								t.Fatal(err)
							}
							body["type"] = "response.create"
							if err := conn.WriteJSON(body); err != nil {
								t.Fatal(err)
							}
							readWebsocketUntilType(t, conn, "response.completed")
						} else {
							reasoningAutoRequest(t, env, "/v1/responses", body, nil)
						}
					}
					requests := business.snapshot()
					assertReasoningAutoEfforts(t, requests, policy.upstreamEffort)
					if got := gjson.GetBytes(requests[0], "model").String(); got != "gpt-5.6-sol" {
						t.Fatalf("upstream model=%q, want base model", got)
					}
					if transport != "chat" && gjson.GetBytes(requests[0], "reasoning.summary").String() != "auto" {
						t.Fatalf("reasoning summary lost: %s", requests[0])
					}
					if got := len(judge.snapshot()); got != policy.decisionCalls {
						t.Fatalf("decision calls=%d, want %d", got, policy.decisionCalls)
					}
				})
			}
		})
	}
}

func TestReasoningAutoExplicitSettingsRequireFreshDecision(t *testing.T) {
	t.Parallel()
	for _, explicit := range []struct {
		name, model, effort, upstreamEffort string
	}{
		{name: "low_suffix", model: "gpt-5.6-sol(low)", effort: "high", upstreamEffort: "low"},
		{name: "none_suffix", model: "gpt-5.6-sol(none)", effort: "high", upstreamEffort: "none"},
		{name: "budget_suffix", model: "gpt-5.6-sol(8192)", effort: "high", upstreamEffort: "medium"},
	} {
		t.Run(explicit.name, func(t *testing.T) {
			t.Parallel()
			judge := newReasoningAutoJudge(t, "xhigh", "low", "medium")
			business := newReasoningAutoBusiness(t)
			env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
				name: "explicit", upstreamProtocol: "openai", models: "gpt-5.6-sol",
			}}, []string{business.server.URL})
			headers := map[string]string{"Session-Id": "explicit-break"}
			reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoChatBody("Analyze a complex migration."), headers)
			body := reasoningAutoChatBody("Use my chosen effort for this turn.")
			body["model"] = explicit.model
			if explicit.effort != "" {
				body["reasoning_effort"] = explicit.effort
			}
			reasoningAutoRequest(t, env, "/v1/chat/completions", body, headers)
			if got := len(judge.snapshot()); got != 1 {
				t.Fatalf("explicit turn invoked decision service; calls=%d", got)
			}
			// A tool continuation normally reuses state, but explicit control forces a new decision.
			reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoToolBody("completed"), headers)
			reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoChatBody("Finish the mechanical work."), headers)
			assertReasoningAutoEfforts(t, business.snapshot(), "xhigh", explicit.upstreamEffort, "xhigh", "medium")
			if got := len(judge.snapshot()); got != 3 {
				t.Fatalf("automatic mode did not reevaluate after explicit turn; calls=%d", got)
			}
		})
	}
}

func TestReasoningAutoFallbackBreaksDowngradeSequence(t *testing.T) {
	t.Parallel()
	for _, fallback := range []string{"invalid_answer", "incomplete_history", "incomplete_tool_history", "multimodal", "unknown_content_block", "unknown_input_item"} {
		t.Run(fallback, func(t *testing.T) {
			t.Parallel()
			responsesContext := fallback == "incomplete_history" || fallback == "incomplete_tool_history" || fallback == "unknown_content_block" || fallback == "unknown_input_item"
			efforts := []string{"high", "low", "low", "low", "medium"}
			if fallback == "invalid_answer" {
				efforts[2] = "not-an-effort"
			}
			if responsesContext || fallback == "multimodal" {
				efforts = []string{"high", "low", "low", "medium"}
			}
			if fallback == "incomplete_tool_history" {
				efforts[3] = "low"
			}
			judge := newReasoningAutoJudge(t, efforts...)
			business := newReasoningAutoBusiness(t)
			path, upstreamProtocol := "/v1/chat/completions", "openai"
			makeBody := reasoningAutoChatBody
			if responsesContext {
				path, upstreamProtocol = "/v1/responses", "codex"
				makeBody = func(text string) map[string]any {
					return map[string]any{"model": "gpt-5.6-sol", "input": []any{map[string]any{"role": "user", "content": text}}}
				}
			}
			env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
				name: "fallback", upstreamProtocol: upstreamProtocol, models: "gpt-5.6-sol",
			}}, []string{business.server.URL})
			headers := map[string]string{"Session-Id": "fallback-break"}
			reasoningAutoRequest(t, env, path, makeBody("Complex unresolved migration."), headers)
			reasoningAutoRequest(t, env, path, makeBody("The plan is ready."), headers)
			body := makeBody("Continue.")
			switch fallback {
			case "incomplete_history":
				body["previous_response_id"] = "unavailable-history"
			case "incomplete_tool_history":
				body["previous_response_id"] = "unavailable-history"
				body["input"] = []any{map[string]any{"type": "function_call_output", "call_id": "call-migration", "output": "mechanical step completed"}}
			case "unknown_content_block":
				body["input"] = []any{map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "unrecognized_content", "text": "opaque unresolved constraint"},
				}}}
			case "unknown_input_item":
				body["input"] = append(body["input"].([]any), map[string]any{"type": "future_context", "output": "opaque unresolved constraint"})
			case "multimodal":
				body["messages"] = []any{map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "text", "text": "Finish using the attached diagram."},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.invalid/diagram.png"}},
				}}}
			}
			reasoningAutoRequest(t, env, path, body, headers)
			reasoningAutoRequest(t, env, path, makeBody("Finish the mechanical work."), headers)
			reasoningAutoRequest(t, env, path, makeBody("Report the result."), headers)
			lastEffort := "medium"
			if fallback == "incomplete_tool_history" {
				lastEffort = "low"
			}
			assertReasoningAutoEfforts(t, business.snapshot(), "high", "high", "high", "high", lastEffort)
			decisions := judge.snapshot()
			if len(decisions) != len(efforts) {
				t.Fatalf("decision calls=%d, want %d", len(decisions), len(efforts))
			}
			entry := waitForProxyLogMatching(t, env, func(entry *model.LogEntry) bool {
				if entry.ReasoningDecision == nil {
					return false
				}
				decision := entry.ReasoningDecision
				switch fallback {
				case "incomplete_history", "incomplete_tool_history", "multimodal", "unknown_content_block", "unknown_input_item":
					return decision.ContextIncomplete && decision.Status == "fallback" && decision.Reason == "incomplete_context"
				default:
					return decision.Status == "fallback" && decision.Reason == "invalid_response"
				}
			})
			if entry == nil {
				t.Fatalf("%s did not persist an explanatory decision log", fallback)
			}
		})
	}
}

func TestReasoningAutoTruncatedContextKeepsInstructions(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"chat", "codex_agents", "codex_user_instructions"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			judge := newReasoningAutoJudge(t, "low")
			business := newReasoningAutoBusiness(t)
			upstream, endpoint, field := "openai", "/v1/chat/completions", "messages"
			markers := []string{"System rule: preserve historical state transitions.", "Developer rule: preserve concurrent writes."}
			items := []any{
				map[string]any{"role": "system", "content": markers[0] + strings.Repeat("规则。", 5000)},
				map[string]any{"role": "developer", "content": markers[1] + strings.Repeat("约束。", 5000)},
			}
			body := map[string]any{"model": "gpt-5.6-sol"}
			if kind != "chat" {
				upstream, endpoint, field = "codex", "/v1/responses", "input"
				prefix := "# AGENTS.md instructions\n"
				if kind == "codex_user_instructions" {
					prefix = "<user_instructions>\n"
				}
				markers = []string{"Top-level rule: preserve historical state transitions.", prefix + "Repository rule: preserve concurrent writes."}
				body["instructions"] = markers[0] + strings.Repeat("规则。", 5000)
				items = []any{map[string]any{"role": "user", "content": markers[1] + strings.Repeat("约束。", 5000)}}
			}
			items = append(items,
				map[string]any{"role": "user", "content": "Perform the migration."},
				map[string]any{"role": "assistant", "content": strings.Repeat("中间步骤已完成。", 5000)},
				map[string]any{"role": "user", "content": "Continue with verification."},
				map[string]any{"role": "assistant", "content": "Recent tail: verification is still pending."},
			)
			body[field] = items
			env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
				name: "instructions", upstreamProtocol: upstream, models: "gpt-5.6-sol",
			}}, []string{business.server.URL})
			reasoningAutoRequest(t, env, endpoint, body, nil)
			decisions := judge.snapshot()
			if len(decisions) != 1 {
				t.Fatalf("decision requests=%d, want 1", len(decisions))
			}
			conversation := decisions[0].Conversation
			if len(conversation) > 24*1024 || !utf8.ValidString(conversation) {
				t.Fatalf("invalid truncated context: bytes=%d UTF-8=%v", len(conversation), utf8.ValidString(conversation))
			}
			for _, required := range append(markers, "original task: Perform the migration.", "latest user request: Continue with verification.", "Recent tail: verification is still pending.") {
				if !strings.Contains(conversation, required) {
					t.Errorf("truncated decision omitted %q", required)
				}
			}
			assertReasoningAutoEfforts(t, business.snapshot(), "low")
		})
	}
}

func TestReasoningAutoToolMultimodalOutputBreaksDowngrade(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"function_call_output", "custom_tool_call_output"} {
		for _, contentType := range []string{"input_image", "image_url", "input_file"} {
			for _, encoded := range []bool{false, true} {
				for _, reuse := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/encoded=%v/reuse=%v", kind, contentType, encoded, reuse), func(t *testing.T) {
						t.Parallel()
						judge := newReasoningAutoJudge(t, "high", "low", "low", "low")
						business := newReasoningAutoBusiness(t)
						env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
							name: "tool-output", upstreamProtocol: "codex", models: "gpt-5.6-sol",
						}}, []string{business.server.URL})
						headers := map[string]string{"Session-Id": "multimodal-output"}
						makeBody := func(text string) map[string]any {
							return map[string]any{"model": "gpt-5.6-sol", "input": []any{map[string]any{"role": "user", "content": text}}}
						}
						reasoningAutoRequest(t, env, "/v1/responses", makeBody("Inspect the visual failure in this UI."), headers)
						reasoningAutoRequest(t, env, "/v1/responses", makeBody("The plan is ready."), headers)
						part := map[string]any{"type": contentType}
						switch contentType {
						case "input_image":
							part["image_url"], part["detail"] = "data:image/png;base64,AA==", "original"
						case "image_url":
							part["image_url"] = map[string]any{"url": "https://example.invalid/screenshot.png"}
						case "input_file":
							part["file_data"], part["filename"] = "data:application/pdf;base64,AA==", "diagram.pdf"
						}
						var output any = []any{map[string]any{"type": "input_text", "text": "Captured evidence."}, part}
						if encoded {
							data, err := json.Marshal(output)
							if err != nil {
								t.Fatal(err)
							}
							output = string(data)
						}
						body := makeBody("Inspect the visual failure in this UI.")
						call := map[string]any{"type": strings.TrimSuffix(kind, "_output"), "name": "capture", "call_id": "call-evidence"}
						if kind == "function_call_output" {
							call["arguments"] = "{}"
						} else {
							call["input"] = "{}"
						}
						items := append(body["input"].([]any), call, map[string]any{"type": kind, "call_id": "call-evidence", "output": output})
						if !reuse {
							items = append(items, map[string]any{"role": "user", "content": "Continue using the captured evidence."})
						}
						body["input"] = items
						reasoningAutoRequest(t, env, "/v1/responses", body, headers)
						reasoningAutoRequest(t, env, "/v1/responses", makeBody("Finish the mechanical work."), headers)
						reasoningAutoRequest(t, env, "/v1/responses", makeBody("Report the result."), headers)
						assertReasoningAutoEfforts(t, business.snapshot(), "high", "high", "high", "high", "low")
						if got := len(judge.snapshot()); got != 4 {
							t.Fatalf("decision calls=%d, want 4; multimodal evidence must skip judgment", got)
						}
						entry := waitForProxyLogMatching(t, env, func(entry *model.LogEntry) bool {
							d := entry.ReasoningDecision
							return d != nil && d.ContextIncomplete && d.Status == "fallback" && d.Reason == "incomplete_context"
						})
						if entry == nil {
							t.Fatal("multimodal tool output did not record incomplete context")
						}
						if reuse && entry.ReasoningDecision.Trigger != "tool_continuation" {
							t.Fatalf("expected reuse turn, got trigger %q", entry.ReasoningDecision.Trigger)
						}
					})
				}
			}
		}
	}
}

func TestReasoningAutoTextToolOutputRemainsComplete(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"function_call_output", "custom_tool_call_output"} {
		for _, output := range []any{
			`{"metadata":{"exit_code":0},"text":"completed"}`,
			`[{"file":"a.go","size":12}]`,
			`[1,2,3]`,
			`[{"type":"input_text","text":"completed"}]`,
			[]any{map[string]any{"type": "input_text", "text": "completed"}},
		} {
			t.Run(fmt.Sprintf("%s/%T/%v", kind, output, output), func(t *testing.T) {
				t.Parallel()
				judge := newReasoningAutoJudge(t, "high")
				business := newReasoningAutoBusiness(t)
				env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
					name: "text-output", upstreamProtocol: "codex", models: "gpt-5.6-sol",
				}}, []string{business.server.URL})
				body := map[string]any{"model": "gpt-5.6-sol", "input": []any{
					map[string]any{"role": "user", "content": "Complete the migration."},
					map[string]any{"type": kind, "call_id": "call-text", "output": output},
					map[string]any{"role": "user", "content": "Verify the result."},
				}}
				reasoningAutoRequest(t, env, "/v1/responses", body, nil)
				if got := len(judge.snapshot()); got != 1 {
					t.Fatalf("pure text output skipped judgment: requests=%d", got)
				}
				assertReasoningAutoEfforts(t, business.snapshot(), "high")
			})
		}
	}
}

func TestReasoningAutoTruncatedContextCountsTowardDowngrade(t *testing.T) {
	t.Parallel()
	judge := newReasoningAutoJudge(t, "high", "low", "low")
	business := newReasoningAutoBusiness(t)
	env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
		name: "truncated", upstreamProtocol: "openai", models: "gpt-5.6-sol",
	}}, []string{business.server.URL})
	headers := map[string]string{"Session-Id": "truncated-downgrade"}
	reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoChatBody("Complex unresolved migration."), headers)
	reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoChatBody("The plan is ready."), headers)
	body := reasoningAutoToolBody(strings.Repeat("并发写入已处理。", 5000))
	body["messages"] = append(body["messages"].([]any), map[string]any{"role": "user", "content": "Finish."})
	reasoningAutoRequest(t, env, "/v1/chat/completions", body, headers)
	assertReasoningAutoEfforts(t, business.snapshot(), "high", "high", "low")
	decisions := judge.snapshot()
	if len(decisions) != 3 {
		t.Fatalf("decision calls=%d, want 3", len(decisions))
	}
	if got := decisions[2].Conversation; len(got) > 24*1024 || !strings.Contains(got, "latest user request: Finish.") {
		t.Fatalf("truncated decision context len=%d lacks latest user request", len(got))
	}
	entry := waitForProxyLogMatching(t, env, func(entry *model.LogEntry) bool {
		decision := entry.ReasoningDecision
		return decision != nil && decision.ContextTruncated && decision.Status == "decided" && decision.AppliedEffort == "low"
	})
	if entry == nil {
		t.Fatal("truncated downgrade did not persist its decision log")
	}
}

func TestReasoningAutoFirstFailureUsesModelDefault(t *testing.T) {
	t.Parallel()
	judge := newReasoningAutoJudge(t, "invalid", "high")
	business := newReasoningAutoBusiness(t)
	env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
		name: "first-fallback", upstreamProtocol: "openai", models: "gpt-5.6-sol",
	}}, []string{business.server.URL})
	headers := map[string]string{"Session-Id": "first-failure"}
	reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoChatBody("Business should succeed when the first decision is invalid."), headers)
	reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoChatBody("Try the next automatic turn."), headers)
	assertReasoningAutoEfforts(t, business.snapshot(), "", "high")
	if got := len(judge.snapshot()); got != 2 {
		t.Fatalf("next turn did not recover from decision failure: calls=%d", got)
	}
}

func TestReasoningAutoChannelRulesOverrideAppliedEffort(t *testing.T) {
	t.Parallel()
	judge := newReasoningAutoJudge(t, "xhigh", "medium")
	business := newReasoningAutoBusiness(t)
	env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
		name: "rule-override", upstreamProtocol: "openai", models: "gpt-5.6-sol",
		customRequestRules: &model.CustomRequestRules{Body: []model.CustomBodyRule{{
			Action: model.RuleActionOverride, Path: "reasoning_effort", Value: json.RawMessage(`"low"`),
		}}},
	}}, []string{business.server.URL})
	headers := map[string]string{"Session-Id": "rules"}
	reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoChatBody("Complex task."), headers)
	reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoToolBody("completed"), headers)
	assertReasoningAutoEfforts(t, business.snapshot(), "low", "low")
	decisions := judge.snapshot()
	if len(decisions) != 2 || decisions[1].PreviousEffort != "" {
		t.Fatalf("unapplied suggestion became effective state: decisions=%+v", decisions)
	}
	entry := waitForProxyLogMatching(t, env, func(entry *model.LogEntry) bool {
		return entry.ReasoningDecision != nil && entry.ReasoningDecision.SuggestedEffort == "xhigh"
	})
	if entry == nil || entry.ThinkingEffort != "low" || entry.ReasoningDecision.AppliedEffort != "low" || entry.ReasoningDecision.Reason != "channel_rules_override" {
		t.Fatalf("channel override was not explained in the business log: %+v", entry)
	}
}

func TestReasoningAutoIgnoresResponsesSubresources(t *testing.T) {
	t.Parallel()
	judge := newReasoningAutoJudge(t, "high")
	requests := make(chan []byte, 1)
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"cmp-1","object":"response.compaction","output":[{"type":"compaction","encrypted_content":"opaque"}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	}))
	env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
		name: "compact", upstreamProtocol: "codex", models: "gpt-5.6-sol",
	}}, []string{upstream.URL})
	reasoningAutoRequest(t, env, "/v1/responses/compact", map[string]any{"model": "gpt-5.6-sol", "input": []any{
		map[string]any{"role": "user", "content": "long history"},
	}}, map[string]string{"Session-Id": "compact"})
	assertReasoningAutoEfforts(t, [][]byte{<-requests}, "")
	if got := len(judge.snapshot()); got != 0 {
		t.Fatalf("compaction called the decision model: calls=%d", got)
	}
}

func TestReasoningAutoIdentityIsolation(t *testing.T) {
	t.Parallel()
	for _, dimension := range []string{"no_session", "token", "thread", "model", "protocol"} {
		t.Run(dimension, func(t *testing.T) {
			t.Parallel()
			judge := newReasoningAutoJudge(t, "high", "low")
			business := newReasoningAutoBusiness(t)
			env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
				name: "isolated", upstreamProtocol: "openai", models: "gpt-5.6-sol,gpt-5.6-luna",
			}}, []string{business.server.URL})
			headers := map[string]string{"Session-Id": "shared-session", "Thread-Id": "thread-a"}
			if dimension == "no_session" {
				headers = nil
			}
			reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoChatBody("Complex task."), headers)
			body := reasoningAutoChatBody("Mechanical task.")
			path := "/v1/chat/completions"
			switch dimension {
			case "token":
				injectAPIToken(env.server.authService, "second-token", 0, 2)
				headers["Authorization"] = "Bearer second-token"
			case "thread":
				headers["Thread-Id"] = "thread-b"
			case "model":
				body["model"] = "gpt-5.6-luna"
			case "protocol":
				path = "/v1/responses"
				body = map[string]any{"model": "gpt-5.6-sol", "input": []any{map[string]any{"role": "user", "content": "Mechanical task."}}}
			}
			reasoningAutoRequest(t, env, path, body, headers)
			assertReasoningAutoEfforts(t, business.snapshot(), "high", "low")
			decisions := judge.snapshot()
			if len(decisions) != 2 || decisions[1].PreviousEffort != "" {
				t.Fatalf("reasoning state leaked across %s: %+v", dimension, decisions)
			}
		})
	}
}

func TestReasoningAutoStateExpiresWithExecutionSessionTTL(t *testing.T) {
	t.Parallel()
	judge := newReasoningAutoJudge(t, "high", "low")
	business := newReasoningAutoBusiness(t)
	env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
		name: "state-expiration", upstreamProtocol: "openai", models: "gpt-5.6-sol",
	}}, []string{business.server.URL})
	const ttl = 50 * time.Millisecond
	env.server.responsesExecutionSessions.ttlOverride = ttl
	headers := map[string]string{"Session-Id": "expired-session"}
	reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoChatBody("Complex migration."), headers)
	time.Sleep(2 * ttl)
	reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoChatBody("Independent mechanical work after expiry."), headers)
	assertReasoningAutoEfforts(t, business.snapshot(), "high", "low")
	decisions := judge.snapshot()
	if len(decisions) != 2 || decisions[1].PreviousEffort != "" {
		t.Fatalf("expired state remained available to the decision service: %+v", decisions)
	}
}

func TestReasoningAutoStateCapacityKeepsExistingSessionsAndIndependentOverflow(t *testing.T) {
	t.Parallel()
	const retainedSessions = 1024
	efforts := make([]string, retainedSessions+2)
	for i := range efforts {
		efforts[i] = "high"
	}
	efforts[len(efforts)-1] = "low"
	judge := newReasoningAutoJudge(t, efforts...)
	business := newReasoningAutoBusiness(t)
	env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
		name: "bounded-state", upstreamProtocol: "openai", models: "gpt-5.6-sol",
	}}, []string{business.server.URL})
	started := time.Now()
	for session := range retainedSessions {
		reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoChatBody("Complex task."), map[string]string{
			"Session-Id": fmt.Sprintf("retained-session-%d", session),
		})
	}
	overflowHeaders := map[string]string{"Session-Id": "overflow-session"}
	reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoChatBody("New complex task."), overflowHeaders)
	reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoChatBody("New mechanical task."), overflowHeaders)
	// Reuse the last admitted identity to verify the boundary accepts all 1024 sessions.
	reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoToolBody("completed"), map[string]string{
		"Session-Id": fmt.Sprintf("retained-session-%d", retainedSessions-1),
	})
	decisions := judge.snapshot()
	if len(decisions) != retainedSessions+2 || decisions[retainedSessions].PreviousEffort != "" || decisions[retainedSessions+1].PreviousEffort != "" {
		t.Fatalf("overflow retained state or displaced an existing session: decision count=%d", len(decisions))
	}
	requests := business.snapshot()
	if len(requests) != retainedSessions+3 {
		t.Fatalf("capacity limit rejected business requests: sends=%d", len(requests))
	}
	assertReasoningAutoEfforts(t, requests[retainedSessions:], "high", "low", "high")
	t.Logf("%d retained sessions, %d successful business sends, %d decision calls completed in %s", retainedSessions, len(requests), len(decisions), time.Since(started))
}

func TestReasoningAutoFailoverReusesSuggestionAndCommitsSupportedTarget(t *testing.T) {
	t.Parallel()
	for _, target := range []struct{ name, model, effort string }{
		{name: "unknown", model: "private-unknown-model"},
		{name: "supported", model: "gpt-5.5", effort: "xhigh"},
	} {
		t.Run(target.name, func(t *testing.T) {
			t.Parallel()
			judge := newReasoningAutoJudge(t, "xhigh")
			firstRequests := make(chan []byte, 1)
			first := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				firstRequests <- body
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"error":{"type":"server_error","message":"temporary outage"}}`)
			}))
			t.Cleanup(first.Close)
			business := newReasoningAutoBusiness(t)
			env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{
				{name: "first", upstreamProtocol: "openai", priority: 100, modelEntries: []model.ModelEntry{{Model: "gpt-5.6-sol", RedirectModel: target.model}}},
				{name: "supported", upstreamProtocol: "openai", priority: 90, models: "gpt-5.6-sol"},
			}, []string{first.URL, business.server.URL})
			headers := map[string]string{"Session-Id": "retry-turn"}
			reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoChatBody("Complex migration."), headers)
			assertReasoningAutoEfforts(t, [][]byte{<-firstRequests}, target.effort)
			// The unsupported first target must not consume the supported target's commit.
			reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoToolBody("mechanical step completed"), headers)
			assertReasoningAutoEfforts(t, business.snapshot(), "xhigh", "xhigh")
			if got := len(judge.snapshot()); got != 1 {
				t.Fatalf("route retry or tool continuation repeated decision: calls=%d", got)
			}
		})
	}
	t.Run("keys", func(t *testing.T) {
		t.Parallel()
		judge := newReasoningAutoJudge(t, "xhigh")
		requests := make(chan []byte, 2)
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil || !json.Valid(body) {
				t.Errorf("invalid key retry request: body=%s error=%v", body, err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			requests <- body
			w.Header().Set("Content-Type", "application/json")
			if r.Header.Get("Authorization") == "Bearer sk-first" {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"error":{"type":"server_error","message":"first key unavailable"}}`)
				return
			}
			if r.Header.Get("Authorization") != "Bearer sk-second" {
				t.Errorf("unexpected retry key: %q", r.Header.Get("Authorization"))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, `{"id":"chat-key-retry","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
		}))
		env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
			name: "retry-keys", upstreamProtocol: "openai", models: "gpt-5.6-sol", apiKey: "sk-first", retryOtherKeysOnFailure: true,
		}}, []string{upstream.URL})
		if err := env.store.CreateAPIKeysBatch(context.Background(), []*model.APIKey{{
			ChannelID: 2, KeyIndex: 1, APIKey: "sk-second", Priority: -1, KeyStrategy: model.KeyStrategySequential,
		}}); err != nil {
			t.Fatal(err)
		}
		reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoChatBody("Complex task requiring a second key."), map[string]string{"Session-Id": "key-retry"})
		if len(requests) != 2 {
			t.Fatalf("same-channel key retry sent %d attempts, want 2", len(requests))
		}
		assertReasoningAutoEfforts(t, [][]byte{<-requests, <-requests}, "xhigh", "xhigh")
		if got := len(judge.snapshot()); got != 1 {
			t.Fatalf("same-channel key retry repeated decision: calls=%d", got)
		}
	})
}

func TestReasoningAutoOverriddenRetryKeepsCommittedState(t *testing.T) {
	t.Parallel()
	judge := newReasoningAutoJudge(t, "xhigh", "low")
	firstRequests := make(chan []byte, 2)
	first := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		firstRequests <- body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"type":"server_error","message":"temporary outage"}}`)
	}))
	business := newReasoningAutoBusiness(t)
	env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{
		{name: "first", upstreamProtocol: "openai", priority: 100, models: "gpt-5.6-sol"},
		{name: "override", upstreamProtocol: "openai", priority: 90, models: "gpt-5.6-sol",
			customRequestRules: &model.CustomRequestRules{Body: []model.CustomBodyRule{{
				Action: model.RuleActionOverride, Path: "reasoning_effort", Value: json.RawMessage(`"low"`),
			}}}},
	}, []string{first.URL, business.server.URL})
	headers := map[string]string{"Session-Id": "committed-retry"}
	reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoChatBody("Complex migration."), headers)
	assertReasoningAutoEfforts(t, [][]byte{<-firstRequests}, "xhigh")
	// The first attempt committed xhigh; the overridden retry must not force a new decision.
	reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoToolBody("mechanical step completed"), headers)
	if got := len(judge.snapshot()); got != 1 {
		t.Fatalf("overridden retry discarded committed state: decision calls=%d", got)
	}
}

func TestReasoningAutoOlderHTTPDecisionCannotReplaceNewerState(t *testing.T) {
	t.Parallel()
	judge := newReasoningAutoJudge(t, "low", "xhigh")
	olderStarted := make(chan struct{})
	releaseOlder := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseOlder) }) })
	respond := judge.respond
	judge.respond = func(w http.ResponseWriter, r *http.Request, index int) {
		if index == 0 {
			close(olderStarted)
			select {
			case <-releaseOlder:
			case <-r.Context().Done():
				return
			}
		}
		respond(w, r, index)
	}
	business := newReasoningAutoBusiness(t)
	env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
		name: "concurrent", upstreamProtocol: "openai", models: "gpt-5.6-sol",
	}}, []string{business.server.URL})
	headers := map[string]string{"Session-Id": "concurrent-session"}
	olderDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		olderDone <- doProxyRequest(t, env.engine, "/v1/chat/completions", reasoningAutoChatBody("Older simple task."), headers)
	}()
	select {
	case <-olderStarted:
	case <-time.After(time.Second):
		t.Fatal("older decision did not start")
	}
	reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoChatBody("Newer complex task."), headers)
	releaseOnce.Do(func() { close(releaseOlder) })
	select {
	case response := <-olderDone:
		if response.Code != http.StatusOK {
			t.Fatalf("older request status=%d body=%s", response.Code, response.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("older request did not finish")
	}
	reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoToolBody("completed"), headers)
	assertReasoningAutoEfforts(t, business.snapshot(), "xhigh", "low", "xhigh")
	if got := len(judge.snapshot()); got != 2 {
		t.Fatalf("tool continuation should reuse newer state; decisions=%d", got)
	}
}

func TestReasoningAutoCanceledBeforeBusinessSendDoesNotCommit(t *testing.T) {
	t.Parallel()
	judge := newReasoningAutoJudge(t, "high", "xhigh")
	decisionStarted := make(chan struct{})
	respond := judge.respond
	judge.respond = func(w http.ResponseWriter, r *http.Request, index int) {
		if index == 1 {
			close(decisionStarted)
			<-r.Context().Done()
			return
		}
		respond(w, r, index)
	}
	business := newReasoningAutoBusiness(t)
	env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
		name: "cancel", upstreamProtocol: "openai", models: "gpt-5.6-sol",
	}}, []string{business.server.URL})
	headers := map[string]string{"Session-Id": "cancel-before-send"}
	reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoChatBody("Complex task."), headers)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	encoded, err := json.Marshal(reasoningAutoChatBody("Even more complex task."))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(encoded)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer test-api-key")
	request.Header.Set("Session-Id", headers["Session-Id"])
	done := make(chan struct{})
	go func() {
		env.engine.ServeHTTP(httptest.NewRecorder(), request)
		close(done)
	}()
	select {
	case <-decisionStarted:
	case <-time.After(time.Second):
		t.Fatal("cancelled request did not reach decision service")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled request did not finish")
	}
	reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoToolBody("completed"), headers)
	assertReasoningAutoEfforts(t, business.snapshot(), "high", "high")
	if got := len(judge.snapshot()); got != 2 {
		t.Fatalf("cancelled turn changed effective state; decisions=%d", got)
	}
}

func TestReasoningAutoWebsocketIncrementalAndReplayShareDecision(t *testing.T) {
	t.Parallel()
	judge := newReasoningAutoJudge(t, "low", "xhigh")
	requests := make(chan []byte, 3)
	var handshakes atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade reasoning business websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		connection := handshakes.Add(1)
		turns := 1
		if connection == 1 {
			turns = 2
		}
		for turn := range turns {
			_, body, err := conn.ReadMessage()
			if err != nil {
				t.Errorf("read reasoning websocket business request: %v", err)
				return
			}
			requests <- body
			if connection == 1 && turn == 1 {
				_ = conn.WriteJSON(map[string]any{
					"type": "error", "status": http.StatusBadRequest,
					"error": map[string]any{"type": "invalid_request_error", "code": "previous_response_not_found", "message": "No response found for previous_response_id resp-first"},
				})
				return
			}
			id, text := "resp-first", "settled migration context"
			if connection != 1 {
				id, text = "resp-replayed", "complex analysis complete"
			}
			if err := conn.WriteJSON(reasoningAutoCompletedEvent(id, text)); err != nil {
				t.Errorf("write reasoning websocket completion: %v", err)
				return
			}
		}
	}))
	t.Cleanup(upstream.Close)
	env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
		name: "native", upstreamProtocol: "codex", websockets: true, models: "gpt-5.6-sol",
	}}, []string{upstream.URL})
	conn := dialResponsesWebsocketWithSessionID(t, env.engine, "ws-history")
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "gpt-5.6-sol(auto)", "reasoning": map[string]any{"effort": "high", "summary": "auto"}, "input": []any{
		map[string]any{"type": "reasoning", "id": "rs-history", "encrypted_content": "opaque-encrypted-context"},
		map[string]any{"role": "user", "content": "Migrate the database without losing concurrent writes."},
	}}); err != nil {
		t.Fatal(err)
	}
	readWebsocketUntilType(t, conn, "response.completed")
	if err := conn.WriteJSON(map[string]any{"type": "response.create", "previous_response_id": "resp-first", "reasoning": map[string]any{"effort": "low", "summary": "auto"}, "input": []any{
		map[string]any{"role": "user", "content": "Continue; now prove the rollback remains lock-free."},
	}}); err != nil {
		t.Fatal(err)
	}
	readWebsocketUntilType(t, conn, "response.completed")
	first, incremental, replay := <-requests, <-requests, <-requests
	assertReasoningAutoEfforts(t, [][]byte{first, incremental, replay}, "low", "xhigh", "xhigh")
	for _, body := range [][]byte{first, incremental, replay} {
		if gjson.GetBytes(body, "model").String() != "gpt-5.6-sol" || gjson.GetBytes(body, "reasoning.summary").String() != "auto" {
			t.Fatalf("native WS model or summary changed: %s", body)
		}
	}
	if gjson.GetBytes(incremental, "input.#").Int() != 1 || gjson.GetBytes(incremental, "previous_response_id").String() != "resp-first" {
		t.Fatalf("native turn was not incremental: %s", incremental)
	}
	if gjson.GetBytes(replay, "input.#").Int() != 4 || gjson.GetBytes(replay, "previous_response_id").Exists() || handshakes.Load() != 2 {
		t.Fatalf("reconnected turn lost full replay: handshakes=%d body=%s", handshakes.Load(), replay)
	}
	if gjson.GetBytes(replay, "input.0.type").String() != "reasoning" || gjson.GetBytes(replay, "input.0.encrypted_content").String() != "opaque-encrypted-context" {
		t.Fatalf("reconnected replay lost valid opaque reasoning: %s", replay)
	}
	decisions := judge.snapshot()
	if len(decisions) != 2 {
		t.Fatalf("WS replay repeated decision call: calls=%d", len(decisions))
	}
	for _, retained := range []string{"without losing concurrent writes", "settled migration context", "prove the rollback remains lock-free"} {
		if !strings.Contains(decisions[1].Conversation, retained) {
			t.Fatalf("incremental WS decision lost %q: %q", retained, decisions[1].Conversation)
		}
	}
}

func TestReasoningAutoHTTPAndWebsocketShareState(t *testing.T) {
	t.Parallel()
	judge := newReasoningAutoJudge(t, "high", "low", "medium")
	business := newReasoningAutoBusiness(t)
	env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
		name: "shared-transports", upstreamProtocol: "codex", models: "gpt-5.6-sol",
	}}, []string{business.server.URL})
	headers := map[string]string{"Session-Id": "same-execution", "Thread-Id": "same-thread"}
	reasoningAutoRequest(t, env, "/v1/responses", map[string]any{
		"model": "gpt-5.6-sol", "stream": false, "input": []any{map[string]any{"role": "user", "content": "Analyze a difficult migration."}},
	}, headers)
	conn := dialResponsesWebsocketWithTokenAndHeaders(t, env.engine, "test-api-key", http.Header{
		"Session-Id": []string{headers["Session-Id"]}, "Thread-Id": []string{headers["Thread-Id"]},
	})
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "gpt-5.6-sol", "input": []any{
		map[string]any{"role": "user", "content": "The migration is settled; perform the mechanical work."},
	}}); err != nil {
		t.Fatal(err)
	}
	readWebsocketUntilType(t, conn, "response.completed")
	reasoningAutoRequest(t, env, "/v1/responses", map[string]any{
		"model": "gpt-5.6-sol", "stream": true, "previous_response_id": "resp-auto-2",
		"input": []any{
			map[string]any{"type": "reasoning", "id": "rs-local-history", "encrypted_content": "opaque-local-history"},
			map[string]any{"type": "function_call", "id": "fc-verify", "call_id": "call-verify", "name": "verify_migration", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "call-verify", "output": "migration verification completed"},
			map[string]any{"role": "user", "content": "Continue and report the result."},
		},
	}, headers)
	assertReasoningAutoEfforts(t, business.snapshot(), "high", "high", "medium")
	decisions := judge.snapshot()
	if len(decisions) != 3 || decisions[1].PreviousEffort != "high" || decisions[2].PreviousEffort != "high" {
		t.Fatalf("transport switch lost reasoning state: %+v", decisions)
	}
	if !strings.Contains(decisions[2].Conversation, "migration is settled") || !strings.Contains(decisions[2].Conversation, "business answer") || !strings.Contains(decisions[2].Conversation, "migration verification completed") {
		t.Fatalf("HTTP continuation lost locally owned WS history: %q", decisions[2].Conversation)
	}
	entry := waitForProxyLogMatching(t, env, func(entry *model.LogEntry) bool {
		return entry.ReasoningDecision != nil && entry.ReasoningDecision.SuggestedEffort == "medium"
	})
	if entry == nil || entry.ThinkingEffort != "medium" || entry.ReasoningDecision.AppliedEffort != "medium" || entry.ReasoningDecision.ContextIncomplete {
		t.Fatalf("valid opaque reasoning history incorrectly caused fallback: %+v", entry)
	}
}

func TestReasoningAutoZedProviderEnvelopeCommitsEffort(t *testing.T) {
	t.Parallel()
	judge := newReasoningAutoJudge(t, "high", "low", "low")
	credential, err := (&zedauth.Credential{
		UserID: "business-user", NativeCredential: json.RawMessage(`{"token":"native"}`),
		AccessToken: "business-access", ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan []byte, 4)
	var turn atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || !json.Valid(body) {
			t.Errorf("invalid Zed business request: body=%s error=%v", body, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer business-access" || gjson.GetBytes(body, "provider").String() != zedauth.ProviderOpenAI {
			t.Errorf("unexpected Zed business wire: path=%q authorization=%q body=%s", r.URL.Path, r.Header.Get("Authorization"), body)
		}
		requests <- body
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(w, "{\"status\":\"started\"}\n")
		_ = json.NewEncoder(w).Encode(map[string]any{"event": map[string]any{"type": "response.output_text.delta", "delta": "business answer"}})
		_ = json.NewEncoder(w).Encode(map[string]any{"event": reasoningAutoCompletedEvent(fmt.Sprintf("resp-zed-auto-%d", turn.Add(1)), "business answer")})
		_, _ = io.WriteString(w, "{\"status\":\"stream_ended\"}\n")
	}))
	env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
		name: "zed-business", upstreamProtocol: "codex", authType: model.AuthTypeZedOAuth,
		oauthCredential: credential, models: "gpt-5.6-sol",
	}}, []string{upstream.URL})
	headers := map[string]string{"Session-Id": "zed-state"}
	for _, text := range []string{"Analyze the difficult migration.", "The plan is settled.", "Finish the mechanical work."} {
		reasoningAutoRequest(t, env, "/v1/responses", map[string]any{
			"model": "gpt-5.6-sol", "input": []any{map[string]any{"role": "user", "content": text}},
		}, headers)
	}
	reasoningAutoRequest(t, env, "/v1/responses", map[string]any{
		"model": "gpt-5.6-sol", "input": []any{
			map[string]any{"role": "user", "content": "Finish the settled migration."},
			map[string]any{"type": "function_call", "id": "fc-migration", "call_id": "call-migration", "name": "migration_step", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "call-migration", "output": "step completed"},
		},
	}, headers)
	got := make([]string, 4)
	for i := range got {
		got[i] = gjson.GetBytes(<-requests, "provider_request.reasoning.effort").String()
	}
	if !slices.Equal(got, []string{"high", "high", "low", "low"}) || len(judge.snapshot()) != 3 {
		t.Fatalf("Zed did not commit actual envelope effort: efforts=%v decisions=%d", got, len(judge.snapshot()))
	}
	entry := waitForProxyLogMatching(t, env, func(entry *model.LogEntry) bool {
		return entry.ReasoningDecision != nil && entry.ReasoningDecision.Status == "reused"
	})
	if entry == nil || entry.ThinkingEffort != "low" || entry.ReasoningDecision.AppliedEffort != "low" {
		t.Fatalf("Zed reuse log lost actual provider effort: %+v", entry)
	}
	logs, err := env.store.ListLogs(context.Background(), time.Now().Add(-time.Minute), 10, 0, &model.LogFilter{LogSource: model.LogSourceProxy})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, entry := range logs {
		if entry.ReasoningDecision == nil || entry.ReasoningDecision.AppliedEffort != entry.ThinkingEffort {
			t.Fatalf("Zed business log disagrees with actual effort: %+v", entry)
		}
		counts[entry.ThinkingEffort]++
	}
	if len(logs) != 4 || counts["high"] != 2 || counts["low"] != 2 {
		t.Fatalf("Zed business log efforts=%v, log count=%d", counts, len(logs))
	}
}

func TestReasoningAutoWebsocketInheritedEffortIsNotClientExplicit(t *testing.T) {
	t.Parallel()
	judge := newReasoningAutoJudge(t, "xhigh", "low", "medium", "high")
	business := newReasoningAutoBusiness(t)
	env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
		name: "ws-explicit", upstreamProtocol: "codex", models: "gpt-5.6-sol",
	}}, []string{business.server.URL})
	conn := dialResponsesWebsocketWithSessionID(t, env.engine, "ws-inherited-effort")
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for turn, text := range []string{"Complex migration.", "Use a low effort body hint.", "Continue.", "Finish the settled plan."} {
		body := map[string]any{"type": "response.create", "input": []any{map[string]any{"role": "user", "content": text}}}
		if turn == 0 {
			body["model"] = "gpt-5.6-sol"
		} else {
			body["previous_response_id"] = fmt.Sprintf("resp-auto-%d", turn)
		}
		if turn == 1 {
			body["reasoning"] = map[string]any{"effort": "low"}
		}
		if err := conn.WriteJSON(body); err != nil {
			t.Fatal(err)
		}
		readWebsocketUntilType(t, conn, "response.completed")
		wantCalls := turn + 1
		if got := len(judge.snapshot()); got != wantCalls {
			t.Fatalf("after WS turn %d decision calls=%d, want %d", turn+1, got, wantCalls)
		}
	}
	assertReasoningAutoEfforts(t, business.snapshot(), "xhigh", "xhigh", "medium", "high")
}

func TestReasoningAutoLogsCorrelateAndDecisionDoesNotChargeClient(t *testing.T) {
	t.Parallel()
	judge := newReasoningAutoJudge(t, "xhigh")
	business := newReasoningAutoBusiness(t)
	env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
		name: "billable-business", upstreamProtocol: "openai", models: "gpt-5.6-sol",
	}}, []string{business.server.URL})
	ctx := context.Background()
	for _, channelID := range []int64{1, 2} {
		if err := env.store.UpdateAPIKeyCostMultipliers(ctx, channelID, map[int]float64{0: 1}); err != nil {
			t.Fatal(err)
		}
	}
	channel, err := env.store.GetConfig(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	inputPrice, outputPrice := 10.0, 20.0
	channel.ModelEntries[0].Pricing = &util.CustomModelPrice{InputPrice: &inputPrice, OutputPrice: &outputPrice}
	if _, err := env.store.UpdateConfig(ctx, channel.ID, channel); err != nil {
		t.Fatal(err)
	}
	client := &model.AuthToken{
		Token: model.HashToken("test-api-key"), CreatedAt: time.Now(), IsActive: true,
		AllowedChannelIDs: []int64{2}, CostLimitMicroUSD: 1_000_000, MaxConcurrency: 1,
	}
	if err := env.store.CreateAuthToken(ctx, client); err != nil {
		t.Fatal(err)
	}
	if err := env.server.authService.ReloadAuthTokens(); err != nil {
		t.Fatal(err)
	}
	reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoChatBody("Complex task with independent decision billing."), map[string]string{"Session-Id": "billing"})
	assertReasoningAutoEfforts(t, business.snapshot(), "xhigh")
	proxyLog := waitForProxyLog(t, env, "gpt-5.6-sol")
	var decisionLog *model.LogEntry
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		logs, err := env.store.ListLogs(ctx, time.Now().Add(-time.Minute), 10, 0, &model.LogFilter{LogSource: model.LogSourceDecision})
		if err != nil {
			t.Fatal(err)
		}
		if len(logs) == 1 {
			decisionLog = logs[0]
			break
		}
	}
	if decisionLog == nil || proxyLog.ReasoningDecision == nil || decisionLog.ReasoningDecision == nil {
		t.Fatalf("missing decision logs/metadata: proxy=%+v decision=%+v", proxyLog, decisionLog)
	}
	if proxyLog.ThinkingEffort != "xhigh" || proxyLog.ReasoningDecision.AppliedEffort != "xhigh" || proxyLog.ReasoningDecision.SuggestedEffort != "xhigh" {
		t.Fatalf("business log did not retain suggested/applied effort: %+v", proxyLog)
	}
	if proxyLog.ReasoningDecision.DecisionID == "" || proxyLog.ReasoningDecision.DecisionID != decisionLog.ReasoningDecision.DecisionID {
		t.Fatalf("decision correlation mismatch: proxy=%+v decision=%+v", proxyLog.ReasoningDecision, decisionLog.ReasoningDecision)
	}
	if decisionLog.AuthTokenID != 0 || decisionLog.ChannelID != 1 || decisionLog.InputTokens != 1000 || decisionLog.OutputTokens != 100 || decisionLog.Cost <= 0 || decisionLog.ReasoningDecision.CostUnknown {
		t.Fatalf("decision call lost independent usage/cost identity: %+v", decisionLog)
	}
	if proxyLog.AuthTokenID != client.ID || proxyLog.ChannelID != 2 || proxyLog.InputTokens != 3 || proxyLog.OutputTokens != 2 {
		t.Fatalf("decision usage contaminated business log: %+v", proxyLog)
	}
	var stored *model.AuthToken
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		stored, err = env.store.GetAuthToken(ctx, client.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.SuccessCount == 1 {
			break
		}
	}
	if stored.SuccessCount != 1 || stored.PromptTokensTotal != 3 || stored.CompletionTokensTotal != 2 || math.Abs(stored.TotalCostUSD-proxyLog.Cost) > 1e-10 || stored.CostUsedMicroUSD != util.USDToMicroUSD(proxyLog.Cost) {
		t.Fatalf("decision call charged client quota: token=%+v business cost=%g decision cost=%g", stored, proxyLog.Cost, decisionLog.Cost)
	}
}
