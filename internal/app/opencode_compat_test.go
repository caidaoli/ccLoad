package app

import (
	"context"
	"net/http"
	"regexp"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/testutil"
)

func TestOpenCodeSessionHeaderUsesStableInputOrExecutionIdentity(t *testing.T) {
	dst := http.Header{}
	src := http.Header{"X-Session-Id": {"stable-session"}}
	ensureOpenCodeSessionHeader(dst, src, "execution-fallback")
	if got := dst.Get(opencodeSessionHeader); got != "stable-session" {
		t.Fatalf("session header=%q, want stable-session", got)
	}

	dst = http.Header{}
	ensureOpenCodeSessionHeader(dst, nil, "execution-fallback")
	if got := dst.Get(opencodeSessionHeader); got != "execution-fallback" {
		t.Fatalf("session header=%q, want execution fallback", got)
	}

	dst = http.Header{opencodeSessionHeader: {"explicit"}}
	ensureOpenCodeSessionHeader(dst, nil, "other-session")
	if got := dst.Get(opencodeSessionHeader); got != "explicit" {
		t.Fatalf("explicit session header=%q, want explicit", got)
	}
}

func TestOpenCodeSessionHeaderUsesDeterministicKnownHeaderFallback(t *testing.T) {
	src := http.Header{
		"X-Zeta-Session-Id":   {"zeta"},
		"X-Alpha-Session-Id":  {"alpha"},
		"X-Parent-Session-Id": {"parent"},
	}
	dst := http.Header{}
	ensureOpenCodeSessionHeader(dst, src, "execution-fallback")
	if got := dst.Get(opencodeSessionHeader); got != "alpha" {
		t.Fatalf("session header=%q, want alphabetically first session id", got)
	}
}

func TestOpenCodeChannelRecognizesOfficialGoEndpoint(t *testing.T) {
	tests := []struct {
		name string
		cfg  *model.Config
		want bool
	}{
		{
			name: "base endpoint",
			cfg: &model.Config{
				Name: "unrelated channel name",
				URLs: model.ChannelURLs{{URL: "https://opencode.ai/zen/go"}},
			},
			want: true,
		},
		{
			name: "responses endpoint",
			cfg: &model.Config{
				Name: "unrelated channel name",
				URLs: model.ChannelURLs{{URL: "https://opencode.ai/zen/go/v1/responses"}},
			},
			want: true,
		},
		{
			name: "exact URL marker",
			cfg: &model.Config{
				Name: "unrelated channel name",
				URLs: model.ChannelURLs{{URL: "https://opencode.ai/zen/go#"}},
			},
			want: true,
		},
		{
			name: "name without endpoint",
			cfg:  &model.Config{Name: "OpenCode Go"},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isOpenCodeChannel(tt.cfg); got != tt.want {
				t.Fatalf("isOpenCodeChannel()=%v, want %v for %+v", got, tt.want, tt.cfg)
			}
		})
	}
}

func TestOpenCodeChannelRejectsLookalikeEndpoints(t *testing.T) {
	for _, endpoint := range []string{
		"https://opencode.ai.evil.example/zen/go",
		"https://proxy-opencode.ai/zen/go",
		"https://opencode.ai:8443/zen/go",
		"http://opencode.ai/zen/go",
		"https://opencode.ai/zen/gopher",
		"https://opencode.ai/zen/go-extra",
		"https://opencode.ai/other/zen/go",
		"https://opencode.ai/zen%2Fgo",
		"https://opencode.ai/zen/go/../other",
	} {
		t.Run(endpoint, func(t *testing.T) {
			cfg := &model.Config{
				Name: "OpenCode Go",
				URLs: model.ChannelURLs{{URL: endpoint}},
			}
			if isOpenCodeChannel(cfg) {
				t.Fatalf("lookalike endpoint was recognized: %q", endpoint)
			}
		})
	}
}

