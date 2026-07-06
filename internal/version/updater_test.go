package version

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCompareSemanticVersions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		a, b string
		want int
	}{
		{a: "v2.43.10", b: "v2.43.9", want: 1},
		{a: "2.43.9", b: "v2.43.10", want: -1},
		{a: "v2.43.0", b: "2.43", want: 0},
		{a: "dev", b: "v2.43.0", want: -1},
		{a: "v2.43.0", b: "dev", want: 1},
	}

	for _, tt := range tests {
		if got := compareSemanticVersions(tt.a, tt.b); got != tt.want {
			t.Fatalf("compareSemanticVersions(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestReleaseAssetNameAndDownloadURL(t *testing.T) {
	t.Parallel()

	name, ok := releaseAssetName("linux", "amd64")
	if !ok || name != "ccload-linux-amd64" {
		t.Fatalf("releaseAssetName(linux, amd64) = %q, %v", name, ok)
	}

	release := GitHubRelease{
		TagName: "v2.44.0",
		HTMLURL: "https://github.com/caidaoli/ccLoad/releases/tag/v2.44.0",
	}
	assetURL, err := releaseDownloadURL(release, name)
	if err != nil {
		t.Fatalf("releaseDownloadURL(asset): %v", err)
	}
	if assetURL != "https://github.com/caidaoli/ccLoad/releases/download/v2.44.0/ccload-linux-amd64" {
		t.Fatalf("asset download URL = %q", assetURL)
	}
	checksumURL, err := releaseDownloadURL(release, "checksums.txt")
	if err != nil {
		t.Fatalf("releaseDownloadURL(checksums): %v", err)
	}
	if checksumURL != "https://github.com/caidaoli/ccLoad/releases/download/v2.44.0/checksums.txt" {
		t.Fatalf("checksum download URL = %q", checksumURL)
	}

	if _, ok := releaseAssetName("windows", "arm64"); ok {
		t.Fatalf("windows/arm64 must be unsupported")
	}
}

func TestFetchLatestReleaseReadsUnfollowedRedirectLocation(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/latest" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/caidaoli/ccLoad/releases/tag/v2.44.0", http.StatusFound)
	}))
	defer server.Close()

	client := server.Client()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}

	release, err := fetchLatestRelease(context.Background(), client, server.URL+"/latest")
	if err != nil {
		t.Fatalf("fetchLatestRelease: %v", err)
	}
	if release.TagName != "v2.44.0" {
		t.Fatalf("TagName = %q", release.TagName)
	}
	wantURL := server.URL + "/caidaoli/ccLoad/releases/tag/v2.44.0"
	if release.HTMLURL != wantURL {
		t.Fatalf("HTMLURL = %q, want %q", release.HTMLURL, wantURL)
	}
}

func TestChecksumVerification(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "ccload-linux-amd64")
	body := []byte("new binary")
	if err := os.WriteFile(path, body, 0o755); err != nil {
		t.Fatalf("write test file: %v", err)
	}
	sum := sha256.Sum256(body)
	goodChecksum := hex.EncodeToString(sum[:])

	checksums, err := parseChecksums([]byte(goodChecksum + "  ccload-linux-amd64\n"))
	if err != nil {
		t.Fatalf("parseChecksums: %v", err)
	}
	if err := verifyFileChecksum(path, "ccload-linux-amd64", checksums); err != nil {
		t.Fatalf("verifyFileChecksum: %v", err)
	}

	badChecksums, err := parseChecksums([]byte("0000000000000000000000000000000000000000000000000000000000000000  ccload-linux-amd64\n"))
	if err != nil {
		t.Fatalf("parse bad checksum fixture: %v", err)
	}
	if err := verifyFileChecksum(path, "ccload-linux-amd64", badChecksums); err == nil {
		t.Fatalf("verifyFileChecksum must reject checksum mismatch")
	}
}

func TestUpdateOnceReplacesPendingVersionWithNewerDownloadedRelease(t *testing.T) {
	origVersion := Version
	t.Cleanup(func() { Version = origVersion })
	Version = "v1.0.0"

	binaries := map[string][]byte{
		"v1.0.1": []byte("binary v1.0.1"),
		"v1.0.2": []byte("binary v1.0.2"),
	}
	latest := "v1.0.1"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			http.Redirect(w, r, "/caidaoli/ccLoad/releases/tag/"+latest, http.StatusFound)
		case "/caidaoli/ccLoad/releases/tag/v1.0.1", "/caidaoli/ccLoad/releases/tag/v1.0.2":
			_, _ = fmt.Fprintf(w, "<html><title>%s</title></html>", latest)
		case "/caidaoli/ccLoad/releases/download/v1.0.1/ccload-linux-amd64", "/caidaoli/ccLoad/releases/download/v1.0.2/ccload-linux-amd64":
			tag := filepath.Base(filepath.Dir(r.URL.Path))
			_, _ = w.Write(binaries[tag])
		case "/caidaoli/ccLoad/releases/download/v1.0.1/checksums.txt", "/caidaoli/ccLoad/releases/download/v1.0.2/checksums.txt":
			tag := filepath.Base(filepath.Dir(r.URL.Path))
			sum := sha256.Sum256(binaries[tag])
			_, _ = fmt.Fprintf(w, "%s  ccload-linux-amd64\n", hex.EncodeToString(sum[:]))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	exePath := filepath.Join(t.TempDir(), "ccload")
	if err := os.WriteFile(exePath, []byte("old"), 0o755); err != nil {
		t.Fatalf("write old executable: %v", err)
	}

	ctx, cancel := contextWithCleanup(t)
	updater, err := NewAutoUpdater(AutoUpdateOptions{
		Interval:            time.Hour,
		RestartPollInterval: time.Millisecond,
		LatestReleaseURL:    server.URL + "/latest",
		ExecutablePath:      exePath,
		Client:              server.Client(),
		GOOS:                "linux",
		GOARCH:              "amd64",
		ActiveRequests:      func() int { return 1 },
		Restart:             func() { t.Fatalf("restart must wait for idle requests") },
	})
	if err != nil {
		t.Fatalf("NewAutoUpdater: %v", err)
	}
	defer func() {
		cancel()
		updater.wg.Wait()
	}()

	if err := updater.updateOnce(ctx); err != nil {
		t.Fatalf("first updateOnce: %v", err)
	}
	if state := updater.State(); state.PendingVersion != "v1.0.1" || !state.PendingRestart {
		t.Fatalf("after first update state = %+v, want pending v1.0.1", state)
	}

	latest = "v1.0.2"
	if err := updater.updateOnce(ctx); err != nil {
		t.Fatalf("second updateOnce: %v", err)
	}
	if state := updater.State(); state.PendingVersion != "v1.0.2" || !state.PendingRestart {
		t.Fatalf("after second update state = %+v, want pending v1.0.2", state)
	}
	got, err := os.ReadFile(exePath)
	if err != nil {
		t.Fatalf("read executable: %v", err)
	}
	if string(got) != "binary v1.0.2" {
		t.Fatalf("executable content = %q, want newest binary", got)
	}
}

func contextWithCleanup(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithCancel(context.Background())
}
