package sql

import "testing"

func TestCooldownSelectLockClause(t *testing.T) {
	tests := []struct {
		name       string
		driverName string
		want       string
	}{
		{name: "mysql locks row", driverName: "mysql", want: " FOR UPDATE"},
		{name: "sqlite has no row lock clause", driverName: "sqlite", want: ""},
		{name: "unknown driver stays portable", driverName: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &SQLStore{driverName: tt.driverName}
			if got := store.cooldownSelectLockClause(); got != tt.want {
				t.Fatalf("cooldownSelectLockClause() = %q, want %q", got, tt.want)
			}
		})
	}
}
