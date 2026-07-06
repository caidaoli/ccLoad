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

func TestReleaseAssetSelection(t *testing.T) {
	t.Parallel()

	release := GitHubRelease{
		Assets: []GitHubAsset{
			{Name: "checksums.txt", BrowserDownloadURL: "https://example.test/checksums.txt"},
			{Name: "ccload-linux-amd64", BrowserDownloadURL: "https://example.test/linux-amd64"},
			{Name: "ccload-darwin-arm64", BrowserDownloadURL: "https://example.test/darwin-arm64"},
		},
	}

	name, ok := releaseAssetName("linux", "amd64")
	if !ok || name != "ccload-linux-amd64" {
		t.Fatalf("releaseAssetName(linux, amd64) = %q, %v", name, ok)
	}

	asset, ok := findReleaseAsset(release, name)
	if !ok || asset.BrowserDownloadURL != "https://example.test/linux-amd64" {
		t.Fatalf("findReleaseAsset(%q) = %#v, %v", name, asset, ok)
	}

	if _, ok := releaseAssetName("windows", "arm64"); ok {
		t.Fatalf("windows/arm64 must be unsupported")
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
			_, _ = fmt.Fprintf(w, `{
				"tag_name": %q,
				"html_url": "https://example.test/releases/%s",
				"assets": [
					{"name":"ccload-linux-amd64","browser_download_url":"%s/%s/ccload-linux-amd64"},
					{"name":"checksums.txt","browser_download_url":"%s/%s/checksums.txt"}
				]
			}`, latest, latest, httptestURL(r), latest, httptestURL(r), latest)
		case "/v1.0.1/ccload-linux-amd64", "/v1.0.2/ccload-linux-amd64":
			_, _ = w.Write(binaries[filepath.Base(filepath.Dir(r.URL.Path))])
		case "/v1.0.1/checksums.txt", "/v1.0.2/checksums.txt":
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

func httptestURL(r *http.Request) string {
	return "http://" + r.Host
}

func contextWithCleanup(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithCancel(context.Background())
}
