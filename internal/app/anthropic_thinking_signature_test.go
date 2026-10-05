package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/anthropicauth"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"

	"github.com/tidwall/gjson"
)

func testAnthropicOAuthChannel() *model.Config {
	return &model.Config{AuthType: model.AuthTypeAnthropicOAuth}
}

func TestAnthropicRetryBodyFor400StripsInvalidThinkingSignature(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"claude-sonnet-5-5","thinking":{"type":"adaptive"},"output_config":{"effort":"max"},"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]},{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"CAQSnot-on-this-body"},{"type":"thinking","thinking":"foreign plan","signature":"8cda4dfbe7d4496c894702ac","cache_control":{"type":"ephemeral"}},{"type":"text","text":"ok"},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"}}]}]}`)
	res := &fwResult{
		Status: http.StatusBadRequest,
		Body:   []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"messages.17.content.337: Invalid ` + "`signature`" + ` in ` + "`thinking`" + ` block"},"request_id":"req_011CfhQgbE5tFRWBe35h6kaB"}`),
	}

	got, strategy, ok := anthropicRetryBodyFor400(protocol.Anthropic, testAnthropicOAuthChannel(), protocol.TransformPlan{TranslatedBody: body}, res)
	if !ok || strategy != "strip_anthropic_invalid_thinking_signature" {
		t.Fatalf("retry = (%q, %v), body=%s", strategy, ok, got)
	}
	if gjson.GetBytes(got, "thinking.type").String() != "adaptive" {
		t.Fatalf("current-turn thinking disabled: %s", got)
	}
	if gjson.GetBytes(got, "output_config.effort").String() != "max" {
		t.Fatalf("effort dropped: %s", got)
	}
	content := gjson.GetBytes(got, "messages.1.content")
	if content.Get("#").Int() != 2 {
		t.Fatalf("assistant content = %s", content.Raw)
	}
	if content.Get("0.text").String() != "ok" || content.Get("1.name").String() != "Bash" {
		t.Fatalf("text/tool_use lost: %s", content.Raw)
	}
	for _, block := range content.Array() {
		if block.Get("type").String() == "thinking" || block.Get("type").String() == "redacted_thinking" {
			t.Fatalf("thinking blocks survived: %s", content.Raw)
		}
	}
	if gjson.GetBytes(got, "messages.1.content.#(text==foreign plan)").Exists() {
		t.Fatalf("thinking must be omitted, not rewritten as text: %s", got)
	}
}

func TestAnthropicRetryBodyFor400StripsOnOfficialAnthropicAPIKeyURL(t *testing.T) {
	t.Parallel()
	body := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":"8cda4dfbe7d4496c894702ac"},{"type":"text","text":"ok"}]}]}`)
	res := &fwResult{
		Status: http.StatusBadRequest,
		Body:   []byte(`{"error":{"type":"invalid_request_error","message":"messages.17.content.337: Invalid signature in thinking block"}}`),
	}
	cfg := &model.Config{
		AuthType: model.AuthTypeAPIKey,
		URLs:     channelURLsForTest("https://api.anthropic.com"),
	}
	got, strategy, ok := anthropicRetryBodyFor400(protocol.Anthropic, cfg, protocol.TransformPlan{TranslatedBody: body}, res)
	if !ok || strategy != "strip_anthropic_invalid_thinking_signature" {
		t.Fatalf("retry = (%q, %v), body=%s", strategy, ok, got)
	}
	if gjson.GetBytes(got, "messages.0.content.0.text").String() != "ok" {
		t.Fatalf("assistant content = %s", got)
	}
	if gjson.GetBytes(got, `messages.0.content.#(type=="thinking")`).Exists() {
		t.Fatalf("thinking survived: %s", got)
	}
}