func TestOpenCodeSessionHeaderIsStableForRepeatedBuilders(t *testing.T) {
	src := http.Header{
		"X-Session-Id":       {"native-session"},
		"X-OpenCode-Session": {"explicit-session"},
	}
	first := http.Header{}
	second := http.Header{}
	ensureOpenCodeSessionHeader(first, src, "execution-session")
	ensureOpenCodeSessionHeader(second, src, "execution-session")
	if got := first.Get(opencodeSessionHeader); got != "explicit-session" {
		t.Fatalf("explicit source header=%q, want explicit-session", got)
	}
	if got := second.Get(opencodeSessionHeader); got != first.Get(opencodeSessionHeader) {
		t.Fatalf("same session was not stable: first=%q second=%q", first.Get(opencodeSessionHeader), got)
	}

	dst := http.Header{
		"X-OpenCode-Session": {"destination-explicit"},
		"X-Session-Id":       {"destination-native"},
	}
	ensureOpenCodeSessionHeader(dst, src, "other-session")
	if got := dst.Get(opencodeSessionHeader); got != "destination-explicit" {
		t.Fatalf("explicit destination header=%q, want destination-explicit", got)
	}
}

func TestOpenCodeSessionHeaderFallsBackToUUID(t *testing.T) {
	dst := http.Header{}
	ensureOpenCodeSessionHeader(dst, nil, "")
	got := dst.Get(opencodeSessionHeader)
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(got) {
		t.Fatalf("session header=%q is not a UUIDv4", got)
	}
}

func TestOpenCodeSessionHeaderIsUsedByProxyAndAdminBuilders(t *testing.T) {
	const endpoint = "https://opencode.ai/zen/go"
	cfg := &model.Config{
		ID:   4,
		Name: "OpenCode Go",
		URLs: model.ChannelURLs{{URL: endpoint}},
	}

	srv := newInMemoryServer(t)
	body := []byte(`{"model":"gpt-5.4","input":[]}`)
	proxyCtx := &requestContext{
		ctx:              context.Background(),
		startTime:        time.Now(),
		clientProtocol:   protocol.OpenAI,
		upstreamProtocol: protocol.Codex,
		transformPlan: protocol.TransformPlan{
			ClientProtocol:   protocol.OpenAI,
			UpstreamProtocol: protocol.Codex,
			RequestFamily:    protocol.RequestFamilyResponses,
			OriginalBody:     body,
			TranslatedBody:   body,
			OriginalModel:    "gpt-5.4",
			ActualModel:      "gpt-5.4",
		},
	}
	proxyReq, err := srv.buildProxyRequest(
		proxyCtx,
		cfg,
		"sk-test-key",
		http.MethodPost,
		body,
		http.Header{"X-Session-Id": {"proxy-session"}},
		"",
		"/v1/responses",
		endpoint,
	)
	if err != nil {
		t.Fatalf("buildProxyRequest failed: %v", err)
	}
	if got := proxyReq.Header.Get(opencodeSessionHeader); got != "proxy-session" {
		t.Fatalf("proxy OpenCode session=%q, want proxy-session", got)
	}

	adminTestReq := &testutil.TestChannelRequest{
		Model:          "gpt-5.4",
		ClientProtocol: "openai",
		SessionID:      "admin-session",
	}
	adminReq, _, cancel, err := srv.buildTestUpstreamRequestForProtocol(
		context.Background(), cfg, "sk-test-key", adminTestReq,
		adminTestReq.Model, "openai", "codex", endpoint,
	)
	if cancel != nil {
		defer cancel()
	}
	if err != nil {
		t.Fatalf("buildTestUpstreamRequestForProtocol failed: %v", err)
	}
	if got, want := adminReq.Header.Get(opencodeSessionHeader), adminTestReq.ResolveSessionID(); got != want {
		t.Fatalf("admin OpenCode session=%q, want resolved session %q", got, want)
	}
}
