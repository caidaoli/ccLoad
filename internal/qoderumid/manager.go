// Package qoderumid manages the official Qoder runtime-info component.
package qoderumid

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const identityTTL = 50 * time.Minute
const maxOutput = 1 << 20

// Status describes component availability without exposing local paths or identity values.
type Status struct {
	Available        bool   `json:"available"`
	Source           string `json:"source"`
	InstallSupported bool   `json:"install_supported"`
	Platform         string `json:"platform"`
	Error            string `json:"error,omitempty"`
}

type component struct{ path, source string }
type installation struct {
	done chan struct{}
	err  error
}

type identityKey struct{ region, uid string }
type identity struct {
	headers http.Header
	expires time.Time
}

// Manager stores downloaded components below the gateway's persistent data directory.
// One Manager should be shared by all Qoder accounts.
type Manager struct {
	dataDir          string
	client           *http.Client
	goos, arch, home string
	processTimeout   time.Duration
	mu               sync.Mutex
	installing       *installation
	lastError        string
	cache            map[identityKey]identity
	runGate          chan struct{}
}

// NewManager creates a component manager using the persistent gateway data directory.
func NewManager(dataDir string, client *http.Client) *Manager {
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	home, _ := os.UserHomeDir()
	return &Manager{dataDir: dataDir, client: client, goos: runtime.GOOS, arch: runtime.GOARCH, home: home, processTimeout: 25 * time.Second, cache: make(map[identityKey]identity), runGate: make(chan struct{}, 1)}
}

func (m *Manager) supported() bool {
	return m.goos == "linux" && (m.arch == "amd64" || m.arch == "arm64")
}
func (m *Manager) installedPath() string {
	return filepath.Join(m.dataDir, "qoder-umid", "runtime-info")
}

// Status reports safe operational fields; it never includes component paths or identity values.
func (m *Manager) Status() Status {
	c := m.discover("global")
	m.mu.Lock()
	defer m.mu.Unlock()
	return Status{Available: c.path != "", Source: c.source, InstallSupported: m.supported(), Platform: m.goos + "/" + m.arch, Error: m.lastError}
}

// Ensure reuses an existing official component or downloads one on supported Linux hosts.
// Concurrent callers share installation and can independently cancel their wait.
func (m *Manager) Ensure(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.discover("global").path != "" {
		return nil
	}
	m.mu.Lock()
	if pending := m.installing; pending != nil {
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-pending.done:
		}
		return pending.err
	}
	if m.discover("global").path != "" {
		m.mu.Unlock()
		return nil
	}
	pending := &installation{done: make(chan struct{})}
	m.installing = pending
	m.mu.Unlock()
	err := m.install(ctx)
	m.mu.Lock()
	pending.err = err
	m.lastError = ""
	if err != nil {
		m.lastError = err.Error()
	}
	m.installing = nil
	close(pending.done)
	m.mu.Unlock()
	return err
}

// Install explicitly retries a missing component; a working installation is preserved.
func (m *Manager) Install(ctx context.Context) error { return m.Ensure(ctx) }

