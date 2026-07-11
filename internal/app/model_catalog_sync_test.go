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

func TestModelCatalogSyncReportsStructuredResultMetadata(t *testing.T) {
	util.RestoreEmbeddedModelCatalog()
	t.Cleanup(util.RestoreEmbeddedModelCatalog)

	const etag = `"result-metadata"`
	var requests atomic.Int32
	catalogServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch requests.Add(1) {
		case 1:
			w.Header().Set("ETag", etag)
			_, _ = io.WriteString(w, modelsDevCatalogJSON())
		case 2:
			w.WriteHeader(http.StatusNotModified)
		case 3:
			_, _ = io.WriteString(w, `{"openai":`)
		default:
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	defer catalogServer.Close()

	syncer := NewModelCatalogSyncer(catalogServer.Client(), catalogServer.URL, filepath.Join(t.TempDir(), "model-catalog.json"))
	updated, err := syncer.Sync(context.Background())
	if err != nil {
		t.Fatalf("updated Sync failed: %v", err)
	}
	if updated.Status != ModelCatalogSyncUpdated || updated.ModelCount != len(util.ModelsDevOfficialProviders) ||
		updated.ProviderCount != len(util.ModelsDevOfficialProviders) || updated.SkippedModelCount != 1 ||
		updated.ETag != etag || updated.Duration <= 0 {
		t.Fatalf("updated result = %#v", updated)
	}

	notModified, err := syncer.Sync(context.Background())
	if err != nil {
		t.Fatalf("not-modified Sync failed: %v", err)
	}
	if notModified.Status != ModelCatalogSyncNotModified || notModified.ModelCount != updated.ModelCount ||
		notModified.ProviderCount != updated.ProviderCount || notModified.SkippedModelCount != updated.SkippedModelCount ||
		notModified.ETag != etag || notModified.Duration <= 0 {
		t.Fatalf("not-modified result = %#v", notModified)
	}

	failed, err := syncer.Sync(context.Background())
	if err == nil {
		t.Fatal("invalid catalog Sync returned nil error")
	}
	if failed.Status != ModelCatalogSyncFailed || failed.Stage != "parse" || failed.Err == nil || failed.Duration <= 0 {
		t.Fatalf("failed result = %#v, error = %v", failed, err)
	}
}

func TestModelCatalogSyncNotModifiedDoesNotReadBodyOrRewriteCache(t *testing.T) {
	util.RestoreEmbeddedModelCatalog()
	t.Cleanup(util.RestoreEmbeddedModelCatalog)

	const etag = `"not-modified"`
	var requests atomic.Int32
	unreadBody := &unreadableModelCatalogBody{}
	cachePath := filepath.Join(t.TempDir(), "model-catalog.json")
	syncer := NewModelCatalogSyncer(
		&http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			switch requests.Add(1) {
			case 1:
				return modelCatalogHTTPResponse(req, http.StatusOK, etag, io.NopCloser(strings.NewReader(modelsDevCatalogJSON()))), nil
			case 2:
				if got := req.Header.Get("If-None-Match"); got != etag {
					return modelCatalogHTTPResponse(req, http.StatusBadRequest, "", io.NopCloser(strings.NewReader("missing etag"))), nil
				}
				return modelCatalogHTTPResponse(req, http.StatusNotModified, "", unreadBody), nil
			default:
				return modelCatalogHTTPResponse(req, http.StatusInternalServerError, "", io.NopCloser(strings.NewReader("unexpected request"))), nil
			}
		})},
		"https://models.dev/api.json",
		cachePath,
	)

	if _, err := syncer.Sync(context.Background()); err != nil {
		t.Fatalf("initial Sync failed: %v", err)
	}
	cacheBefore, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read cache before 304: %v", err)
	}
	result, err := syncer.Sync(context.Background())
	if err != nil {
		t.Fatalf("304 Sync failed: %v", err)
	}
	if result.Status != ModelCatalogSyncNotModified {
		t.Fatalf("304 status = %q, want %q", result.Status, ModelCatalogSyncNotModified)
	}
	if got := unreadBody.reads.Load(); got != 0 {
		t.Fatalf("304 body reads = %d, want 0", got)
	}
	cacheAfter, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read cache after 304: %v", err)
	}
	if string(cacheAfter) != string(cacheBefore) {
		t.Fatal("304 response rewrote the cache")
	}
}

