package util

import (
	"testing"

	"github.com/bytedance/sonic"
)

func TestFlexibleBool_UnmarshalJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		want    bool
		wantErr bool
	}{
		{name: "bool_true", raw: `true`, want: true},
		{name: "bool_false", raw: `false`, want: false},
		{name: "string_true", raw: `"true"`, want: true},
		{name: "string_false", raw: `"false"`, want: false},
		{name: "string_one", raw: `"1"`, want: true},
		{name: "null", raw: `null`, want: false},
		{name: "invalid_string", raw: `"maybe"`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got FlexibleBool
			err := sonic.Unmarshal([]byte(tt.raw), &got)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unmarshal failed: %v", err)
			}
			if got.Bool() != tt.want {
				t.Fatalf("FlexibleBool(%s) = %v, want %v", tt.raw, got.Bool(), tt.want)
			}
		})
	}
}
