package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/util"
)

func TestModelCatalogSyncUpdatesCacheAndUsesConditionalETag(t *testing.T) {
	util.RestoreEmbeddedModelCatalog()
	t.Cleanup(util.RestoreEmbeddedModelCatalog)

	const etag = `"catalog-v1"`
	var requests atomic.Int32
	catalogServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch requests.Add(1) {
		case 1:
			if got := r.Header.Get("If-None-Match"); got != "" {
				http.Error(w, "unexpected initial etag", http.StatusBadRequest)
				return
			}
			w.Header().Set("ETag", etag)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, modelsDevCatalogJSON())
		case 2:
			if got := r.Header.Get("If-None-Match"); got != etag {
				http.Error(w, "missing conditional etag", http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusNotModified)
		default:
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	defer catalogServer.Close()

	cachePath := filepath.Join(t.TempDir(), "cache", "model-catalog.json")
	syncer := NewModelCatalogSyncer(catalogServer.Client(), catalogServer.URL, cachePath)

	result, err := syncer.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync update failed: %v", err)
	}
	if result.Status != ModelCatalogSyncUpdated {
		t.Fatalf("update status = %q, want %q", result.Status, ModelCatalogSyncUpdated)
	}
	if got := util.CurrentModelCatalogETag(); got != etag {
		t.Fatalf("installed etag = %q, want %q", got, etag)
	}

	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	var snapshot util.ModelCatalogSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatalf("decode normalized cache: %v", err)
	}
	if snapshot.Version != util.ModelCatalogSchemaVersion || snapshot.ETag != etag {
		t.Fatalf("cache snapshot = %#v", snapshot)
	}
	info, err := os.Stat(cachePath)
	if err != nil {
		t.Fatalf("stat cache: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("cache mode = %o, want 600", got)
	}

	result, err = syncer.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync conditional request failed: %v", err)
	}
	if result.Status != ModelCatalogSyncNotModified {
		t.Fatalf("conditional status = %q, want %q", result.Status, ModelCatalogSyncNotModified)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
}

