package qoderumid

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

/**
 * @brief TestMain uses this test executable as the official-runtime fixture.
 * Every component source must request international environment 3.
 */
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && (os.Args[1] == "4" || os.Args[1] == "3") {
		if os.Args[1] != "3" {
			os.Exit(2)
		}
		switch os.Getenv("CCLOAD_UMID_FIXTURE") {
		case "account":
			var payload struct {
				Account string `json:"account"`
			}
			if json.NewDecoder(os.Stdin).Decode(&payload) != nil {
				os.Exit(2)
			}
			value, _ := json.Marshal(map[string]string{"machineToken": payload.Account, "machineCode": "code", "machineType": "4"})
			fmt.Println(string(value))
		case "block":
			if marker := os.Getenv("CCLOAD_UMID_MARKER"); marker != "" {
				_ = os.WriteFile(marker, []byte("started"), 0600)
			}
			time.Sleep(time.Hour)
		case "large":
			_, _ = os.Stdout.Write(bytes.Repeat([]byte("x"), maxOutput+1))
		case "invalid":
			fmt.Println(`{"machineToken":"secret\r\nInjected: yes","machineCode":"code","machineType":"4"}`)
		case "missing":
			fmt.Println(`{"machineToken":"secret"}`)
		case "failure":
			os.Exit(1)
		case "environment":
			value, _ := json.Marshal(map[string]string{"machineToken": os.Args[1], "machineCode": "code", "machineType": "4"})
			fmt.Println(string(value))
		default:
			fmt.Println(`{"machineToken":"token","machineCode":"code","machineType":"4"}`)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(body []byte) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body))}
}

func testManager(t *testing.T) *Manager {
	t.Helper()
	m := NewManager(t.Context(), t.TempDir(), nil)
	m.goos = "linux"
	m.arch = "amd64"
	m.home = t.TempDir()
	return m
}

func fixtureELF(arch string) []byte {
	data := make([]byte, 64)
	copy(data, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(data[16:18], 2)
	machine := uint16(62)
	if arch == "arm64" {
		machine = 183
	}
	binary.LittleEndian.PutUint16(data[18:20], machine)
	binary.LittleEndian.PutUint32(data[20:24], 1)
	binary.LittleEndian.PutUint16(data[52:54], 64)
	return data
}
func fixtureArchive(t *testing.T, name string, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
func fixtureMachO(arch string) []byte {
	data := make([]byte, 64)
	binary.LittleEndian.PutUint32(data[0:4], 0xfeedfacf)
	cpu := uint32(0x01000007)
	if arch == "arm64" {
		cpu = 0x0100000c
	}
	binary.LittleEndian.PutUint32(data[4:8], cpu)
	binary.LittleEndian.PutUint32(data[12:16], 2)
	return data
}
func fixtureBinary(goos, arch string) []byte {
	if goos == "darwin" {
		return fixtureMachO(arch)
	}
	return fixtureELF(arch)
}

// fixtureBundle embeds payloads the way qodercli.js does: one base64 string literal each.
func fixtureBundle(t *testing.T, binaries ...[]byte) []byte {
	t.Helper()
	var bundle strings.Builder
	for i, data := range binaries {
		fmt.Fprintf(&bundle, "const fixture%d=%q;", i, base64.StdEncoding.EncodeToString(data))
	}
	return fixtureArchive(t, bundleEntry, []byte(bundle.String()))
}
func fixturePackage(t *testing.T, arch string) ([]byte, []byte) {
	t.Helper()
	data := fixtureELF(arch)
	return fixtureBundle(t, data), data
}
func packageClient(archive []byte, integrity string, calls *atomic.Int32) *http.Client {
	return &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if req.URL.String() == metadataURL {
			body, _ := json.Marshal(map[string]any{"version": "test", "dist": map[string]string{"tarball": "https://registry.npmjs.org/@qoder-ai/qodercli/-/qodercli-test.tgz", "integrity": integrity}})
			return response(body), nil
		}
		return response(archive), nil
	})}
}
func checksum(data []byte) string {
	sum := sha512.Sum512(data)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

func TestEnsureInstallsOfficialComponentConcurrently(t *testing.T) {
	platforms := [][2]string{{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "amd64"}, {"darwin", "arm64"}}
	var binaries [][]byte
	for _, platform := range platforms {
		binaries = append(binaries, fixtureBinary(platform[0], platform[1]))
	}
	// Like the official bundle, one package carries every platform's payload.
	archive := fixtureBundle(t, binaries...)
	for i, platform := range platforms {
		t.Run(platform[0]+"/"+platform[1], func(t *testing.T) {
			binary := binaries[i]
			var calls atomic.Int32
			m := testManager(t)
			m.goos, m.arch = platform[0], platform[1]
			m.client = packageClient(archive, checksum(archive), &calls)
			var wg sync.WaitGroup
			for range 20 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if err := m.Ensure(context.Background()); err != nil {
						t.Error(err)
					}
				}()
			}
			wg.Wait()
			if calls.Load() != 2 {
				t.Fatalf("requests=%d, want metadata+archive", calls.Load())
			}
			installed, err := os.ReadFile(m.installedPath())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(installed, binary) {
				t.Fatal("installed bytes differ")
			}
			status := m.Status()
			if !status.Available || status.Source != "data" || !status.InstallSupported {
				t.Fatalf("status=%+v", status)
			}
			m.Install()
			if m.Status().Installing || calls.Load() != 2 {
				t.Fatal("existing component was downloaded again")
			}
			entries, err := os.ReadDir(filepath.Dir(m.installedPath()))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Fatal("staging file was leaked")
			}
		})
	}
}