func TestAnthropicRetryBodyFor400SkipsThinkingStripOnNonAnthropicOAuth(t *testing.T) {
	t.Parallel()
	body := []byte(`{"thinking":{"type":"adaptive"},"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":"8cda4dfbe7d4496c894702ac"},{"type":"text","text":"ok"}]}]}`)
	res := &fwResult{
		Status: http.StatusBadRequest,
		Body:   []byte(`{"error":{"type":"invalid_request_error","message":"messages.17.content.337: Invalid signature in thinking block"}}`),
	}
	for _, cfg := range []*model.Config{
		nil,
		{AuthType: model.AuthTypeAPIKey},
		{AuthType: model.AuthTypeZAIOAuth, URLs: channelURLsForTest("https://api.z.ai/api/anthropic")},
		{AuthType: model.AuthTypeAPIKey, URLs: channelURLsForTest("https://open.bigmodel.cn/api/paas/v4")},
		{AuthType: model.AuthTypeAPIKey, URLs: channelURLsForTest("https://api.deepseek.com")},
		{AuthType: model.AuthTypeAPIKey, URLs: channelURLsForTest("https://api.openai.com/v1")},
	} {
		got, strategy, ok := anthropicRetryBodyFor400(protocol.Anthropic, cfg, protocol.TransformPlan{TranslatedBody: body}, res)
		if ok {
			t.Fatalf("signature rewrite ran for auth=%v strategy=%q body=%s", cfg, strategy, got)
		}
		if gjson.GetBytes(body, "messages.0.content.0.type").String() != "thinking" {
			t.Fatalf("input mutated without retry: %s", body)
		}
	}
	if _, _, ok := anthropicRetryBodyFor400(protocol.OpenAI, testAnthropicOAuthChannel(), protocol.TransformPlan{TranslatedBody: body}, res); ok {
		t.Fatal("signature strip ran for OpenAI protocol")
	}
}

func TestAnthropicRetryBodyFor400UnsupportedThinkingStillDisablesControls(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"claude-opus-4-6","thinking":{"type":"adaptive"},"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"keep this"}]}]}`)
	res := &fwResult{
		Status: http.StatusBadRequest,
		Body:   []byte(`{"error":{"type":"invalid_request_error","message":"thinking blocks are not supported"}}`),
	}
	got, strategy, ok := anthropicRetryBodyFor400(protocol.Anthropic, nil, protocol.TransformPlan{TranslatedBody: body}, res)
	if !ok || strategy != "downgrade_anthropic_thinking" {
		t.Fatalf("retry = (%q, %v), body=%s", strategy, ok, got)
	}
	if gjson.GetBytes(got, "thinking").Exists() {
		t.Fatalf("unsupported-thinking retry must drop thinking controls: %s", got)
	}
}

func TestRejectedAnthropicToolPathIgnoresInvalidThinkingSignature(t *testing.T) {
	t.Parallel()
	body := []byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}]}]}`)
	errorBody := []byte(`{"error":{"message":"messages.17.content.337: Invalid signature in thinking block"}}`)
	path, _, toolError := rejectedAnthropicToolPath(body, errorBody)
	if toolError || path != "" {
		t.Fatalf("signature error classified as tool: path=%q toolError=%v", path, toolError)
	}
}

func TestApplyAnthropicMessagesAPIInvariantsDoesNotRewriteThinking(t *testing.T) {
	t.Parallel()
	in := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"foreign plan","signature":"8cda4dfbe7d4496c894702ac"},{"type":"text","text":"ok"}]}]}`)
	out := applyAnthropicMessagesAPIInvariants(in)
	if string(out) != string(in) {
		t.Fatalf("first-pass invariants must not rewrite thinking:\n%s\n%s", in, out)
	}
}

func TestApplyAnthropicMessagesAPIInvariantsNoopsWithoutThinking(t *testing.T) {
	t.Parallel()
	in := []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	out := applyAnthropicMessagesAPIInvariants(in)
	if string(out) != string(in) {
		t.Fatalf("clean body rewritten:\n%s\n%s", in, out)
	}
}

func TestIsAnthropicInvalidThinkingSignatureError(t *testing.T) {
	t.Parallel()
	if !isAnthropicInvalidThinkingSignatureError(`invalid_request_error  messages.17.content.337: invalid ` + "`signature`" + ` in ` + "`thinking`" + ` block`) {
		t.Fatal("production signature 400 must match")
	}
	if isAnthropicInvalidThinkingSignatureError("thinking blocks are not supported") {
		t.Fatal("unsupported-thinking must not use the signature stripper")
	}
	if isAnthropicInvalidThinkingSignatureError("messages.3.output_config: extra inputs are not permitted") {
		t.Fatal("unrelated 400 matched")
	}
}

