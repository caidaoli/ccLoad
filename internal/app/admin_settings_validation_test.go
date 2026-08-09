package app

import (
	"testing"

	"ccLoad/internal/config"
)

func TestValidateSettingValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		key       string
		valueType string
		value     string
		wantErr   bool
	}{
		{name: "int_ok_generic", key: "any_int", valueType: "int", value: "0", wantErr: false},
		{name: "int_invalid", key: "any_int", valueType: "int", value: "x", wantErr: true},
		{name: "int_generic_min_minus_1_ok", key: "any_int", valueType: "int", value: "-1", wantErr: false},
		{name: "int_generic_less_than_minus_1_reject", key: "any_int", valueType: "int", value: "-2", wantErr: true},

		{name: "int_max_key_retries_reject_0", key: "max_key_retries", valueType: "int", value: "0", wantErr: true},
		{name: "int_max_key_retries_ok_1", key: "max_key_retries", valueType: "int", value: "1", wantErr: false},
		{name: "float_channel_check_interval_ok_0", key: "channel_check_interval_hours", valueType: "float", value: "0", wantErr: false},
		{name: "float_channel_check_interval_ok_0.5", key: "channel_check_interval_hours", valueType: "float", value: "0.5", wantErr: false},
		{name: "float_channel_check_interval_ok_1", key: "channel_check_interval_hours", valueType: "float", value: "1", wantErr: false},
		{name: "float_channel_check_interval_reject_negative", key: "channel_check_interval_hours", valueType: "float", value: "-0.1", wantErr: true},
		{name: "int_auto_update_interval_ok_disabled", key: "auto_update_interval_hours", valueType: "int", value: "0", wantErr: false},
		{name: "int_auto_update_interval_ok_min", key: "auto_update_interval_hours", valueType: "int", value: "1", wantErr: false},
		{name: "int_auto_update_interval_reject_fraction", key: "auto_update_interval_hours", valueType: "int", value: "0.5", wantErr: true},
		{name: "int_auto_update_interval_reject_negative", key: "auto_update_interval_hours", valueType: "int", value: "-1", wantErr: true},
		{name: "int_responses_ws_max_connections_ok_default", key: "responses_ws_max_connections", valueType: "int", value: "0", wantErr: false},
		{name: "int_responses_ws_max_connections_ok_positive", key: "responses_ws_max_connections", valueType: "int", value: "64", wantErr: false},
		{name: "int_responses_ws_max_connections_reject_negative", key: "responses_ws_max_connections", valueType: "int", value: "-1", wantErr: true},
		{name: "int_responses_ws_max_connections_per_token_ok_default", key: "responses_ws_max_connections_per_token", valueType: "int", value: "0", wantErr: false},
		{name: "int_responses_ws_max_connections_per_token_reject_negative", key: "responses_ws_max_connections_per_token", valueType: "int", value: "-1", wantErr: true},
		{name: "int_responses_ws_max_transcript_bytes_ok", key: "responses_ws_max_transcript_bytes", valueType: "int", value: "134217728", wantErr: false},
		{name: "int_responses_ws_max_transcript_bytes_ok_default", key: "responses_ws_max_transcript_bytes", valueType: "int", value: "0", wantErr: false},
		{name: "int_responses_ws_max_transcript_bytes_reject_negative", key: "responses_ws_max_transcript_bytes", valueType: "int", value: "-1", wantErr: true},

		{name: "int_log_retention_days_ok_disabled", key: "log_retention_days", valueType: "int", value: "-1", wantErr: false},
		{name: "int_log_retention_days_reject_0", key: "log_retention_days", valueType: "int", value: "0", wantErr: true},
		{name: "int_log_retention_days_ok_min", key: "log_retention_days", valueType: "int", value: "1", wantErr: false},
		{name: "int_log_retention_days_ok_max", key: "log_retention_days", valueType: "int", value: "365", wantErr: false},
		{name: "int_log_retention_days_reject_over", key: "log_retention_days", valueType: "int", value: "366", wantErr: true},

		{name: "int_max_concurrency_reject_0", key: "max_concurrency", valueType: "int", value: "0", wantErr: true},
		{name: "int_max_concurrency_ok", key: "max_concurrency", valueType: "int", value: "1000", wantErr: false},
		{name: "int_max_body_bytes_reject_negative", key: "max_body_bytes", valueType: "int", value: "-1", wantErr: true},
		{name: "int_max_body_bytes_ok", key: "max_body_bytes", valueType: "int", value: "10485760", wantErr: false},
		{name: "int_max_image_body_bytes_ok", key: "max_image_body_bytes", valueType: "int", value: "20971520", wantErr: false},
		{name: "int_cooldown_auth_reject_0", key: "cooldown_auth_seconds", valueType: "int", value: "0", wantErr: true},
		{name: "int_cooldown_auth_ok", key: "cooldown_auth_seconds", valueType: "int", value: "300", wantErr: false},
		{name: "int_cooldown_min_reject_0", key: "cooldown_min_seconds", valueType: "int", value: "0", wantErr: true},
		{name: "int_cooldown_max_ok", key: "cooldown_max_seconds", valueType: "int", value: "1800", wantErr: false},

		{name: "bool_ok_true", key: "any_bool", valueType: "bool", value: "true", wantErr: false},
		{name: "bool_ok_false", key: "any_bool", valueType: "bool", value: "false", wantErr: false},
		{name: "bool_ok_1", key: "any_bool", valueType: "bool", value: "1", wantErr: false},
		{name: "bool_ok_0", key: "any_bool", valueType: "bool", value: "0", wantErr: false},
		{name: "bool_reject", key: "any_bool", valueType: "bool", value: "yes", wantErr: true},

		{name: "duration_ok_0", key: "any_duration", valueType: "duration", value: "0", wantErr: false},
		{name: "duration_ok_10", key: "any_duration", valueType: "duration", value: "10", wantErr: false},
		{name: "duration_reject_negative", key: "any_duration", valueType: "duration", value: "-1", wantErr: true},
		{name: "duration_reject_non_int", key: "any_duration", valueType: "duration", value: "1.5", wantErr: true},
		{name: "duration_upstream_connection_reuse_limit_ok_disabled", key: "upstream_connection_reuse_limit_seconds", valueType: "duration", value: "0", wantErr: false},
		{name: "duration_upstream_connection_reuse_limit_ok_positive", key: "upstream_connection_reuse_limit_seconds", valueType: "duration", value: "540", wantErr: false},
		{name: "duration_upstream_connection_reuse_limit_reject_negative", key: "upstream_connection_reuse_limit_seconds", valueType: "duration", value: "-1", wantErr: true},

		{name: "string_accepts_any", key: "any_string", valueType: "string", value: "", wantErr: false},
		{name: "string_auto_update_channel_stable", key: "auto_update_channel", valueType: "string", value: "stable", wantErr: false},
		{name: "string_auto_update_channel_preview", key: "auto_update_channel", valueType: "string", value: "preview", wantErr: false},
		{name: "string_auto_update_channel_reject_beta", key: "auto_update_channel", valueType: "string", value: "beta", wantErr: true},
		{name: "string_auto_update_channel_reject_empty", key: "auto_update_channel", valueType: "string", value: "", wantErr: true},
		{name: "string_codex_base_url_accepts_empty", key: config.CodexBaseURLSettingKey, valueType: "string", value: "", wantErr: false},
		{name: "string_codex_base_url_accepts_https", key: config.CodexBaseURLSettingKey, valueType: "string", value: "https://gateway.example/backend-api/codex/responses", wantErr: false},
		{name: "string_xai_base_url_accepts_v1", key: config.XAIBaseURLSettingKey, valueType: "string", value: "https://gateway.example/v1", wantErr: false},
		{name: "string_antigravity_url_accepts_http", key: config.AntigravityURLSettingKey, valueType: "string", value: "http://gateway.example", wantErr: false},
		{name: "string_anthropic_base_url_accepts_https", key: config.AnthropicBaseURLSettingKey, valueType: "string", value: "https://gateway.example/anthropic", wantErr: false},
		{name: "string_oauth_base_url_rejects_relative", key: config.CodexBaseURLSettingKey, valueType: "string", value: "/responses", wantErr: true},
		{name: "string_oauth_base_url_rejects_credentials", key: config.XAIBaseURLSettingKey, valueType: "string", value: "https://user:pass@gateway.example/v1", wantErr: true},
		{name: "string_oauth_base_url_rejects_query", key: config.AntigravityURLSettingKey, valueType: "string", value: "https://gateway.example?token=secret", wantErr: true},
		{name: "string_anthropic_base_url_rejects_query", key: config.AnthropicBaseURLSettingKey, valueType: "string", value: "https://gateway.example/anthropic?token=secret", wantErr: true},
		{name: "string_oauth_base_url_rejects_non_http", key: config.CodexBaseURLSettingKey, valueType: "string", value: "ftp://gateway.example/responses", wantErr: true},

		{name: "unknown_type_reject", key: "k", valueType: "wtf", value: "x", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSettingValue(tt.key, tt.valueType, tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateSettingValue(%q,%q,%q) err=%v, wantErr=%v", tt.key, tt.valueType, tt.value, err, tt.wantErr)
			}
		})
	}
}