func TestEnsureRejectsUntrustedOrInvalidPackage(t *testing.T) {
	good, _ := fixturePackage(t, "amd64")
	wrongArch, _ := fixturePackage(t, "arm64")
	wrongOS := fixtureBundle(t, fixtureMachO("amd64"))
	unsafe := fixtureArchive(t, "../runtime-info", []byte("x"))
	for _, tt := range []struct {
		name      string
		archive   []byte
		integrity string
	}{
		{"missing integrity", good, ""}, {"unsupported digest", good, "sha1-abc"}, {"malformed digest", good, "sha512-bad"},
		{"checksum mismatch", good, checksum([]byte("changed"))}, {"wrong architecture", wrongArch, checksum(wrongArch)},
		{"wrong operating system", wrongOS, checksum(wrongOS)},
		{"path traversal", unsafe, checksum(unsafe)}, {"invalid archive", []byte("x"), checksum([]byte("x"))},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			m := testManager(t)
			m.client = packageClient(tt.archive, tt.integrity, &calls)
			if err := m.Ensure(context.Background()); err == nil {
				t.Fatal("invalid package accepted")
			}
			if m.Status().Available {
				t.Fatal("invalid component published")
			}
			if _, err := os.Stat(m.installedPath()); !os.IsNotExist(err) {
				t.Fatalf("component exists: %v", err)
			}
			encoded, _ := json.Marshal(m.Status())
			if bytes.Contains(encoded, []byte(m.dataDir)) {
				t.Fatal("status leaks data directory")
			}
		})
	}
}

func TestUnsupportedPlatformAndRegion(t *testing.T) {
	m := testManager(t)
	m.arch = "386"
	if m.Status().InstallSupported {
		t.Fatal("386 claims automatic installation")
	}
	if err := m.Ensure(context.Background()); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("error=%v", err)
	}
	for _, region := range []string{"cn", "china"} {
		headers, err := m.Headers(context.Background(), region, "uid")
		if err != nil || len(headers) != 0 {
			t.Fatalf("headers=%v error=%v", headers, err)
		}
	}
	if _, err := m.Headers(context.Background(), "unknown", "uid"); err == nil {
		t.Fatal("unknown region accepted")
	}
}

