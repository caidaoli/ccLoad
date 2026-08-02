package app

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
)

// ────────────────────────────────────────────────────────────────────────────
// helpers

func createFPChannel(t *testing.T, srv *Server, upstreamURL, modelName string) int64 {
	t.Helper()
	ctx := context.Background()
	cfg := &model.Config{
		Name:                  "fp-api-channel",
		URLs:                  model.ChannelURLs{{URL: upstreamURL}},
		Priority:              1,
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
		ModelEntries:          []model.ModelEntry{{Model: modelName}},
		Enabled:               true,
	}
	created, err := srv.store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-fp-api"},
	}); err != nil {
		t.Fatalf("CreateAPIKeysBatch: %v", err)
	}
	return created.ID
}

// cyclicUpstream returns an upstream that emits numbers cycling 1..50.
func cyclicUpstreamFP(t *testing.T) *testHTTPServer {
	t.Helper()
	var counter atomic.Int32
	return newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(counter.Add(1)%50) + 1
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(openAIReply(fmt.Sprintf("%d", n)))
	}))
}

func pollFPJobViaHandler(t *testing.T, srv *Server, jobID string) *FingerprintJobView {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/fingerprints/jobs/"+jobID, nil))
		c.Params = gin.Params{{Key: "id", Value: jobID}}
		srv.HandleFingerprintJob(c)
		if w.Code != http.StatusOK {
			t.Fatalf("HandleFingerprintJob returned %d", w.Code)
		}
		var resp APIResponse[FingerprintJobView]
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		if resp.Data.Status != "running" {
			return &resp.Data
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("job %s still running after 30s", jobID)
	return nil
}

func TestFingerprintAPI_HistoryUsesCurrentDistributionScore(t *testing.T) {
	srv := newInMemoryServer(t)
	inflatedOldScore := 0.5*1.0 + 0.5*(0.8379*math.Exp(-0.2482))
	betterDistributionOldScore := 0.5*0.4 + 0.5*(0.75*math.Exp(-0.02))
	matchesJSON, err := json.Marshal([]FingerprintMatch{
		{
			Score:            inflatedOldScore,
			CosineSimilarity: 0.8379,
			JSDivergence:     0.2482,
			ModeScore:        1,
			ModeMatch:        true,
			Baseline:         model.ModelFingerprint{Name: "same-mode"},
		},
		{
			Score:            betterDistributionOldScore,
			CosineSimilarity: 0.75,
			JSDivergence:     0.02,
			ModeScore:        0.4,
			Baseline:         model.ModelFingerprint{Name: "better-distribution"},
		},
	})
	if err != nil {
		t.Fatalf("marshal matches: %v", err)
	}
	record := &model.FingerprintTestRecord{
		Model:       "gemini-3.1-flash-lite",
		SampleCount: 100,
		BestScore:   inflatedOldScore,
		MatchesJSON: string(matchesJSON),
	}
	if err := srv.store.CreateFingerprintTestResult(context.Background(), record); err != nil {
		t.Fatalf("create history: %v", err)
	}

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/fingerprints/test-results", nil))
	srv.HandleListFingerprintTestResults(c)
	if w.Code != http.StatusOK {
		t.Fatalf("list history: want 200 got %d — %s", w.Code, w.Body.String())
	}
	var resp APIResponse[[]*model.FingerprintTestRecord]
	mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
	if len(resp.Data) != 1 || len(resp.Data[0].Matches) != 2 {
		t.Fatalf("unexpected history response: %+v", resp.Data)
	}
	want := util.FingerprintDistributionScore(0.75, 0.02)
	if math.Abs(resp.Data[0].BestScore-want) > 1e-12 {
		t.Fatalf("best score=%f, want current score=%f", resp.Data[0].BestScore, want)
	}
	match, ok := resp.Data[0].Matches[0].(map[string]any)
	if !ok {
		t.Fatalf("unexpected match type %T", resp.Data[0].Matches[0])
	}
	if score, ok := match["score"].(float64); !ok || math.Abs(score-want) > 1e-12 {
		t.Fatalf("match score=%v, want current score=%f", match["score"], want)
	}
	baseline, ok := match["baseline"].(map[string]any)
	if !ok || baseline["name"] != "better-distribution" {
		t.Fatalf("history was not reordered by current score: %v", match["baseline"])
	}
}

// ────────────────────────────────────────────────────────────────────────────
// TestFingerprintAPI_ListGetDelete

func TestFingerprintAPI_ListGetDelete(t *testing.T) {
	srv := newInMemoryServer(t)
	ctx := context.Background()

	// empty list
	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/fingerprints", nil))
	srv.HandleListFingerprints(c)
	if w.Code != http.StatusOK {
		t.Fatalf("list empty: want 200 got %d", w.Code)
	}
	var listResp APIResponse[json.RawMessage]
	mustUnmarshalJSON(t, w.Body.Bytes(), &listResp)
	if !listResp.Success {
		t.Fatalf("list empty: success=false")
	}

	// insert a fingerprint
	dist := util.FingerprintDistribution(make([]int, 50))
	fp := &model.ModelFingerprint{
		Name:          "test-fp",
		Model:         "gpt-test",
		SampleCount:   50,
		Distribution:  dist,
		PromptVersion: util.FingerprintPromptVersion,
	}
	created, err := srv.store.CreateModelFingerprint(ctx, fp)
	if err != nil {
		t.Fatalf("CreateModelFingerprint: %v", err)
	}
	if created.ID == 0 {
		t.Fatal("created fingerprint id=0")
	}

	// GET :id
	c, w = newTestContext(t, newRequest(http.MethodGet, "/admin/fingerprints/"+fmt.Sprint(created.ID), nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(created.ID)}}
	srv.HandleGetFingerprint(c)
	if w.Code != http.StatusOK {
		t.Fatalf("get: want 200 got %d — %s", w.Code, w.Body.String())
	}

	// GET :id not found
	c, w = newTestContext(t, newRequest(http.MethodGet, "/admin/fingerprints/999", nil))
	c.Params = gin.Params{{Key: "id", Value: "999"}}
	srv.HandleGetFingerprint(c)
	if w.Code != http.StatusNotFound {
		t.Fatalf("get 404: want 404 got %d", w.Code)
	}

	// DELETE :id
	c, w = newTestContext(t, newRequest(http.MethodDelete, "/admin/fingerprints/"+fmt.Sprint(created.ID), nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(created.ID)}}
	srv.HandleDeleteFingerprint(c)
	if w.Code != http.StatusOK {
		t.Fatalf("delete: want 200 got %d — %s", w.Code, w.Body.String())
	}

	// now GET should 404
	c, w = newTestContext(t, newRequest(http.MethodGet, "/admin/fingerprints/"+fmt.Sprint(created.ID), nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(created.ID)}}
	srv.HandleGetFingerprint(c)
	if w.Code != http.StatusNotFound {
		t.Fatalf("get after delete: want 404 got %d", w.Code)
	}
}

// ────────────────────────────────────────────────────────────────────────────
// TestFingerprintAPI_CalibrateValidation

func TestFingerprintAPI_CalibrateValidation(t *testing.T) {
	srv := newInMemoryServer(t)
	upstream := cyclicUpstreamFP(t)
	defer upstream.Close()
	channelID := createFPChannel(t, srv, upstream.URL, "fp-model")

	cases := []struct {
		name   string
		body   map[string]any
		status int
	}{
		{
			name:   "missing name",
			body:   map[string]any{"channel_id": channelID, "model": "fp-model", "client_protocol": "openai"},
			status: http.StatusBadRequest,
		},
		{
			name:   "missing model",
			body:   map[string]any{"name": "n", "channel_id": channelID, "client_protocol": "openai"},
			status: http.StatusBadRequest,
		},
		{
			name:   "missing client protocol",
			body:   map[string]any{"name": "n", "channel_id": channelID, "model": "fp-model"},
			status: http.StatusBadRequest,
		},
		{
			name:   "invalid client protocol",
			body:   map[string]any{"name": "n", "channel_id": channelID, "model": "fp-model", "client_protocol": "unknown"},
			status: http.StatusBadRequest,
		},
		{
			name:   "channel not found",
			body:   map[string]any{"name": "n", "channel_id": int64(9999), "model": "fp-model", "client_protocol": "openai"},
			status: http.StatusBadRequest,
		},
		{
			name:   "model not in channel",
			body:   map[string]any{"name": "n", "channel_id": channelID, "model": "other-model", "client_protocol": "openai"},
			status: http.StatusBadRequest,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/fingerprints/calibrate", tt.body))
			srv.HandleCalibrateFingerprint(c)
			if w.Code != tt.status {
				t.Errorf("want %d got %d — %s", tt.status, w.Code, w.Body.String())
			}
		})
	}
}