func TestRememberedAnthropicThinkingOmitIsSessionAndModelScoped(t *testing.T) {
	t.Parallel()
	session := "sess-omit-" + t.Name()
	headers := http.Header{"X-Claude-Code-Session-Id": []string{session}}
	poisoned := []byte(`{"model":"claude-sonnet-5-5","thinking":{"type":"adaptive"},"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":"CAQSstill-claude-shaped-but-rejected"},{"type":"text","text":"ok"}]}]}`)
	otherModel := []byte(`{"model":"claude-sonnet-5","thinking":{"type":"adaptive"},"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":"CAQSstill-claude-shaped-but-rejected"},{"type":"text","text":"ok"}]}]}`)
	t.Cleanup(func() {
		anthropicThinkingOmitSessions.Delete(anthropicThinkingOmitKey(headers, poisoned))
		anthropicThinkingOmitSessions.Delete(anthropicThinkingOmitKey(headers, otherModel))
	})
	if anthropicThinkingOmitRemembered(headers, poisoned) {
		t.Fatal("session was remembered before the signature 400")
	}
	rememberAnthropicThinkingOmit(headers, poisoned)
	got, ok := omitRememberedAnthropicThinkingHistory(testAnthropicOAuthChannel(), headers, poisoned)
	if !ok || gjson.GetBytes(got, `messages.0.content.#(type=="thinking")`).Exists() {
		t.Fatalf("remembered sonnet-5-5 turn did not omit thinking: %s", got)
	}
	if gjson.GetBytes(got, "thinking.type").String() != "adaptive" || gjson.GetBytes(got, "messages.0.content.0.text").String() != "ok" {
		t.Fatalf("current-turn controls or text lost: %s", got)
	}
	if _, ok := omitRememberedAnthropicThinkingHistory(testAnthropicOAuthChannel(), headers, otherModel); ok {
		t.Fatal("sonnet-5 on the same session was omitted")
	}
	otherSession := headers.Clone()
	otherSession.Set("X-Claude-Code-Session-Id", session+"-other")
	if _, ok := omitRememberedAnthropicThinkingHistory(testAnthropicOAuthChannel(), otherSession, poisoned); ok {
		t.Fatal("a different session was omitted")
	}
	if _, ok := omitRememberedAnthropicThinkingHistory(&model.Config{AuthType: model.AuthTypeZAIOAuth}, headers, poisoned); ok {
		t.Fatal("Z.ai used the official Anthropic omit memory")
	}
	rememberAnthropicThinkingOmit(nil, []byte(`{"model":"claude-sonnet-5-5","messages":[]}`))
	if anthropicThinkingOmitRemembered(nil, []byte(`{"model":"claude-sonnet-5-5","messages":[]}`)) {
		t.Fatal("a request without a session id was remembered")
	}
}

