package util

import "testing"

func TestParseBool(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		raw   string
		wantV bool
		wantO bool
	}{
		{name: "true_1", raw: "1", wantV: true, wantO: true},
		{name: "true_true", raw: "true", wantV: true, wantO: true},
		{name: "true_yes", raw: "yes", wantV: true, wantO: true},
		{name: "true_chinese", raw: "启用", wantV: true, wantO: true},
		{name: "true_spaces_case", raw: "  ON  ", wantV: true, wantO: true},
		{name: "false_0", raw: "0", wantV: false, wantO: true},
		{name: "false_false", raw: "false", wantV: false, wantO: true},
		{name: "false_no", raw: "no", wantV: false, wantO: true},
		{name: "false_chinese", raw: "禁用", wantV: false, wantO: true},
		{name: "false_spaces_case", raw: "  Off  ", wantV: false, wantO: true},
		{name: "invalid", raw: "maybe", wantV: false, wantO: false},
		{name: "empty", raw: "", wantV: false, wantO: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotV, gotO := ParseBool(tt.raw)
			if gotV != tt.wantV || gotO != tt.wantO {
				t.Fatalf("ParseBool(%q) = (%v,%v), want (%v,%v)", tt.raw, gotV, gotO, tt.wantV, tt.wantO)
			}
		})
	}
}

func TestParseBoolDefault(t *testing.T) {
	t.Parallel()

	if got := ParseBoolDefault("true", false); got != true {
		t.Fatalf("expected true override default, got %v", got)
	}
	if got := ParseBoolDefault("invalid", true); got != true {
		t.Fatalf("expected default true on invalid input, got %v", got)
	}
	if got := ParseBoolDefault("", false); got != false {
		t.Fatalf("expected default false on empty input, got %v", got)
	}
}
