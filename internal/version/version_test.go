package version

import (
	"io"
	"os"
	"strings"
	"testing"
)

func TestNormalizeVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want string
	}{
		{in: "v1.2.3", want: "1.2.3"},
		{in: "  v0.0.1  ", want: "0.0.1"},
		{in: "1.0.0", want: "1.0.0"},
		{in: "v", want: ""},
		{in: "", want: ""},
	}

	for _, tt := range tests {
		if got := normalizeVersion(tt.in); got != tt.want {
			t.Fatalf("normalizeVersion(%q)=%q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestGetUpdateInfo_ReadsCheckerState(t *testing.T) {
	// 不打网络，不跑 goroutine，只验证读路径。
	checker.mu.Lock()
	checker.hasUpdate = true
	checker.latestVersion = "v9.9.9"
	checker.releaseURL = "https://example.com"
	checker.mu.Unlock()

	hasUpdate, latest, url := GetUpdateInfo()
	if !hasUpdate || latest != "v9.9.9" || url != "https://example.com" {
		t.Fatalf("GetUpdateInfo() = (%v,%q,%q), want (true,%q,%q)", hasUpdate, latest, url, "v9.9.9", "https://example.com")
	}
}

func TestPrintBanner_NonTTY(t *testing.T) {
	// term.IsTerminal 在 pipe/文件上应为 false，走非彩色分支，输出稳定可测。
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe failed: %v", err)
	}
	os.Stderr = w
	defer func() {
		os.Stderr = old
		_ = r.Close()
	}()

	origVersion, origCommit, origBuildTime, origBuiltBy := Version, Commit, BuildTime, BuiltBy
	Version, Commit, BuildTime, BuiltBy = "test-ver", "test-commit", "test-time", "test-by"
	defer func() { Version, Commit, BuildTime, BuiltBy = origVersion, origCommit, origBuildTime, origBuiltBy }()

	PrintBanner()
	_ = w.Close()

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stderr failed: %v", err)
	}
	s := string(out)
	for _, mustContain := range []string{
		"API Load Balancer & Proxy",
		"Version:",
		"test-ver",
		"Commit:",
		"test-commit",
		"Build Time:",
		"test-time",
		"Built By:",
		"test-by",
		"Repo:",
		"ccLoad",
	} {
		if !strings.Contains(s, mustContain) {
			t.Fatalf("banner output missing %q, got:\n%s", mustContain, s)
		}
	}
}
