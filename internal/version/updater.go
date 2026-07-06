package version

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	updateDownloadTimeout      = 5 * time.Minute
	defaultRestartPollInterval = 10 * time.Second
)

// UpdateState exposes the current auto-update state.
type UpdateState struct {
	PendingRestart bool
	PendingVersion string
	Updating       bool
	LastCheck      time.Time
	LastError      string
}

// AutoUpdateOptions configures an AutoUpdater.
type AutoUpdateOptions struct {
	Interval            time.Duration
	RestartPollInterval time.Duration
	LatestReleaseURL    string
	ExecutablePath      string
	GOOS                string
	GOARCH              string
	Client              *http.Client
	ActiveRequests      func() int
	Restart             func()
}

// AutoUpdater downloads verified release binaries and restarts when idle.
type AutoUpdater struct {
	interval            time.Duration
	restartPollInterval time.Duration
	latestReleaseURL    string
	executablePath      string
	goos                string
	goarch              string
	client              *http.Client
	activeRequests      func() int
	restart             func()

	mu             sync.Mutex
	state          UpdateState
	waitingRestart bool
	restartCalled  bool
	wg             sync.WaitGroup
}

// NewAutoUpdater creates an updater with conservative defaults.
func NewAutoUpdater(opts AutoUpdateOptions) (*AutoUpdater, error) {
	if opts.Interval <= 0 {
		return nil, fmt.Errorf("auto update interval must be positive")
	}
	if opts.Restart == nil {
		return nil, fmt.Errorf("restart callback is required")
	}
	if opts.ActiveRequests == nil {
		opts.ActiveRequests = func() int { return 0 }
	}
	if opts.RestartPollInterval <= 0 {
		opts.RestartPollInterval = defaultRestartPollInterval
	}
	if opts.LatestReleaseURL == "" {
		opts.LatestReleaseURL = githubReleaseAPI
	}
	if opts.GOOS == "" {
		opts.GOOS = runtime.GOOS
	}
	if opts.GOARCH == "" {
		opts.GOARCH = runtime.GOARCH
	}
	if opts.Client == nil {
		opts.Client = &http.Client{}
	}
	if opts.ExecutablePath == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("resolve executable path: %w", err)
		}
		opts.ExecutablePath = exe
	}

	return &AutoUpdater{
		interval:            opts.Interval,
		restartPollInterval: opts.RestartPollInterval,
		latestReleaseURL:    opts.LatestReleaseURL,
		executablePath:      opts.ExecutablePath,
		goos:                opts.GOOS,
		goarch:              opts.GOARCH,
		client:              opts.Client,
		activeRequests:      opts.ActiveRequests,
		restart:             opts.Restart,
	}, nil
}

// Run checks immediately, then at the configured interval, until ctx is canceled.
func (u *AutoUpdater) Run(ctx context.Context) {
	defer u.wg.Wait()

	if strings.EqualFold(strings.TrimSpace(Version), "dev") || strings.TrimSpace(Version) == "" {
		log.Printf("[AutoUpdater] disabled for development version %q", Version)
		return
	}
	if _, ok := releaseAssetName(u.goos, u.goarch); !ok {
		log.Printf("[AutoUpdater] unsupported platform: %s/%s", u.goos, u.goarch)
		return
	}

	u.runCheck(ctx)

	ticker := time.NewTicker(u.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			u.runCheck(ctx)
		}
	}
}

// State returns a snapshot of the updater state.
func (u *AutoUpdater) State() UpdateState {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.state
}

func (u *AutoUpdater) runCheck(ctx context.Context) {
	if err := u.updateOnce(ctx); err != nil {
		u.mu.Lock()
		u.state.LastError = err.Error()
		u.state.LastCheck = time.Now()
		u.mu.Unlock()
		log.Printf("[AutoUpdater] update check failed: %v", err)
	}
}

