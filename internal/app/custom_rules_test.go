package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"ccLoad/internal/model"

	"github.com/tidwall/gjson"
)

func TestApplyHeaderRules_BasicActions(t *testing.T) {
	h := http.Header{}
	h.Set("User-Agent", "orig")
	h.Set("Accept", "application/json")
	h.Add("X-Multi", "a")

	rules := []model.CustomHeaderRule{
		{Action: model.RuleActionRemove, Name: "User-Agent"},
		{Action: model.RuleActionOverride, Name: "X-Foo", Value: "bar"},
		{Action: model.RuleActionAppend, Name: "X-Multi", Value: "b"},
	}

	applyHeaderRules(h, rules)

	if h.Get("User-Agent") != "" {
		t.Errorf("expected User-Agent removed, got %q", h.Get("User-Agent"))
	}
	if got := h.Get("X-Foo"); got != "bar" {
		t.Errorf("expected X-Foo=bar, got %q", got)
	}
	values := h.Values("X-Multi")
	if len(values) != 2 || values[0] != "a" || values[1] != "b" {
		t.Errorf("expected X-Multi=[a,b], got %v", values)
	}
}

func TestApplyHeaderRules_SkipAuthBlacklist(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer real")
	h.Set("x-api-key", "k1")
	h.Set("x-goog-api-key", "gk1")

	rules := []model.CustomHeaderRule{
		{Action: model.RuleActionRemove, Name: "authorization"},
		{Action: model.RuleActionOverride, Name: "X-Api-Key", Value: "hijack"},
		{Action: model.RuleActionOverride, Name: "X-Goog-Api-Key", Value: "hijack"},
	}

	applyHeaderRules(h, rules)

	if got := h.Get("Authorization"); got != "Bearer real" {
		t.Errorf("auth header should be protected, got %q", got)
	}
	if got := h.Get("x-api-key"); got != "k1" {
		t.Errorf("x-api-key should be protected, got %q", got)
	}
	if got := h.Get("x-goog-api-key"); got != "gk1" {
		t.Errorf("x-goog-api-key should be protected, got %q", got)
	}
}

func TestApplyHeaderRules_NoOpOnNilOrEmpty(t *testing.T) {
	applyHeaderRules(nil, []model.CustomHeaderRule{{Action: model.RuleActionRemove, Name: "x"}})
	h := http.Header{"X": {"v"}}
	applyHeaderRules(h, nil)
	if h.Get("X") != "v" {
		t.Errorf("expected no mutation, got %q", h.Get("X"))
	}
}

func TestApplyHeaderRules_RemoveTokenFromCSV(t *testing.T) {
	h := http.Header{}
	h.Set("Anthropic-Beta", "claude-code-20250219, context-1m-2025-08-07, interleaved-thinking-2025-05-14")

	applyHeaderRules(h, []model.CustomHeaderRule{
		{Action: model.RuleActionRemove, Name: "Anthropic-Beta", Value: "context-1m-2025-08-07"},
	})

	got := h.Get("Anthropic-Beta")
	want := "claude-code-20250219, interleaved-thinking-2025-05-14"
	if got != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}

func TestApplyHeaderRules_RemoveTokenEmptiesHeader(t *testing.T) {
	h := http.Header{}
	h.Set("Anthropic-Beta", "context-1m-2025-08-07")

	applyHeaderRules(h, []model.CustomHeaderRule{
		{Action: model.RuleActionRemove, Name: "Anthropic-Beta", Value: "context-1m-2025-08-07"},
	})

	if values := h.Values("Anthropic-Beta"); len(values) != 0 {
		t.Errorf("expected header fully removed, got %v", values)
	}
}

