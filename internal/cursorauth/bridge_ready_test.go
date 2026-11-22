package cursorauth

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"

	sdkv1 "ccLoad/internal/cursorauth/sdkgen/sdk/v1"
)

func TestParseBridgeReadyLineValidatesLoopbackAndAllowsFutureFields(t *testing.T) {
	token := filepath.Join(t.TempDir(), "token")
	line := `cursor-sdk-bridge ready {"schemaVersion":1,"transport":"tcp","protocol":"connect",` +
		`"url":"http://127.0.0.1:43123","authTokenFile":` + quoteJSON(token) +
		`,"pid":42,"serverVersion":"1.0.0","futureField":true}`
	ready, matched, err := parseBridgeReadyLine(line)
	if err != nil || !matched || ready.PID != 42 {
		t.Fatalf("parse = (%+v, %v, %v)", ready, matched, err)
	}
	unsafe := strings.ReplaceAll(line, "http://127.0.0.1:43123", "http://example.com:43123")
	if _, _, err := parseBridgeReadyLine(unsafe); err == nil {
		t.Fatal("non-loopback discovery URL was accepted")
	}
}

func TestBridgeLivePinnedBinary(t *testing.T) {
	if os.Getenv("CURSOR_SDK_BRIDGE_BIN") == "" {
		t.Skip("set CURSOR_SDK_BRIDGE_BIN to run the live bridge smoke test")
	}
	bridge := newBridge()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := bridge.client(ctx)
	if err != nil {
		t.Fatalf("Client() error = %v", err)
	}
	if client == nil || client.agent == nil || client.control == nil {
		t.Fatal("bridge returned an incomplete client")
	}
	if err := bridge.close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

/**
 * @brief 用指定测试账号验证锁定版 Bridge 的工具暂停、原会话恢复和真实用量。
 * @param t 测试上下文。设置 CURSOR_SDK_SMOKE_MODEL 时才会产生推理消费。
 */
func TestBridgeLiveToolContinuation(t *testing.T) {
	modelID := strings.TrimSpace(os.Getenv("CURSOR_SDK_SMOKE_MODEL"))
	if modelID == "" {
		t.Skip("set CURSOR_SDK_SMOKE_MODEL and CURSOR_API_KEY to run paid inference")
	}
	credential := &Credential{APIKey: strings.TrimSpace(os.Getenv("CURSOR_API_KEY"))}
	if credential.APIKey == "" {
		t.Fatal("CURSOR_SDK_SMOKE_MODEL requires CURSOR_API_KEY for a test account")
	}
	t.Setenv("CURSOR_SDK_BRIDGE_BIN", "")
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	path, err := EnsureBridge(ctx)
	if err != nil {
		t.Fatal(err)
	}
	runner := NewSDKRunner(path)
	t.Cleanup(func() {
		closeCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if err := runner.Close(closeCtx); err != nil {
			t.Errorf("close bridge: %v", err)
		}
	})
	scope := SessionScope{ChannelID: 1, TokenID: 1}
	const tool = "lookup"
	request := Request{
		SessionScope: scope, Model: modelID,
		Prompt: "Call lookup exactly once. Then reply with the marker returned by the tool.",
		Tools: []Tool{{Name: tool, Description: "Returns the marker requested by the user.",
			Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}},
	}
	marker := uuid.NewString()
	var total Usage
	var reportedTotal int64
	for turn := range 2 {
		events, err := runner.Run(WithRawResponseCapture(ctx), credential, request)
		if err != nil {
			t.Fatalf("turn %d startup: %s", turn+1, strings.ReplaceAll(err.Error(), credential.APIKey, "[redacted]"))
		}
		var callID string
		var final Event
		for event := range events {
			if event.Err != nil {
				t.Fatalf("turn %d: %s", turn+1, strings.ReplaceAll(event.Err.Error(), credential.APIKey, "[redacted]"))
			}
			if event.Usage != nil {
				total.Add(event.Usage)
			}
			if len(event.RawResponse) != 0 {
				var envelope sdkv1.RunStreamMessage
				if err := protojson.Unmarshal(event.RawResponse, &envelope); err != nil {
					t.Fatal(err)
				}
				if result := envelope.GetResult(); result != nil {
					reportedTotal = result.GetResult().GetUsage().GetTotalTokens()
				}
			}
			if call := event.ToolCall; call != nil {
				if turn != 0 || callID != "" || call.Name != tool || call.ID == "" {
					t.Fatal("expected exactly one lookup call on the first turn")
				}
				callID = call.ID
			}
			if event.Done {
				final = event
			}
		}
		if turn == 0 {
			if callID == "" || final.Done {
				t.Fatal("first turn did not pause for the tool result")
			}
			// With no prompt, a lost session cannot pass by starting a fresh inference.
			request = Request{SessionScope: scope, ToolResults: []ToolResult{{CallID: callID, Output: marker}}}
		} else if !final.Done || final.Replayed || !strings.Contains(final.Text, marker) {
			t.Fatal("resumed run did not return the tool marker")
		}
	}
	sum := total.InputTokens + total.CacheReadTokens + total.CacheWriteTokens + total.OutputTokens
	if total.OutputTokens <= 0 || sum <= total.OutputTokens ||
		(total.TotalTokens > 0 && total.TotalTokens != sum) || (reportedTotal > 0 && reportedTotal != int64(sum)) {
		t.Fatalf("inconsistent SDK usage: accumulated=%+v, terminal total=%d", total, reportedTotal)
	}
	t.Logf("model=%s input=%d output=%d cacheRead=%d cacheWrite=%d", modelID, total.InputTokens,
		total.OutputTokens, total.CacheReadTokens, total.CacheWriteTokens)
}

func TestBridgeEnvironmentScrubsAllCursorOverrides(t *testing.T) {
	t.Setenv("CURSOR_API_KEY", "secret")
	t.Setenv("CURSOR_STORE_CALLBACK_URL", "http://attacker")
	env := bridgeEnvironment()
	for _, value := range env {
		if strings.HasPrefix(value, "CURSOR_") && value != "CURSOR_SDK_CLIENT_LANGUAGE=go" {
			t.Fatalf("unsafe environment survived: %s", value)
		}
	}
	if os.Getenv("CURSOR_API_KEY") != "secret" {
		t.Fatal("test setup was unexpectedly mutated")
	}
}

func TestBridgePathFindsManagedInstallation(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("CURSOR_SDK_BRIDGE_BIN", "")
	t.Setenv("SQLITE_PATH", filepath.Join("data", "ccload.db"))
	managed := managedBridgeBinaryPath(runtime.GOOS)
	if !filepath.IsAbs(managed) {
		t.Fatalf("managedBridgeBinaryPath() = %q, want absolute", managed)
	}
	if err := os.MkdirAll(filepath.Dir(managed), 0o755); err != nil {
		t.Fatalf("create managed bridge directory: %v", err)
	}
	if err := os.WriteFile(managed, []byte("bridge"), 0o755); err != nil {
		t.Fatalf("write managed bridge: %v", err)
	}
	bridge := newBridge()
	path, err := bridge.bridgePath()
	if err != nil || path != managed {
		t.Fatalf("bridgePath() = (%q, %v), want (%q, nil)", path, err, managed)
	}
}

func TestBridgePathAbsolutizesRelativeOverride(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	name := "cursor-sdk-bridge"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(name, []byte("bridge"), 0o755); err != nil {
		t.Fatalf("write bridge override: %v", err)
	}
	t.Setenv("CURSOR_SDK_BRIDGE_BIN", name)
	path, err := newBridge().bridgePath()
	if err != nil {
		t.Fatalf("bridgePath() error = %v", err)
	}
	want := filepath.Join(dir, name)
	if path != want || !filepath.IsAbs(path) {
		t.Fatalf("bridgePath() = %q, want absolute %q", path, want)
	}
}

/**
 * @brief 启动失败同时保留 Bridge 缺失分类和操作系统原始错误
 * @param t 测试上下文
 */
func TestBridgeSpawnPreservesExecutableStartError(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "cursor-sdk-bridge")
	if err := os.WriteFile(binary, []byte("bridge"), 0o755); err != nil {
		t.Fatalf("write bridge binary: %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing-cursor-sdk-bridge")
	bridge := newBridge(binary)
	defer bridge.lifeStop()
	bridge.command = func(string, ...string) *exec.Cmd {
		return exec.Command(missing)
	}

	_, err := bridge.spawn()
	// 缺失程序在 Windows 可表现为 exec.ErrNotFound，Unix 通常为 os.ErrNotExist。
	if !errors.Is(err, ErrAgentMissing) || (!errors.Is(err, os.ErrNotExist) && !errors.Is(err, exec.ErrNotFound)) {
		t.Fatalf("spawn() error = %v, want ErrAgentMissing wrapping the executable lookup/start error", err)
	}
}

/**
 * @brief 缓存目录不可创建时使用备用目录，仅在支持的平台断言 POSIX 权限
 * @param t 测试上下文
 */
func TestBridgeStateRootFallsBackWhenCacheCannotBeCreated(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "cache-file")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("create blocked cache path: %v", err)
	}
	fallback := filepath.Join(t.TempDir(), "fallback")

	got := firstWritableBridgeStateRoot([]string{
		filepath.Join(blocked, "ccload", "cursor-sdk"),
		fallback,
	})
	want, err := filepath.Abs(fallback)
	if err != nil {
		t.Fatalf("resolve fallback path: %v", err)
	}
	if got != want {
		t.Fatalf("firstWritableBridgeStateRoot() = %q, want %q", got, want)
	}
	info, err := os.Stat(got)
	if err != nil {
		t.Fatalf("stat fallback state root: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		t.Fatalf("fallback state root mode = %v, want 0700", info.Mode())
	}
}

func TestBridgeStateRootFallsBackToTempWhenPersistentRootsCannotBeCreated(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("create blocked persistent root: %v", err)
	}
	t.Setenv("SQLITE_PATH", filepath.Join(blocked, "data", "ccload.db"))
	switch runtime.GOOS {
	case "windows":
		t.Setenv("LocalAppData", filepath.Join(blocked, "cache"))
	case "darwin":
		t.Setenv("HOME", blocked)
	default:
		t.Setenv("XDG_CACHE_HOME", filepath.Join(blocked, "cache"))
	}
	temporary := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("TMP", temporary)
		t.Setenv("TEMP", temporary)
	} else {
		t.Setenv("TMPDIR", temporary)
	}

	got := bridgeStateRoot()
	want := filepath.Join(temporary, "ccload", "cursor-sdk")
	if got != want {
		t.Fatalf("bridgeStateRoot() = %q, want temporary fallback %q", got, want)
	}
}

func quoteJSON(value string) string {
	return `"` + strings.ReplaceAll(value, `\`, `\\`) + `"`
}
