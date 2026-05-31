package app

import (
	"strings"
	"testing"

	"ccLoad/internal/model"
)

func TestChannelRequestValidate_RejectsUnsupportedProtocolTransforms(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		channelType string
		transforms  []string
		wantErr     string
	}{
		{
			name:        "gemini upstream rejects self transform",
			channelType: "gemini",
			transforms:  []string{"gemini"},
			wantErr:     `duplicates channel_type "gemini"`,
		},
		{
			name:        "anthropic upstream rejects duplicate transform",
			channelType: "anthropic",
			transforms:  []string{"openai", "openai"},
			wantErr:     `duplicate protocol "openai"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := ChannelRequest{
				Name:               "test",
				APIKey:             "sk-test",
				URL:                "https://example.com",
				ChannelType:        tt.channelType,
				ProtocolTransforms: tt.transforms,
				Models: []model.ModelEntry{
					{Model: "test-model"},
				},
			}

			err := req.Validate()
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestChannelRequestValidate_AllowsDocumentedProtocolTransforms(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		channelType string
		transforms  []string
		want        []string
	}{
		{
			name:        "gemini upstream supports three client protocols",
			channelType: "gemini",
			transforms:  []string{"codex", "openai", "anthropic"},
			want:        []string{"anthropic", "codex", "openai"},
		},
		{
			name:        "anthropic upstream supports all other client protocols",
			channelType: "anthropic",
			transforms:  []string{"codex", "openai", "gemini"},
			want:        []string{"codex", "gemini", "openai"},
		},
		{
			name:        "openai upstream supports anthropic and codex",
			channelType: "openai",
			transforms:  []string{"codex", "anthropic"},
			want:        []string{"anthropic", "codex"},
		},
		{
			name:        "codex upstream supports anthropic and openai",
			channelType: "codex",
			transforms:  []string{"openai", "anthropic"},
			want:        []string{"anthropic", "openai"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := ChannelRequest{
				Name:               "test",
				APIKey:             "sk-test",
				URL:                "https://example.com",
				ChannelType:        tt.channelType,
				ProtocolTransforms: tt.transforms,
				Models: []model.ModelEntry{
					{Model: "test-model"},
				},
			}

			if err := req.Validate(); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if len(req.ProtocolTransforms) != len(tt.want) {
				t.Fatalf("expected %d transforms, got %#v", len(tt.want), req.ProtocolTransforms)
			}
			for i, want := range tt.want {
				if req.ProtocolTransforms[i] != want {
					t.Fatalf("expected transforms %#v, got %#v", tt.want, req.ProtocolTransforms)
				}
			}
		})
	}
}

func TestChannelRequestValidate_DefaultsProtocolTransformModeToUpstream(t *testing.T) {
	t.Parallel()

	req := ChannelRequest{
		Name:               "test",
		APIKey:             "sk-test",
		URL:                "https://example.com",
		ChannelType:        "anthropic",
		ProtocolTransforms: []string{"openai"},
		Models: []model.ModelEntry{
			{Model: "test-model"},
		},
	}

	if err := req.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if req.ProtocolTransformMode != model.ProtocolTransformModeUpstream {
		t.Fatalf("expected protocol transform mode %q, got %q", model.ProtocolTransformModeUpstream, req.ProtocolTransformMode)
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
			got, err := validateChannelBaseURL(raw)
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

	got, err := validateChannelBaseURL("https://api.example.com/openai/")
	if err != nil {
		t.Fatalf("validateChannelBaseURL() error = %v", err)
	}
	if got != "https://api.example.com/openai" {
		t.Fatalf("normalized URL = %q, want public host with trimmed path", got)
	}
}

func TestChannelRequestValidate_RejectsInvalidProtocolTransformMode(t *testing.T) {
	t.Parallel()

	req := ChannelRequest{
		Name:                  "test",
		APIKey:                "sk-test",
		URL:                   "https://example.com",
		ChannelType:           "anthropic",
		ProtocolTransformMode: "remote",
		ProtocolTransforms:    []string{"openai"},
		Models: []model.ModelEntry{
			{Model: "test-model"},
		},
	}

	err := req.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), `invalid protocol_transform_mode`) {
		t.Fatalf("expected invalid mode error, got %v", err)
	}
}