func TestApplyHeaderRules_RemoveTokenNoMatchKeepsHeader(t *testing.T) {
	h := http.Header{}
	h.Set("Anthropic-Beta", "claude-code-20250219")

	applyHeaderRules(h, []model.CustomHeaderRule{
		{Action: model.RuleActionRemove, Name: "Anthropic-Beta", Value: "context-1m-2025-08-07"},
	})

	if got := h.Get("Anthropic-Beta"); got != "claude-code-20250219" {
		t.Errorf("expected header untouched, got %q", got)
	}
}

// rawHeaderValues 按原样大小写读取请求头，绕开 http.Header 的 canonical 语义 ——
// Claude Code CLI / ZCode 指纹路径刻意写小写头名。
func rawHeaderValues(h http.Header, name string) []string {
	return h[name]
}

func TestApplyHeaderRules_RewritesRawLowercaseHeaderKeysInPlace(t *testing.T) {
	// 指纹路径按线上原样大小写写头，规则必须就地改写，不能再 canonical 化出第二个同名头。
	h := http.Header{}
	setRawHeader(h, "anthropic-beta", "claude-code-20250219,oauth-2025-04-20")
	setRawHeader(h, "x-app", "cli")
	setRawHeader(h, "user-agent", "claude-cli/2.1.220 (external, cli)")

	applyHeaderRules(h, []model.CustomHeaderRule{
		{Action: model.RuleActionRemove, Name: "Anthropic-Beta", Value: "oauth-2025-04-20"},
		{Action: model.RuleActionAppend, Name: "Anthropic-Beta", Value: "context-1m-2025-08-07"},
		{Action: model.RuleActionOverride, Name: "User-Agent", Value: "custom-agent"},
		{Action: model.RuleActionRemove, Name: "X-App"},
	})

	for _, canonical := range []string{"Anthropic-Beta", "User-Agent"} {
		if len(rawHeaderValues(h, canonical)) != 0 {
			t.Fatalf("rule canonicalized a raw header key: %v", h)
		}
	}
	if got := rawHeaderValues(h, "anthropic-beta"); len(got) != 2 ||
		got[0] != "claude-code-20250219" || got[1] != "context-1m-2025-08-07" {
		t.Fatalf("anthropic-beta = %v", got)
	}
	if got := rawHeaderValues(h, "user-agent"); len(got) != 1 || got[0] != "custom-agent" {
		t.Fatalf("user-agent = %v", got)
	}
	if len(rawHeaderValues(h, "x-app")) != 0 {
		t.Fatalf("x-app should be removed: %v", h)
	}
}

func TestApplyHeaderRules_CollapsesDuplicateCaseVariants(t *testing.T) {
	h := http.Header{}
	setRawHeader(h, "anthropic-beta", "claude-code-20250219")
	h["Anthropic-Beta"] = []string{"oauth-2025-04-20"}

	applyHeaderRules(h, []model.CustomHeaderRule{
		{Action: model.RuleActionAppend, Name: "anthropic-beta", Value: "context-1m-2025-08-07"},
	})

	if len(rawHeaderValues(h, "Anthropic-Beta")) != 0 {
		t.Fatalf("canonical variant survived: %v", h)
	}
	got := rawHeaderValues(h, "anthropic-beta")
	if len(got) != 3 || got[0] != "claude-code-20250219" || got[1] != "oauth-2025-04-20" || got[2] != "context-1m-2025-08-07" {
		t.Fatalf("anthropic-beta = %v", got)
	}
}

func TestApplyHeaderRules_ProtectsRawLowercaseAuthHeaders(t *testing.T) {
	h := http.Header{}
	setRawHeader(h, "x-api-key", "sk-real")
	setRawHeader(h, "authorization", "Bearer real")

	applyHeaderRules(h, []model.CustomHeaderRule{
		{Action: model.RuleActionOverride, Name: "X-Api-Key", Value: "hijack"},
		{Action: model.RuleActionRemove, Name: "Authorization"},
	})

	if got := rawHeaderValues(h, "x-api-key"); len(got) != 1 || got[0] != "sk-real" {
		t.Fatalf("x-api-key = %v", got)
	}
	if got := rawHeaderValues(h, "authorization"); len(got) != 1 || got[0] != "Bearer real" {
		t.Fatalf("authorization = %v", got)
	}
}