func TestAnthropicThinkingOmitMemoryExpires(t *testing.T) {
	t.Parallel()
	headers := http.Header{"X-Claude-Code-Session-Id": []string{"sess-expire-" + t.Name()}}
	body := []byte(`{"model":"claude-sonnet-5-5","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	key := anthropicThinkingOmitKey(headers, body)
	anthropicThinkingOmitSessions.Store(key, time.Now().Add(-time.Second))
	t.Cleanup(func() { anthropicThinkingOmitSessions.Delete(key) })
	if anthropicThinkingOmitRemembered(headers, body) {
		t.Fatal("expired omit memory was still active")
	}
}

func TestStripAnthropicHistoryThinkingBlocksFastPath(t *testing.T) {
	t.Parallel()
	body := []byte(`{"thinking":{"type":"adaptive"},"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	if _, ok := stripAnthropicHistoryThinkingBlocks(body); ok {
		t.Fatal("top-level thinking controls must not trigger history strip")
	}
}

func TestCloakOfficialAnthropicThinkingHistoryOmitsForeignCarriers(t *testing.T) {
	t.Parallel()
	body := []byte(`{"thinking":{"type":"adaptive"},"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"omp plan","signature":"skip_thought_signature_validator"},{"type":"text","text":"ok"}]}]}`)
	got, ok := cloakOfficialAnthropicThinkingHistory(testAnthropicOAuthChannel(), body)
	if !ok {
		t.Fatal("foreign thinking must be omitted on official Anthropic")
	}
	if gjson.GetBytes(got, "thinking.type").String() != "adaptive" {
		t.Fatalf("current-turn thinking disabled: %s", got)
	}
	if gjson.GetBytes(got, `messages.0.content.#(type=="thinking")`).Exists() {
		t.Fatalf("foreign thinking survived: %s", got)
	}
	if gjson.GetBytes(got, "messages.0.content.#(text==omp plan)").Exists() {
		t.Fatalf("foreign thinking rewritten as text: %s", got)
	}
	if gjson.GetBytes(got, "messages.0.content.0.text").String() != "ok" {
		t.Fatalf("assistant text lost: %s", got)
	}
}

func TestCloakOfficialAnthropicThinkingHistoryKeepsUnrecognizedCarriers(t *testing.T) {
	t.Parallel()
	// An unrecognized carrier may be a Claude format the detector has not
	// learned yet; the signature 400 retry handles a real mismatch.
	body := []byte(`{"thinking":{"type":"adaptive"},"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":"8cda4dfbe7d4496c894702ac"},{"type":"text","text":"ok"}]}]}`)
	if got, ok := cloakOfficialAnthropicThinkingHistory(testAnthropicOAuthChannel(), body); ok {
		t.Fatalf("unrecognized carrier omitted before any signature 400: %s", got)
	}
}

func TestCloakOfficialAnthropicThinkingHistorySkipsOtherAuthTypes(t *testing.T) {
	t.Parallel()
	body := []byte(`{"thinking":{"type":"adaptive"},"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":"skip_thought_signature_validator"},{"type":"text","text":"ok"}]}]}`)
	for _, cfg := range []*model.Config{
		nil,
		{AuthType: model.AuthTypeAPIKey},
		{AuthType: model.AuthTypeZAIOAuth, URLs: channelURLsForTest("https://api.z.ai/api/anthropic")},
		{AuthType: model.AuthTypeXAIOAuth, URLs: channelURLsForTest("https://cli-chat-proxy.grok.com/v1")},
		{AuthType: model.AuthTypeCursorOAuth, URLs: channelURLsForTest("https://api2.cursor.sh")},
		{AuthType: model.AuthTypeAntigravityOAuth, URLs: channelURLsForTest("https://daily-cloudcode-pa.googleapis.com")},
		{AuthType: model.AuthTypeCodexOAuth},
		{AuthType: model.AuthTypeCodeBuddyOAuth, URLs: channelURLsForTest("https://www.workbuddy.ai/v2/chat/completions")},
		{AuthType: model.AuthTypeZedOAuth},
		{AuthType: model.AuthTypeAPIKey, URLs: channelURLsForTest("https://open.bigmodel.cn/api/paas/v4")},
		{AuthType: model.AuthTypeAPIKey, URLs: channelURLsForTest("https://api.deepseek.com")},
		{AuthType: model.AuthTypeAPIKey, URLs: channelURLsForTest("https://api.openai.com/v1")},
		{AuthType: model.AuthTypeAPIKey, URLs: channelURLsForTest("https://ai.hdd.sb")},
	} {
		if got, ok := cloakOfficialAnthropicThinkingHistory(cfg, body); ok {
			t.Fatalf("cloak ran for auth=%v body=%s", cfg, got)
		}
	}
}

func TestFinishAnthropicPassthroughCloaksForeignThinkingOnOfficial(t *testing.T) {
	t.Parallel()
	body := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"foreign plan","signature":"skip_thought_signature_validator"},{"type":"text","text":"ok"}]}]}`)
	got, _, err := finishAnthropicPassthrough(body, false, testAnthropicOAuthChannel(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(got, `messages.0.content.#(type=="thinking")`).Exists() {
		t.Fatalf("native official passthrough leaked foreign thinking: %s", got)
	}
	got, _, err = finishAnthropicPassthrough(body, false, &model.Config{
		AuthType: model.AuthTypeZAIOAuth,
		URLs:     channelURLsForTest("https://api.z.ai/api/anthropic"),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("Z.ai passthrough rewritten:\n%s\n%s", body, got)
	}
}

