package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ccLoad/internal/version"
)

func TestStartAutoUpdateLoopAllowsExplicitContainerOptIn(t *testing.T) {
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

	server.StartAutoUpdateLoop()
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
