package cursorauth

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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
	if !errors.Is(err, ErrAgentMissing) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("spawn() error = %v, want ErrAgentMissing wrapping os.ErrNotExist", err)
	}
}

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
	if info.Mode().Perm() != 0o700 {
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
