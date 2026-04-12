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
			name:        "openai upstream cannot expose anthropic",
			channelType: "openai",
			transforms:  []string{"anthropic"},
			wantErr:     "unsupported protocol transform anthropic -> openai",
		},
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
			name:        "anthropic upstream supports openai and codex",
			channelType: "anthropic",
			transforms:  []string{"codex", "openai"},
			want:        []string{"codex", "openai"},
		},
		{
			name:        "openai upstream supports codex",
			channelType: "openai",
			transforms:  []string{"codex"},
			want:        []string{"codex"},
		},
		{
			name:        "codex upstream supports openai",
			channelType: "codex",
			transforms:  []string{"openai"},
			want:        []string{"openai"},
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