func (u *AutoUpdater) updateOnce(ctx context.Context) error {
	release, err := u.fetchLatestRelease(ctx)
	if err != nil {
		return err
	}

	u.mu.Lock()
	u.state.LastCheck = time.Now()
	u.state.LastError = ""
	u.mu.Unlock()

	baseline := u.baselineVersion()
	if compareSemanticVersions(release.TagName, baseline) <= 0 {
		return nil
	}

	assetName, ok := releaseAssetName(u.goos, u.goarch)
	if !ok {
		return fmt.Errorf("unsupported platform: %s/%s", u.goos, u.goarch)
	}
	asset, ok := findReleaseAsset(release, assetName)
	if !ok {
		return fmt.Errorf("release %s missing asset %s", release.TagName, assetName)
	}
	checksumAsset, ok := findReleaseAsset(release, "checksums.txt")
	if !ok {
		return fmt.Errorf("release %s missing checksums.txt", release.TagName)
	}

	u.setUpdating(true)
	defer u.setUpdating(false)

	if err := u.downloadVerifyAndReplace(ctx, release.TagName, assetName, asset.BrowserDownloadURL, checksumAsset.BrowserDownloadURL); err != nil {
		return err
	}
	u.markPending(release.TagName)
	u.ensureRestartWaiter(ctx)
	return nil
}

func (u *AutoUpdater) fetchLatestRelease(ctx context.Context) (GitHubRelease, error) {
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u.latestReleaseURL, nil)
	if err != nil {
		return GitHubRelease{}, fmt.Errorf("create release request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", OutboundUserAgent())

	resp, err := u.client.Do(req)
	if err != nil {
		return GitHubRelease{}, fmt.Errorf("fetch latest release: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return GitHubRelease{}, fmt.Errorf("fetch latest release: status %d", resp.StatusCode)
	}

	var release GitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return GitHubRelease{}, fmt.Errorf("decode latest release: %w", err)
	}
	if strings.TrimSpace(release.TagName) == "" {
		return GitHubRelease{}, fmt.Errorf("latest release missing tag_name")
	}
	return release, nil
}

func (u *AutoUpdater) downloadVerifyAndReplace(ctx context.Context, tag, assetName, assetURL, checksumURL string) error {
	if strings.TrimSpace(assetURL) == "" || strings.TrimSpace(checksumURL) == "" {
		return fmt.Errorf("release %s has empty download URL", tag)
	}

	checksumBytes, err := u.downloadBytes(ctx, checksumURL)
	if err != nil {
		return fmt.Errorf("download checksums: %w", err)
	}
	checksums, err := parseChecksums(checksumBytes)
	if err != nil {
		return fmt.Errorf("parse checksums: %w", err)
	}

	dir := filepath.Dir(u.executablePath)
	tmp, err := os.CreateTemp(dir, ".ccload-update-*")
	if err != nil {
		return fmt.Errorf("create temp binary: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if err := u.downloadToFile(ctx, assetURL, tmp); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("download asset %s: %w", assetName, err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp binary: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp binary: %w", err)
	}

	if err := verifyFileChecksum(tmpPath, assetName, checksums); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, u.executablePath); err != nil {
		return fmt.Errorf("replace executable: %w", err)
	}
	log.Printf("[AutoUpdater] prepared %s; restart pending", tag)
	return nil
}

func (u *AutoUpdater) downloadBytes(ctx context.Context, rawURL string) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", OutboundUserAgent())

	resp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