func TestModelCatalogSyncRejectsOversizedResponse(t *testing.T) {
	util.RestoreEmbeddedModelCatalog()
	t.Cleanup(util.RestoreEmbeddedModelCatalog)

	catalogServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"too-large"`)
		_, _ = io.WriteString(w, modelsDevCatalogJSON()+strings.Repeat(" ", 16<<20))
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

func TestModelCatalogSyncSetsRequestDeadlineEvenWithoutClientTimeout(t *testing.T) {
	type deadlineObservation struct {
		hasDeadline bool
		remaining   time.Duration
	}
	observed := make(chan deadlineObservation, 1)
	syncer := NewModelCatalogSyncer(
		&http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			deadline, ok := req.Context().Deadline()
			observed <- deadlineObservation{hasDeadline: ok, remaining: time.Until(deadline)}
			return modelCatalogHTTPResponse(req, http.StatusNotModified, "", io.NopCloser(strings.NewReader("must not be read"))), nil
		})},
		"https://models.dev/api.json",
		filepath.Join(t.TempDir(), "model-catalog.json"),
	)

	result, err := syncer.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync failed: %v", err)
	}
	if result.Status != ModelCatalogSyncNotModified {
		t.Fatalf("status = %q, want %q", result.Status, ModelCatalogSyncNotModified)
	}
	observation := <-observed
	if !observation.hasDeadline || observation.remaining <= 0 || observation.remaining > 15*time.Second {
		t.Fatalf("request deadline = has:%t remaining:%s, want (0, 15s]", observation.hasDeadline, observation.remaining)
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
	if result.PersistenceError == nil {
		t.Fatal("cache write failure was omitted from Sync result")
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

func TestModelCatalogSyncServerStartIsIdempotent(t *testing.T) {
	util.RestoreEmbeddedModelCatalog()
	t.Cleanup(util.RestoreEmbeddedModelCatalog)
	t.Setenv("CCLOAD_MODEL_CATALOG_CACHE", filepath.Join(t.TempDir(), "model-catalog.json"))

	requests, firstRequest, extraRequest, release := installBlockingModelCatalogTransport(t)
	defer releaseModelCatalogTransport(release)

	srv := newModelCatalogSyncTestServer(t, 1)
	srv.StartModelCatalogSync()
	waitForModelCatalogRequest(t, firstRequest)
	srv.StartModelCatalogSync()
	assertNoExtraModelCatalogRequest(t, extraRequest)

	close(release)
	waitForModelCatalogETag(t, `"blocking"`)
	if got := requests.Load(); got != 1 {
		t.Fatalf("sequential Start requests = %d, want 1", got)
	}
}

func TestModelCatalogSyncServerConcurrentStartIsIdempotent(t *testing.T) {
	util.RestoreEmbeddedModelCatalog()
	t.Cleanup(util.RestoreEmbeddedModelCatalog)
	t.Setenv("CCLOAD_MODEL_CATALOG_CACHE", filepath.Join(t.TempDir(), "model-catalog.json"))

	requests, firstRequest, extraRequest, release := installBlockingModelCatalogTransport(t)
	defer releaseModelCatalogTransport(release)

	srv := newModelCatalogSyncTestServer(t, 1)
	start := make(chan struct{})
	var starters sync.WaitGroup
	starters.Add(2)
	for range 2 {
		go func() {
			defer starters.Done()
			<-start
			srv.StartModelCatalogSync()
		}()
	}
	close(start)
	starters.Wait()
	waitForModelCatalogRequest(t, firstRequest)
	assertNoExtraModelCatalogRequest(t, extraRequest)

	close(release)
	waitForModelCatalogETag(t, `"blocking"`)
	if got := requests.Load(); got != 1 {
		t.Fatalf("concurrent Start requests = %d, want 1", got)
	}
}

func TestModelCatalogSyncServerDoesNotStartAfterShutdown(t *testing.T) {
	util.RestoreEmbeddedModelCatalog()
	t.Cleanup(util.RestoreEmbeddedModelCatalog)

	const etag = `"after-shutdown"`
	cachePath := filepath.Join(t.TempDir(), "model-catalog.json")
	writeNormalizedCatalogCache(t, cachePath, normalizedCatalogSnapshot(t, etag))
	t.Setenv("CCLOAD_MODEL_CATALOG_CACHE", cachePath)

	srv := newModelCatalogSyncTestServer(t, 0)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}

	srv.StartModelCatalogSync()
	if got := util.CurrentModelCatalogETag(); got != "" {
		t.Fatalf("StartModelCatalogSync after Shutdown loaded cache etag %q", got)
	}
}

func modelsDevCatalogJSON() string {
	providers := make(map[string]any, len(util.ModelsDevOfficialProviders))
	for _, provider := range util.ModelsDevOfficialProviders {
		modelID := provider + "-catalog-test"
		if provider == "openai" {
			modelID = "gpt-catalog-test"
		}
		providers[provider] = map[string]any{
			"id": provider,
			"models": map[string]any{
				modelID: map[string]any{
					"id":           provider + "/" + modelID,
					"release_date": "2026-01-01",
					"modalities":   map[string]any{"output": []string{"text"}},
					"cost":         map[string]any{"input": 1.25, "output": 5.0},
				},
			},
		}
	}
	providers["openai"].(map[string]any)["models"].(map[string]any)["invalid-catalog-model"] = map[string]any{
		"id":         "openai/invalid-catalog-model",
		"modalities": map[string]any{"output": []string{"text"}},
		"cost":       map[string]any{"input": -1, "output": 5.0},
	}

	raw, err := json.Marshal(providers)
	if err != nil {
		panic(err)
	}
	return string(raw)
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

type unreadableModelCatalogBody struct {
	reads atomic.Int32
}

func (b *unreadableModelCatalogBody) Read([]byte) (int, error) {
	b.reads.Add(1)
	return 0, errors.New("304 body must not be read")
}

func (*unreadableModelCatalogBody) Close() error {
	return nil
}

func modelCatalogHTTPResponse(req *http.Request, status int, etag string, body io.ReadCloser) *http.Response {
	header := make(http.Header)
	if etag != "" {
		header.Set("ETag", etag)
	}
	return &http.Response{
		StatusCode: status,
		Header:     header,
		Body:       body,
		Request:    req,
	}
}

func installBlockingModelCatalogTransport(t *testing.T) (*atomic.Int32, <-chan struct{}, <-chan struct{}, chan struct{}) {
	t.Helper()

	originalTransport := http.DefaultTransport
	requests := &atomic.Int32{}
	firstRequest := make(chan struct{})
	extraRequest := make(chan struct{}, 1)
	release := make(chan struct{})
	var firstRequestOnce sync.Once
	http.DefaultTransport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		requestNumber := requests.Add(1)
		if requestNumber == 1 {
			firstRequestOnce.Do(func() { close(firstRequest) })
		} else {
			select {
			case extraRequest <- struct{}{}:
			default:
			}
		}
		<-release
		return modelCatalogHTTPResponse(req, http.StatusOK, `"blocking"`, io.NopCloser(strings.NewReader(modelsDevCatalogJSON()))), nil
	})
	t.Cleanup(func() {
		http.DefaultTransport = originalTransport
	})
	return requests, firstRequest, extraRequest, release
}

func releaseModelCatalogTransport(release chan struct{}) {
	select {
	case <-release:
	default:
		close(release)
	}
}

func waitForModelCatalogRequest(t *testing.T, request <-chan struct{}) {
	t.Helper()
	select {
	case <-request:
	case <-time.After(time.Second):
		t.Fatal("model catalog request did not start")
	}
}

func assertNoExtraModelCatalogRequest(t *testing.T, request <-chan struct{}) {
	t.Helper()
	select {
	case <-request:
		t.Fatal("StartModelCatalogSync started a second request")
	case <-time.After(100 * time.Millisecond):
	}
}

func waitForModelCatalogETag(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for util.CurrentModelCatalogETag() != want && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := util.CurrentModelCatalogETag(); got != want {
		t.Fatalf("installed etag = %q, want %q", got, want)
	}
}

type modelCatalogSyncSettingStore struct {
	storage.Store
	intervalHours string
}

func (s modelCatalogSyncSettingStore) ListAllSettings(ctx context.Context) ([]*model.SystemSetting, error) {
	settings, err := s.Store.ListAllSettings(ctx)
	if err != nil {
		return nil, err
	}
	return append(settings, &model.SystemSetting{
		Key:   "model_catalog_sync_interval_hours",
		Value: s.intervalHours,
	}), nil
}

func (s modelCatalogSyncSettingStore) Close() error {
	if closer, ok := s.Store.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}

func newModelCatalogSyncTestServer(t *testing.T, intervalHours float64) *Server {
	t.Helper()

	store, err := storage.CreateSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("CreateSQLiteStore failed: %v", err)
	}
	srv := NewServer(modelCatalogSyncSettingStore{
		Store:         store,
		intervalHours: strconv.FormatFloat(intervalHours, 'f', -1, 64),
	})
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			t.Errorf("Server.Shutdown failed: %v", err)
		}
	})

	return srv
}
