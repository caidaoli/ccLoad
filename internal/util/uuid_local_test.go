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
