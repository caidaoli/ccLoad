package cursorauth

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	cursorbridge "ccLoad/third_party/cursor-sdk-bridge"
)

func TestEmbeddedBridgeManifestMatchesPinnedVersion(t *testing.T) {
	t.Parallel()

	manifest, err := parseBridgeReleaseManifest(cursorbridge.LockFile())
	if err != nil {
		t.Fatalf("parseBridgeReleaseManifest() error = %v", err)
	}
	if manifest.version != BridgeVersion {
		t.Fatalf("manifest version = %q, want %q", manifest.version, BridgeVersion)
	}
}

func TestBridgeInstallerInstallsVerifiedArchiveOnce(t *testing.T) {
	t.Parallel()

	spec, err := bridgeArchiveForPlatform("darwin", "arm64")
	if err != nil {
		t.Fatalf("bridgeArchiveForPlatform() error = %v", err)
	}
	binary := []byte("#!/bin/sh\necho bridge-ready\n")
	archive := testBridgeArchive(t, spec.entryName, binary)
	digest := sha256.Sum256(archive)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		wantPath := "/" + BridgeVersion + "/" + spec.archiveName
		if request.URL.Path != wantPath {
			t.Errorf("request path = %q, want %q", request.URL.Path, wantPath)
		}
		_, _ = w.Write(archive)
	}))
	defer server.Close()

	stateRoot := filepath.Join(t.TempDir(), "cursor-sdk")
	installer := bridgeInstaller{
		goos:       "darwin",
		goarch:     "arm64",
		stateRoot:  stateRoot,
		installDir: filepath.Join(stateRoot, "bin", BridgeVersion),
		baseURL:    server.URL,
		client:     server.Client(),
		manifest: bridgeReleaseManifest{
			version: BridgeVersion,
			hashes:  map[string]string{spec.hashKey: hex.EncodeToString(digest[:])},
		},
		license: []byte("test license\n"),
	}
	path, err := installer.ensure(context.Background(), false)
	if err != nil {
		t.Fatalf("ensure() error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read installed bridge: %v", err)
	}
	if !bytes.Equal(got, binary) {
		t.Fatalf("installed bridge = %q, want %q", got, binary)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat installed bridge: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("installed bridge mode = %v, want executable", info.Mode())
	}
	rootInfo, err := os.Stat(stateRoot)
	if err != nil {
		t.Fatalf("stat state root: %v", err)
	}
	if rootInfo.Mode().Perm() != 0o700 {
		t.Fatalf("state root mode = %v, want 0700", rootInfo.Mode())
	}
	license, err := os.ReadFile(filepath.Join(installer.installDir, "LICENSE"))
	if err != nil || string(license) != "test license\n" {
		t.Fatalf("installed license = %q, err = %v", license, err)
	}

	secondPath, err := installer.ensure(context.Background(), false)
	if err != nil || secondPath != path {
		t.Fatalf("second ensure() = (%q, %v), want (%q, nil)", secondPath, err, path)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("download requests = %d, want 1", got)
	}
}

func TestBridgeInstallerRejectsChecksumWithoutPublishingFiles(t *testing.T) {
	t.Parallel()

	spec, err := bridgeArchiveForPlatform("linux", "amd64")
	if err != nil {
		t.Fatalf("bridgeArchiveForPlatform() error = %v", err)
	}
	archive := testBridgeArchive(t, spec.entryName, []byte("not trusted"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	}))
	defer server.Close()
	installDir := t.TempDir()
	installer := bridgeInstaller{
		goos:       "linux",
		goarch:     "amd64",
		installDir: installDir,
		baseURL:    server.URL,
		client:     server.Client(),
		manifest: bridgeReleaseManifest{
			version: BridgeVersion,
			hashes:  map[string]string{spec.hashKey: string(make([]byte, 64))},
		},
		license: []byte("test license\n"),
	}
	if _, err := installer.ensure(context.Background(), false); err == nil {
		t.Fatal("ensure() accepted an archive with the wrong checksum")
	}
	entries, err := os.ReadDir(installDir)
	if err != nil {
		t.Fatalf("read install directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("checksum failure published files: %v", entries)
	}
}

