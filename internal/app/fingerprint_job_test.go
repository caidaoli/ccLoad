package app

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/util"
)

// openAIReply returns a minimal OpenAI JSON response with the given number as content.
func openAIReply(content string) []byte {
	return []byte(fmt.Sprintf(
		`{"id":"chatcmpl-test","choices":[{"message":{"content":%q}}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`,
		content,
	))
}

// createFingerprintChannel creates a channel + key in the test server store and returns the channel ID.
func createFingerprintChannel(t *testing.T, srv *Server, upstreamURL string) int64 {
	t.Helper()
	ctx := context.Background()
	cfg := &model.Config{
		Name:         "fp-test-channel",
		URL:          upstreamURL,
		Priority:     1,
		ChannelType:  "openai",
		ModelEntries: []model.ModelEntry{{Model: "fp-model"}},
		Enabled:      true,
	}
	created, err := srv.store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-fp-test"},
	}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}
	return created.ID
}

// pollJob polls until the job is no longer "running" or timeout.
func pollJob(t *testing.T, mgr *FingerprintJobManager, jobID string) *FingerprintJobView {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		v, ok := mgr.Get(jobID)
		if !ok {
			t.Fatalf("job %s disappeared", jobID)
		}
		if v.Status != "running" {
			return v
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("job %s still running after 30s", jobID)
	return nil
}

// TestFingerprintCalibrateAndTest exercises the full calibrate→test flow with a mock upstream.
func TestFingerprintCalibrateAndTest(t *testing.T) {
	// Upstream returns numbers cycling through 1..50 to produce a valid (>= 40 valid samples) distribution.
	var counter atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(counter.Add(1)%50) + 1
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(openAIReply(fmt.Sprintf("%d", n)))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	channelID := createFingerprintChannel(t, srv, upstream.URL)

	mgr := srv.fingerprintJobs

	// ---- Calibrate ----
	jobID, err := mgr.StartCalibrate(srv, calibrateReq{
		Name:        "test-baseline",
		ChannelID:   channelID,
		Model:       "fp-model",
		Iterations:  50, // minimum valid
		Concurrency: 5,
		KeyIndex:    0,
	})
	if err != nil {
		t.Fatalf("StartCalibrate: %v", err)
	}
	if jobID == "" || len(jobID) < 5 {
		t.Fatalf("expected non-empty job id, got %q", jobID)
	}
	if jobID[:4] != "fpj_" {
		t.Fatalf("job id prefix want fpj_, got %q", jobID[:4])
	}

	view := pollJob(t, mgr, jobID)
	if view.Status != "succeeded" {
		t.Fatalf("calibrate status=%s error=%s", view.Status, view.Error)
	}
	if view.Progress.Success < util.FingerprintMinValidSamples {
		t.Fatalf("expected >= %d successes, got %d", util.FingerprintMinValidSamples, view.Progress.Success)
	}
	fp, ok := view.Result.(*model.ModelFingerprint)
	if !ok || fp == nil {
		t.Fatalf("result is not *model.ModelFingerprint: %T", view.Result)
	}
	if fp.ID == 0 {
		t.Fatalf("persisted fingerprint must have non-zero ID")
	}
	if fp.SampleCount < util.FingerprintMinValidSamples {
		t.Fatalf("sample_count=%d < %d", fp.SampleCount, util.FingerprintMinValidSamples)
	}
	if fp.PromptVersion != util.FingerprintPromptVersion {
		t.Fatalf("prompt_version=%s, want %s", fp.PromptVersion, util.FingerprintPromptVersion)
	}

	// ---- Test against baseline (same upstream ⇒ high score) ----
	jobID2, err := mgr.StartTest(srv, testFingerprintReq{
		ChannelID:     channelID,
		Model:         "fp-model",
		FingerprintID: &fp.ID,
		Iterations:    50,
		Concurrency:   5,
		KeyIndex:      0,
	})
	if err != nil {
		t.Fatalf("StartTest: %v", err)
	}

	view2 := pollJob(t, mgr, jobID2)
	if view2.Status != "succeeded" {
		t.Fatalf("test status=%s error=%s", view2.Status, view2.Error)
	}
	result, ok := view2.Result.(*FingerprintTestResult)
	if !ok || result == nil {
		t.Fatalf("test result is not *FingerprintTestResult: %T", view2.Result)
	}
	if len(result.Matches) == 0 {
		t.Fatalf("expected at least one match")
	}
	if result.Matches[0].Score < 0.3 {
		t.Fatalf("expected reasonable score against same upstream, got %.4f", result.Matches[0].Score)
	}

	logs, err := srv.store.ListLogs(context.Background(), time.Now().Add(-time.Minute), 100, 0, &model.LogFilter{
		LogSource: model.LogSourceManualTest,
	})
	if err != nil {
		t.Fatalf("ListLogs: %v", err)
	}
	if len(logs) != 50 {
		t.Fatalf("expected one log per fingerprint comparison call, got %d", len(logs))
	}
	for _, entry := range logs {
		if entry.ChannelID != channelID {
			t.Fatalf("log channel_id=%d, want %d", entry.ChannelID, channelID)
		}
		if entry.Model != "fp-model" {
			t.Fatalf("log model=%q, want fp-model", entry.Model)
		}
		if entry.StatusCode != http.StatusOK {
			t.Fatalf("log status_code=%d, want %d", entry.StatusCode, http.StatusOK)
		}
	}
}

// TestFingerprintJobInsufficientSamples checks that a 500-only upstream fails the job without writing a DB row.
func TestFingerprintJobInsufficientSamples(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"type":"server_error","message":"always fails"}}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	channelID := createFingerprintChannel(t, srv, upstream.URL)
	mgr := srv.fingerprintJobs

	jobID, err := mgr.StartCalibrate(srv, calibrateReq{
		Name:        "should-fail",
		ChannelID:   channelID,
		Model:       "fp-model",
		Iterations:  50,
		Concurrency: 2,
	})
	if err != nil {
		t.Fatalf("StartCalibrate: %v", err)
	}

	view := pollJob(t, mgr, jobID)
	if view.Status != "failed" {
		t.Fatalf("want status=failed, got=%s error=%s", view.Status, view.Error)
	}
	if view.Result != nil {
		t.Fatalf("no result should be set on failure")
	}

	// Verify no DB row was created
	all, err := srv.store.ListModelFingerprints(context.Background())
	if err != nil {
		t.Fatalf("ListModelFingerprints: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("expected no persisted fingerprints, got %d", len(all))
	}
}

