package cursorauth

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	cursorbridge "ccLoad/third_party/cursor-sdk-bridge"
)

const (
	bridgeReleaseBaseURL = "https://github.com/cursor/sdk-bridge/releases/download"
	maxBridgeArchiveSize = 128 << 20
	maxBridgeBinarySize  = 512 << 20
)

type bridgeReleaseManifest struct {
	version string
	hashes  map[string]string
}

type bridgeArchiveSpec struct {
	archiveName string
	entryName   string
	binaryName  string
	hashKey     string
}

type bridgeInstaller struct {
	goos       string
	goarch     string
	stateRoot  string
	installDir string
	baseURL    string
	client     *http.Client
	manifest   bridgeReleaseManifest
	license    []byte
}

var bridgeInstallMu sync.Mutex

// EnsureBridge returns an existing bridge or installs the pinned release into
// ccLoad's persistent data directory. Explicit operator overrides remain
// authoritative: an invalid CURSOR_SDK_BRIDGE_BIN is reported, not bypassed.
func EnsureBridge(ctx context.Context) (string, error) {
	bridgeInstallMu.Lock()
	defer bridgeInstallMu.Unlock()

	bridge := newBridge()
	defer bridge.lifeStop()
	path, locateErr := bridge.bridgePath()
	forceInstall := false
	temporaryRoot := temporaryBridgeStateRoot()
	temporaryPath := managedBridgeBinaryPathAt(temporaryRoot, runtime.GOOS)
	temporaryBroken := false
	if locateErr == nil {
		if probeErr := probeBridgeBinary(ctx, path); probeErr == nil {
			return path, nil
		} else if strings.TrimSpace(os.Getenv("CURSOR_SDK_BRIDGE_BIN")) != "" {
			return "", fmt.Errorf("validate CURSOR_SDK_BRIDGE_BIN %q: %w", path, probeErr)
		}
		managed := managedBridgeBinaryPath(runtime.GOOS)
		if managed != path && isUsableBridgeFile(managed, runtime.GOOS) {
			if probeErr := probeBridgeBinary(ctx, managed); probeErr == nil {
				return managed, nil
			}
		}
		forceInstall = true
	} else if strings.TrimSpace(os.Getenv("CURSOR_SDK_BRIDGE_BIN")) != "" {
		return "", locateErr
	}
	if temporaryPath != path && isUsableBridgeFile(temporaryPath, runtime.GOOS) {
		if probeErr := probeBridgeBinary(ctx, temporaryPath); probeErr == nil {
			return temporaryPath, nil
		}
		temporaryBroken = true
	}

	manifest, manifestErr := parseBridgeReleaseManifest(cursorbridge.LockFile())
	if manifestErr != nil {
		return "", fmt.Errorf("load cursor-sdk-bridge release lock: %w", manifestErr)
	}
	locations := []bridgeInstallLocation{{stateRoot: bridgeStateRoot(), force: forceInstall}}
	if locations[0].stateRoot != temporaryRoot {
		locations = append(locations, bridgeInstallLocation{stateRoot: temporaryRoot, force: temporaryBroken})
	}
	client := &http.Client{}
	install := func(ctx context.Context, stateRoot string, force bool) (string, error) {
		installer := bridgeInstaller{
			goos:       runtime.GOOS,
			goarch:     runtime.GOARCH,
			stateRoot:  stateRoot,
			installDir: filepath.Join(stateRoot, "bin", manifest.version),
			baseURL:    bridgeReleaseBaseURL,
			client:     client,
			manifest:   manifest,
			license:    []byte(cursorbridge.LicenseFile()),
		}
		return installer.ensure(ctx, force)
	}
	return ensureBridgeInStateRoots(ctx, locations, install, probeBridgeBinary)
}

type bridgeInstallLocation struct {
	stateRoot string
	force     bool
}

