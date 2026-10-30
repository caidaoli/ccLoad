package app

import (
	"bytes"
	"cmp"
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
	Conversation string `json:"conversation"`
	// Levels are the criteria keys in request order.
	Levels []string `json:"-"`
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
		// "effort:confidence" adds the model's confidence score to the answer.
		choice, confidence, scored := strings.Cut(efforts[index], ":")
		answer := map[string]any{"type": "choice", "choice": choice}
		if scored {
			answer["confidence"] = json.RawMessage(confidence)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "judge", "answers": map[string]any{"effort": answer},
			"usage": map[string]any{"input_tokens": 1000, "output_tokens": 100},
		})
	}
	judge.server = newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer sk-judge" {
			t.Errorf("unexpected decision request: %s %s, authorization=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body struct {
			Model     string `json:"model"`
			State     string `json:"state"`
			Questions struct {
				Effort struct {
					Criteria json.RawMessage `json:"criteria"`
				} `json:"effort"`
			} `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode decision request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// The state carries only the conversation; the effective level stays local.
		var request reasoningAutoJudgeRequest
		state := json.NewDecoder(strings.NewReader(body.State))
		state.DisallowUnknownFields()
		if err := state.Decode(&request); err != nil {
			t.Errorf("decode decision state: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if body.Model != "judge" || !utf8.ValidString(request.Conversation) {
			t.Errorf("invalid decision state: model=%q, valid UTF-8=%v", body.Model, utf8.ValidString(request.Conversation))
		}
		gjson.ParseBytes(body.Questions.Effort.Criteria).ForEach(func(level, _ gjson.Result) bool {
			request.Levels = append(request.Levels, level.String())
			return true
		})
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

// reasoningAutoJudgedPreviousEfforts returns the effective level each judged
// business turn started from, in request order.
func reasoningAutoJudgedPreviousEfforts(t testing.TB, env *proxyTestEnv, want int) []string {
	t.Helper()
	var judged []*model.LogEntry
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		logs, err := env.store.ListLogs(context.Background(), time.Now().Add(-time.Minute), want*4+100, 0, &model.LogFilter{LogSource: model.LogSourceProxy})
		if err != nil {
			t.Fatal(err)
		}
		judged = judged[:0]
		for _, entry := range logs {
			if entry.ClientProtocol != util.ProtocolSystemOne && entry.ReasoningDecision != nil && entry.ReasoningDecision.SuggestedEffort != "" {
				judged = append(judged, entry)
			}
		}
		if len(judged) >= want {
			break
		}
	}
	if len(judged) != want {
		t.Fatalf("judged business logs=%d, want %d", len(judged), want)
	}
	slices.SortFunc(judged, func(a, b *model.LogEntry) int { return cmp.Compare(a.ID, b.ID) })
	previous := make([]string, len(judged))
	for i, entry := range judged {
		previous[i] = entry.ReasoningDecision.PreviousEffort
	}
	return previous
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
		if r.URL.Path == "/v1/messages" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"msg-auto","type":"message","role":"assistant","model":"gpt-5.6-sol","content":[{"type":"text","text":"business answer"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`)
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
		name: "reasoning-judge", authType: model.AuthTypeSystemOne, models: "judge", apiKey: "sk-judge",
	}}, channels...)
	upstreamURLs := map[int]string{0: judgeURL}
	for i, url := range urls {
		upstreamURLs[i+1] = url
	}
	policy, err := json.Marshal(model.ReasoningAutoConfig{
		Enabled: true, DecisionChannelID: 1, DecisionModel: "judge",
		BusinessModels: []string{"gpt-5.6-sol", "gpt-5.6-luna", "kimi-k3", "grok-4.7"}, TimeoutMS: 2000,
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
		for _, path := range []string{"reasoning_effort", "reasoning.effort", "output_config.effort"} {
			if got[i] == "" {
				got[i] = gjson.GetBytes(request, path).String()
			}
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
	if previous := reasoningAutoJudgedPreviousEfforts(t, env, 4); !slices.Equal(previous, []string{"", "low", "xhigh", "xhigh"}) {
		t.Fatalf("previous efforts=%v", previous)
	}
	if !strings.Contains(decisions[2].Conversation, "lock-free migration") || !strings.Contains(decisions[2].Conversation, "继续") || !strings.Contains(decisions[3].Conversation, "finish the mechanical work") {
		t.Fatalf("decision lost the original task or latest instruction: %q", decisions[3].Conversation)
	}
}

// The judge chooses among the business model's own catalog levels, listed in
// ascending order, so a model with max can be sent max. Catalogs ccLoad has no
// executor for (kimi, xai) still supply levels for generic channels.
func TestReasoningAutoDecisionOffersCatalogLevels(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		model, choice string
		levels        []string
	}{
		{"gpt-5.6-sol", "max", []string{"low", "medium", "high", "xhigh", "max"}},
		{"kimi-k3", "max", []string{"low", "high", "max"}},
		{"grok-4.7", "xhigh", []string{"low", "medium", "high", "xhigh"}},
	} {
		t.Run(tc.model, func(t *testing.T) {
			t.Parallel()
			judge := newReasoningAutoJudge(t, tc.choice)
			business := newReasoningAutoBusiness(t)
			env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
				name: "business", upstreamProtocol: "openai", models: tc.model,
			}}, []string{business.server.URL})
			body := reasoningAutoChatBody("Prove the scheduler never deadlocks under any interleaving.")
			body["model"] = tc.model
			reasoningAutoRequest(t, env, "/v1/chat/completions", body, nil)
			assertReasoningAutoEfforts(t, business.snapshot(), tc.choice)
			decisions := judge.snapshot()
			if len(decisions) != 1 || !slices.Equal(decisions[0].Levels, tc.levels) {
				t.Fatalf("decision levels=%v, want the %s catalog in ascending order", decisions, tc.model)
			}
		})
	}
}

