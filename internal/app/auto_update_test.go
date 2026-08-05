package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/version"
)

func TestStartUpdateManagerContainerChecksWithoutSelfUpdate(t *testing.T) {
	var metadataRequests atomic.Int64
	var downloadRequests atomic.Int64
	releaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/caidaoli/ccLoad/releases/latest":
			metadataRequests.Add(1)
			http.Redirect(w, r, "/caidaoli/ccLoad/releases/tag/v2.0.0", http.StatusFound)
		case "/caidaoli/ccLoad/releases/tag/v2.0.0":
			_, _ = w.Write([]byte("<html></html>"))
		default:
			downloadRequests.Add(1)
			http.Error(w, "download must not run", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(releaseServer.Close)

	t.Setenv("CCLOAD_CONTAINER", "1")
	t.Setenv("CCLOAD_ALLOW_SELF_UPDATE", "")
	t.Setenv("CCLOAD_RELEASE_BASE_URL", releaseServer.URL+"/caidaoli/ccLoad/releases/latest/download")

	originalVersion := version.Version
	version.Version = "v1.0.0"
	t.Cleanup(func() { version.Version = originalVersion })

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
	deadline := time.Now().Add(time.Second)
	for server.updateManager.State().LatestVersion == "" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if metadataRequests.Load() != 1 {
		t.Fatalf("release metadata requests=%d, want 1", metadataRequests.Load())
	}

	state := server.updateManager.State()
	if !state.HasUpdate || state.LatestVersion != "v2.0.0" {
		t.Fatalf("update state=%+v, want available v2.0.0", state)
	}
	c, w := newTestContext(t, newRequest(http.MethodGet, "/public/version", nil))
	server.HandlePublicVersion(c)
	resp := mustParseAPIResponse[struct {
		HasUpdate     bool   `json:"has_update"`
		LatestVersion string `json:"latest_version"`
	}](t, w.Body.Bytes())
	if !resp.Data.HasUpdate || resp.Data.LatestVersion != "v2.0.0" {
		t.Fatalf("public version state=%+v, want available v2.0.0", resp.Data)
	}
	time.Sleep(20 * time.Millisecond)
	if downloadRequests.Load() != 0 || restartCalls.Load() != 0 {
		t.Fatalf("container check-only mode downloaded=%d restarted=%d", downloadRequests.Load(), restartCalls.Load())
	}

	cancel()
	server.wg.Wait()
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

func TestStartUpdateManagerAllowsExplicitContainerOptIn(t *testing.T) {
	requested := make(chan struct{}, 1)
	releaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case requested <- struct{}{}:
		default:
		}
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(releaseServer.Close)

	t.Setenv("CCLOAD_CONTAINER", "1")
	t.Setenv("CCLOAD_ALLOW_SELF_UPDATE", "1")
	t.Setenv("CCLOAD_RELEASE_BASE_URL", releaseServer.URL+"/caidaoli/ccLoad/releases/latest/download")

	originalVersion := version.Version
	version.Version = "v1.0.0"
	t.Cleanup(func() { version.Version = originalVersion })

	originalRestartFunc := RestartFunc
	RestartFunc = func() {}
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
	select {
	case <-requested:
	case <-time.After(time.Second):
		t.Fatal("容器显式允许自更新后未执行版本检查")
	}

	cancel()
	done := make(chan struct{})
	go func() {
		server.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("自动更新循环未在上下文取消后退出")
	}
}