func ensureBridgeInStateRoots(
	ctx context.Context,
	locations []bridgeInstallLocation,
	install func(context.Context, string, bool) (string, error),
	probe func(context.Context, string) error,
) (string, error) {
	if len(locations) == 0 {
		return "", errors.New("cursor-sdk-bridge install state root is unavailable")
	}
	var previousErr error
	for index, location := range locations {
		path, err := install(ctx, location.stateRoot, location.force)
		if err != nil {
			installErr := fmt.Errorf("install cursor-sdk-bridge in %q: %w", location.stateRoot, err)
			if index+1 < len(locations) && isBridgeStorageError(err) {
				previousErr = errors.Join(previousErr, installErr)
				continue
			}
			return "", errors.Join(previousErr, installErr)
		}
		if err := probe(ctx, path); err == nil {
			return path, nil
		} else {
			validationErr := fmt.Errorf("validate installed cursor-sdk-bridge %q: %w", path, err)
			if index+1 >= len(locations) || !isBridgeExecutablePathError(err) {
				return "", errors.Join(previousErr, validationErr)
			}
			previousErr = errors.Join(previousErr, validationErr)
		}
	}
	return "", previousErr
}

func isBridgeStorageError(err error) bool {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return true
	}
	var linkErr *os.LinkError
	return errors.As(err, &linkErr)
}

func isBridgeExecutablePathError(err error) bool {
	if !errors.Is(err, ErrAgentMissing) {
		return false
	}
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) {
		return false
	}
	return errors.Is(pathErr.Err, os.ErrNotExist) || errors.Is(pathErr.Err, os.ErrPermission)
}

func (i bridgeInstaller) ensure(ctx context.Context, force bool) (string, error) {
	spec, err := bridgeArchiveForPlatform(i.goos, i.goarch)
	if err != nil {
		return "", err
	}
	target := filepath.Join(i.installDir, spec.binaryName)
	if !force && isUsableBridgeFile(target, i.goos) {
		return target, nil
	}
	if i.client == nil {
		i.client = &http.Client{}
	}
	if strings.TrimSpace(i.baseURL) == "" {
		return "", errors.New("cursor-sdk-bridge release base URL is empty")
	}
	expected := i.manifest.hashes[spec.hashKey]
	if expected == "" {
		return "", fmt.Errorf("cursor-sdk-bridge release lock is missing %s", spec.hashKey)
	}
	stateRoot := i.stateRoot
	if stateRoot == "" {
		stateRoot = i.installDir
	}
	if err := prepareBridgeStateRoot(stateRoot); err != nil {
		return "", err
	}
	if err := os.MkdirAll(i.installDir, 0o755); err != nil {
		return "", fmt.Errorf("create cursor-sdk-bridge install directory: %w", err)
	}

	archivePath, actual, err := i.downloadArchive(ctx, spec.archiveName)
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(archivePath) }()
	if actual != expected {
		return "", fmt.Errorf(
			"cursor-sdk-bridge archive checksum mismatch: got %s, want %s",
			actual,
			expected,
		)
	}
	if len(i.license) == 0 {
		return "", errors.New("cursor-sdk-bridge license is empty")
	}
	if err := atomicWriteBridgeFile(
		filepath.Join(i.installDir, "LICENSE"),
		i.license,
		0o644,
	); err != nil {
		return "", fmt.Errorf("install cursor-sdk-bridge license: %w", err)
	}
	if err := extractBridgeBinary(archivePath, spec.entryName, target, i.goos, force); err != nil {
		return "", err
	}
	return target, nil
}

func probeBridgeBinary(ctx context.Context, path string) error {
	probe := newBridge(path)
	if _, err := probe.client(ctx); err != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*BridgeShutdownGrace)
		_ = probe.close(closeCtx)
		cancel()
		return err
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 2*BridgeShutdownGrace)
	err := probe.close(closeCtx)
	cancel()
	return err
}

