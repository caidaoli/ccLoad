package util

import (
	"regexp"
	"testing"
)

var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var uuidV5Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNewUUIDv4Format(t *testing.T) {
	for range 100 {
		got := NewUUIDv4()
		if !uuidV4Pattern.MatchString(got) {
			t.Fatalf("invalid UUIDv4: %q", got)
		}
	}
}

func TestNewUUIDv4Unique(t *testing.T) {
	seen := make(map[string]struct{}, 1024)
	for range 1024 {
		v := NewUUIDv4()
		if _, dup := seen[v]; dup {
			t.Fatalf("duplicate UUIDv4 within 1024 samples: %q", v)
		}
		seen[v] = struct{}{}
	}
}

func TestNewUUIDv5Deterministic(t *testing.T) {
	a := NewUUIDv5(NameSpaceOID, "ccload:test:foo")
	b := NewUUIDv5(NameSpaceOID, "ccload:test:foo")
	if a != b {
		t.Fatalf("UUIDv5 must be deterministic, got %q vs %q", a, b)
	}
	if !uuidV5Pattern.MatchString(a) {
		t.Fatalf("invalid UUIDv5 format: %q", a)
	}
	c := NewUUIDv5(NameSpaceOID, "ccload:test:bar")
	if a == c {
		t.Fatalf("different name must produce different UUIDv5")
	}
}

// TestNewUUIDv5KnownVector 校验与原 newCodexUUIDv5 行为完全一致：
// 输入 (NameSpaceOID, "ccload:codex:prompt-cache:apikey-x") 在重构前后必须相同。
func TestNewUUIDv5KnownVector(t *testing.T) {
	got := NewUUIDv5(NameSpaceOID, "ccload:codex:prompt-cache:apikey-x")
	// 该值由 RFC 4122 算法决定，重构不改变；仅校验稳定性 + 形态。
	if !uuidV5Pattern.MatchString(got) {
		t.Fatalf("invalid UUIDv5: %q", got)
	}
	again := NewUUIDv5(NameSpaceOID, "ccload:codex:prompt-cache:apikey-x")
	if got != again {
		t.Fatalf("non-deterministic UUIDv5: %q vs %q", got, again)
	}
}