func TestFingerprintAPI_CalibrateRejectsPersistedDuplicateName(t *testing.T) {
	srv := newInMemoryServer(t)
	upstream := cyclicUpstreamFP(t)
	defer upstream.Close()
	channelID := createFPChannel(t, srv, upstream.URL, "fp-model")

	if _, err := srv.store.CreateModelFingerprint(context.Background(), &model.ModelFingerprint{
		Name:          "existing-baseline",
		Model:         "fp-model",
		Distribution:  []float64{},
		PromptVersion: util.FingerprintPromptVersion,
	}); err != nil {
		t.Fatalf("CreateModelFingerprint: %v", err)
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/fingerprints/calibrate", map[string]any{
		"name":            " existing-baseline ",
		"channel_id":      channelID,
		"model":           "fp-model",
		"client_protocol": "openai",
	}))
	srv.HandleCalibrateFingerprint(c)
	if w.Code != http.StatusConflict {
		t.Fatalf("want 409 got %d — %s", w.Code, w.Body.String())
	}
	var resp APIResponse[any]
	mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
	if resp.Error != `baseline name "existing-baseline" already exists or is being calibrated; choose a different name` {
		t.Fatalf("error=%q", resp.Error)
	}
}

func TestFingerprintAPI_CalibrateRejectsRunningDuplicateName(t *testing.T) {
	srv := newInMemoryServer(t)
	blocked := make(chan struct{})
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-blocked
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer func() {
		close(blocked)
		upstream.Close()
	}()
	channelID := createFPChannel(t, srv, upstream.URL, "fp-model")
	body := map[string]any{
		"name":            "running-baseline",
		"channel_id":      channelID,
		"model":           "fp-model",
		"client_protocol": "openai",
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/fingerprints/calibrate", body))
	srv.HandleCalibrateFingerprint(c)
	if w.Code != http.StatusOK {
		t.Fatalf("first calibrate: want 200 got %d — %s", w.Code, w.Body.String())
	}

	c, w = newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/fingerprints/calibrate", body))
	srv.HandleCalibrateFingerprint(c)
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate calibrate: want 409 got %d — %s", w.Code, w.Body.String())
	}
}

