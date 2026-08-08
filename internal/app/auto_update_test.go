package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
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

	originalRestartFunc := RestartFunc
	var restartCalls atomic.Int64
	RestartFunc = func() { restartCalls.Add(1) }
	t.Cleanup(func() { RestartFunc = originalRestartFunc })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	server := &Server{
		configService: newStubConfigService(map[string]string{
			"auto_update_interval_hours": "1",
			"auto_update_channel":        "stable",
		}),
		baseCtx: ctx,
	}

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