func (i bridgeInstaller) downloadArchive(ctx context.Context, archiveName string) (string, string, error) {
	url := strings.TrimRight(i.baseURL, "/") + "/" + i.manifest.version + "/" + archiveName
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", fmt.Errorf("create cursor-sdk-bridge download request: %w", err)
	}
	request.Header.Set("User-Agent", "ccLoad cursor-sdk-bridge installer")
	response, err := i.client.Do(request)
	if err != nil {
		return "", "", fmt.Errorf("download cursor-sdk-bridge archive: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf(
			"download cursor-sdk-bridge archive: unexpected HTTP status %d",
			response.StatusCode,
		)
	}

	temp, err := os.CreateTemp(i.installDir, ".cursor-sdk-bridge-archive-*")
	if err != nil {
		return "", "", fmt.Errorf("create cursor-sdk-bridge archive: %w", err)
	}
	path := temp.Name()
	fail := func(err error) (string, string, error) {
		_ = temp.Close()
		_ = os.Remove(path)
		return "", "", err
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(temp, hash), io.LimitReader(response.Body, maxBridgeArchiveSize+1))
	if err != nil {
		return fail(fmt.Errorf("write cursor-sdk-bridge archive: %w", err))
	}
	if written > maxBridgeArchiveSize {
		return fail(fmt.Errorf("cursor-sdk-bridge archive exceeds %d bytes", maxBridgeArchiveSize))
	}
	if err := temp.Sync(); err != nil {
		return fail(fmt.Errorf("sync cursor-sdk-bridge archive: %w", err))
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(path)
		return "", "", fmt.Errorf("close cursor-sdk-bridge archive: %w", err)
	}
	return path, hex.EncodeToString(hash.Sum(nil)), nil
}

func extractBridgeBinary(archivePath, entryName, target, goos string, replace bool) error {
	archive, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open cursor-sdk-bridge archive: %w", err)
	}
	defer func() { _ = archive.Close() }()
	gzipReader, err := gzip.NewReader(archive)
	if err != nil {
		return fmt.Errorf("open cursor-sdk-bridge gzip stream: %w", err)
	}
	defer func() { _ = gzipReader.Close() }()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, nextErr := tarReader.Next()
		if errors.Is(nextErr, io.EOF) {
			return fmt.Errorf("cursor-sdk-bridge archive is missing %s", entryName)
		}
		if nextErr != nil {
			return fmt.Errorf("read cursor-sdk-bridge archive: %w", nextErr)
		}
		if header.Name != entryName {
			continue
		}
		if !header.FileInfo().Mode().IsRegular() {
			return fmt.Errorf("cursor-sdk-bridge archive entry %s is not a regular file", entryName)
		}
		if header.Size <= 0 || header.Size > maxBridgeBinarySize {
			return fmt.Errorf("cursor-sdk-bridge binary has invalid size %d", header.Size)
		}

		temp, createErr := os.CreateTemp(filepath.Dir(target), ".cursor-sdk-bridge-*")
		if createErr != nil {
			return fmt.Errorf("create cursor-sdk-bridge binary: %w", createErr)
		}
		tempPath := temp.Name()
		fail := func(err error) error {
			_ = temp.Close()
			_ = os.Remove(tempPath)
			return err
		}
		written, copyErr := io.CopyN(temp, tarReader, header.Size)
		if copyErr != nil {
			return fail(fmt.Errorf("extract cursor-sdk-bridge binary: %w", copyErr))
		}
		if written != header.Size {
			return fail(fmt.Errorf("extract cursor-sdk-bridge binary: wrote %d bytes, want %d", written, header.Size))
		}
		if chmodErr := temp.Chmod(0o755); chmodErr != nil {
			return fail(fmt.Errorf("chmod cursor-sdk-bridge binary: %w", chmodErr))
		}
		if syncErr := temp.Sync(); syncErr != nil {
			return fail(fmt.Errorf("sync cursor-sdk-bridge binary: %w", syncErr))
		}
		if closeErr := temp.Close(); closeErr != nil {
			_ = os.Remove(tempPath)
			return fmt.Errorf("close cursor-sdk-bridge binary: %w", closeErr)
		}
		if replace && goos == "windows" {
			if removeErr := os.Remove(target); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return fail(fmt.Errorf("replace cursor-sdk-bridge binary: %w", removeErr))
			}
		}
		if renameErr := os.Rename(tempPath, target); renameErr != nil {
			_ = os.Remove(tempPath)
			if isUsableBridgeFile(target, goos) {
				return nil
			}
			return fmt.Errorf("install cursor-sdk-bridge binary: %w", renameErr)
		}
		return nil
	}
}

