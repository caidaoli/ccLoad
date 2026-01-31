package app

import "testing"

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

		{name: "int_log_retention_days_ok_disabled", key: "log_retention_days", valueType: "int", value: "-1", wantErr: false},
		{name: "int_log_retention_days_reject_0", key: "log_retention_days", valueType: "int", value: "0", wantErr: true},
		{name: "int_log_retention_days_ok_min", key: "log_retention_days", valueType: "int", value: "1", wantErr: false},
		{name: "int_log_retention_days_ok_max", key: "log_retention_days", valueType: "int", value: "365", wantErr: false},
		{name: "int_log_retention_days_reject_over", key: "log_retention_days", valueType: "int", value: "366", wantErr: true},

		{name: "bool_ok_true", key: "any_bool", valueType: "bool", value: "true", wantErr: false},
		{name: "bool_ok_false", key: "any_bool", valueType: "bool", value: "false", wantErr: false},
		{name: "bool_ok_1", key: "any_bool", valueType: "bool", value: "1", wantErr: false},
		{name: "bool_ok_0", key: "any_bool", valueType: "bool", value: "0", wantErr: false},
		{name: "bool_reject", key: "any_bool", valueType: "bool", value: "yes", wantErr: true},

		{name: "duration_ok_0", key: "any_duration", valueType: "duration", value: "0", wantErr: false},
		{name: "duration_ok_10", key: "any_duration", valueType: "duration", value: "10", wantErr: false},
		{name: "duration_reject_negative", key: "any_duration", valueType: "duration", value: "-1", wantErr: true},
		{name: "duration_reject_non_int", key: "any_duration", valueType: "duration", value: "1.5", wantErr: true},

		{name: "string_accepts_any", key: "any_string", valueType: "string", value: "", wantErr: false},

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
