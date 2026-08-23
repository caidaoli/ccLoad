package app

import (
	"encoding/json"
	"strings"
	"testing"

	"ccLoad/internal/model"
)

func TestChannelRequestValidate_StructuredURLs(t *testing.T) {
	t.Parallel()

	req := ChannelRequest{
		Name:   "test",
		APIKey: "sk-test",
		URLs: model.ChannelURLs{
			{URL: " https://api.example.com/ ", Protocols: []string{"CODEX", "openai", "codex"}},
			{URL: "https://api.example.com/v1/responses", Exact: true, Protocols: []string{"codex"}},
		},
		Models: []model.ModelEntry{{Model: "test-model"}},
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if got := req.URLs[0]; got.URL != "https://api.example.com" || got.Exact ||
		len(got.Protocols) != 2 || got.Protocols[0] != "codex" || got.Protocols[1] != "openai" {
		t.Fatalf("normalized first URL=%+v", got)
	}
	if got := req.ToConfig().URLs[1]; got.URL != "https://api.example.com/v1/responses" || !got.Exact {
		t.Fatalf("ToConfig exact URL=%+v", got)
	}

	payload, err := json.Marshal(req.ToConfig())
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var object map[string]any
	if err := json.Unmarshal(payload, &object); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if _, ok := object["url"]; ok {
		t.Fatalf("legacy url field leaked into JSON: %s", payload)
	}
	if urls, ok := object["urls"].([]any); !ok || len(urls) != 2 {
		t.Fatalf("structured urls missing from JSON: %s", payload)
	}
}

func TestChannelRequestValidate_XAIVersionedBaseURLRejectsEndpointPath(t *testing.T) {
	t.Parallel()

	req := ChannelRequest{
		Name:     "xAI",
		AuthType: model.AuthTypeXAIOAuth,
		URLs:     model.ChannelURLs{{URL: "https://cli-chat-proxy.grok.com/v1/responses"}},
		Models:   []model.ModelEntry{{Model: "grok"}},
	}
	err := req.Validate()
	if err == nil || !strings.Contains(err.Error(), "API endpoint path") {
		t.Fatalf("Validate() error=%v, want endpoint path validation error", err)
	}
}

func TestChannelRequestValidate_NormalizesProtocolTransformMode(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		mode string
		want string
	}{
		{name: "default", want: model.ProtocolTransformModeAuto},
		{name: "auto", mode: " AUTO ", want: model.ProtocolTransformModeAuto},
		{name: "upstream", mode: "upstream", want: model.ProtocolTransformModeUpstream},
		{name: "local", mode: "local", want: model.ProtocolTransformModeLocal},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := ChannelRequest{
				Name:                  "test",
				APIKey:                "sk-test",
				URLs:                  model.ChannelURLs{{URL: "https://example.com"}},
				ProtocolTransformMode: tt.mode,
				Models:                []model.ModelEntry{{Model: "test-model"}},
			}
			if err := req.Validate(); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if req.ProtocolTransformMode != tt.want {
				t.Fatalf("protocol_transform_mode=%q, want %q", req.ProtocolTransformMode, tt.want)
			}
			if got := req.ToConfig().GetProtocolTransformMode(); got != tt.want {
				t.Fatalf("ToConfig mode=%q, want %q", got, tt.want)
			}
		})
	}
}

func TestChannelRequestValidate_RejectsInvalidProtocolTransformMode(t *testing.T) {
	t.Parallel()

	req := ChannelRequest{
		Name:                  "test",
		APIKey:                "sk-test",
		URLs:                  model.ChannelURLs{{URL: "https://example.com"}},
		ProtocolTransformMode: "remote",
		Models:                []model.ModelEntry{{Model: "test-model"}},
	}
	err := req.Validate()
	if err == nil || !strings.Contains(err.Error(), "protocol_transform_mode") {
		t.Fatalf("Validate() error=%v, want protocol_transform_mode validation error", err)
	}
}

