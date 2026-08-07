package version

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/semver"
)

const (
	requestTimeout             = 10 * time.Second
	updateDownloadTimeout      = 5 * time.Minute
	defaultRestartPollInterval = 10 * time.Second
	updateRetryDelay1          = time.Minute
	updateRetryDelay2          = 2 * time.Minute
	updateRetryDelay3          = 4 * time.Minute
)

var gitDescribeVersionPattern = regexp.MustCompile(`^(v?[0-9]+\.[0-9]+\.[0-9]+)-([0-9]+)-g([0-9a-fA-F]+)$`)

// UpdateState exposes release discovery and application state.
type UpdateState struct {
	HasUpdate      bool
	LatestVersion  string
	ReleaseURL     string
	PendingRestart bool
	PendingVersion string
	Updating       bool
	LastCheck      time.Time
	LastError      string
}

// UpdateManagerOptions configures an UpdateManager.
type UpdateManagerOptions struct {
	Interval            time.Duration
	Channel             ReleaseChannel
	ApplyUpdates        bool
	RestartPollInterval time.Duration
	RetryDelays         []time.Duration
	ReleaseSources      []ReleaseSource
	ExecutablePath      string
	GOOS                string
	GOARCH              string
	Client              *http.Client
	ActiveRequests      func() int
	Restart             func()
}