// The live request list shows the level actually sent, with the client's own
// level alongside only when the automatic decision changed it.
func TestReasoningAutoActiveRequestShowsSentEffort(t *testing.T) {
	t.Parallel()
	judge := newReasoningAutoJudge(t, "xhigh")
	var envRef atomic.Pointer[proxyTestEnv]
	var seen []*ActiveRequest
	business := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		seen = envRef.Load().server.activeRequests.List()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chat-auto","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	}))
	env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
		name: "business", upstreamProtocol: "openai", models: "gpt-5.6-sol",
	}}, []string{business.URL})
	envRef.Store(env)
	body := reasoningAutoChatBody("Prove the scheduler never deadlocks under any interleaving.")
	body["reasoning_effort"] = "low"
	reasoningAutoRequest(t, env, "/v1/chat/completions", body, nil)
	if len(seen) != 1 || seen[0].ThinkingEffort != "xhigh" || seen[0].ClientEffort != "low" {
		t.Fatalf("active requests=%+v, want sent xhigh with client low", seen)
	}
}

// An unsure answer is recorded but decides nothing: the client's own level or
// the session's established level stands, and the next turn asks again.
func TestReasoningAutoLowConfidenceKeepsLevel(t *testing.T) {
	t.Parallel()
	judge := newReasoningAutoJudge(t, "medium:0.1", "high:0.9", "low:0.2", "low:0.8")
	business := newReasoningAutoBusiness(t)
	env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
		name: "business", upstreamProtocol: "openai", models: "gpt-5.6-sol",
	}}, []string{business.server.URL})
	headers := map[string]string{"Session-Id": "low-confidence"}
	messages := []any{map[string]any{"role": "user", "content": "Explain this error."}}
	reasoningAutoRequest(t, env, "/v1/chat/completions", map[string]any{"model": "gpt-5.6-sol", "messages": messages, "reasoning_effort": "xhigh"}, headers)
	entry := waitForProxyLogMatching(t, env, func(entry *model.LogEntry) bool {
		return entry.ClientProtocol != util.ProtocolSystemOne && entry.ReasoningDecision != nil && entry.ReasoningDecision.Reason == "low_confidence"
	})
	if decision := entry.ReasoningDecision; decision.Status != "fallback" || decision.SuggestedEffort != "medium" ||
		decision.Confidence == nil || *decision.Confidence != 0.1 || decision.AppliedEffort != "" {
		t.Fatalf("low confidence decision=%+v", decision)
	}
	for _, next := range []string{"Redesign the storage engine for concurrent writers.", "继续"} {
		messages = append(messages, map[string]any{"role": "user", "content": next})
		reasoningAutoRequest(t, env, "/v1/chat/completions", map[string]any{"model": "gpt-5.6-sol", "messages": messages}, headers)
	}
	// A tool continuation would reuse the level; the unsure turn forces a new decision.
	reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoToolBody("completed"), headers)
	assertReasoningAutoEfforts(t, business.snapshot(), "xhigh", "high", "high", "high")
	decisions := judge.snapshot()
	if len(decisions) != 4 {
		t.Fatalf("decisions=%d, want 4", len(decisions))
	}
	if previous := reasoningAutoJudgedPreviousEfforts(t, env, 4); previous[3] != "high" {
		t.Fatalf("previous efforts=%v", previous)
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

// Tool content is judged by its first text line, array or string: a
// marker deep in a dump is not a failure, a failing status line is.
func TestReasoningAutoArrayToolContentUsesFirstLine(t *testing.T) {
	t.Parallel()
	judge := newReasoningAutoJudge(t, "low", "high", "xhigh")
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
	// Coded compiler diagnostics (rustc error[E0382]:) are a failing status line too.
	tool["content"] = "error[E0382]: borrow of moved value: `snapshot`\n --> src/raft.rs:12:5"
	reasoningAutoRequest(t, env, "/v1/chat/completions", body, headers)
	assertReasoningAutoEfforts(t, business.snapshot(), "low", "low", "high", "xhigh")
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

// Codex CLI leads with additional_tools and may end a turn with
// configuration_update; neither makes the context incomplete or hides user input.
func TestReasoningAutoCodexMetadataItems(t *testing.T) {
	t.Parallel()
	judge := newReasoningAutoJudge(t, "low", "xhigh")
	business := newReasoningAutoBusiness(t)
	env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
		name: "codex-metadata", upstreamProtocol: "codex", models: "gpt-5.6-sol",
	}}, []string{business.server.URL})
	message := func(role, kind, text string) map[string]any {
		return map[string]any{"type": "message", "role": role, "content": []any{map[string]any{"type": kind, "text": text}}}
	}
	input := []any{
		map[string]any{"type": "additional_tools", "role": "developer", "tools": []any{map[string]any{
			"type": "custom", "name": "exec", "description": "Run JavaScript code to orchestrate tool calls"}}},
		message("user", "input_text", "Review the uncommitted changes."),
		map[string]any{"type": "configuration_update", "reasoning": map[string]any{"effort": "high"}},
	}
	headers := map[string]string{"Session-Id": "codex-metadata"}
	reasoningAutoRequest(t, env, "/v1/responses", map[string]any{"model": "gpt-5.6-sol", "stream": true, "input": input}, headers)
	input = append(input,
		message("assistant", "output_text", "The changes look fine."),
		message("user", "input_text", "Now redesign the ledger migration for zero downtime."),
		map[string]any{"type": "configuration_update", "reasoning": map[string]any{"effort": "high"}},
	)
	reasoningAutoRequest(t, env, "/v1/responses", map[string]any{"model": "gpt-5.6-sol", "stream": true, "input": input}, headers)
	assertReasoningAutoEfforts(t, business.snapshot(), "low", "xhigh")
	decisions := judge.snapshot()
	if len(decisions) != 2 {
		t.Fatalf("decision requests=%d, want 2", len(decisions))
	}
	if strings.Contains(decisions[0].Conversation, "orchestrate tool calls") || !strings.Contains(decisions[0].Conversation, "Review the uncommitted changes.") {
		t.Fatalf("decision context mishandled Codex metadata items: %s", decisions[0].Conversation)
	}
}