func TestApplyHeaderRules_RemoveTokenAcrossMultiValues(t *testing.T) {
	h := http.Header{}
	h.Add("X-Multi", "a, b")
	h.Add("X-Multi", "b, c")

	applyHeaderRules(h, []model.CustomHeaderRule{
		{Action: model.RuleActionRemove, Name: "X-Multi", Value: "b"},
	})

	values := h.Values("X-Multi")
	if len(values) != 2 || values[0] != "a" || values[1] != "c" {
		t.Errorf("expected [a, c], got %v", values)
	}
}

func TestApplyHeaderRules_RemoveEmptyValueDeletesEntireHeader(t *testing.T) {
	h := http.Header{}
	h.Set("Anthropic-Beta", "claude-code-20250219, context-1m-2025-08-07")

	applyHeaderRules(h, []model.CustomHeaderRule{
		{Action: model.RuleActionRemove, Name: "Anthropic-Beta"},
	})

	if values := h.Values("Anthropic-Beta"); len(values) != 0 {
		t.Errorf("expected header deleted, got %v", values)
	}
}

func TestApplyBodyRules_NonJSONPassthrough(t *testing.T) {
	body := []byte("raw binary bytes")
	rules := []model.CustomBodyRule{{Action: model.RuleActionRemove, Path: "foo"}}

	out := applyBodyRules("application/octet-stream", body, rules)
	if !bytes.Equal(out, body) {
		t.Errorf("expected passthrough, got %q", out)
	}

	out = applyBodyRules("", body, rules)
	if !bytes.Equal(out, body) {
		t.Errorf("empty content-type should passthrough")
	}
}

func TestApplyBodyRules_InvalidJSONPassthrough(t *testing.T) {
	body := []byte("{not json}")
	rules := []model.CustomBodyRule{{Action: model.RuleActionRemove, Path: "foo"}}

	out := applyBodyRules("application/json", body, rules)
	if !bytes.Equal(out, body) {
		t.Errorf("expected passthrough on malformed json")
	}
}

func TestApplyBodyRules_EmptyBodyOrRules(t *testing.T) {
	rules := []model.CustomBodyRule{{Action: model.RuleActionOverride, Path: "x", Value: json.RawMessage("1")}}
	if out := applyBodyRules("application/json", nil, rules); len(out) != 0 {
		t.Errorf("nil body should stay nil")
	}
	body := []byte(`{"a":1}`)
	if out := applyBodyRules("application/json", body, nil); !bytes.Equal(out, body) {
		t.Errorf("nil rules should passthrough")
	}
}

func TestApplyBodyRules_OverrideTopLevel(t *testing.T) {
	body := []byte(`{"temperature":0.5,"max_tokens":100}`)
	rules := []model.CustomBodyRule{
		{Action: model.RuleActionOverride, Path: "max_tokens", Value: json.RawMessage("4096")},
	}

	out := applyBodyRules("application/json", body, rules)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("parse result: %v", err)
	}
	if v, _ := got["max_tokens"].(float64); v != 4096 {
		t.Errorf("max_tokens expected 4096, got %v", got["max_tokens"])
	}
	if v, _ := got["temperature"].(float64); v != 0.5 {
		t.Errorf("temperature should remain 0.5, got %v", got["temperature"])
	}
}

func TestApplyBodyRules_PreservesUnchangedFieldOrder(t *testing.T) {
	body := []byte(`{"z":1,"nested":{"first":1,"second":2},"a":2}`)
	rules := []model.CustomBodyRule{
		{Action: model.RuleActionOverride, Path: "nested.second", Value: json.RawMessage("3")},
	}

	out := applyBodyRules("application/json", body, rules)
	assertFieldOrder(t, string(out), `"z"`, `"nested"`, `"a"`)
	assertFieldOrder(t, string(out), `"first"`, `"second"`)
}

