package app

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
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
		Name:         "fp-api-channel",
		URL:          upstreamURL,
		Priority:     1,
		ChannelType:  "openai",
		ModelEntries: []model.ModelEntry{{Model: modelName}},
		Enabled:      true,
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
			body:   map[string]any{"channel_id": channelID, "model": "fp-model"},
			status: http.StatusBadRequest,
		},
		{
			name:   "missing model",
			body:   map[string]any{"name": "n", "channel_id": channelID},
			status: http.StatusBadRequest,
		},
		{
			name:   "channel not found",
			body:   map[string]any{"name": "n", "channel_id": int64(9999), "model": "fp-model"},
			status: http.StatusBadRequest,
		},
		{
			name:   "model not in channel",
			body:   map[string]any{"name": "n", "channel_id": channelID, "model": "other-model"},
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

// ────────────────────────────────────────────────────────────────────────────
// TestFingerprintAPI_TestValidation_NoBaseline

func TestFingerprintAPI_TestValidation_NoBaseline(t *testing.T) {
	srv := newInMemoryServer(t)
	upstream := cyclicUpstreamFP(t)
	defer upstream.Close()
	channelID := createFPChannel(t, srv, upstream.URL, "fp-model")

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/fingerprints/test", map[string]any{
		"channel_id": channelID,
		"model":      "fp-model",
	}))
	srv.HandleTestFingerprint(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 (no baselines) got %d — %s", w.Code, w.Body.String())
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
			"name":       fmt.Sprintf("baseline-%d", i),
			"channel_id": channelID,
			"model":      "fp-model",
		}))
		srv.HandleCalibrateFingerprint(c)
		if w.Code != http.StatusOK {
			t.Fatalf("slot %d: want 200 got %d — %s", i, w.Code, w.Body.String())
		}
	}

	// third request should 429
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/fingerprints/calibrate", map[string]any{
		"name":       "overflow",
		"channel_id": channelID,
		"model":      "fp-model",
	}))
	srv.HandleCalibrateFingerprint(c)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429 got %d — %s", w.Code, w.Body.String())
	}
}

// ────────────────────────────────────────────────────────────────────────────
// TestFingerprintAPI_CalibrateAndTest (integration: calibrate → poll → test → poll → delete)

func TestFingerprintAPI_CalibrateAndTest(t *testing.T) {
	upstream := cyclicUpstreamFP(t)
	defer upstream.Close()

	srv := newInMemoryServer(t)
	channelID := createFPChannel(t, srv, upstream.URL, "fp-model")

	// ── Calibrate ──────────────────────────────────────────────────────────
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/fingerprints/calibrate", map[string]any{
		"name":        "integration-baseline",
		"channel_id":  channelID,
		"model":       "fp-model",
		"iterations":  50,
		"concurrency": 5,
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
	fpID := listResp.Data[0].ID

	// ── Test ───────────────────────────────────────────────────────────────
	c, w = newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/fingerprints/test", map[string]any{
		"channel_id":     channelID,
		"model":          "fp-model",
		"fingerprint_id": fpID,
		"iterations":     50,
		"concurrency":    5,
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
		"name":       "cancel-test",
		"channel_id": channelID,
		"model":      "fp-model",
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