func TestProxyLogRecordsAnthropicThinkingOmission(t *testing.T) {
	const modelName = "claude-sonnet-4-6"
	credentialJSON := anthropicProxyTestCredential(t, "oauth-anthropic-token")
	credential, err := anthropicauth.ParseCredential([]byte(credentialJSON))
	if err != nil {
		t.Fatal(err)
	}
	// Native Claude Code passthrough keeps history thinking; the simulated path
	// already drops signatures that are not Claude-shaped while normalizing.
	env := setupProxyTestEnv(t, []testChannel{{
		name: "official-anthropic-oauth", upstreamProtocol: "anthropic", models: modelName,
		authType: model.AuthTypeAnthropicOAuth, oauthCredential: credentialJSON,
	}}, map[int]string{0: "https://api.anthropic.com"})
	var bodies [][]byte
	env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, body)
		status, payload := http.StatusOK,
			`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-sonnet-4-6","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
		if strings.Contains(string(body), `"type":"thinking"`) {
			status, payload = http.StatusBadRequest,
				`{"type":"error","error":{"type":"invalid_request_error","message":"messages.1.content.0: Invalid `+"`signature`"+` in `+"`thinking`"+` block"}}`
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(payload)),
		}, nil
	})}
	send := func(session, signature string) []byte {
		t.Helper()
		bodies = nil
		identity, err := json.Marshal(map[string]string{
			"device_id": credential.DeviceID, "account_uuid": credential.AccountUUID, "session_id": session,
		})
		if err != nil {
			t.Fatal(err)
		}
		response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
			"model":      modelName,
			"max_tokens": 1024,
			"thinking":   map[string]any{"type": "adaptive"},
			"metadata":   map[string]any{"user_id": string(identity)},
			"system": []any{
				map[string]any{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.220.abc; cc_entrypoint=cli; cch=00000;"},
				map[string]any{"type": "text", "text": "native prompt"},
			},
			"messages": []any{
				map[string]any{"role": "user", "content": "hi"},
				map[string]any{"role": "assistant", "content": []any{
					map[string]any{"type": "thinking", "thinking": "plan", "signature": signature},
					map[string]any{"type": "text", "text": "ok"},
				}},
				map[string]any{"role": "user", "content": "next"},
			},
		}, map[string]string{
			"User-Agent":               "claude-cli/" + anthropicCLIVersion + " (external, cli)",
			"X-App":                    "cli",
			"Anthropic-Beta":           "claude-code-20250219",
			"X-Claude-Code-Session-Id": session,
		})
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		return bodies[len(bodies)-1]
	}
	waitMessage := func(count int) string {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			logs, err := env.store.ListLogs(context.Background(), time.Now().Add(-time.Minute), 20, 0,
				&model.LogFilter{LogSource: model.LogSourceProxy})
			if err != nil {
				t.Fatal(err)
			}
			if len(logs) == count {
				return logs[0].Message
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("proxy logs did not reach %d", count)
		return ""
	}
	session := "sess-log-" + t.Name()
	t.Cleanup(func() {
		anthropicThinkingOmitSessions.Range(func(key, _ any) bool {
			if strings.HasPrefix(key.(string), session) {
				anthropicThinkingOmitSessions.Delete(key)
			}
			return true
		})
	})

	// An unrecognized carrier is sent; the signature 400 retry strips it.
	send(session, "8cda4dfbe7d4496c894702ac")
	if len(bodies) != 2 {
		t.Fatalf("upstream sends=%d, want 400 then retry", len(bodies))
	}
	if got := waitMessage(1); got != "ok [strip_anthropic_invalid_thinking_signature]" {
		t.Fatalf("retry log message=%q", got)
	}
	// The same session omits history thinking before the first send.
	send(session, "8cda4dfbe7d4496c894702ac")
	if len(bodies) != 1 {
		t.Fatalf("remembered session upstream sends=%d, want 1", len(bodies))
	}
	if got := waitMessage(2); got != "ok [omit_anthropic_remembered_thinking]" {
		t.Fatalf("remembered omit log message=%q", got)
	}
	// A carrier identified as another provider is omitted on a fresh session.
	send(session+"-foreign", "skip_thought_signature_validator")
	if len(bodies) != 1 {
		t.Fatalf("foreign carrier upstream sends=%d, want 1", len(bodies))
	}
	if got := waitMessage(3); got != "ok [omit_anthropic_foreign_thinking]" {
		t.Fatalf("foreign omit log message=%q", got)
	}
}