func TestApplyBodyRules_OverrideNestedCreatePath(t *testing.T) {
	body := []byte(`{"model":"x"}`)
	rules := []model.CustomBodyRule{
		{Action: model.RuleActionOverride, Path: "thinking.budget_tokens", Value: json.RawMessage("8192")},
	}

	out := applyBodyRules("application/json; charset=utf-8", body, rules)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	thinking, ok := got["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("thinking should be object, got %T", got["thinking"])
	}
	if v, _ := thinking["budget_tokens"].(float64); v != 8192 {
		t.Errorf("budget_tokens expected 8192, got %v", thinking["budget_tokens"])
	}
}

func TestApplyBodyRules_CreatesMissingNumericSegmentsAsObjects(t *testing.T) {
	body := []byte(`{"model":"x"}`)
	rules := []model.CustomBodyRule{
		{Action: model.RuleActionOverride, Path: "metadata.0.value", Value: json.RawMessage(`"kept"`)},
	}

	out := applyBodyRules("application/json", body, rules)
	if got := gjson.GetBytes(out, `metadata.0.value`).String(); got != "kept" {
		t.Fatalf("metadata.0.value = %q, body=%s", got, out)
	}
	if !gjson.GetBytes(out, "metadata").IsObject() || !gjson.GetBytes(out, "metadata.0").IsObject() {
		t.Fatalf("missing numeric segments must be objects: %s", out)
	}
}

func TestApplyBodyRules_PathConflictLeavesBodyUntouched(t *testing.T) {
	body := []byte(`{"metadata":"scalar","model":"x"}`)
	rules := []model.CustomBodyRule{
		{Action: model.RuleActionOverride, Path: "metadata.value", Value: json.RawMessage(`"ignored"`)},
	}

	out := applyBodyRules("application/json", body, rules)
	if !bytes.Equal(out, body) {
		t.Fatalf("path conflict changed body: %s", out)
	}
}

func TestApplyBodyRules_RemovePathConflictLeavesBodyUntouched(t *testing.T) {
	body := []byte(`{"metadata":"scalar","model":"x"}`)
	rules := []model.CustomBodyRule{
		{Action: model.RuleActionRemove, Path: "metadata.value"},
	}

	out := applyBodyRules("application/json", body, rules)
	if !bytes.Equal(out, body) {
		t.Fatalf("remove path conflict changed body: %s", out)
	}
}

func TestApplyBodyRules_OverrideWithObjectValue(t *testing.T) {
	body := []byte(`{"model":"x"}`)
	rules := []model.CustomBodyRule{
		{Action: model.RuleActionOverride, Path: "thinking", Value: json.RawMessage(`{"type":"adaptive"}`)},
	}

	out := applyBodyRules("application/json", body, rules)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	thinking, ok := got["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("thinking should be object, got %T", got["thinking"])
	}
	if thinking["type"] != "adaptive" {
		t.Errorf("thinking.type expected adaptive, got %v", thinking["type"])
	}
}

func TestApplyBodyRules_RemoveExisting(t *testing.T) {
	body := []byte(`{"a":1,"b":2,"c":{"d":3}}`)
	rules := []model.CustomBodyRule{
		{Action: model.RuleActionRemove, Path: "b"},
		{Action: model.RuleActionRemove, Path: "c.d"},
	}

	out := applyBodyRules("application/json", body, rules)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, exists := got["b"]; exists {
		t.Errorf("b should be removed")
	}
	c, _ := got["c"].(map[string]any)
	if _, exists := c["d"]; exists {
		t.Errorf("c.d should be removed")
	}
}