// ────────────────────────────────────────────────────────────────────────────
// TestFingerprintAPI_TestValidation_NoBaseline

func TestFingerprintAPI_TestValidation_NoBaseline(t *testing.T) {
	srv := newInMemoryServer(t)
	upstream := cyclicUpstreamFP(t)
	defer upstream.Close()
	channelID := createFPChannel(t, srv, upstream.URL, "fp-model")

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/fingerprints/test", map[string]any{
		"channel_id":      channelID,
		"model":           "fp-model",
		"client_protocol": "openai",
	}))
	srv.HandleTestFingerprint(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 (no baselines) got %d — %s", w.Code, w.Body.String())
	}
}

func TestFingerprintAPI_TestValidation_ClientProtocol(t *testing.T) {
	srv := newInMemoryServer(t)
	upstream := cyclicUpstreamFP(t)
	defer upstream.Close()
	channelID := createFPChannel(t, srv, upstream.URL, "fp-model")

	for _, clientProtocol := range []string{"", "unknown"} {
		c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/fingerprints/test", map[string]any{
			"channel_id":      channelID,
			"model":           "fp-model",
			"client_protocol": clientProtocol,
		}))
		srv.HandleTestFingerprint(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("client_protocol=%q: want 400 got %d — %s", clientProtocol, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "client_protocol") {
			t.Fatalf("client_protocol=%q: error must identify client_protocol — %s", clientProtocol, w.Body.String())
		}
	}
}

// ────────────────────────────────────────────────────────────────────────────
// TestFingerprintAPI_JobNotFound

func TestFingerprintAPI_JobNotFound(t *testing.T) {
	srv := newInMemoryServer(t)

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/fingerprints/jobs/nonexistent", nil))
	c.Params = gin.Params{{Key: "id", Value: "nonexistent"}}
	srv.HandleFingerprintJob(c)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404 got %d", w.Code)
	}

	c, w = newTestContext(t, newRequest(http.MethodPost, "/admin/fingerprints/jobs/nonexistent/cancel", nil))
	c.Params = gin.Params{{Key: "id", Value: "nonexistent"}}
	srv.HandleCancelFingerprintJob(c)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404 got %d", w.Code)
	}
}