// TestFingerprintJobCancel verifies that cancellation stops the job cleanly.
func TestFingerprintJobCancel(t *testing.T) {
	var mu sync.Mutex
	var started int
	// Upstream blocks until request context is done.
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		started++
		mu.Unlock()
		// Block until client cancels
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	channelID := createFingerprintChannel(t, srv, upstream.URL)
	mgr := srv.fingerprintJobs

	jobID, err := mgr.StartCalibrate(srv, calibrateReq{
		Name:        "cancel-test",
		ChannelID:   channelID,
		Model:       "fp-model",
		Iterations:  50,
		Concurrency: 2,
	})
	if err != nil {
		t.Fatalf("StartCalibrate: %v", err)
	}

	// Wait until at least one worker has started, then cancel.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := started
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := mgr.Cancel(jobID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	view := pollJob(t, mgr, jobID)
	if view.Status != "cancelled" && view.Status != "failed" {
		// Both are acceptable: cancelled if context fired before sampling, failed if not enough samples.
		t.Fatalf("expected cancelled or failed after cancel, got %s", view.Status)
	}
}

// TestFingerprintJobManagerMaxRunning ensures a third concurrent job is rejected.
func TestFingerprintJobManagerMaxRunning(t *testing.T) {
	// Upstream that blocks forever (so jobs stay running).
	var mu sync.Mutex
	started := 0
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		started++
		mu.Unlock()
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	channelID := createFingerprintChannel(t, srv, upstream.URL)
	mgr := srv.fingerprintJobs

	// Job 1
	id1, err := mgr.StartCalibrate(srv, calibrateReq{Name: "j1", ChannelID: channelID, Model: "fp-model", Iterations: 50, Concurrency: 2})
	if err != nil {
		t.Fatalf("job1: %v", err)
	}
	// Job 2
	id2, err := mgr.StartCalibrate(srv, calibrateReq{Name: "j2", ChannelID: channelID, Model: "fp-model", Iterations: 50, Concurrency: 2})
	if err != nil {
		t.Fatalf("job2: %v", err)
	}
	// Job 3 should be rejected
	_, err = mgr.StartCalibrate(srv, calibrateReq{Name: "j3", ChannelID: channelID, Model: "fp-model", Iterations: 50, Concurrency: 2})
	if err == nil {
		t.Fatalf("expected error for 3rd concurrent job")
	}

	// Clean up
	_ = mgr.Cancel(id1)
	_ = mgr.Cancel(id2)
}

// TestFingerprintTestNoBaselines verifies that starting a test with no baselines yields failed status.
func TestFingerprintTestNoBaselines(t *testing.T) {
	var counter atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(counter.Add(1)%50) + 1
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(openAIReply(fmt.Sprintf("%d", n)))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	channelID := createFingerprintChannel(t, srv, upstream.URL)
	mgr := srv.fingerprintJobs

	jobID, err := mgr.StartTest(srv, testFingerprintReq{
		ChannelID:   channelID,
		Model:       "fp-model",
		Iterations:  50,
		Concurrency: 5,
	})
	if err != nil {
		t.Fatalf("StartTest: %v", err)
	}

	view := pollJob(t, mgr, jobID)
	if view.Status != "failed" {
		t.Fatalf("expected failed when no baselines, got %s: %s", view.Status, view.Error)
	}
}
