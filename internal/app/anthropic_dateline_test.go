package app

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"ccLoad/internal/anthropicauth"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"

	"github.com/tidwall/gjson"
)

func TestNormalizeAnthropicDatelineScope(t *testing.T) {
	t.Parallel()
	const canonical = "Today's date is 2026-09-25."
	for _, testCase := range []struct {
		name string
		path string
		body string
		want string
	}{
		{name: "system string slash", path: "system",
			body: `{"system":"Today’s date is 2026/09/25.","messages":[]}`, want: canonical},
		{name: "system block modifier letter", path: "system.0.text",
			body: `{"system":[{"type":"text","text":"env\nTodayʼs date is 2026-09-25."}],"messages":[]}`, want: "env\n" + canonical},
		{name: "reminder in text block", path: "messages.0.content.0.text",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>\nTodayʹs date is 2026/09/25.\n</system-reminder>"}]}]}`,
			want: "<system-reminder>\n" + canonical + "\n</system-reminder>"},
		{name: "reminder in string content", path: "messages.0.content",
			body: `{"messages":[{"role":"user","content":"<system-reminder>Today’s date is 2026-09-25.</system-reminder>"}]}`,
			want: "<system-reminder>" + canonical + "</system-reminder>"},
		{name: "user prose outside reminder", path: "messages.0.content",
			body: `{"messages":[{"role":"user","content":"Today’s date is 2026/09/25."}]}`, want: "Today’s date is 2026/09/25."},
		{name: "tool result", path: "messages.0.content.0.content",
			body: `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"<system-reminder>Today’s date is 2026/09/25.</system-reminder>"}]}]}`,
			want: "<system-reminder>Today’s date is 2026/09/25.</system-reminder>"},
		{name: "mixed separators", path: "system",
			body: `{"system":"Today’s date is 2026-09/25.","messages":[]}`, want: "Today’s date is 2026-09/25."},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			out := normalizeAnthropicDateline([]byte(testCase.body))
			if got := gjson.GetBytes(out, testCase.path).String(); got != testCase.want {
				t.Fatalf("%s = %q, want %q (body %s)", testCase.path, got, testCase.want, out)
			}
		})
	}

	clean := []byte(`{"system":"Today's date is 2026-09-25.","messages":[{"role":"user","content":"hi"}]}`)
	if out := normalizeAnthropicDateline(clean); !bytes.Equal(out, clean) {
		t.Fatalf("canonical body rewritten: %s", out)
	}
}

func TestAnthropicBuildProxyRequestNormalizesDatelineForOAuthOnly(t *testing.T) {
	srv := newInMemoryServer(t)
	credentialJSON, err := (&anthropicauth.Credential{
		Type: anthropicauth.ChannelType, AccessToken: "access", RefreshToken: "refresh",
		Expired: "2030-01-01T00:00:00Z", AccountUUID: "3f2b7c18-9d4e-4a6b-8c51-7e0a2d9b4f36",
	}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	officialURL := model.ChannelURLs{{URL: "https://api.anthropic.com", Protocols: []string{"anthropic"}}}
	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":1024,` +
		`"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.280.abc; cc_entrypoint=cli; cch=4d721;"},` +
		`{"type":"text","text":"Today’s date is 2026/09/25."}],` +
		`"metadata":{"user_id":"{\"device_id\":\"device\",\"account_uuid\":\"\",\"session_id\":\"e03895ad-8b34-4a84-bbf6-002e8909b17b\"}"},` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>\nToday’s date is 2026/09/25.\n</system-reminder>"},{"type":"text","text":"hello"}]}]}`)
	headers := http.Header{
		"Content-Type":   {"application/json"},
		"User-Agent":     {"claude-cli/" + anthropicCLIVersion + " (external, cli)"},
		"X-App":          {"cli"},
		"Anthropic-Beta": {"claude-code-20250219,oauth-2025-04-20"},
	}

	for _, testCase := range []struct {
		name      string
		cfg       *model.Config
		apiKey    string
		canonical bool
	}{
		{name: "oauth", apiKey: "oauth-access", canonical: true, cfg: &model.Config{
			ID: 93, Name: "anthropic-oauth", AuthType: model.AuthTypeAnthropicOAuth, OAuthCredential: credentialJSON, URLs: officialURL,
		}},
		{name: "api key", apiKey: "sk-ant-api03-test", cfg: &model.Config{
			ID: 94, Name: "anthropic-key", URLs: officialURL,
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			reqCtx := &requestContext{
				ctx: context.Background(), startTime: time.Now(),
				clientProtocol: protocol.Anthropic, upstreamProtocol: protocol.Anthropic,
			}
			request, err := srv.buildProxyRequest(
				reqCtx, testCase.cfg, testCase.apiKey, http.MethodPost, bytes.Clone(body), headers.Clone(),
				"", "/v1/messages", testCase.cfg.GetURLs()[0],
			)
			if err != nil {
				t.Fatalf("buildProxyRequest() error = %v", err)
			}
			out, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			want := "Today’s date is 2026/09/25."
			if testCase.canonical {
				want = "Today's date is 2026-09-25."
			}
			if got := gjson.GetBytes(out, "system.1.text").String(); got != want {
				t.Fatalf("system dateline = %q, want %q", got, want)
			}
			if got := gjson.GetBytes(out, "messages.0.content.0.text").String(); got != "<system-reminder>\n"+want+"\n</system-reminder>" {
				t.Fatalf("reminder dateline = %q, want %q", got, want)
			}
			if !testCase.canonical {
				return
			}
			resigned, err := finalizeAnthropicCCH(bytes.Clone(out))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(resigned, out) {
				t.Fatalf("outbound CCH does not sign the normalized body:\n got %s\nwant %s",
					gjson.GetBytes(out, "system.0.text"), gjson.GetBytes(resigned, "system.0.text"))
			}
		})
	}
}