func atomicWriteBridgeFile(target string, content []byte, mode os.FileMode) error {
	if info, err := os.Stat(target); err == nil && info.Mode().IsRegular() {
		return nil
	}
	temp, err := os.CreateTemp(filepath.Dir(target), ".cursor-sdk-bridge-file-*")
	if err != nil {
		return err
	}
	path := temp.Name()
	fail := func(err error) error {
		_ = temp.Close()
		_ = os.Remove(path)
		return err
	}
	if _, err := temp.Write(content); err != nil {
		return fail(err)
	}
	if err := temp.Chmod(mode); err != nil {
		return fail(err)
	}
	if err := temp.Sync(); err != nil {
		return fail(err)
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	if err := os.Rename(path, target); err != nil {
		_ = os.Remove(path)
		if info, statErr := os.Stat(target); statErr == nil && info.Mode().IsRegular() {
			return nil
		}
		return err
	}
	return nil
}

func bridgeArchiveForPlatform(goos, goarch string) (bridgeArchiveSpec, error) {
	upstreamOS := goos
	upstreamArch := goarch
	binaryName := "cursor-sdk-bridge"
	entryName := "bin/cursor-sdk-bridge"
	switch goos {
	case "darwin", "linux":
	case "windows":
		upstreamOS = "win32"
		binaryName += ".exe"
		entryName += ".exe"
	default:
		return bridgeArchiveSpec{}, fmt.Errorf("unsupported cursor-sdk-bridge operating system: %s", goos)
	}
	switch goarch {
	case "amd64":
		upstreamArch = "x64"
	case "arm64":
	default:
		return bridgeArchiveSpec{}, fmt.Errorf("unsupported cursor-sdk-bridge architecture: %s", goarch)
	}
	if upstreamOS == "win32" && upstreamArch != "x64" {
		return bridgeArchiveSpec{}, fmt.Errorf("unsupported cursor-sdk-bridge platform: %s/%s", goos, goarch)
	}
	return bridgeArchiveSpec{
		archiveName: "cursor-sdk-bridge-standalone-" + upstreamOS + "-" + upstreamArch + ".tar.gz",
		entryName:   entryName,
		binaryName:  binaryName,
		hashKey:     "sha256_" + upstreamOS + "_" + upstreamArch,
	}, nil
}

func parseBridgeReleaseManifest(raw string) (bridgeReleaseManifest, error) {
	values := make(map[string]string)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !ok || key == "" || value == "" {
			return bridgeReleaseManifest{}, fmt.Errorf("invalid cursor-sdk-bridge lock line %q", line)
		}
		if _, duplicate := values[key]; duplicate {
			return bridgeReleaseManifest{}, fmt.Errorf("duplicate cursor-sdk-bridge lock key %q", key)
		}
		values[key] = value
	}
	version := values["version"]
	if version != BridgeVersion {
		return bridgeReleaseManifest{}, fmt.Errorf("bridge lock version %q does not match %q", version, BridgeVersion)
	}
	hashes := make(map[string]string)
	for key, value := range values {
		if !strings.HasPrefix(key, "sha256_") {
			continue
		}
		decoded, err := hex.DecodeString(value)
		if err != nil || len(decoded) != sha256.Size {
			return bridgeReleaseManifest{}, fmt.Errorf("invalid %s in cursor-sdk-bridge lock", key)
		}
		hashes[key] = strings.ToLower(value)
	}
	return bridgeReleaseManifest{version: version, hashes: hashes}, nil
}

func managedBridgeBinaryPath(goos string) string {
	return managedBridgeBinaryPathAt(bridgeStateRoot(), goos)
}

func managedBridgeBinaryPathAt(stateRoot, goos string) string {
	name := "cursor-sdk-bridge"
	if goos == "windows" {
		name += ".exe"
	}
	return filepath.Join(stateRoot, "bin", BridgeVersion, name)
}

func isUsableBridgeFile(path, goos string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return goos == "windows" || info.Mode().Perm()&0o111 != 0
}
