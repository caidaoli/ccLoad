package app

import (
	"net/http"
	"testing"
	"time"

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
	body := []byte(`{"thinking":{"type":"adaptive"},"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"omp plan","signature":"8cda4dfbe7d4496c894702ac"},{"type":"text","text":"ok"}]}]}`)
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

func TestCloakOfficialAnthropicThinkingHistorySkipsOtherAuthTypes(t *testing.T) {
	t.Parallel()
	body := []byte(`{"thinking":{"type":"adaptive"},"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":"8cda4dfbe7d4496c894702ac"},{"type":"text","text":"ok"}]}]}`)
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
	body := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"foreign plan","signature":"8cda4dfbe7d4496c894702ac"},{"type":"text","text":"ok"}]}]}`)
	got, err := finishAnthropicPassthrough(body, false, testAnthropicOAuthChannel(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(got, `messages.0.content.#(type=="thinking")`).Exists() {
		t.Fatalf("native official passthrough leaked foreign thinking: %s", got)
	}
	got, err = finishAnthropicPassthrough(body, false, &model.Config{
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
