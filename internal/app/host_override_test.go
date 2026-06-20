package app

import (
	"context"
	"net"
	"testing"
)

func TestParseHostOverrides(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected map[string]string
	}{
		{"empty", "", nil},
		{"single", "anyrouter.top=47.246.23.200", map[string]string{"anyrouter.top": "47.246.23.200"}},
		{"multiple", "a.com=1.2.3.4,b.com=5.6.7.8", map[string]string{"a.com": "1.2.3.4", "b.com": "5.6.7.8"}},
		{"whitespace trimmed", " a.com = 1.2.3.4 , b.com=5.6.7.8 ", map[string]string{"a.com": "1.2.3.4", "b.com": "5.6.7.8"}},
		{"skip malformed", "good.com=1.2.3.4,bad,also=good=2.3.4.5", map[string]string{"good.com": "1.2.3.4"}},
		{"skip non-ip value", "good.com=1.2.3.4,bad.com=example.org", map[string]string{"good.com": "1.2.3.4"}},
		{"all invalid", "a.com=not-an-ip,b.com=999.999.999.999", nil},
		{"ipv6 value", "v6.com=::1", map[string]string{"v6.com": "::1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseHostOverrides(tt.input)
			if tt.expected == nil {
				if got != nil {
					t.Fatalf("expected nil, got %v", got)
				}
				return
			}
			if len(got) != len(tt.expected) {
				t.Fatalf("length mismatch: got %v, want %v", got, tt.expected)
			}
			for k, v := range tt.expected {
				if got[k] != v {
					t.Errorf("key %q: got %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestWrapDialerWithHostOverrides(t *testing.T) {
	var dialedAddr string
	fakeDial := func(_ context.Context, network, addr string) (net.Conn, error) {
		dialedAddr = addr
		return nil, net.UnknownNetworkError("test")
	}

	overrides := map[string]string{
		"anyrouter.top": "47.246.23.200",
	}
	wrapped := wrapDialerWithHostOverrides(fakeDial, overrides)

	// 命中覆盖：host 替换为 IP，port 保留
	_, _ = wrapped(context.Background(), "tcp", "anyrouter.top:443")
	if dialedAddr != "47.246.23.200:443" {
		t.Errorf("override hit: got %q, want %q", dialedAddr, "47.246.23.200:443")
	}

	// 未命中：原样透传
	_, _ = wrapped(context.Background(), "tcp", "other.com:8080")
	if dialedAddr != "other.com:8080" {
		t.Errorf("override miss: got %q, want %q", dialedAddr, "other.com:8080")
	}

	// addr 无端口（边界）
	_, _ = wrapped(context.Background(), "tcp", "anyrouter.top")
	if dialedAddr != "anyrouter.top" {
		t.Errorf("no port: should pass through unchanged, got %q", dialedAddr)
	}
}

func TestWrapDialerWithHostOverrides_NilOverrides(t *testing.T) {
	called := false
	fakeDial := func(_ context.Context, network, addr string) (net.Conn, error) {
		called = true
		return nil, net.UnknownNetworkError("test")
	}

	wrapped := wrapDialerWithHostOverrides(fakeDial, nil)
	_, _ = wrapped(context.Background(), "tcp", "any.com:443")
	if !called {
		t.Error("nil overrides: original dialer should be called")
	}
}