// Codex multi-agent v2 delivers tasks and reports as agent_message items whose
// payload may be encrypted; they are conversation, not unknown items.
func TestReasoningAutoCodexAgentMessages(t *testing.T) {
	t.Parallel()
	judge := newReasoningAutoJudge(t, "high", "xhigh")
	business := newReasoningAutoBusiness(t)
	env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
		name: "codex-agents", upstreamProtocol: "codex", models: "gpt-5.6-sol",
	}}, []string{business.server.URL})
	part := func(kind, value string) map[string]any { return map[string]any{"type": kind, "text": value} }
	agentMessage := func(author, recipient string, content ...any) map[string]any {
		return map[string]any{"type": "agent_message", "author": author, "recipient": recipient, "content": content}
	}
	input := []any{
		map[string]any{"type": "message", "role": "user", "content": []any{part("input_text", "Review the uncommitted changes.")}},
		agentMessage("/root", "/root/storage_review", part("input_text", "Message Type: NEW_TASK\nTask name: /root/storage_review\nSender: /root\nPayload:\n"),
			map[string]any{"type": "encrypted_content", "encrypted_content": "gAAAAABqy1lo" + strings.Repeat("Ab0-", 120) + "=="}),
	}
	headers := map[string]string{"Session-Id": "codex-root", "Thread-Id": "storage-review"}
	reasoningAutoRequest(t, env, "/v1/responses", map[string]any{"model": "gpt-5.6-sol", "stream": true, "input": input}, headers)
	input = append(input,
		agentMessage("/root/web_review", "/root/storage_review", part("input_text", "Message Type: FINAL_ANSWER\nTask name: /root/storage_review\nSender: /root/web_review\nPayload:\nNo frontend findings.")),
		map[string]any{"type": "function_call", "call_id": "call_1", "name": "shell", "arguments": `{"command":"go test ./internal/storage"}`},
		map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "Exit code: 1\nFAIL hybrid_store_test.go"},
	)
	reasoningAutoRequest(t, env, "/v1/responses", map[string]any{"model": "gpt-5.6-sol", "stream": true, "input": input}, headers)
	assertReasoningAutoEfforts(t, business.snapshot(), "high", "xhigh")
	decisions := judge.snapshot()
	if len(decisions) != 2 {
		t.Fatalf("decision requests=%d, want 2 (agent messages must not mark context incomplete)", len(decisions))
	}
	first, second := decisions[0].Conversation, decisions[1].Conversation
	if !strings.Contains(first, "Task name: /root/storage_review") || !strings.Contains(first, "[encrypted]") || strings.Contains(first, "gAAAAA") ||
		!strings.Contains(second, "No frontend findings.") {
		t.Fatalf("agent messages mishandled: first=%s\nsecond=%s", first, second)
	}
}