func TestInstallCancellationAndRetry(t *testing.T) {
	archive, _ := fixturePackage(t, "amd64")
	var calls atomic.Int32
	m := testManager(t)
	lifetime, shutdown := context.WithCancel(context.Background())
	defer shutdown()
	m.ctx = lifetime
	started := make(chan struct{})
	m.client = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- m.Ensure(ctx) }()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("wait error=%v", err)
	}
	if !m.Status().Installing {
		t.Fatal("canceling the only waiter aborted the installation")
	}
	shutdown()
	waitInstalled(t, m)
	if status := m.Status(); status.Error == "" || status.Available {
		t.Fatalf("shutdown status=%+v", status)
	}
	m.ctx = t.Context()
	m.client = packageClient(archive, checksum(archive), &calls)
	if err := m.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.Status().Error != "" {
		t.Fatal("successful retry retained stale error")
	}
}

func waitInstalled(t *testing.T, m *Manager) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for m.Status().Installing {
		if time.Now().After(deadline) {
			t.Fatal("installation did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

type gatedReader struct {
	gate <-chan struct{}
	io.Reader
}

func (g gatedReader) Read(p []byte) (int, error) {
	<-g.gate
	return g.Reader.Read(p)
}

func TestStatusReportsInstallProgress(t *testing.T) {
	archive, _ := fixturePackage(t, "amd64")
	half := len(archive) / 2
	gate := make(chan struct{})
	var calls atomic.Int32
	metadata := packageClient(archive, checksum(archive), &calls)
	m := testManager(t)
	m.client = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == metadataURL {
			return metadata.Transport.RoundTrip(r)
		}
		body := io.MultiReader(bytes.NewReader(archive[:half]), gatedReader{gate, bytes.NewReader(archive[half:])})
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(body), ContentLength: int64(len(archive))}, nil
	})}
	if status := m.Status(); status.Installing || status.Progress != nil {
		t.Fatalf("idle status=%+v", status)
	}
	m.Install()
	deadline := time.Now().Add(5 * time.Second)
	for {
		status := m.Status()
		if p := status.Progress; status.Installing && p != nil && p.Phase == PhaseDownload && p.Downloaded == int64(half) {
			if p.Total != int64(len(archive)) || status.Available {
				t.Fatalf("download status=%+v progress=%+v", status, *p)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("download progress not reported: %+v", status)
		}
		time.Sleep(time.Millisecond)
	}
	close(gate)
	waitInstalled(t, m)
	if status := m.Status(); !status.Available || status.Installing || status.Progress != nil {
		t.Fatalf("installed status=%+v", status)
	}
}

func fakeComponent(t *testing.T, m *Manager) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Dir(m.installedPath()), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Link(exe, m.installedPath()); err != nil {
		t.Fatal(err)
	}
}

func TestHeadersCacheAndIdentityValidation(t *testing.T) {
	m := testManager(t)
	fakeComponent(t, m)
	t.Setenv("CCLOAD_UMID_FIXTURE", "valid")
	headers, err := m.Headers(context.Background(), "global", "first")
	if err != nil {
		t.Fatal(err)
	}
	if headers.Get("Cosy-MachineToken") != "token" || headers.Get("Cosy-MachineCode") != "code" || headers.Get("Cosy-MachineType") != "4" {
		t.Fatalf("headers=%v", headers)
	}
	headers.Set("Cosy-MachineToken", "mutated")
	t.Setenv("CCLOAD_UMID_FIXTURE", "invalid")
	cached, err := m.Headers(context.Background(), "global", "first")
	if err != nil || cached.Get("Cosy-MachineToken") != "token" {
		t.Fatalf("cache=%v error=%v", cached, err)
	}
	if _, err = m.Headers(context.Background(), "global", "second"); err == nil {
		t.Fatal("different UID reused cached identity or accepted header injection")
	}
	for _, mode := range []string{"large", "missing", "failure"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("CCLOAD_UMID_FIXTURE", mode)
			if _, err := m.Headers(context.Background(), "global", mode); err == nil {
				t.Fatal("invalid component output accepted")
			}
		})
	}
}