func TestChannelRequestValidate_NormalizesAvailableTime(t *testing.T) {
	t.Parallel()

	req := ChannelRequest{
		Name:               "scheduled-channel",
		APIKey:             "sk-test",
		URLs:               model.ChannelURLs{{URL: "https://example.com"}},
		Models:             []model.ModelEntry{{Model: "test-model"}},
		AvailableTimeStart: " 22:00 ",
		AvailableTimeEnd:   "08:00",
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if req.AvailableTimeStart != "22:00" || req.AvailableTimeEnd != "08:00" {
		t.Fatalf("normalized available time = %q-%q", req.AvailableTimeStart, req.AvailableTimeEnd)
	}
	cfg := req.ToConfig()
	if cfg.AvailableTimeStart != "22:00" || cfg.AvailableTimeEnd != "08:00" {
		t.Fatalf("ToConfig available time = %q-%q", cfg.AvailableTimeStart, cfg.AvailableTimeEnd)
	}
}

func TestChannelRequestToConfigPreservesRetryOtherKeysOnFailure(t *testing.T) {
	t.Parallel()

	cfg := (&ChannelRequest{
		Name:                    "independent-key-relay",
		APIKey:                  "sk-test",
		URLs:                    model.ChannelURLs{{URL: "https://relay.example.com"}},
		Models:                  []model.ModelEntry{{Model: "test-model"}},
		RetryOtherKeysOnFailure: true,
	}).ToConfig()

	if !cfg.RetryOtherKeysOnFailure {
		t.Fatal("retry_other_keys_on_failure was not copied to config")
	}
}

func TestChannelRequestValidate_NormalizesAPIKeyAllowedModels(t *testing.T) {
	t.Parallel()

	req := ChannelRequest{
		Name: "scoped-keys",
		APIKeys: []ChannelAPIKeyRequest{{
			APIKey: " sk-test ", AllowedModels: []string{" GPT-5 ", "gpt-5", "claude-sonnet-4(max)"},
		}},
		URLs: model.ChannelURLs{{URL: "https://example.com"}},
		Models: []model.ModelEntry{
			{Model: "gpt-5"},
			{Model: "gpt-5(max)"},
			{Model: "claude-sonnet-4"},
		},
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	got := req.APIKeys[0].AllowedModels
	if len(got) != 2 || got[0] != "gpt-5" || got[1] != "claude-sonnet-4" {
		t.Fatalf("allowed_models=%v, want canonical channel models", got)
	}

	req.APIKeys[0].AllowedModels = []string{"unknown"}
	if err := req.Validate(); err == nil || !strings.Contains(err.Error(), "must exist in channel models") {
		t.Fatalf("Validate() error=%v, want unknown model rejection", err)
	}

	wildcard := ChannelRequest{
		Name:    "wildcard-scoped-key",
		APIKeys: []ChannelAPIKeyRequest{{APIKey: "sk-test", AllowedModels: []string{"discovered-model"}}},
		URLs:    model.ChannelURLs{{URL: "https://example.com"}},
		Models:  []model.ModelEntry{{Model: "*"}},
	}
	if err := wildcard.Validate(); err != nil {
		t.Fatalf("wildcard Validate() error = %v", err)
	}
	if got := wildcard.APIKeys[0].AllowedModels; len(got) != 1 || got[0] != "discovered-model" {
		t.Fatalf("wildcard allowed_models=%v, want discovered-model", got)
	}
}

func TestValidateChannelBaseURLAllowsLocalAndPrivateHosts(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"http://localhost:8080":          "http://localhost:8080",
		"http://localhost.:8080":         "http://localhost.:8080",
		"http://127.0.0.1:8080":          "http://127.0.0.1:8080",
		"http://127.0.0.1.":              "http://127.0.0.1.",
		"http://127.1":                   "http://127.1",
		"http://2130706433":              "http://2130706433",
		"http://017700000001":            "http://017700000001",
		"http://10.0.0.1":                "http://10.0.0.1",
		"http://172.16.0.1":              "http://172.16.0.1",
		"http://192.168.1.1":             "http://192.168.1.1",
		"http://169.254.169.254/latest":  "http://169.254.169.254/latest",
		"http://[::1]:8080":              "http://[::1]:8080",
		"http://[::ffff:127.0.0.1]:8080": "http://[::ffff:127.0.0.1]:8080",
		"http://[fc00::1]":               "http://[fc00::1]",
		"http://[fe80::1%25lo0]":         "http://[fe80::1%lo0]",
	}

	for raw, want := range tests {
		t.Run(raw, func(t *testing.T) {
			got, err := validateChannelBaseURL(raw, model.AuthTypeAPIKey)
			if err != nil {
				t.Fatalf("validateChannelBaseURL(%q) error = %v", raw, err)
			}
			if got != want {
				t.Fatalf("validateChannelBaseURL(%q) = %q, want %q", raw, got, want)
			}
		})
	}
}

func TestValidateChannelBaseURLAllowsPublicHost(t *testing.T) {
	t.Parallel()

	got, err := validateChannelBaseURL("https://api.example.com/openai/", model.AuthTypeAPIKey)
	if err != nil {
		t.Fatalf("validateChannelBaseURL() error = %v", err)
	}
	if got != "https://api.example.com/openai" {
		t.Fatalf("normalized URL = %q, want public host with trimmed path", got)
	}
}
