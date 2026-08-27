package app

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/version"
)

func TestNormalizeAutoUpdateIntervalHours(t *testing.T) {
	t.Parallel()
	if got := normalizeAutoUpdateIntervalHours(0); got != 0 {
		t.Fatalf("zero=%d, want disabled", got)
	}
	if got := normalizeAutoUpdateIntervalHours(-1); got != defaultAutoUpdateIntervalHours {
		t.Fatalf("negative=%d, want default %d", got, defaultAutoUpdateIntervalHours)
	}
	overflow := int(maxSettingDurationHours + 1)
	if got := normalizeAutoUpdateIntervalHours(overflow); got != defaultAutoUpdateIntervalHours {
		t.Fatalf("overflow=%d, want default %d", got, defaultAutoUpdateIntervalHours)
	}
}

func TestStartUpdateManagerContainerSkipsReleaseChecks(t *testing.T) {
	requested := make(chan struct{}, 1)
	releaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requested <- struct{}{}
		http.Error(w, "container must not request release metadata", http.StatusInternalServerError)
	}))
	t.Cleanup(releaseServer.Close)

	t.Setenv("CCLOAD_CONTAINER", "1")
	t.Setenv("CCLOAD_RELEASE_BASE_URL", releaseServer.URL+"/caidaoli/ccLoad/releases/latest/download")

	var restartCalls atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	server := &Server{
		configService: newStubConfigService(map[string]string{
			"auto_update_interval_hours": "1",
			"auto_update_channel":        "stable",
		}),
		baseCtx: ctx,
	}
	server.SetRestartFunc(func() { restartCalls.Add(1) })

	server.StartUpdateManager()
	if server.updateManager != nil {
		t.Fatal("container runtime must not start the update manager")
	}
	select {
	case <-requested:
		t.Fatal("container runtime requested release metadata")
	case <-time.After(50 * time.Millisecond):
	}
	if restartCalls.Load() != 0 {
		t.Fatalf("container runtime restarted %d times", restartCalls.Load())
	}
}

func TestStartUpdateManagerDisabledMakesNoReleaseRequest(t *testing.T) {
	requested := make(chan struct{}, 1)
	releaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requested <- struct{}{}
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	t.Cleanup(releaseServer.Close)

	t.Setenv("CCLOAD_RELEASE_BASE_URL", releaseServer.URL+"/caidaoli/ccLoad/releases/latest/download")
	server := &Server{
		configService: newStubConfigService(map[string]string{
			"auto_update_interval_hours": "0",
			"auto_update_channel":        "preview",
		}),
		baseCtx: context.Background(),
	}

	server.StartUpdateManager()
	select {
	case <-requested:
		t.Fatal("auto_update_interval_hours=0 must not request release metadata")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHandleManualUpdateRunsFullUpdate(t *testing.T) {
	origVersion := version.Version
	t.Cleanup(func() { version.Version = origVersion })
	version.Version = "v1.0.0"

	application := []byte("new application")
	releaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			http.Redirect(w, r, "/caidaoli/ccLoad/releases/tag/v1.1.0", http.StatusFound)
		case "/caidaoli/ccLoad/releases/tag/v1.1.0":
			_, _ = fmt.Fprint(w, "<html></html>")
		case "/download/v1.1.0/checksums.txt":
			sum := sha256.Sum256(application)
			_, _ = fmt.Fprintf(w, "%x  ccload-linux-amd64\n", sum)
		case "/download/v1.1.0/ccload-linux-amd64":
			_, _ = w.Write(application)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(releaseServer.Close)

	executablePath := filepath.Join(t.TempDir(), "ccload")
	if err := os.WriteFile(executablePath, []byte("old application"), 0o755); err != nil {
		t.Fatalf("write old executable: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	manager, err := version.NewUpdateManager(version.UpdateManagerOptions{
		Interval:     0,
		ApplyUpdates: true,
		ReleaseSources: []version.ReleaseSource{{
			Name:            "test",
			LatestURL:       releaseServer.URL + "/latest",
			DownloadBaseURL: releaseServer.URL + "/download",
		}},
		ExecutablePath:      executablePath,
		GOOS:                "linux",
		GOARCH:              "amd64",
		Client:              releaseServer.Client(),
		ActiveRequests:      func() int { return 1 },
		RestartPollInterval: time.Hour,
		Restart:             func() {},
	})
	if err != nil {
		t.Fatalf("NewUpdateManager: %v", err)
	}

	server := &Server{updateManager: manager, baseCtx: ctx}
	c, w := newTestContext(t, newRequest(http.MethodPost, "/admin/update/check", nil))
	server.HandleManualUpdate(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	var response APIResponse[version.UpdateState]
	mustUnmarshalJSON(t, w.Body.Bytes(), &response)
	if !response.Success {
		t.Fatalf("response unsuccessful: %+v", response)
	}
	if !response.Data.HasUpdate || !response.Data.PendingRestart || response.Data.PendingVersion != "v1.1.0" {
		t.Fatalf("update state = %+v", response.Data)
	}
	got, err := os.ReadFile(executablePath)
	if err != nil {
		t.Fatalf("read executable: %v", err)
	}
	if string(got) != string(application) {
		t.Fatalf("executable content = %q, want %q", got, application)
	}
}