// ────────────────────────────────────────────────────────────────────────────
// TestFingerprintAPI_TooManyJobs

func TestFingerprintAPI_TooManyJobs(t *testing.T) {
	srv := newInMemoryServer(t)
	// block upstream so jobs stay running
	blocked := make(chan struct{})
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blocked
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer func() { close(blocked); upstream.Close() }()

	channelID := createFPChannel(t, srv, upstream.URL, "fp-model")

	// fill all slots (maxRunning=2)
	for i := 0; i < 2; i++ {
		c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/fingerprints/calibrate", map[string]any{
			"name":            fmt.Sprintf("baseline-%d", i),
			"channel_id":      channelID,
			"model":           "fp-model",
			"client_protocol": "openai",
		}))
		srv.HandleCalibrateFingerprint(c)
		if w.Code != http.StatusOK {
			t.Fatalf("slot %d: want 200 got %d — %s", i, w.Code, w.Body.String())
		}
	}

	// third request should 429
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/fingerprints/calibrate", map[string]any{
		"name":            "overflow",
		"channel_id":      channelID,
		"model":           "fp-model",
		"client_protocol": "openai",
	}))
	srv.HandleCalibrateFingerprint(c)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429 got %d — %s", w.Code, w.Body.String())
	}
}

// ────────────────────────────────────────────────────────────────────────────
// TestFingerprintAPI_StreamCalibrateAndTestMergesBeforeValidation exercises the public
// calibrate/test APIs and proves that stream deltas are merged before range validation.