// A subagent thread's only user messages are injected context; its task is the
// parent's NEW_TASK and must survive truncation as the task digest.
func TestReasoningAutoCodexSubagentTaskSurvivesTruncation(t *testing.T) {
	t.Parallel()
	judge := newReasoningAutoJudge(t, "high")
	business := newReasoningAutoBusiness(t)
	env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
		name: "codex-subagent", upstreamProtocol: "codex", models: "gpt-5.6-sol",
	}}, []string{business.server.URL})
	text := func(value string) map[string]any { return map[string]any{"type": "input_text", "text": value} }
	encrypted := map[string]any{"type": "encrypted_content", "encrypted_content": "gAAAAABqy1lo" + strings.Repeat("Ab0-", 120) + "=="}
	agentMessage := func(author, header string) map[string]any {
		return map[string]any{"type": "agent_message", "author": author, "recipient": "/root/storage_review",
			"content": []any{text(header + "Task name: /root/storage_review\nSender: " + author + "\nPayload:\n"), encrypted}}
	}
	input := []any{
		map[string]any{"type": "message", "role": "user", "content": []any{text("# AGENTS.md instructions for /repo\n\nUse gofmt.")}},
		agentMessage("/root", "Message Type: NEW_TASK\n"),
		map[string]any{"type": "function_call", "call_id": "call_1", "name": "shell", "arguments": `{"command":"rg -n state"}`},
		map[string]any{"type": "function_call_output", "call_id": "call_1", "output": strings.Repeat("internal/app/reasoning_state.go: match\n", 1200)},
		agentMessage("/root/web_review", "Message Type: MESSAGE\n"),
		agentMessage("/root", "Message Type: MESSAGE\n"),
	}
	headers := map[string]string{"Session-Id": "codex-root", "Thread-Id": "storage-review"}
	reasoningAutoRequest(t, env, "/v1/responses", map[string]any{"model": "gpt-5.6-sol", "stream": true, "input": input}, headers)
	decisions := judge.snapshot()
	if len(decisions) != 1 {
		t.Fatalf("decision requests=%d, want 1", len(decisions))
	}
	conversation := decisions[0].Conversation
	head, _, _ := strings.Cut(conversation, "[context truncated")
	if !strings.Contains(head, "original task: Message Type: NEW_TASK\nTask name: /root/storage_review") ||
		!strings.Contains(head, "latest user request: Message Type: MESSAGE\nTask name: /root/storage_review\nSender: /root\n") {
		t.Fatalf("subagent task digest missing: %s", head)
	}
}

