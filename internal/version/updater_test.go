package version

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

	source := ReleaseSource{
		Name:            "ghproxy.net",
		DownloadBaseURL: "https://ghproxy.net/https://github.com/caidaoli/ccLoad/releases/download",
	}
	assetURL, err := releaseDownloadURL(source, "v2.44.0", name)
	if err != nil {
		t.Fatalf("releaseDownloadURL(asset): %v", err)
	}
	if assetURL != "https://ghproxy.net/https://github.com/caidaoli/ccLoad/releases/download/v2.44.0/ccload-linux-amd64" {
		t.Fatalf("asset download URL = %q", assetURL)
	}
	checksumURL, err := releaseDownloadURL(source, "v2.44.0", "checksums.txt")
	if err != nil {
		t.Fatalf("releaseDownloadURL(checksums): %v", err)
	}
	if checksumURL != "https://ghproxy.net/https://github.com/caidaoli/ccLoad/releases/download/v2.44.0/checksums.txt" {
		t.Fatalf("checksum download URL = %q", checksumURL)
	}

	if _, ok := releaseAssetName("windows", "arm64"); ok {
		t.Fatalf("windows/arm64 must be unsupported")
	}
}

func TestAutoUpdaterReleaseSourceConfiguration(t *testing.T) {
	origVersion := Version
	t.Cleanup(func() { Version = origVersion })
	Version = "v1.0.0"

	t.Run("defaults prefer ghproxy then GitHub", func(t *testing.T) {
		t.Setenv("CCLOAD_RELEASE_BASE_URL", "")
		assertAutoUpdaterLatestRequests(t, []string{
			"https://ghproxy.net/https://github.com/caidaoli/ccLoad/releases/latest",
			"https://github.com/caidaoli/ccLoad/releases/latest",
		})
	})

	t.Run("custom base disables built-in fallback", func(t *testing.T) {
		t.Setenv("CCLOAD_RELEASE_BASE_URL", "https://mirror.example/https://github.com/caidaoli/ccLoad/releases/latest/download/")
		assertAutoUpdaterLatestRequests(t, []string{
			"https://mirror.example/https://github.com/caidaoli/ccLoad/releases/latest",
		})
	})

	t.Run("invalid custom base fails", func(t *testing.T) {
		t.Setenv("CCLOAD_RELEASE_BASE_URL", "https://mirror.example/releases/download")
		_, err := NewAutoUpdater(AutoUpdateOptions{
			Interval: time.Hour,
			Restart:  func() {},
		})
		if err == nil {
			t.Fatal("NewAutoUpdater must reject an invalid custom release base")
		}
	})
}

