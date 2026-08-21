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