func TestComponentCancellationAndTimeout(t *testing.T) {
	for _, mode := range []string{"cancel", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			m := testManager(t)
			fakeComponent(t, m)
			t.Setenv("CCLOAD_UMID_FIXTURE", "block")
			marker := filepath.Join(t.TempDir(), "started")
			t.Setenv("CCLOAD_UMID_MARKER", marker)
			if mode == "timeout" {
				m.processTimeout = 100 * time.Millisecond
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started := time.Now()
			result := make(chan error, 1)
			go func() { _, err := m.Headers(ctx, "global", "uid"); result <- err }()
			if mode == "cancel" {
				deadline := time.After(3 * time.Second)
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
			wait:
				for {
					select {
					case <-deadline:
						t.Fatal("fixture did not start")
					case <-ticker.C:
						if _, err := os.Stat(marker); err == nil {
							break wait
						}
					}
				}
				cancel()
			}
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("blocking child was not canceled")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("component cancellation did not kill child")
			}
			if time.Since(started) > 2*time.Second {
				t.Fatal("component cancellation took too long")
			}
		})
	}
}

func TestDownloadURLAndSizeContracts(t *testing.T) {
	for _, raw := range []string{"http://registry.npmjs.org/@qoder-ai/qodercli/latest", "https://evil.example/@qoder-ai/qodercli/latest", "https://registry.npmjs.org/@qoder-ai/other/-/package.tgz", "https://registry.npmjs.org/@qoder-ai/qodercli/latest?token=secret"} {
		m := testManager(t)
		if _, err := m.download(context.Background(), raw, 10, nil); err == nil {
			t.Fatalf("URL accepted: %s", raw)
		}
	}
	m := testManager(t)
	m.client = &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		r := response([]byte("elevenbytes"))
		r.ContentLength = -1
		return r, nil
	})}
	if _, err := m.download(context.Background(), metadataURL, 10, nil); err == nil {
		t.Fatal("oversized streaming response accepted")
	}
	m.client = &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		r := response(nil)
		r.StatusCode = 302
		r.Header.Set("Location", "https://evil.example/package.tgz")
		return r, nil
	})}
	if _, err := m.download(context.Background(), metadataURL, 10, nil); err == nil {
		t.Fatal("external registry redirect accepted")
	}
}

func TestWaitingInstallerCancellationDoesNotCancelLeader(t *testing.T) {
	archive, _ := fixturePackage(t, "amd64")
	var calls atomic.Int32
	base := packageClient(archive, checksum(archive), &calls)
	started := make(chan struct{})
	release := make(chan struct{})
	m := testManager(t)
	m.client = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == metadataURL {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		return base.Transport.RoundTrip(r)
	})}
	leader := make(chan error, 1)
	go func() { leader <- m.Ensure(context.Background()) }()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := m.Ensure(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting installer error=%v", err)
	}
	close(release)
	if err := <-leader; err != nil {
		t.Fatal(err)
	}
	if !m.Status().Available || calls.Load() != 2 {
		t.Fatal("waiting caller disrupted shared installation")
	}
}

/**
 * @brief TestDiscoversOfficialClientAndCLI verifies installed runtime discovery and execution.
 */
