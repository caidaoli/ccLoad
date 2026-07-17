package sql

import "testing"

func TestRebindPostgres(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"SELECT 1", "SELECT 1"},
		{"SELECT * FROM t WHERE id = ?", "SELECT * FROM t WHERE id = $1"},
		{"INSERT INTO t(a,b) VALUES(?, ?)", "INSERT INTO t(a,b) VALUES($1, $2)"},
		{"WHERE name = '?' AND id = ?", "WHERE name = '?' AND id = $1"},
		{"WHERE name = 'it''s' AND id = ?", "WHERE name = 'it''s' AND id = $1"},
		{"a = ? AND b = ? AND c = ?", "a = $1 AND b = $2 AND c = $3"},
	}
	for _, tt := range tests {
		got := RebindPostgres(tt.in)
		if got != tt.want {
			t.Fatalf("RebindPostgres(%q)=%q, want %q", tt.in, got, tt.want)
		}
	}
}
