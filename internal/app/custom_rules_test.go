package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"ccLoad/internal/model"
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