func (u *AutoUpdater) downloadToFile(ctx context.Context, rawURL string, dst *os.File) error {
	reqCtx, cancel := context.WithTimeout(ctx, updateDownloadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", OutboundUserAgent())

	resp, err := u.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	_, err = io.Copy(dst, resp.Body)
	return err
}

func (u *AutoUpdater) baselineVersion() string {
	u.mu.Lock()
	pending := u.state.PendingVersion
	u.mu.Unlock()

	if compareSemanticVersions(pending, Version) > 0 {
		return pending
	}
	return Version
}

func (u *AutoUpdater) setUpdating(updating bool) {
	u.mu.Lock()
	u.state.Updating = updating
	u.mu.Unlock()
}

func (u *AutoUpdater) markPending(version string) {
	u.mu.Lock()
	u.state.PendingRestart = true
	u.state.PendingVersion = version
	u.mu.Unlock()
}

func (u *AutoUpdater) ensureRestartWaiter(ctx context.Context) {
	u.mu.Lock()
	if u.waitingRestart {
		u.mu.Unlock()
		return
	}
	u.waitingRestart = true
	u.wg.Add(1)
	u.mu.Unlock()

	go func() {
		defer u.wg.Done()
		u.waitForIdleAndRestart(ctx)
	}()
}

func (u *AutoUpdater) waitForIdleAndRestart(ctx context.Context) {
	ticker := time.NewTicker(u.restartPollInterval)
	defer ticker.Stop()

	for {
		if u.readyToRestart() {
			u.callRestartOnce()
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (u *AutoUpdater) readyToRestart() bool {
	u.mu.Lock()
	state := u.state
	restartCalled := u.restartCalled
	u.mu.Unlock()

	if restartCalled || !state.PendingRestart || state.Updating {
		return false
	}
	active := u.activeRequests()
	if active > 0 {
		log.Printf("[AutoUpdater] restart delayed: %d active request(s)", active)
		return false
	}
	return true
}

func (u *AutoUpdater) callRestartOnce() {
	u.mu.Lock()
	if u.restartCalled {
		u.mu.Unlock()
		return
	}
	u.restartCalled = true
	version := u.state.PendingVersion
	u.mu.Unlock()

	log.Printf("[AutoUpdater] restarting into %s", version)
	u.restart()
}

func releaseAssetName(goos, goarch string) (string, bool) {
	switch goos + "/" + goarch {
	case "darwin/amd64":
		return "ccload-darwin-amd64", true
	case "darwin/arm64":
		return "ccload-darwin-arm64", true
	case "linux/amd64":
		return "ccload-linux-amd64", true
	case "linux/arm64":
		return "ccload-linux-arm64", true
	default:
		return "", false
	}
}

func findReleaseAsset(release GitHubRelease, name string) (GitHubAsset, bool) {
	for _, asset := range release.Assets {
		if asset.Name == name {
			return asset, true
		}
	}
	return GitHubAsset{}, false
}

func parseChecksums(data []byte) (map[string]string, error) {
	checksums := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, fmt.Errorf("invalid checksum line %q", line)
		}
		hash := strings.ToLower(fields[0])
		if len(hash) != sha256.Size*2 {
			return nil, fmt.Errorf("invalid sha256 length for %s", fields[1])
		}
		if _, err := hex.DecodeString(hash); err != nil {
			return nil, fmt.Errorf("invalid sha256 for %s: %w", fields[1], err)
		}
		name := strings.TrimPrefix(fields[1], "*")
		checksums[name] = hash
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(checksums) == 0 {
		return nil, errors.New("empty checksums")
	}
	return checksums, nil
}

func verifyFileChecksum(path, assetName string, checksums map[string]string) error {
	want := strings.ToLower(strings.TrimSpace(checksums[assetName]))
	if want == "" {
		return fmt.Errorf("checksum missing for %s", assetName)
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open file for checksum: %w", err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hash file: %w", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return fmt.Errorf("checksum mismatch for %s", assetName)
	}
	return nil
}

func compareSemanticVersions(a, b string) int {
	av, aok := parseSemanticVersion(a)
	bv, bok := parseSemanticVersion(b)
	if !aok && !bok {
		return 0
	}
	if !aok {
		return -1
	}
	if !bok {
		return 1
	}
	for i := 0; i < len(av) || i < len(bv); i++ {
		var ai, bi int
		if i < len(av) {
			ai = av[i]
		}
		if i < len(bv) {
			bi = bv[i]
		}
		if ai > bi {
			return 1
		}
		if ai < bi {
			return -1
		}
	}
	return 0
}

func parseSemanticVersion(v string) ([]int, bool) {
	v = normalizeVersion(v)
	if idx := strings.IndexAny(v, "-+"); idx >= 0 {
		v = v[:idx]
	}
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			return nil, false
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return nil, false
		}
		out = append(out, n)
	}
	return out, len(out) > 0
}