// Codex prewarms a session with generate:false and no user input. It is not a
// turn: no decision, the client's level is sent, and session state is untouched.
func TestReasoningAutoPrewarmLeavesStateUntouched(t *testing.T) {
	t.Parallel()
	judge := newReasoningAutoJudge(t, "high")
	business := newReasoningAutoBusiness(t)
	env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
		name: "codex-prewarm", upstreamProtocol: "codex", models: "gpt-5.6-sol",
	}}, []string{business.server.URL})
	headers := map[string]string{"Session-Id": "codex-prewarm"}
	input := []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Fix the flaky migration test."}}}}
	request := func(body map[string]any) {
		body["model"], body["stream"] = "gpt-5.6-sol", true
		reasoningAutoRequest(t, env, "/v1/responses", body, headers)
	}
	request(map[string]any{"input": input})
	for i := range 2 {
		input = append(input,
			map[string]any{"type": "function_call", "call_id": fmt.Sprintf("call-%d", i), "name": "shell", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": fmt.Sprintf("call-%d", i), "output": "ok"})
		if i == 1 {
			request(map[string]any{"generate": false, "reasoning": map[string]any{"effort": "low"}, "input": []any{
				map[string]any{"type": "message", "role": "developer", "content": []any{map[string]any{"type": "input_text", "text": "You are Codex."}}}}})
		}
		request(map[string]any{"input": input})
	}
	assertReasoningAutoEfforts(t, business.snapshot(), "high", "high", "low", "high")
	if got := len(judge.snapshot()); got != 1 {
		t.Fatalf("decision calls=%d, want 1: prewarm must neither call nor force a re-decision", got)
	}
	entry := waitForProxyLogMatching(t, env, func(e *model.LogEntry) bool {
		return e.ReasoningDecision != nil && e.ReasoningDecision.Reason == "prewarm"
	})
	if d := entry.ReasoningDecision; d.Status != "skipped" || d.Trigger != "" || d.ContextIncomplete {
		t.Fatalf("prewarm log metadata=%+v", d)
	}
}

// Codex's guardian reviewer keeps the level Codex chose and never calls the judge.
func TestReasoningAutoCodexGuardianSkipsDecision(t *testing.T) {
	t.Parallel()
	judge := newReasoningAutoJudge(t)
	business := newReasoningAutoBusiness(t)
	env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
		name: "codex-guardian", upstreamProtocol: "codex", models: "gpt-5.6-luna",
	}}, []string{business.server.URL})
	reasoningAutoRequest(t, env, "/v1/responses", map[string]any{"model": "gpt-5.6-luna", "stream": true, "reasoning": map[string]any{"effort": "low"},
		"input": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text",
			"text": "Assess the exact planned action below: git fetch upstream"}}}}},
		map[string]string{"Session-Id": "codex-guardian", "X-OpenAI-Subagent": "guardian"})
	assertReasoningAutoEfforts(t, business.snapshot(), "low")
	if got := len(judge.snapshot()); got != 0 {
		t.Fatalf("decision calls=%d, want 0 for the guardian reviewer", got)
	}
	entry := waitForProxyLogMatching(t, env, func(e *model.LogEntry) bool {
		return e.ReasoningDecision != nil && e.ReasoningDecision.Reason == "guardian"
	})
	if d := entry.ReasoningDecision; d.Status != "skipped" || d.Trigger != "" {
		t.Fatalf("guardian log metadata=%+v", d)
	}
}

// Encrypted payloads in tool calls and their outputs are unreadable noise; the
// judge sees a placeholder while the readable arguments stay.
func TestReasoningAutoDropsEncryptedPayloads(t *testing.T) {
	t.Parallel()
	judge := newReasoningAutoJudge(t, "medium")
	business := newReasoningAutoBusiness(t)
	env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
		name: "codex-encrypted", upstreamProtocol: "codex", models: "gpt-5.6-sol",
	}}, []string{business.server.URL})
	token := func(seed string) string { return "gAAAAABqy1lo" + strings.Repeat(seed, 120) + "==" }
	spawn, _ := json.Marshal(map[string]any{"task_name": "storage_review", "fork_turns": "none", "message": token("Ab0-")})
	reasoningAutoRequest(t, env, "/v1/responses", map[string]any{"model": "gpt-5.6-sol", "stream": true, "input": []any{
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Review the storage layer with sub-agents."}}},
		map[string]any{"type": "function_call", "call_id": "call-1", "name": "spawn_agent", "arguments": string(spawn)},
		map[string]any{"type": "function_call_output", "call_id": "call-1", "output": "spawned storage_review"},
		map[string]any{"type": "custom_tool_call", "call_id": "call-2", "name": "exec", "input": `await tools.followup_task({target:"storage_review",message:"` + token("x_9Z") + `"})`},
		map[string]any{"type": "custom_tool_call_output", "call_id": "call-2", "output": `{"agent_message":"` + token("Qq1_") + `"}`},
	}}, map[string]string{"Session-Id": "codex-encrypted"})
	decisions := judge.snapshot()
	if len(decisions) != 1 {
		t.Fatalf("decision calls=%d, want 1", len(decisions))
	}
	conversation := decisions[0].Conversation
	if strings.Contains(conversation, "gAAAAA") || strings.Count(conversation, "[encrypted]") != 3 ||
		!strings.Contains(conversation, "storage_review") || !strings.Contains(conversation, "followup_task") {
		t.Fatalf("encrypted payloads not replaced or readable arguments lost: %s", conversation)
	}
}