func TestModelCatalogSyncRejectsOversizedResponse(t *testing.T) {
	util.RestoreEmbeddedModelCatalog()
	t.Cleanup(util.RestoreEmbeddedModelCatalog)

	catalogServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"too-large"`)
		_, _ = io.WriteString(w, modelsDevCatalogJSON()+strings.Repeat(" ", modelCatalogMaxBodyBytes))
	}))
	defer catalogServer.Close()

	cachePath := filepath.Join(t.TempDir(), "model-catalog.json")
	syncer := NewModelCatalogSyncer(catalogServer.Client(), catalogServer.URL, cachePath)
	if _, err := syncer.Sync(context.Background()); err == nil {
		t.Fatal("Sync accepted a response larger than the hard limit")
	}
	if got := util.CurrentModelCatalogETag(); got != "" {
		t.Fatalf("installed etag = %q, want empty", got)
	}
	if _, err := os.Stat(cachePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oversized response wrote cache: %v", err)
	}
}

func TestModelCatalogSyncRespectsCancellation(t *testing.T) {
	started := make(chan struct{})
	catalogServer := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer catalogServer.Close()

	syncer := NewModelCatalogSyncer(catalogServer.Client(), catalogServer.URL, filepath.Join(t.TempDir(), "model-catalog.json"))
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := syncer.Sync(ctx)
		errCh <- err
	}()

	select {
	case <-started:
		cancel()
	case <-time.After(time.Second):
		cancel()
		t.Fatal("catalog request did not start")
	}
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Sync error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Sync did not stop after context cancellation")
	}
}

func TestModelCatalogSyncRespectsHTTPClientTimeout(t *testing.T) {
	catalogServer := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer catalogServer.Close()

	syncer := NewModelCatalogSyncer(
		&http.Client{Timeout: 25 * time.Millisecond},
		catalogServer.URL,
		filepath.Join(t.TempDir(), "model-catalog.json"),
	)
	_, err := syncer.Sync(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Sync error = %v, want client timeout", err)
	}
}

func TestModelCatalogSyncInvalidResponseKeepsLastGoodCatalog(t *testing.T) {
	util.RestoreEmbeddedModelCatalog()
	t.Cleanup(util.RestoreEmbeddedModelCatalog)

	const etag = `"last-good"`
	var requests atomic.Int32
	catalogServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch requests.Add(1) {
		case 1:
			w.Header().Set("ETag", etag)
			_, _ = io.WriteString(w, modelsDevCatalogJSON())
		case 2:
			if got := r.Header.Get("If-None-Match"); got != etag {
				http.Error(w, "missing conditional etag", http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, `{"openai":`)
		default:
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	defer catalogServer.Close()

	cachePath := filepath.Join(t.TempDir(), "model-catalog.json")
	syncer := NewModelCatalogSyncer(catalogServer.Client(), catalogServer.URL, cachePath)
	if _, err := syncer.Sync(context.Background()); err != nil {
		t.Fatalf("initial Sync failed: %v", err)
	}
	lastGoodCache, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read last-good cache: %v", err)
	}

	if _, err := syncer.Sync(context.Background()); err == nil {
		t.Fatal("Sync accepted invalid JSON")
	}
	if got := util.CurrentModelCatalogETag(); got != etag {
		t.Fatalf("installed etag after invalid response = %q, want %q", got, etag)
	}
	cache, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read cache after invalid response: %v", err)
	}
	if string(cache) != string(lastGoodCache) {
		t.Fatal("invalid response replaced the last-good cache")
	}
}

func TestModelCatalogSyncSkipsConcurrentAttempt(t *testing.T) {
	util.RestoreEmbeddedModelCatalog()
	t.Cleanup(util.RestoreEmbeddedModelCatalog)

	started := make(chan struct{})
	release := make(chan struct{})
	catalogServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.Header().Set("ETag", `"concurrent"`)
		_, _ = io.WriteString(w, modelsDevCatalogJSON())
	}))
	defer catalogServer.Close()

	syncer := NewModelCatalogSyncer(catalogServer.Client(), catalogServer.URL, filepath.Join(t.TempDir(), "model-catalog.json"))
	type syncOutcome struct {
		result ModelCatalogSyncResult
		err    error
	}
	firstDone := make(chan syncOutcome, 1)
	go func() {
		result, err := syncer.Sync(context.Background())
		firstDone <- syncOutcome{result: result, err: err}
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first Sync did not start")
	}
	result, err := syncer.Sync(context.Background())
	if err != nil {
		t.Fatalf("concurrent Sync returned error: %v", err)
	}
	if result.Status != ModelCatalogSyncSkipped {
		t.Fatalf("concurrent status = %q, want %q", result.Status, ModelCatalogSyncSkipped)
	}

	close(release)
	select {
	case outcome := <-firstDone:
		if outcome.err != nil {
			t.Fatalf("first Sync failed: %v", outcome.err)
		}
		if outcome.result.Status != ModelCatalogSyncUpdated {
			t.Fatalf("first status = %q, want %q", outcome.result.Status, ModelCatalogSyncUpdated)
		}
	case <-time.After(time.Second):
		t.Fatal("first Sync did not finish")
	}
}

func TestModelCatalogSyncKeepsInstalledCatalogWhenCacheWriteFails(t *testing.T) {
	util.RestoreEmbeddedModelCatalog()
	t.Cleanup(util.RestoreEmbeddedModelCatalog)

	const etag = `"memory-only"`
	catalogServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", etag)
		_, _ = io.WriteString(w, modelsDevCatalogJSON())
	}))
	defer catalogServer.Close()

	cachePath := t.TempDir()
	syncer := NewModelCatalogSyncer(catalogServer.Client(), catalogServer.URL, cachePath)
	result, err := syncer.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync failed after cache write error: %v", err)
	}
	if result.Status != ModelCatalogSyncUpdated {
		t.Fatalf("status = %q, want %q", result.Status, ModelCatalogSyncUpdated)
	}
	if got := util.CurrentModelCatalogETag(); got != etag {
		t.Fatalf("installed etag = %q, want %q", got, etag)
	}
	info, err := os.Stat(cachePath)
	if err != nil {
		t.Fatalf("stat original cache path: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("cache write failure replaced the existing directory")
	}
}

func TestModelCatalogSyncLoadsValidCache(t *testing.T) {
	util.RestoreEmbeddedModelCatalog()
	t.Cleanup(util.RestoreEmbeddedModelCatalog)

	const etag = `"cached"`
	cachePath := filepath.Join(t.TempDir(), "model-catalog.json")
	writeNormalizedCatalogCache(t, cachePath, normalizedCatalogSnapshot(t, etag))

	syncer := NewModelCatalogSyncer(nil, "", cachePath)
	if err := syncer.LoadCache(); err != nil {
		t.Fatalf("LoadCache failed: %v", err)
	}
	if got := util.CurrentModelCatalogETag(); got != etag {
		t.Fatalf("installed etag = %q, want %q", got, etag)
	}
	models, source, _ := util.CommonCatalogModels("openai", 1)
	if len(models) != 1 || models[0] != "gpt-catalog-test" || source != "cache" {
		t.Fatalf("loaded catalog = models:%v source:%q", models, source)
	}
}

func TestModelCatalogSyncRejectsInvalidCacheAndKeepsCurrentCatalog(t *testing.T) {
	for _, tt := range []struct {
		name  string
		cache []byte
	}{
		{name: "corrupt_json", cache: []byte(`{"openai":`)},
		{name: "unsupported_schema", cache: unsupportedCatalogCache(t)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			util.RestoreEmbeddedModelCatalog()
			t.Cleanup(util.RestoreEmbeddedModelCatalog)

			const previousETag = `"previous"`
			if err := util.InstallModelCatalog(normalizedCatalogSnapshot(t, previousETag), "models.dev"); err != nil {
				t.Fatalf("install previous catalog: %v", err)
			}
			cachePath := filepath.Join(t.TempDir(), "model-catalog.json")
			if err := os.WriteFile(cachePath, tt.cache, 0o600); err != nil {
				t.Fatalf("write invalid cache: %v", err)
			}

			syncer := NewModelCatalogSyncer(nil, "", cachePath)
			if err := syncer.LoadCache(); err == nil {
				t.Fatal("LoadCache accepted invalid cache")
			}
			if got := util.CurrentModelCatalogETag(); got != previousETag {
				t.Fatalf("installed etag = %q, want %q", got, previousETag)
			}
		})
	}
}

func TestModelCatalogSyncServerWithDisabledIntervalLoadsCacheWithoutRequest(t *testing.T) {
	util.RestoreEmbeddedModelCatalog()
	t.Cleanup(util.RestoreEmbeddedModelCatalog)

	const etag = `"cached-on-start"`
	cachePath := filepath.Join(t.TempDir(), "model-catalog.json")
	writeNormalizedCatalogCache(t, cachePath, normalizedCatalogSnapshot(t, etag))
	t.Setenv("CCLOAD_MODEL_CATALOG_CACHE", cachePath)

	originalTransport := http.DefaultTransport
	var requests atomic.Int32
	http.DefaultTransport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("model catalog request must be disabled")
	})
	t.Cleanup(func() {
		http.DefaultTransport = originalTransport
	})

	srv := newModelCatalogSyncTestServer(t, 0)
	if got := util.CurrentModelCatalogETag(); got != "" {
		t.Fatalf("NewServer loaded catalog before StartModelCatalogSync: %q", got)
	}

	srv.StartModelCatalogSync()
	if got := util.CurrentModelCatalogETag(); got != etag {
		t.Fatalf("installed cache etag = %q, want %q", got, etag)
	}
	time.Sleep(50 * time.Millisecond)
	if got := requests.Load(); got != 0 {
		t.Fatalf("disabled interval made %d HTTP requests", got)
	}
}

func TestModelCatalogSyncServerRunsImmediatelyAndStopsWithServer(t *testing.T) {
	util.RestoreEmbeddedModelCatalog()
	t.Cleanup(util.RestoreEmbeddedModelCatalog)

	const etag = `"started"`
	t.Setenv("CCLOAD_MODEL_CATALOG_CACHE", filepath.Join(t.TempDir(), "model-catalog.json"))

	originalTransport := http.DefaultTransport
	var requests atomic.Int32
	var requestOnce sync.Once
	requestStarted := make(chan struct{})
	http.DefaultTransport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		requestOnce.Do(func() { close(requestStarted) })
		header := make(http.Header)
		header.Set("ETag", etag)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     header,
			Body:       io.NopCloser(strings.NewReader(modelsDevCatalogJSON())),
			Request:    req,
		}, nil
	})
	t.Cleanup(func() {
		http.DefaultTransport = originalTransport
	})

	srv := newModelCatalogSyncTestServer(t, 1)
	if got := requests.Load(); got != 0 {
		t.Fatalf("NewServer made %d model catalog requests", got)
	}

	srv.StartModelCatalogSync()
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("StartModelCatalogSync did not synchronize immediately")
	}
	deadline := time.Now().Add(time.Second)
	for util.CurrentModelCatalogETag() != etag && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := util.CurrentModelCatalogETag(); got != etag {
		t.Fatalf("immediate sync installed etag = %q, want %q", got, etag)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests before shutdown = %d, want 1", got)
	}
}

func modelsDevCatalogJSON() string {
	return `{
  "openai": {
    "id": "openai",
    "models": {
      "gpt-catalog-test": {
        "id": "gpt-catalog-test",
        "release_date": "2026-01-01",
        "modalities": {"output": ["text"]},
        "cost": {"input": 1.25, "output": 5.0}
      }
    }
  }
}`
}

func normalizedCatalogSnapshot(t *testing.T, etag string) *util.ModelCatalogSnapshot {
	t.Helper()
	snapshot, err := util.ParseModelsDevCatalog(strings.NewReader(modelsDevCatalogJSON()), etag, time.Now().UTC())
	if err != nil {
		t.Fatalf("parse normalized catalog fixture: %v", err)
	}
	return snapshot
}

func writeNormalizedCatalogCache(t *testing.T, cachePath string, snapshot *util.ModelCatalogSnapshot) {
	t.Helper()
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal normalized catalog: %v", err)
	}
	if err := os.WriteFile(cachePath, data, 0o600); err != nil {
		t.Fatalf("write normalized catalog cache: %v", err)
	}
}

func unsupportedCatalogCache(t *testing.T) []byte {
	t.Helper()
	snapshot := normalizedCatalogSnapshot(t, `"unsupported"`)
	snapshot.Version = util.ModelCatalogSchemaVersion + 1
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal unsupported catalog: %v", err)
	}
	return data
}

func newModelCatalogSyncTestServer(t *testing.T, intervalHours float64) *Server {
	t.Helper()

	store, err := storage.CreateSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("CreateSQLiteStore failed: %v", err)
	}
	srv := NewServer(store)
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			t.Errorf("Server.Shutdown failed: %v", err)
		}
	})

	srv.configService.mu.Lock()
	srv.configService.cache["model_catalog_sync_interval_hours"] = &model.SystemSetting{
		Key:   "model_catalog_sync_interval_hours",
		Value: strconv.FormatFloat(intervalHours, 'f', -1, 64),
	}
	srv.configService.mu.Unlock()
	return srv
}
