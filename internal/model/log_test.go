package model

import "testing"

func TestLogSourceCheckinNormalizesAsPersistedSource(t *testing.T) {
	t.Parallel()
	if got := NormalizeStoredLogSource(LogSourceCheckin); got != LogSourceCheckin {
		t.Fatalf("NormalizeStoredLogSource(%q)=%q, want exact checkin source", LogSourceCheckin, got)
	}
}
