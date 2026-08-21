package cursorauth

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCLIRunnerStreamsAskModeOutput(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "cursor-agent")
	source := "#!/bin/sh\n" +
		"echo '{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"He\"}]}}'\n" +
		"echo '{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"Hello\"}]}}'\n" +
		"echo '{\"type\":\"result\",\"result\":\"Hello\"}'\n"
	if err := os.WriteFile(script, []byte(source), 0o755); err != nil {
		t.Fatalf("write fake agent: %v", err)
	}
	t.Setenv("CURSOR_AGENT_PATH", script)
	runner := NewCLIRunner()
	events, err := runner.Run(context.Background(), &Credential{AccessToken: "tok"}, "claude-sonnet-5", "hi")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	var deltas []string
	var final Event
	for event := range events {
		if event.Err != nil {
			t.Fatalf("event error = %v", event.Err)
		}
		if event.Delta != "" {
			deltas = append(deltas, event.Delta)
		}
		if event.Done {
			final = event
		}
	}
	if strings.Join(deltas, "") != "Hello" || final.Text != "Hello" {
		t.Fatalf("deltas = %q final = %+v", deltas, final)
	}
}

func TestCLIRunnerReportsMissingBinary(t *testing.T) {
	t.Setenv("CURSOR_AGENT_PATH", "")
	runner := &CLIRunner{
		LookPath: func(string) (string, error) { return "", errors.New("not found") },
		Timeout:  time.Second,
	}
	_, err := runner.Run(context.Background(), &Credential{AccessToken: "tok"}, "claude-sonnet-5", "hi")
	if !errors.Is(err, ErrAgentMissing) {
		t.Fatalf("err = %v", err)
	}
}

func TestClassifyAgentErrorDetectsAuth(t *testing.T) {
	t.Parallel()
	err := classifyAgentError("Authentication required. Please log in.", errors.New("exit 1"), "")
	if err == nil || !strings.Contains(err.Error(), "not authenticated") {
		t.Fatalf("err = %v", err)
	}
}

func envMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, kv := range env {
		key, value, ok := strings.Cut(kv, "=")
		if ok {
			out[key] = value
		}
	}
	return out
}

func TestCursorAgentEnvironUsesAuthTokenForSession(t *testing.T) {
	t.Setenv("CURSOR_AUTH_TOKEN", "stale")
	t.Setenv("CURSOR_API_KEY", "leaked")
	t.Setenv("CURSOR_ACCESS_TOKEN", "wrong-name")
	env := envMap(cursorAgentEnviron("/tmp/home", "/tmp/xdg", &Credential{AccessToken: "tok"}))
	if env["CURSOR_AUTH_TOKEN"] != "tok" {
		t.Fatalf("CURSOR_AUTH_TOKEN = %q", env["CURSOR_AUTH_TOKEN"])
	}
	if env["AGENT_CLI_CREDENTIAL_STORE"] != "file" {
		t.Fatalf("AGENT_CLI_CREDENTIAL_STORE = %q", env["AGENT_CLI_CREDENTIAL_STORE"])
	}
	if env["HOME"] != "/tmp/home" || env["XDG_CONFIG_HOME"] != "/tmp/xdg" {
		t.Fatalf("home env = %q xdg = %q", env["HOME"], env["XDG_CONFIG_HOME"])
	}
	if _, ok := env["CURSOR_API_KEY"]; ok {
		t.Fatal("session tokens must not be sent as CURSOR_API_KEY")
	}
	if _, ok := env["CURSOR_ACCESS_TOKEN"]; ok {
		t.Fatal("cursor-agent reads CURSOR_AUTH_TOKEN, not CURSOR_ACCESS_TOKEN")
	}
}

func TestCursorAgentEnvironPinsCompileCacheOutsideTempHome(t *testing.T) {
	t.Setenv("NODE_COMPILE_CACHE", "/tmp/home/stale-cache")
	env := envMap(cursorAgentEnviron("/tmp/home", "/tmp/xdg", &Credential{AccessToken: "tok"}))
	cache := env["NODE_COMPILE_CACHE"]
	if cache == "" {
		t.Fatal("NODE_COMPILE_CACHE must be set")
	}
	if strings.HasPrefix(cache, "/tmp/home") || cache == "/tmp/home/stale-cache" {
		t.Fatalf("compile cache must outlive the isolated HOME: %q", cache)
	}
	if cache != cursorAgentCompileCacheDir() {
		t.Fatalf("NODE_COMPILE_CACHE = %q, want %q", cache, cursorAgentCompileCacheDir())
	}
}

func TestCursorAgentEnvironPrefersAPIKey(t *testing.T) {
	t.Parallel()
	env := envMap(cursorAgentEnviron("/tmp/home", "/tmp/xdg", &Credential{AccessToken: "tok", APIKey: "key"}))
	if env["CURSOR_API_KEY"] != "key" {
		t.Fatalf("CURSOR_API_KEY = %q", env["CURSOR_API_KEY"])
	}
	if _, ok := env["CURSOR_AUTH_TOKEN"]; ok {
		t.Fatal("API key channels must not pin a stale session via CURSOR_AUTH_TOKEN")
	}
}

func TestCLIRunnerWritesPlatformAuthFiles(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "cursor-agent")
	source := "#!/bin/sh\n" +
		"test -f \"$HOME/.cursor/auth.json\" || { echo missing-darwin >&2; exit 1; }\n" +
		"test -f \"$XDG_CONFIG_HOME/cursor/auth.json\" || { echo missing-xdg >&2; exit 1; }\n" +
		"test \"$AGENT_CLI_CREDENTIAL_STORE\" = file || { echo bad-store >&2; exit 1; }\n" +
		"test \"$CURSOR_AUTH_TOKEN\" = tok || { echo bad-token >&2; exit 1; }\n" +
		"echo '{\"type\":\"result\",\"result\":\"ok\"}'\n"
	if err := os.WriteFile(script, []byte(source), 0o755); err != nil {
		t.Fatalf("write fake agent: %v", err)
	}
	t.Setenv("CURSOR_AGENT_PATH", script)
	runner := NewCLIRunner()
	events, err := runner.Run(context.Background(), &Credential{AccessToken: "tok"}, "claude-sonnet-5", "hi")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	var final Event
	for event := range events {
		if event.Err != nil {
			t.Fatalf("event error = %v", event.Err)
		}
		if event.Done {
			final = event
		}
	}
	if final.Text != "ok" {
		t.Fatalf("final = %+v", final)
	}
}

func TestCLIRunnerCommandUsesAskMode(t *testing.T) {
	t.Setenv("CURSOR_AGENT_PATH", "")
	var gotArgs []string
	runner := &CLIRunner{
		LookPath: func(string) (string, error) { return "/bin/true", nil },
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			gotArgs = append([]string{name}, args...)
			return exec.CommandContext(ctx, "true")
		},
		Timeout: time.Second,
	}
	events, err := runner.Run(context.Background(), &Credential{AccessToken: "tok"}, "claude-sonnet-5", "hi")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	for range events {
	}
	joined := strings.Join(gotArgs, " ")
	if !strings.Contains(joined, "--mode ask") || !strings.Contains(joined, "--trust") {
		t.Fatalf("args = %q", joined)
	}
}