func assertAutoUpdaterLatestRequests(t *testing.T, want []string) {
	t.Helper()

	requests := make(chan string, len(want)+1)
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			requests <- req.URL.String()
			return nil, errors.New("unavailable")
		}),
	}
	updater, err := NewAutoUpdater(AutoUpdateOptions{
		Interval:       time.Hour,
		ExecutablePath: filepath.Join(t.TempDir(), "ccload"),
		Client:         client,
		GOOS:           "linux",
		GOARCH:         "amd64",
		Restart:        func() {},
	})
	if err != nil {
		t.Fatalf("NewAutoUpdater: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		updater.Run(ctx)
		close(done)
	}()

	for i, wantURL := range want {
		select {
		case got := <-requests:
			if got != wantURL {
				cancel()
				<-done
				t.Fatalf("request %d = %q, want %q", i, got, wantURL)
			}
		case <-time.After(time.Second):
			cancel()
			<-done
			t.Fatalf("timed out waiting for request %d", i)
		}
	}

	deadline := time.Now().Add(time.Second)
	for updater.State().LastError == "" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	select {
	case unexpected := <-requests:
		t.Fatalf("unexpected extra release request %q", unexpected)
	default:
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
		ReleaseSources: []ReleaseSource{{
			Name:            "test",
			LatestURL:       server.URL + "/latest",
			DownloadBaseURL: server.URL + "/caidaoli/ccLoad/releases/download",
		}},
		ExecutablePath: exePath,
		Client:         server.Client(),
		GOOS:           "linux",
		GOARCH:         "amd64",
		ActiveRequests: func() int { return 1 },
		Restart:        func() { t.Fatalf("restart must wait for idle requests") },
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

func TestAutoUpdaterFallsBackToNextReleaseSource(t *testing.T) {
	origVersion := Version
	t.Cleanup(func() { Version = origVersion })
	Version = "v1.0.0"

	for _, failStage := range []string{"latest", "checksums", "asset"} {
		t.Run(failStage, func(t *testing.T) {
			binary := []byte("fallback binary")
			sum := sha256.Sum256(binary)
			checksum := hex.EncodeToString(sum[:]) + "  ccload-linux-amd64\n"

			var mu sync.Mutex
			var requests []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				requests = append(requests, r.URL.Path)
				mu.Unlock()

				source := "github"
				if strings.HasPrefix(r.URL.Path, "/proxy/") {
					source = "proxy"
				}
				switch {
				case strings.HasSuffix(r.URL.Path, "/latest"):
					if source == "proxy" && failStage == "latest" {
						http.Error(w, "proxy unavailable", http.StatusBadGateway)
						return
					}
					http.Redirect(w, r, "/"+source+"/releases/tag/v1.0.1", http.StatusFound)
				case strings.Contains(r.URL.Path, "/releases/tag/"):
					_, _ = fmt.Fprint(w, "<html></html>")
				case strings.HasSuffix(r.URL.Path, "/checksums.txt"):
					if source == "proxy" && failStage == "checksums" {
						http.Error(w, "checksum unavailable", http.StatusBadGateway)
						return
					}
					_, _ = fmt.Fprint(w, checksum)
				case strings.HasSuffix(r.URL.Path, "/ccload-linux-amd64"):
					if source == "proxy" && failStage == "asset" {
						http.Error(w, "asset unavailable", http.StatusBadGateway)
						return
					}
					_, _ = w.Write(binary)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			exePath := filepath.Join(t.TempDir(), "ccload")
			if err := os.WriteFile(exePath, []byte("old"), 0o755); err != nil {
				t.Fatalf("write old executable: %v", err)
			}

			updater, err := NewAutoUpdater(AutoUpdateOptions{
				Interval:            time.Hour,
				RestartPollInterval: time.Millisecond,
				ReleaseSources: []ReleaseSource{
					{Name: "proxy", LatestURL: server.URL + "/proxy/latest", DownloadBaseURL: server.URL + "/proxy/releases/download"},
					{Name: "github", LatestURL: server.URL + "/github/latest", DownloadBaseURL: server.URL + "/github/releases/download"},
				},
				ExecutablePath: exePath,
				Client:         server.Client(),
				GOOS:           "linux",
				GOARCH:         "amd64",
				ActiveRequests: func() int { return 1 },
				Restart:        func() {},
			})
			if err != nil {
				t.Fatalf("NewAutoUpdater: %v", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				updater.Run(ctx)
				close(done)
			}()

			deadline := time.Now().Add(2 * time.Second)
			for updater.State().PendingVersion != "v1.0.1" && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			cancel()
			<-done

			state := updater.State()
			if state.PendingVersion != "v1.0.1" || !state.PendingRestart || state.LastError != "" {
				t.Fatalf("state after fallback = %+v", state)
			}
			got, err := os.ReadFile(exePath)
			if err != nil {
				t.Fatalf("read executable: %v", err)
			}
			if string(got) != string(binary) {
				t.Fatalf("executable content = %q, want fallback binary", got)
			}

			mu.Lock()
			requestLog := strings.Join(requests, "\n")
			mu.Unlock()
			for _, path := range []string{
				"/github/latest",
				"/github/releases/download/v1.0.1/checksums.txt",
				"/github/releases/download/v1.0.1/ccload-linux-amd64",
			} {
				if !strings.Contains(requestLog, path) {
					t.Fatalf("fallback did not request %s; requests:\n%s", path, requestLog)
				}
			}
		})
	}
}

func contextWithCleanup(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithCancel(context.Background())
}
