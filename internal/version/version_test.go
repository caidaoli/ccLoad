package version

import (
	"io"
	"os"
	"strings"
	"testing"
)

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