func TestDiscoversOfficialClientAndCLI(t *testing.T) {
	for _, key := range []string{"LOCALAPPDATA", "ProgramFiles", "PROGRAMFILES", "ProgramW6432", "ProgramFiles(x86)", "QODER_CONFIG_DIR", "QODER_CLI_HOME", "QODER_CONFIG_DIR_NAME", "QODERCN_CONFIG_DIR", "QODERCN_CLI_HOME", "QODERCN_CONFIG_DIR_NAME"} {
		t.Setenv(key, "")
	}
	for _, tc := range []struct {
		name, goos, arch, source, path, env string
	}{
		{"desktop", "darwin", "arm64", "app", "Applications/Qoder.app/Contents/Resources/umid/runtime-info", ""},
		{"CLI directory", "darwin", "arm64", "cli", ".qoder/.bin/umid-darwin-arm64-fixture/runtime-info", ""},
		{"CLI file", "darwin", "arm64", "cli", ".qoder/.bin/runtime-info-darwin-arm64-fixture", ""},
		{"Linux x64", "linux", "amd64", "cli", ".qoder/.bin/runtime-info-linux-x64-fixture", ""},
		{"Windows user desktop", "windows", "amd64", "app", "AppData/Local/Programs/Qoder/resources/umid/runtime-info.exe", ""},
		{"Windows system desktop", "windows", "amd64", "app", "system/Qoder/resources/umid/runtime-info.exe", "ProgramW6432"},
		{"Windows CLI directory", "windows", "amd64", "cli", ".qoder/.bin/umid-win32-x64-fixture/runtime-info.exe", ""},
		{"Windows CLI file", "windows", "amd64", "cli", ".qoder/.bin/runtime-info-win32-x64-fixture.exe", ""},
		{"CLI config directory", "linux", "amd64", "cli", "config/.bin/runtime-info-linux-x64-fixture", "QODER_CONFIG_DIR"},
		{"CN CLI config directory", "linux", "amd64", "cli", "config/.bin/runtime-info-linux-x64-fixture", "QODERCN_CONFIG_DIR"},
		{"CLI home and name", "linux", "amd64", "cli", "cli-home/custom-name/.bin/runtime-info-linux-x64-fixture", "QODER_CLI_HOME"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := testManager(t)
			m.goos, m.arch = tc.goos, tc.arch
			target := filepath.Join(m.home, filepath.FromSlash(tc.path))
			switch tc.env {
			case "ProgramW6432":
				t.Setenv(tc.env, filepath.Join(m.home, "system"))
			case "QODER_CONFIG_DIR", "QODERCN_CONFIG_DIR":
				t.Setenv(tc.env, filepath.Join(m.home, "config"))
			case "QODER_CLI_HOME":
				t.Setenv(tc.env, filepath.Join(m.home, "cli-home"))
				t.Setenv("QODER_CONFIG_DIR_NAME", "custom-name")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				t.Fatal(err)
			}
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			if err = os.Link(exe, target); err != nil {
				t.Fatal(err)
			}
			status := m.Status()
			if !status.Available || status.Source != tc.source || status.InstallSupported != (tc.goos != "windows") {
				t.Fatalf("status=%+v", status)
			}
			mode, token := "account", "fixture-account"
			if tc.goos == "linux" {
				mode, token = "environment", "3"
			}
			t.Setenv("CCLOAD_UMID_FIXTURE", mode)
			headers, err := m.Headers(context.Background(), "global", "fixture-account")
			if err != nil {
				t.Fatal(err)
			}
			if headers.Get("Cosy-MachineToken") != token {
				t.Fatalf("account stdin was not passed: %v", headers)
			}
		})
	}
}

/**
 * @brief TestCLIRuntimeSelection rejects foreign architecture and incomplete cache files.
 */
func TestCLIRuntimeSelection(t *testing.T) {
	for _, key := range []string{"QODER_CONFIG_DIR", "QODER_CLI_HOME", "QODER_CONFIG_DIR_NAME"} {
		t.Setenv(key, "")
	}
	m := testManager(t)
	cache := filepath.Join(m.home, ".qoder", ".bin")
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"runtime-info-linux-arm64-new", "runtime-info-linux-x64-new.tmp", "runtime-info-linux-x64-zz-old"} {
		path := filepath.Join(cache, name)
		if err := os.WriteFile(path, []byte("invalid executable"), 0700); err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(name, "old") {
			past := time.Now().Add(-time.Hour)
			if err := os.Chtimes(path, past, past); err != nil {
				t.Fatal(err)
			}
		}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Link(exe, filepath.Join(cache, "runtime-info-linux-x64-current")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCLOAD_UMID_FIXTURE", "environment")
	headers, err := m.Headers(t.Context(), "global", "user")
	if err != nil || headers.Get("Cosy-MachineToken") != "3" {
		t.Fatalf("runtime selection or environment mismatch: %v %v", headers, err)
	}
}
