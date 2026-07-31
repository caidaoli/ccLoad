package protocol

import "testing"

// TestProtocolEnumeration 守住枚举唯一来源契约：
// AllProtocols 与 IsValid 必须一致，新增协议时两者同步。
func TestProtocolEnumeration(t *testing.T) {
	all := AllProtocols()
	if len(all) != 4 {
		t.Fatalf("AllProtocols() len = %d, want 4", len(all))
	}
	seen := make(map[Protocol]bool, len(all))
	for _, p := range all {
		if !IsValid(p) {
			t.Errorf("IsValid(%q) = false, want true", p)
		}
		if seen[p] {
			t.Errorf("AllProtocols() contains duplicate %q", p)
		}
		seen[p] = true
	}

	for _, invalid := range []Protocol{"", "invalid", "ANTHROPIC", " anthropic"} {
		if IsValid(invalid) {
			t.Errorf("IsValid(%q) = true, want false", invalid)
		}
	}
}