func TestFingerprintAPI_StreamCalibrateAndTestMergesBeforeValidation(t *testing.T) {
	var upstreamCalls atomic.Int32
	var nonStreamingRequests atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode upstream request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if stream, _ := payload["stream"].(bool); !stream {
			nonStreamingRequests.Add(1)
		}

		call := upstreamCalls.Add(1)
		chunks := []string{"1", "23"}
		if call > 40 && call <= 50 {
			// Both chunks are valid numbers in isolation, but the merged value 356 is out of range.
			chunks = []string{"3", "56"}
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, chunk := range chunks {
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", chunk)
		}
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	channelID := createFPChannel(t, srv, upstream.URL, "fp-model")

	// ── Calibrate ──────────────────────────────────────────────────────────
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/fingerprints/calibrate", map[string]any{
		"name":            "integration-baseline",
		"channel_id":      channelID,
		"model":           "fp-model",
		"client_protocol": "openai",
		"stream":          true,
		"iterations":      50,
		"concurrency":     5,
	}))
	srv.HandleCalibrateFingerprint(c)
	if w.Code != http.StatusOK {
		t.Fatalf("calibrate: want 200 got %d — %s", w.Code, w.Body.String())
	}
	var calResp APIResponse[map[string]string]
	mustUnmarshalJSON(t, w.Body.Bytes(), &calResp)
	if !calResp.Success {
		t.Fatalf("calibrate: success=false")
	}
	jobID := calResp.Data["job_id"]
	if jobID == "" {
		t.Fatal("calibrate: empty job_id")
	}

	// ── Poll calibrate job ──────────────────────────────────────────────────
	jobView := pollFPJobViaHandler(t, srv, jobID)
	if jobView.Status != "succeeded" {
		t.Fatalf("calibrate job status want succeeded got %s (err=%s)", jobView.Status, jobView.Error)
	}
	if jobView.Progress.Success != 40 || jobView.Progress.Failed != 10 {
		t.Fatalf("calibrate progress=%+v, want 40 merged-valid and 10 merged-invalid samples", jobView.Progress)
	}

	// ── List fingerprints — should contain our new baseline ─────────────────
	c, w = newTestContext(t, newRequest(http.MethodGet, "/admin/fingerprints", nil))
	srv.HandleListFingerprints(c)
	if w.Code != http.StatusOK {
		t.Fatalf("list: want 200 got %d", w.Code)
	}
	var listResp APIResponse[[]*model.ModelFingerprint]
	mustUnmarshalJSON(t, w.Body.Bytes(), &listResp)
	if len(listResp.Data) == 0 {
		t.Fatal("list: expected ≥1 fingerprint after calibrate")
	}
	fingerprint := listResp.Data[0]
	if fingerprint.SampleCount != 40 || len(fingerprint.RawData) != 40 {
		t.Fatalf("calibrate samples=%d raw=%d, want 40", fingerprint.SampleCount, len(fingerprint.RawData))
	}
	for _, sample := range fingerprint.RawData {
		if sample != 123 {
			t.Fatalf("calibrate sample=%d, want merged stream value 123", sample)
		}
	}
	fpID := fingerprint.ID

	// ── Test ───────────────────────────────────────────────────────────────
	c, w = newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/fingerprints/test", map[string]any{
		"channel_id":      channelID,
		"model":           "fp-model",
		"client_protocol": "openai",
		"stream":          true,
		"fingerprint_id":  fpID,
		"iterations":      50,
		"concurrency":     5,
	}))
	srv.HandleTestFingerprint(c)
	if w.Code != http.StatusOK {
		t.Fatalf("test: want 200 got %d — %s", w.Code, w.Body.String())
	}
	var testResp APIResponse[map[string]string]
	mustUnmarshalJSON(t, w.Body.Bytes(), &testResp)
	testJobID := testResp.Data["job_id"]
	if testJobID == "" {
		t.Fatal("test: empty job_id")
	}

	// ── Poll test job ───────────────────────────────────────────────────────
	testView := pollFPJobViaHandler(t, srv, testJobID)
	if testView.Status != "succeeded" {
		t.Fatalf("test job status want succeeded got %s (err=%s)", testView.Status, testView.Error)
	}
	if testView.Progress.Success != 50 || testView.Progress.Failed != 0 {
		t.Fatalf("test progress=%+v, want 50 merged-valid samples", testView.Progress)
	}

	// result should have non-zero score
	resultBytes, _ := json.Marshal(testView.Result)
	var result FingerprintTestResult
	_ = json.Unmarshal(resultBytes, &result)
	if len(result.Matches) == 0 {
		t.Fatal("test: expected ≥1 match in result")
	}
	if result.Matches[0].Score <= 0 {
		t.Fatalf("test: expected positive score, got %f", result.Matches[0].Score)
	}
	if len(result.RawData) != 50 {
		t.Fatalf("test raw samples=%d, want 50", len(result.RawData))
	}
	for _, sample := range result.RawData {
		if sample != 123 {
			t.Fatalf("test sample=%d, want merged stream value 123", sample)
		}
	}
	if got := upstreamCalls.Load(); got != 100 {
		t.Fatalf("upstream calls=%d, want 100", got)
	}
	if got := nonStreamingRequests.Load(); got != 0 {
		t.Fatalf("non-streaming upstream requests=%d, want 0", got)
	}

	// ── Delete fingerprint ──────────────────────────────────────────────────
	c, w = newTestContext(t, newRequest(http.MethodDelete, "/admin/fingerprints/"+fmt.Sprint(fpID), nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(fpID)}}
	srv.HandleDeleteFingerprint(c)
	if w.Code != http.StatusOK {
		t.Fatalf("delete: want 200 got %d — %s", w.Code, w.Body.String())
	}
}

// ────────────────────────────────────────────────────────────────────────────
// TestFingerprintAPI_CancelJob

func TestFingerprintAPI_CancelJob(t *testing.T) {
	// upstream blocks indefinitely so job stays running
	blocked := make(chan struct{})
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blocked
		w.WriteHeader(http.StatusInternalServerError)
	}))

	srv := newInMemoryServer(t)
	channelID := createFPChannel(t, srv, upstream.URL, "fp-model")

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/fingerprints/calibrate", map[string]any{
		"name":            "cancel-test",
		"channel_id":      channelID,
		"model":           "fp-model",
		"client_protocol": "openai",
	}))
	srv.HandleCalibrateFingerprint(c)
	if w.Code != http.StatusOK {
		t.Fatalf("calibrate: want 200 got %d — %s", w.Code, w.Body.String())
	}
	var resp APIResponse[map[string]string]
	mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
	jobID := resp.Data["job_id"]

	// cancel
	c, w = newTestContext(t, newRequest(http.MethodPost, "/admin/fingerprints/jobs/"+jobID+"/cancel", nil))
	c.Params = gin.Params{{Key: "id", Value: jobID}}
	srv.HandleCancelFingerprintJob(c)
	if w.Code != http.StatusOK {
		t.Fatalf("cancel: want 200 got %d — %s", w.Code, w.Body.String())
	}

	// unblock upstream so goroutines can drain
	close(blocked)
	upstream.Close()
}