func TestApplyBodyRules_RemoveNonExistentNoOp(t *testing.T) {
	body := []byte(`{"a":1}`)
	rules := []model.CustomBodyRule{
		{Action: model.RuleActionRemove, Path: "b.c.d"},
	}

	out := applyBodyRules("application/json", body, rules)
	if !bytes.Equal(out, body) {
		t.Errorf("expected unchanged body, got %q", out)
	}
}

func TestApplyBodyRules_ArrayIndex(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"user","content":"2"}]}`)
	rules := []model.CustomBodyRule{
		{Action: model.RuleActionOverride, Path: "messages.0.role", Value: json.RawMessage(`"system"`)},
		{Action: model.RuleActionRemove, Path: "messages.1"},
	}

	out := applyBodyRules("application/json", body, rules)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message after remove, got %d", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Errorf("messages[0].role expected system, got %v", first["role"])
	}
}

func TestApplyBodyRules_UnicodeAndSpecialObjectKeys(t *testing.T) {
	body := []byte(`{"用户":1,"pipe|key":2}`)
	rules := []model.CustomBodyRule{
		{Action: model.RuleActionOverride, Path: "用户", Value: json.RawMessage(`3`)},
		{Action: model.RuleActionOverride, Path: "pipe|key", Value: json.RawMessage(`4`)},
	}
	out := applyBodyRules("application/json", body, rules)
	if gjson.GetBytes(out, "用户").Int() != 3 {
		t.Fatalf("unicode key override failed: %s", out)
	}
	if gjson.GetBytes(out, `pipe\|key`).Int() != 4 {
		t.Fatalf("special key override failed: %s", out)
	}
}

func TestApplyBodyRules_PlusArrayIndexIsRejected(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user"}]}`)
	rules := []model.CustomBodyRule{
		{Action: model.RuleActionOverride, Path: "messages.+1.role", Value: json.RawMessage(`"system"`)},
	}
	out := applyBodyRules("application/json", body, rules)
	if !bytes.Equal(out, body) {
		t.Fatalf("+1 array index should be rejected, got %s", out)
	}
}

func TestApplyBodyRules_OverrideInvalidPathSkipped(t *testing.T) {
	body := []byte(`{"a":1}`)
	rules := []model.CustomBodyRule{
		{Action: model.RuleActionOverride, Path: "", Value: json.RawMessage("2")},
		{Action: model.RuleActionOverride, Path: "a", Value: json.RawMessage("bad json")},
	}

	out := applyBodyRules("application/json", body, rules)
	// both rules skipped: body unchanged
	if !bytes.Equal(out, body) {
		t.Errorf("expected unchanged body, got %q", out)
	}
}

func TestSplitJSONPath(t *testing.T) {
	cases := map[string][]string{
		"a":               {"a"},
		"a.b":             {"a", "b"},
		"a.b.0":           {"a", "b", "0"},
		" foo . bar ":     {"foo", "bar"},
		"":                nil,
		"   ":             nil,
		"a..b":            nil,
		"a.":              nil,
		".a":              nil,
		"thinking.type":   {"thinking", "type"},
		"messages.0.role": {"messages", "0", "role"},
	}
	for input, expected := range cases {
		got := splitJSONPath(input)
		if len(got) != len(expected) {
			t.Errorf("splitJSONPath(%q): length mismatch, got %v, want %v", input, got, expected)
			continue
		}
		for i := range got {
			if got[i] != expected[i] {
				t.Errorf("splitJSONPath(%q)[%d]: got %q, want %q", input, i, got[i], expected[i])
			}
		}
	}
}

func TestIsJSONContentType(t *testing.T) {
	cases := map[string]bool{
		"application/json":                true,
		"application/json; charset=utf-8": true,
		"application/vnd.api+json":        true,
		"text/plain":                      false,
		"":                                false,
		"application/octet-stream":        false,
	}
	for input, expected := range cases {
		if got := isJSONContentType(input); got != expected {
			t.Errorf("isJSONContentType(%q)=%v, want %v", input, got, expected)
		}
	}
}