// The decision belongs to the business model: Claude Code and Gemini clients are
// judged from their own wire format, and the level reaches any upstream protocol.
func TestReasoningAutoAnthropicAndGeminiClients(t *testing.T) {
	t.Parallel()
	text := func(value string) map[string]any { return map[string]any{"type": "text", "text": value} }
	claudeTurn := func(stream bool, blocks ...any) map[string]any {
		// No thinking field, like Claude Code side requests: the level must still
		// reach a codex upstream instead of the translator's medium default.
		return map[string]any{"model": "gpt-5.6-sol", "max_tokens": 1024, "stream": stream,
			"output_config": map[string]any{"effort": "medium"},
			"system":        []any{text("You are Claude Code.")},
			"messages": []any{
				map[string]any{"role": "user", "content": []any{text("<system-reminder>\nUse gofmt.\n</system-reminder>"), text("Plan a zero-downtime ledger migration.")}},
				map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "thinking", "thinking": "private", "signature": "sig"},
					map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": "go test ./..."}}}},
				map[string]any{"role": "user", "content": blocks},
				map[string]any{"role": "system", "content": "SessionStart hook additional context"},
			}}
	}
	for _, upstream := range []string{"codex", "anthropic"} {
		t.Run("claude_code_to_"+upstream, func(t *testing.T) {
			t.Parallel()
			judge := newReasoningAutoJudge(t, "high", "xhigh", "xhigh")
			business := newReasoningAutoBusiness(t)
			env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
				name: "claude-code", upstreamProtocol: upstream, models: "gpt-5.6-sol",
			}}, []string{business.server.URL})
			headers := map[string]string{"X-Claude-Code-Session-Id": "6f1c1a52-7a43-4c39-9a51-0d3c1c1f4e10"}
			result := func(output string, isError bool) map[string]any {
				return map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "is_error": isError,
					"content": []any{text(output), map[string]any{"type": "tool_reference", "tool_name": "Bash"}}}
			}
			// The codex mock only speaks SSE, so that leg streams like Claude Code does.
			stream := upstream == "codex"
			// Local slash commands and their output are neither task nor instructions.
			reasoningAutoRequest(t, env, "/v1/messages", claudeTurn(stream, text("<local-command-caveat>Caveat: DO NOT respond to these messages.</local-command-caveat>"),
				text("<command-name>/model</command-name>\n<command-message>model</command-message>\n<command-args></command-args>"),
				text("<local-command-stdout>Set model to sol</local-command-stdout>"), text("Start with the schema.")), headers)
			// A tool result with appended reminder text is a continuation, not user input.
			reasoningAutoRequest(t, env, "/v1/messages", claudeTurn(stream, result("ok", false), text("<system-reminder>\ntodo\n</system-reminder>")), headers)
			reasoningAutoRequest(t, env, "/v1/messages", claudeTurn(stream, result("FAIL ledger_test.go: lost writes", true)), headers)
			// Input typed during a tool run rides in the tool result message and is reevaluated at once.
			reasoningAutoRequest(t, env, "/v1/messages", claudeTurn(stream, result("ok", false), text("Also keep the v1 API readable.")), headers)
			assertReasoningAutoEfforts(t, business.snapshot(), "high", "high", "xhigh", "xhigh")
			decisions := judge.snapshot()
			if len(decisions) != 3 {
				t.Fatalf("decision requests=%d, want 3 (continuation reused, failure and new input reevaluated)", len(decisions))
			}
			conversation := decisions[0].Conversation
			if !strings.Contains(conversation, "Plan a zero-downtime ledger migration.") || !strings.Contains(conversation, "go test ./...") ||
				strings.Contains(conversation, "private") || strings.Contains(conversation, "local-command") || strings.Contains(conversation, "/model") {
				t.Fatalf("decision context mishandled Claude Code blocks: %s", conversation)
			}
		})
	}
	t.Run("claude_code_over_responses", func(t *testing.T) {
		t.Parallel()
		judge := newReasoningAutoJudge(t, "medium")
		business := newReasoningAutoBusiness(t)
		env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
			name: "responses-client", upstreamProtocol: "codex", models: "gpt-5.6-sol",
		}}, []string{business.server.URL})
		part := func(value string) map[string]any { return map[string]any{"type": "input_text", "text": value} }
		reminder := "<system-reminder>\n" + strings.Repeat("CLAUDE.md rule\n", 2048) + "</system-reminder>"
		input := []any{
			map[string]any{"type": "message", "role": "user", "content": []any{part(reminder), part("Draw a pelican riding a bicycle in SVG.")}},
			map[string]any{"type": "message", "role": "user", "content": []any{part("<system-reminder>\nSessionStart hook\n</system-reminder>")}},
		}
		headers := map[string]string{"Session-Id": "claude-code-responses"}
		reasoningAutoRequest(t, env, "/v1/responses", map[string]any{"model": "gpt-5.6-sol", "stream": true, "input": input}, headers)
		input = append(input,
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "Write", "arguments": `{"file_path":"pelican.html"}`},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "File created"},
			map[string]any{"type": "message", "role": "user", "content": []any{part("<system-reminder>\ntodo\n</system-reminder>")}},
			map[string]any{"type": "message", "role": "user", "content": []any{part("<local-command-caveat>Caveat</local-command-caveat>"),
				part("<local-command-stdout>Context compacted</local-command-stdout>")}})
		reasoningAutoRequest(t, env, "/v1/responses", map[string]any{"model": "gpt-5.6-sol", "stream": true, "input": input}, headers)
		assertReasoningAutoEfforts(t, business.snapshot(), "medium", "medium")
		decisions := judge.snapshot()
		if len(decisions) != 1 {
			t.Fatalf("decision requests=%d, want 1 (reminder or local command after tool output is a continuation)", len(decisions))
		}
		if !strings.Contains(decisions[0].Conversation, "latest user request: Draw a pelican riding a bicycle in SVG.") {
			t.Fatalf("truncated context lost the task behind reminders: %.300s", decisions[0].Conversation)
		}
		// Injected instructions only get the bounded digest, not the tail as well.
		if got := strings.Count(decisions[0].Conversation, "CLAUDE.md rule"); got > 4096/len("CLAUDE.md rule\n") {
			t.Fatalf("instruction text repeated in truncated tail: %d copies", got)
		}
	})
	t.Run("gemini", func(t *testing.T) {
		t.Parallel()
		judge := newReasoningAutoJudge(t, "low")
		business := newReasoningAutoBusiness(t)
		env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
			name: "gemini-client", upstreamProtocol: "codex", models: "gpt-5.6-sol",
		}}, []string{business.server.URL})
		reasoningAutoRequest(t, env, "/v1beta/models/gpt-5.6-sol:streamGenerateContent?alt=sse", map[string]any{
			"systemInstruction": map[string]any{"parts": []any{map[string]any{"text": "Be brief."}}},
			"contents":          []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "What does HTTP 503 mean?"}}}},
		}, nil)
		assertReasoningAutoEfforts(t, business.snapshot(), "low")
		if decisions := judge.snapshot(); len(decisions) != 1 || !strings.Contains(decisions[0].Conversation, "HTTP 503") {
			t.Fatalf("gemini decision requests=%+v", decisions)
		}
	})
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
	for _, fallback := range []string{"invalid_answer", "incomplete_history", "incomplete_tool_history", "multimodal"} {
		t.Run(fallback, func(t *testing.T) {
			t.Parallel()
			responsesContext := fallback == "incomplete_history" || fallback == "incomplete_tool_history"
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
				case "incomplete_history", "incomplete_tool_history", "multimodal":
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

// The full history is replayed every turn: media and unknown types that the
// model has already answered must not void every later decision.
func TestReasoningAutoHistoricalMediaAndUnknownTypesStayJudged(t *testing.T) {
	t.Parallel()
	const image = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAAB"
	for _, tc := range []struct {
		name, path string
		body       map[string]any
		markers    []string
	}{
		{
			name: "codex", path: "/v1/responses",
			body: map[string]any{"model": "gpt-5.6-sol", "input": []any{
				map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "input_text", "text": "Why does this layout overflow?"},
					map[string]any{"type": "input_image", "image_url": image},
				}},
				map[string]any{"type": "function_call", "name": "screenshot", "call_id": "call-1", "arguments": "{}"},
				map[string]any{"type": "function_call_output", "call_id": "call-1", "output": []any{
					map[string]any{"type": "input_text", "text": "Captured."},
					map[string]any{"type": "input_image", "image_url": image},
				}},
				map[string]any{"role": "assistant", "content": []any{
					map[string]any{"type": "output_text", "text": "The grid has a fixed width."},
					map[string]any{"type": "future_annotation"},
				}},
				map[string]any{"type": "compaction", "encrypted_content": "opaque"},
				map[string]any{"role": "user", "content": "Fix the grid and add a regression test."},
			}},
			markers: []string{"[input_image]", "[future_annotation]", "[compaction]", "Captured."},
		},
		{
			name: "claude_code", path: "/v1/messages",
			body: map[string]any{"model": "gpt-5.6-sol", "max_tokens": 1024, "stream": true, "messages": []any{
				map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "text", "text": "Why does this layout overflow?"},
					map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": image}},
				}},
				map[string]any{"role": "assistant", "content": []any{
					map[string]any{"type": "server_tool_use", "id": "srvtoolu_1", "name": "web_search"},
					map[string]any{"type": "text", "text": "The grid has a fixed width."},
				}},
				map[string]any{"role": "user", "content": "Fix the grid and add a regression test."},
			}},
			markers: []string{"[image]", "[server_tool_use]"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			judge := newReasoningAutoJudge(t, "xhigh")
			business := newReasoningAutoBusiness(t)
			env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
				name: "history", upstreamProtocol: "codex", models: "gpt-5.6-sol",
			}}, []string{business.server.URL})
			reasoningAutoRequest(t, env, tc.path, tc.body, nil)
			decisions := judge.snapshot()
			if len(decisions) != 1 {
				t.Fatalf("decision requests=%d, want 1", len(decisions))
			}
			conversation := decisions[0].Conversation
			for _, marker := range append(tc.markers, "Fix the grid and add a regression test.") {
				if !strings.Contains(conversation, marker) {
					t.Errorf("decision context omitted %q: %s", marker, conversation)
				}
			}
			if strings.Contains(conversation, "iVBORw0KGgo") {
				t.Fatalf("decision context leaked media payload: %s", conversation)
			}
			assertReasoningAutoEfforts(t, business.snapshot(), "xhigh")
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
		return decision != nil && decision.Status == "decided" && decision.AppliedEffort == "low"
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
	if len(decisions) != 2 || reasoningAutoJudgedPreviousEfforts(t, env, 2)[1] != "" {
		t.Fatalf("unapplied suggestion became effective state: decisions=%+v", decisions)
	}
	entry := waitForProxyLogMatching(t, env, func(entry *model.LogEntry) bool {
		return entry.ClientProtocol != util.ProtocolSystemOne && entry.ReasoningDecision != nil && entry.ReasoningDecision.SuggestedEffort == "xhigh"
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
	reasoningAutoRequest(t, env, "/v1/responses", map[string]any{"model": "gpt-5.6-sol", "input": []any{
		map[string]any{"role": "user", "content": "long history"},
		map[string]any{"type": "compaction_trigger"},
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
			if len(decisions) != 2 || reasoningAutoJudgedPreviousEfforts(t, env, 2)[1] != "" {
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
	if len(decisions) != 2 || reasoningAutoJudgedPreviousEfforts(t, env, 2)[1] != "" {
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
	// Under race/load this loop can outlast the auth cache reload interval.
	// Keep its token in the store so reloading cannot remove a memory-only fixture.
	if err := env.store.CreateAuthToken(t.Context(), &model.AuthToken{
		Token: model.HashToken("test-api-key"), CreatedAt: time.Now(), IsActive: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.server.authService.ReloadAuthTokens(); err != nil {
		t.Fatal(err)
	}
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
	if len(decisions) != retainedSessions+2 {
		t.Fatalf("decision count=%d, want %d", len(decisions), retainedSessions+2)
	}
	if previous := reasoningAutoJudgedPreviousEfforts(t, env, retainedSessions+2); previous[retainedSessions] != "" || previous[retainedSessions+1] != "" {
		t.Fatalf("overflow retained state or displaced an existing session: previous=%v", previous[retainedSessions:])
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
	if len(decisions) != 3 || !slices.Equal(reasoningAutoJudgedPreviousEfforts(t, env, 3)[1:], []string{"high", "high"}) {
		t.Fatalf("transport switch lost reasoning state: %+v", decisions)
	}
	if !strings.Contains(decisions[2].Conversation, "migration is settled") || !strings.Contains(decisions[2].Conversation, "business answer") || !strings.Contains(decisions[2].Conversation, "migration verification completed") {
		t.Fatalf("HTTP continuation lost locally owned WS history: %q", decisions[2].Conversation)
	}
	entry := waitForProxyLogMatching(t, env, func(entry *model.LogEntry) bool {
		return entry.ClientProtocol != util.ProtocolSystemOne && entry.ReasoningDecision != nil && entry.ReasoningDecision.SuggestedEffort == "medium"
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
	counts, businessLogs := map[string]int{}, 0
	for _, entry := range logs {
		if entry.ClientProtocol == util.ProtocolSystemOne {
			continue // 决策调用同属代理流量
		}
		if entry.ReasoningDecision == nil || entry.ReasoningDecision.AppliedEffort != entry.ThinkingEffort {
			t.Fatalf("Zed business log disagrees with actual effort: %+v", entry)
		}
		counts[entry.ThinkingEffort]++
		businessLogs++
	}
	if businessLogs != 4 || counts["high"] != 2 || counts["low"] != 2 {
		t.Fatalf("Zed business log efforts=%v, log count=%d", counts, businessLogs)
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

func TestReasoningAutoDecisionLogDoesNotChargeClient(t *testing.T) {
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
	body := reasoningAutoChatBody("Complex task with independent decision billing.")
	body["reasoning_effort"] = "low"
	reasoningAutoRequest(t, env, "/v1/chat/completions", body, map[string]string{"Session-Id": "billing"})
	assertReasoningAutoEfforts(t, business.snapshot(), "xhigh")
	proxyLog := waitForProxyLog(t, env, "gpt-5.6-sol")
	var decisionLog *model.LogEntry
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		logs, err := env.store.ListLogs(ctx, time.Now().Add(-time.Minute), 10, 0, &model.LogFilter{ClientProtocol: util.ProtocolSystemOne})
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
	if proxyLog.ThinkingEffort != "xhigh" || proxyLog.ReasoningDecision.AppliedEffort != "xhigh" || proxyLog.ReasoningDecision.SuggestedEffort != "xhigh" ||
		proxyLog.ReasoningDecision.ClientEffort != "low" {
		t.Fatalf("business log did not retain suggested/applied effort: %+v", proxyLog)
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