// Headers returns the official component's three machine identity headers.
// China-region requests do not require a component. Identity caching is isolated by region and UID.
func (m *Manager) Headers(ctx context.Context, region, uid string) (http.Header, error) {
	region = strings.ToLower(strings.TrimSpace(region))
	if region == "cn" || region == "china" {
		return make(http.Header), nil
	}
	if region != "global" {
		return nil, errors.New("unsupported Qoder region")
	}
	if strings.TrimSpace(uid) == "" {
		return nil, errors.New("qoder UID is required for UMID")
	}
	if err := m.Ensure(ctx); err != nil {
		return nil, err
	}
	key := identityKey{region, uid}
	select {
	case m.runGate <- struct{}{}:
		defer func() { <-m.runGate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if cached, ok := m.cache[key]; ok && time.Now().Before(cached.expires) {
		m.mu.Unlock()
		return cached.headers.Clone(), nil
	}
	m.mu.Unlock()
	headers, err := m.run(ctx, m.discover(region), uid)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	now := time.Now()
	for k, v := range m.cache {
		if !now.Before(v.expires) {
			delete(m.cache, k)
		}
	}
	if len(m.cache) >= 128 {
		for k := range m.cache {
			delete(m.cache, k)
			break
		}
	}
	m.cache[key] = identity{headers: headers.Clone(), expires: now.Add(identityTTL)}
	m.mu.Unlock()
	return headers, nil
}

// cappedWriter enforces the limit while os/exec reads the child's pipe. Returning an
// error also cancels the child, rather than collecting an unlimited output buffer.
type cappedWriter struct {
	buffer   bytes.Buffer
	cancel   context.CancelFunc
	exceeded bool
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if len(p) > maxOutput-w.buffer.Len() {
		w.exceeded = true
		w.cancel()
		return 0, errors.New("UMID component output exceeds limit")
	}
	return w.buffer.Write(p)
}

func (m *Manager) run(parent context.Context, c component, uid string) (http.Header, error) {
	if c.path == "" {
		return nil, errors.New("qoder UMID component is unavailable")
	}
	ctx, cancel := context.WithTimeout(parent, m.processTimeout)
	defer cancel()
	env := "4"
	if c.source == "app" {
		env = "3"
	}
	args := []string{env}
	if m.goos == "darwin" || m.goos == "windows" {
		args = append(args, "--account-stdin")
	}
	cmd := exec.CommandContext(ctx, c.path, args...)
	if len(args) > 1 {
		payload, _ := json.Marshal(map[string]string{"account": uid})
		cmd.Stdin = bytes.NewReader(append(payload, '\n'))
	}
	output := &cappedWriter{cancel: cancel}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	// Also bound pipe draining if a component starts descendants that retain stdout.
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if output.exceeded {
		return nil, errors.New("UMID component output exceeds limit")
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("UMID component canceled: %w", ctx.Err())
	}
	if err != nil {
		return nil, errors.New("UMID component execution failed")
	}
	first, _, _ := bytes.Cut(output.buffer.Bytes(), []byte{'\n'})
	var value map[string]json.RawMessage
	if json.Unmarshal(first, &value) != nil {
		return nil, errors.New("UMID component returned invalid JSON")
	}
	headers := make(http.Header)
	for _, field := range []struct{ key, header string }{{"machineToken", "Cosy-MachineToken"}, {"machineCode", "Cosy-MachineCode"}, {"machineType", "Cosy-MachineType"}} {
		var s string
		if json.Unmarshal(value[field.key], &s) != nil || strings.TrimSpace(s) == "" || len(s) > 4096 {
			return nil, errors.New("UMID component returned an invalid machine identity")
		}
		for _, ch := range []byte(s) {
			if ch < 32 || ch > 126 {
				return nil, errors.New("UMID component returned an invalid machine identity")
			}
		}
		headers.Set(field.header, s)
	}
	return headers, nil
}

func (m *Manager) discover(region string) component {
	if m.dataDir != "" && regularExecutable(m.installedPath(), m.goos) {
		return component{m.installedPath(), "data"}
	}
	variants := []string{"Qoder", "Qoder CN"}
	if region == "cn" {
		variants[0], variants[1] = variants[1], variants[0]
	}
	exe := "runtime-info"
	if m.goos == "windows" {
		exe += ".exe"
	}
	for _, name := range variants {
		var resources []string
		switch m.goos {
		case "darwin":
			for _, root := range []string{"/Applications", filepath.Join(m.home, "Applications")} {
				resources = append(resources, filepath.Join(root, name+".app", "Contents", "Resources"))
			}
		case "windows":
			for _, root := range []string{filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs"), os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)")} {
				if root == "" || root == "Programs" {
					continue
				}
				install := filepath.Join(root, name)
				versions, _ := filepath.Glob(filepath.Join(install, ".qoder-versions", "*", "resources"))
				for i := len(versions) - 1; i >= 0; i-- {
					resources = append(resources, versions[i])
				}
				resources = append(resources, filepath.Join(install, "resources"))
			}
		case "linux":
			slug := strings.ReplaceAll(strings.ToLower(name), " ", "-")
			for _, root := range []string{filepath.Join("/opt", name), filepath.Join("/opt", slug), filepath.Join("/usr/lib", slug), filepath.Join("/snap", slug, "current")} {
				resources = append(resources, filepath.Join(root, "resources"))
			}
		}
		for _, dir := range resources {
			candidate := filepath.Join(dir, "umid", exe)
			if regularExecutable(candidate, m.goos) {
				return component{candidate, "app"}
			}
		}
	}
	for _, dir := range []string{".qoder", ".qoder-cn"} {
		matches, _ := filepath.Glob(filepath.Join(m.home, dir, ".bin", "umid-*", exe))
		for i := len(matches) - 1; i >= 0; i-- {
			if regularExecutable(matches[i], m.goos) {
				return component{matches[i], "cli"}
			}
		}
	}
	return component{}
}

func regularExecutable(path, goos string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && (goos == "windows" || info.Mode().Perm()&0111 != 0)
}
