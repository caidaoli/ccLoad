package cursorauth

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Event is one cursor-agent stream-json update. Text is the cumulative
// assistant text so far; Delta is the newly appended slice.
type Event struct {
	Delta string
	Text  string
	Done  bool
	Err   error
}

// Runner runs one Cursor inference. The production implementation spawns
// cursor-agent; tests inject a fake.
type Runner interface {
	Run(ctx context.Context, credential *Credential, model, prompt string) (<-chan Event, error)
}

// CLIRunner spawns cursor-agent --print --trust --mode ask.
//
// Tool calling is intentionally absent: the CLI ask mode is text-only, which
// is also how cursor2Oauth works. Client `tools` arrays are not forwarded.
type CLIRunner struct {
	LookPath func(string) (string, error)
	Command  func(ctx context.Context, name string, args ...string) *exec.Cmd
	Timeout  time.Duration
}

// ErrAgentMissing reports that cursor-agent is not installed.
var ErrAgentMissing = errors.New("cursor-agent is not installed")

// NewCLIRunner returns the production runner.
func NewCLIRunner() *CLIRunner {
	return &CLIRunner{LookPath: exec.LookPath, Command: exec.CommandContext, Timeout: AgentTimeout}
}

// Run starts cursor-agent and streams assistant text.
func (r *CLIRunner) Run(ctx context.Context, credential *Credential, model, prompt string) (<-chan Event, error) {
	if r == nil {
		return nil, errors.New("cursor-agent runner is unavailable")
	}
	if credential == nil || strings.TrimSpace(credential.AccessToken) == "" {
		return nil, errors.New("cursor credential is missing access_token")
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil, errors.New("cursor prompt is required")
	}
	model = strings.TrimSpace(model)
	if model == "" {
		model = "claude-sonnet-5"
	}
	binary, err := r.lookPath()
	if err != nil {
		return nil, err
	}
	home, err := os.MkdirTemp("", "ccload-cursor-")
	if err != nil {
		return nil, fmt.Errorf("create cursor-agent home: %w", err)
	}
	configDir := filepath.Join(home, "xdg-config")
	if err := os.MkdirAll(filepath.Join(configDir, "cursor"), 0o700); err != nil {
		_ = os.RemoveAll(home)
		return nil, fmt.Errorf("create cursor-agent config: %w", err)
	}
	authJSON, err := credential.AuthFileJSON()
	if err != nil {
		_ = os.RemoveAll(home)
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(configDir, "cursor", "auth.json"), authJSON, 0o600); err != nil {
		_ = os.RemoveAll(home)
		return nil, fmt.Errorf("write cursor-agent auth.json: %w", err)
	}

	timeout := r.Timeout
	if timeout <= 0 {
		timeout = AgentTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	commandFn := r.Command
	if commandFn == nil {
		commandFn = exec.CommandContext
	}
	cmd := commandFn(runCtx, binary, "--print", "--trust", "--mode", "ask", "--model", model, "--output-format", "stream-json", prompt)
	cmd.Dir = home
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"XDG_CONFIG_HOME="+configDir,
		"CURSOR_ACCESS_TOKEN="+strings.TrimSpace(credential.AccessToken),
	)
	cmd.Stdin = nil
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		_ = os.RemoveAll(home)
		return nil, fmt.Errorf("cursor-agent stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		_ = os.RemoveAll(home)
		return nil, fmt.Errorf("cursor-agent stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		_ = os.RemoveAll(home)
		return nil, fmt.Errorf("start cursor-agent: %w", err)
	}

	events := make(chan Event, 16)
	go func() {
		defer close(events)
		defer cancel()
		defer func() { _ = os.RemoveAll(home) }()
		var errBuf strings.Builder
		go func() { _, _ = io.Copy(&errBuf, io.LimitReader(stderr, 64<<10)) }()
		text, runErr := consumeAgentStream(stdout, events)
		waitErr := cmd.Wait()
		stderrText := strings.TrimSpace(errBuf.String())
		if classified := classifyAgentError(stderrText, waitErr, text); classified != nil {
			if runErr == nil {
				runErr = classified
			}
		} else if waitErr != nil && runErr == nil && text == "" {
			runErr = fmt.Errorf("cursor-agent: %w", waitErr)
		}
		if runErr != nil {
			events <- Event{Text: text, Done: true, Err: runErr}
			return
		}
		events <- Event{Text: text, Done: true}
	}()
	return events, nil
}

func (r *CLIRunner) lookPath() (string, error) {
	lookPath := r.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	if override := strings.TrimSpace(os.Getenv("CURSOR_AGENT_PATH")); override != "" {
		if _, err := os.Stat(override); err != nil {
			return "", fmt.Errorf("%w: CURSOR_AGENT_PATH %s: %v", ErrAgentMissing, override, err)
		}
		return override, nil
	}
	path, err := lookPath("cursor-agent")
	if err != nil || strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("%w: install the Cursor CLI from https://cursor.com/install and ensure cursor-agent is on PATH", ErrAgentMissing)
	}
	return path, nil
}

func consumeAgentStream(stdout io.Reader, events chan<- Event) (string, error) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)
	full := ""
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var payload struct {
			Type    string `json:"type"`
			Result  string `json:"result"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &payload) != nil {
			continue
		}
		switch payload.Type {
		case "assistant":
			text := ""
			for _, part := range payload.Message.Content {
				if part.Type == "text" && part.Text != "" {
					text = part.Text
				}
			}
			if text == "" || text == full {
				continue
			}
			delta := text
			if strings.HasPrefix(text, full) {
				delta = text[len(full):]
			}
			full = text
			events <- Event{Delta: delta, Text: full}
		case "result":
			if payload.Result != "" && full == "" {
				full = payload.Result
				events <- Event{Delta: payload.Result, Text: full}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return full, fmt.Errorf("read cursor-agent stdout: %w", err)
	}
	return full, nil
}

func classifyAgentError(stderr string, waitErr error, resultText string) error {
	lower := strings.ToLower(stderr)
	if strings.Contains(lower, "authentication required") || strings.Contains(lower, "not logged in") ||
		strings.Contains(lower, "not authenticated") {
		return errors.New("cursor CLI is not authenticated")
	}
	if waitErr == nil {
		return nil
	}
	if resultText != "" {
		return nil
	}
	if stderr != "" && !strings.Contains(lower, "warning") && !strings.Contains(lower, "warn") {
		return errors.New(stderr)
	}
	return fmt.Errorf("cursor-agent: %w", waitErr)
}