func TestBridgeInstallerForceReplacesBrokenManagedBinary(t *testing.T) {
	t.Parallel()

	spec, err := bridgeArchiveForPlatform("windows", "amd64")
	if err != nil {
		t.Fatalf("bridgeArchiveForPlatform() error = %v", err)
	}
	binary := []byte("replacement bridge")
	archive := testBridgeArchive(t, spec.entryName, binary)
	digest := sha256.Sum256(archive)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	}))
	defer server.Close()
	stateRoot := filepath.Join(t.TempDir(), "cursor-sdk")
	installDir := filepath.Join(stateRoot, "bin", BridgeVersion)
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatalf("create install directory: %v", err)
	}
	target := filepath.Join(installDir, spec.binaryName)
	if err := os.WriteFile(target, []byte("broken bridge"), 0o755); err != nil {
		t.Fatalf("write broken bridge: %v", err)
	}
	installer := bridgeInstaller{
		goos:       "windows",
		goarch:     "amd64",
		stateRoot:  stateRoot,
		installDir: installDir,
		baseURL:    server.URL,
		client:     server.Client(),
		manifest: bridgeReleaseManifest{
			version: BridgeVersion,
			hashes:  map[string]string{spec.hashKey: hex.EncodeToString(digest[:])},
		},
		license: []byte("test license\n"),
	}
	path, err := installer.ensure(context.Background(), true)
	if err != nil {
		t.Fatalf("force ensure() error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, binary) {
		t.Fatalf("replacement bridge = %q, err = %v, want %q", got, err, binary)
	}
}

func TestEnsureBridgeInStateRootsFallsBackAfterExecutableStartFailure(t *testing.T) {
	t.Parallel()

	cacheRoot := filepath.Join("cache", "cursor-sdk")
	temporaryRoot := filepath.Join("tmp", "ccload", "cursor-sdk")
	cachePath := filepath.Join(cacheRoot, "bin", BridgeVersion, "cursor-sdk-bridge")
	var installs []string
	path, err := ensureBridgeInStateRoots(
		context.Background(),
		[]bridgeInstallLocation{
			{stateRoot: cacheRoot, force: true},
			{stateRoot: temporaryRoot},
		},
		func(_ context.Context, stateRoot string, force bool) (string, error) {
			installs = append(installs, stateRoot)
			if stateRoot == temporaryRoot && force {
				t.Fatal("existing temporary fallback was needlessly replaced")
			}
			return filepath.Join(stateRoot, "bin", BridgeVersion, "cursor-sdk-bridge"), nil
		},
		func(_ context.Context, path string) error {
			if path == cachePath {
				startErr := exec.Command(path).Start()
				if startErr == nil {
					t.Fatalf("starting missing bridge %q unexpectedly succeeded", path)
				}
				return fmt.Errorf("%w: start %s: %w", ErrAgentMissing, path, startErr)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("ensureBridgeInStateRoots() error = %v", err)
	}
	wantPath := filepath.Join(temporaryRoot, "bin", BridgeVersion, "cursor-sdk-bridge")
	if path != wantPath {
		t.Fatalf("ensureBridgeInStateRoots() path = %q, want %q", path, wantPath)
	}
	if !slices.Equal(installs, []string{cacheRoot, temporaryRoot}) {
		t.Fatalf("install roots = %v, want cache then temporary fallback", installs)
	}
}

func TestEnsureBridgeInStateRootsDoesNotMaskNonExecutableErrors(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("bridge protocol mismatch")
	var installs int
	_, err := ensureBridgeInStateRoots(
		context.Background(),
		[]bridgeInstallLocation{{stateRoot: "cache"}, {stateRoot: "tmp"}},
		func(_ context.Context, stateRoot string, _ bool) (string, error) {
			installs++
			return filepath.Join(stateRoot, "cursor-sdk-bridge"), nil
		},
		func(context.Context, string) error { return wantErr },
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("ensureBridgeInStateRoots() error = %v, want %v", err, wantErr)
	}
	if installs != 1 {
		t.Fatalf("install attempts = %d, want 1", installs)
	}
}

func TestEnsureBridgeInStateRootsFallsBackAfterPersistentInstallWriteFailure(t *testing.T) {
	t.Parallel()

	persistentRoot := filepath.Join("persistent", "cursor-sdk")
	temporaryRoot := filepath.Join("tmp", "ccload", "cursor-sdk")
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "create install directory",
			err:  &os.PathError{Op: "mkdir", Path: persistentRoot, Err: os.ErrPermission},
		},
		{
			name: "publish installed binary",
			err: &os.LinkError{
				Op:  "rename",
				Old: filepath.Join(persistentRoot, ".cursor-sdk-bridge.tmp"),
				New: filepath.Join(persistentRoot, "cursor-sdk-bridge"),
				Err: os.ErrPermission,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			path, err := ensureBridgeInStateRoots(
				context.Background(),
				[]bridgeInstallLocation{{stateRoot: persistentRoot}, {stateRoot: temporaryRoot}},
				func(_ context.Context, stateRoot string, _ bool) (string, error) {
					if stateRoot == persistentRoot {
						return "", test.err
					}
					return filepath.Join(stateRoot, "bin", BridgeVersion, "cursor-sdk-bridge"), nil
				},
				func(context.Context, string) error { return nil },
			)
			if err != nil {
				t.Fatalf("ensureBridgeInStateRoots() error = %v", err)
			}
			want := filepath.Join(temporaryRoot, "bin", BridgeVersion, "cursor-sdk-bridge")
			if path != want {
				t.Fatalf("ensureBridgeInStateRoots() path = %q, want %q", path, want)
			}
		})
	}
}