// UpdateManager owns release checks and optionally applies verified updates.
type UpdateManager struct {
	interval            time.Duration
	channel             ReleaseChannel
	applyUpdates        bool
	restartPollInterval time.Duration
	retryDelays         []time.Duration
	releaseSources      []ReleaseSource
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

// NewUpdateManager creates a release manager with explicit application policy.
func NewUpdateManager(opts UpdateManagerOptions) (*UpdateManager, error) {
	if opts.Interval <= 0 {
		return nil, fmt.Errorf("auto update interval must be positive")
	}
	if opts.Channel == "" {
		opts.Channel = ReleaseChannelStable
	}
	channel, err := ParseReleaseChannel(string(opts.Channel))
	if err != nil {
		return nil, err
	}
	opts.Channel = channel
	if opts.ApplyUpdates && opts.Restart == nil {
		return nil, fmt.Errorf("restart callback is required")
	}
	if opts.ActiveRequests == nil {
		opts.ActiveRequests = func() int { return 0 }
	}
	if opts.RestartPollInterval <= 0 {
		opts.RestartPollInterval = defaultRestartPollInterval
	}
	if len(opts.RetryDelays) == 0 {
		opts.RetryDelays = []time.Duration{updateRetryDelay1, updateRetryDelay2, updateRetryDelay3}
	}
	for _, delay := range opts.RetryDelays {
		if delay <= 0 {
			return nil, fmt.Errorf("update retry delay must be positive")
		}
	}
	if len(opts.ReleaseSources) == 0 {
		var err error
		opts.ReleaseSources, err = releaseSources(os.Getenv("CCLOAD_RELEASE_BASE_URL"))
		if err != nil {
			return nil, err
		}
	}
	if opts.Client == nil {
		opts.Client = &http.Client{}
	}
	if opts.ApplyUpdates {
		if opts.GOOS == "" {
			opts.GOOS = runtime.GOOS
		}
		if opts.GOARCH == "" {
			opts.GOARCH = runtime.GOARCH
		}
		if opts.ExecutablePath == "" {
			exe, err := os.Executable()
			if err != nil {
				return nil, fmt.Errorf("resolve executable path: %w", err)
			}
			opts.ExecutablePath = exe
		}
	}

	return &UpdateManager{
		interval:            opts.Interval,
		channel:             opts.Channel,
		applyUpdates:        opts.ApplyUpdates,
		restartPollInterval: opts.RestartPollInterval,
		retryDelays:         append([]time.Duration(nil), opts.RetryDelays...),
		releaseSources:      append([]ReleaseSource(nil), opts.ReleaseSources...),
		executablePath:      opts.ExecutablePath,
		goos:                opts.GOOS,
		goarch:              opts.GOARCH,
		client:              opts.Client,
		activeRequests:      opts.ActiveRequests,
		restart:             opts.Restart,
	}, nil
}

// Run checks immediately, then at the configured interval, until ctx is canceled.
func (u *UpdateManager) Run(ctx context.Context) {
	defer u.wg.Wait()

	if strings.EqualFold(strings.TrimSpace(Version), "dev") || strings.TrimSpace(Version) == "" {
		log.Printf("[UpdateManager] disabled for development version %q", Version)
		return
	}
	if u.applyUpdates {
		if _, ok := releaseAssetName(u.goos, u.goarch); !ok {
			u.applyUpdates = false
			log.Printf("[UpdateManager] unsupported update platform %s/%s; continuing in check-only mode", u.goos, u.goarch)
		}
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

// State returns a snapshot of the manager state.
func (u *UpdateManager) State() UpdateState {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.state
}

func (u *UpdateManager) runCheck(ctx context.Context) {
	err := u.updateOnce(ctx)
	for retryIndex, delay := range u.retryDelays {
		if err == nil {
			return
		}
		u.recordUpdateError(err)
		if !isRetryableUpdateError(err) || ctx.Err() != nil {
			log.Printf("[UpdateManager] update check failed: %v", err)
			return
		}
		log.Printf(
			"[UpdateManager] update check failed: %v; retrying in %v (%d/%d)",
			err, delay, retryIndex+1, len(u.retryDelays),
		)
		if !waitForUpdateRetry(ctx, delay) {
			return
		}
		err = u.updateOnce(ctx)
	}
	if err != nil {
		u.recordUpdateError(err)
		log.Printf("[UpdateManager] update check failed after %d retries: %v", len(u.retryDelays), err)
	}
}

func (u *UpdateManager) recordUpdateError(err error) {
	if err != nil {
		u.mu.Lock()
		u.state.LastError = err.Error()
		u.state.LastCheck = time.Now()
		u.mu.Unlock()
	}
}

func waitForUpdateRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

type updateHTTPStatusError struct {
	status int
}

func (e *updateHTTPStatusError) Error() string {
	return fmt.Sprintf("status %d", e.status)
}

type updateTransportError struct {
	err error
}

func (e *updateTransportError) Error() string { return e.err.Error() }
func (e *updateTransportError) Unwrap() error { return e.err }

func isRetryableUpdateError(err error) bool {
	switch typed := err.(type) {
	case nil:
		return false
	case *updateHTTPStatusError:
		return typed.status == http.StatusNotFound ||
			typed.status == http.StatusRequestTimeout ||
			typed.status == http.StatusTooManyRequests ||
			typed.status >= http.StatusInternalServerError
	case *updateTransportError:
		return !errors.Is(typed.err, context.Canceled)
	case interface{ Unwrap() []error }:
		for _, nested := range typed.Unwrap() {
			if isRetryableUpdateError(nested) {
				return true
			}
		}
		return false
	case interface{ Unwrap() error }:
		return isRetryableUpdateError(typed.Unwrap())
	default:
		return false
	}
}

func (u *UpdateManager) updateOnce(ctx context.Context) error {
	release, err := resolveLatestRelease(ctx, u.client, u.releaseSources, u.channel)
	if err != nil {
		return err
	}

	u.mu.Lock()
	previousLatest := u.state.LatestVersion
	u.state.HasUpdate = compareSemanticVersions(release.TagName, Version) > 0
	u.state.LatestVersion = release.TagName
	u.state.ReleaseURL = release.HTMLURL
	u.state.LastCheck = time.Now()
	u.state.LastError = ""
	u.mu.Unlock()

	if compareSemanticVersions(release.TagName, Version) > 0 && previousLatest != release.TagName {
		log.Printf("[UpdateManager] new version available: %s -> %s", Version, release.TagName)
	}
	if !u.applyUpdates || compareSemanticVersions(release.TagName, u.baselineVersion()) <= 0 {
		return nil
	}
	assetName, ok := releaseAssetName(u.goos, u.goarch)
	if !ok {
		return fmt.Errorf("unsupported platform: %s/%s", u.goos, u.goarch)
	}

	var sourceErrors []error
	for _, source := range u.releaseSources {
		assetURL, err := releaseDownloadURL(source, release.TagName, assetName)
		if err != nil {
			sourceErrors = append(sourceErrors, fmt.Errorf("%s: %w", source.Name, err))
			continue
		}
		checksumURL, err := releaseDownloadURL(source, release.TagName, "checksums.txt")
		if err != nil {
			sourceErrors = append(sourceErrors, fmt.Errorf("%s: %w", source.Name, err))
			continue
		}

		u.setUpdating(true)
		err = u.downloadVerifyAndReplace(ctx, release.TagName, assetName, assetURL, checksumURL)
		u.setUpdating(false)
		if err != nil {
			sourceErrors = append(sourceErrors, fmt.Errorf("%s: %w", source.Name, err))
			continue
		}
		u.markPending(release.TagName)
		u.ensureRestartWaiter(ctx)
		return nil
	}
	return fmt.Errorf("download release %s from all sources: %w", release.TagName, errors.Join(sourceErrors...))
}

func (u *UpdateManager) downloadVerifyAndReplace(ctx context.Context, tag, assetName, assetURL, checksumURL string) error {
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
	log.Printf("[UpdateManager] prepared %s; restart pending", tag)
	return nil
}

func (u *UpdateManager) downloadBytes(ctx context.Context, rawURL string) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", OutboundUserAgent())

	resp, err := u.client.Do(req)
	if err != nil {
		return nil, &updateTransportError{err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, &updateHTTPStatusError{status: resp.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, &updateTransportError{err: err}
	}
	return data, nil
}

func (u *UpdateManager) downloadToFile(ctx context.Context, rawURL string, dst *os.File) error {
	reqCtx, cancel := context.WithTimeout(ctx, updateDownloadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", OutboundUserAgent())

	resp, err := u.client.Do(req)
	if err != nil {
		return &updateTransportError{err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return &updateHTTPStatusError{status: resp.StatusCode}
	}
	if _, err = io.Copy(dst, resp.Body); err != nil {
		return &updateTransportError{err: err}
	}
	return nil
}

func (u *UpdateManager) baselineVersion() string {
	u.mu.Lock()
	pending := u.state.PendingVersion
	u.mu.Unlock()

	if compareSemanticVersions(pending, Version) > 0 {
		return pending
	}
	return Version
}

func (u *UpdateManager) setUpdating(updating bool) {
	u.mu.Lock()
	u.state.Updating = updating
	u.mu.Unlock()
}

func (u *UpdateManager) markPending(version string) {
	u.mu.Lock()
	u.state.PendingRestart = true
	u.state.PendingVersion = version
	u.mu.Unlock()
}

func (u *UpdateManager) ensureRestartWaiter(ctx context.Context) {
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

func (u *UpdateManager) waitForIdleAndRestart(ctx context.Context) {
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

func (u *UpdateManager) readyToRestart() bool {
	u.mu.Lock()
	state := u.state
	restartCalled := u.restartCalled
	u.mu.Unlock()

	if restartCalled || !state.PendingRestart || state.Updating {
		return false
	}
	active := u.activeRequests()
	if active > 0 {
		log.Printf("[UpdateManager] restart delayed: %d active request(s)", active)
		return false
	}
	return true
}

func (u *UpdateManager) callRestartOnce() {
	u.mu.Lock()
	if u.restartCalled {
		u.mu.Unlock()
		return
	}
	u.restartCalled = true
	version := u.state.PendingVersion
	u.mu.Unlock()

	log.Printf("[UpdateManager] restarting into %s", version)
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
	av, aok := normalizeSemanticVersion(a)
	bv, bok := normalizeSemanticVersion(b)
	if !aok && !bok {
		return 0
	}
	if !aok {
		return -1
	}
	if !bok {
		return 1
	}
	return semver.Compare(av, bv)
}

func normalizeSemanticVersion(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", false
	}
	// A source build uses git describe (vX.Y.Z-N-gSHA). Treat the distance and
	// commit as build metadata so the matching stable tag cannot downgrade it.
	if match := gitDescribeVersionPattern.FindStringSubmatch(v); match != nil {
		v = match[1] + "+" + match[2] + ".g" + match[3]
	}
	if v[0] != 'v' {
		v = "v" + v
	}
	return v, semver.IsValid(v)
}