func TestBridgeArchiveForPlatform(t *testing.T) {
	t.Parallel()

	tests := []struct {
		goos, goarch string
		archive      string
		entry        string
		hashKey      string
		wantErr      bool
	}{
		{"darwin", "arm64", "cursor-sdk-bridge-standalone-darwin-arm64.tar.gz", "bin/cursor-sdk-bridge", "sha256_darwin_arm64", false},
		{"darwin", "amd64", "cursor-sdk-bridge-standalone-darwin-x64.tar.gz", "bin/cursor-sdk-bridge", "sha256_darwin_x64", false},
		{"linux", "amd64", "cursor-sdk-bridge-standalone-linux-x64.tar.gz", "bin/cursor-sdk-bridge", "sha256_linux_x64", false},
		{"windows", "amd64", "cursor-sdk-bridge-standalone-win32-x64.tar.gz", "bin/cursor-sdk-bridge.exe", "sha256_win32_x64", false},
		{"windows", "arm64", "", "", "", true},
		{"freebsd", "amd64", "", "", "", true},
	}
	for _, test := range tests {
		t.Run(test.goos+"/"+test.goarch, func(t *testing.T) {
			spec, err := bridgeArchiveForPlatform(test.goos, test.goarch)
			if (err != nil) != test.wantErr {
				t.Fatalf("bridgeArchiveForPlatform() error = %v, wantErr = %t", err, test.wantErr)
			}
			if test.wantErr {
				return
			}
			if spec.archiveName != test.archive || spec.entryName != test.entry || spec.hashKey != test.hashKey {
				t.Fatalf("spec = %+v, want archive=%q entry=%q hashKey=%q", spec, test.archive, test.entry, test.hashKey)
			}
		})
	}
}

func TestEnsureBridgeLivePinnedArchive(t *testing.T) {
	if os.Getenv("CURSOR_SDK_BRIDGE_INSTALL_SMOKE") != "1" {
		t.Skip("set CURSOR_SDK_BRIDGE_INSTALL_SMOKE=1 to download and run the pinned bridge")
	}
	t.Setenv("CURSOR_SDK_BRIDGE_BIN", "")
	t.Setenv("PATH", "")
	t.Setenv("SQLITE_PATH", filepath.Join(t.TempDir(), "ccload.db"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	path, err := EnsureBridge(ctx)
	if err != nil {
		t.Fatalf("EnsureBridge() error = %v", err)
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("EnsureBridge() path = %q, want absolute", path)
	}
	bridge := newBridge()
	if _, err := bridge.client(ctx); err != nil {
		t.Fatalf("start installed bridge: %v", err)
	}
	if err := bridge.close(ctx); err != nil {
		t.Fatalf("close installed bridge: %v", err)
	}
}

func testBridgeArchive(t *testing.T, entry string, content []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	gzipWriter := gzip.NewWriter(&output)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{
		Name: entry,
		Mode: 0o755,
		Size: int64(len(content)),
	}); err != nil {
		t.Fatalf("write archive header: %v", err)
	}
	if _, err := tarWriter.Write(content); err != nil {
		t.Fatalf("write archive content: %v", err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	return output.Bytes()
}
