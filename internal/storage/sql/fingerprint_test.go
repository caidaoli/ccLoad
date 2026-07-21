package sql_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

func TestModelFingerprintCRUDAndClearChannel(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	store, err := storage.CreateSQLiteStore(filepath.Join(tmp, "fingerprint.db"))
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()

	chID := int64(1)
	fp := &model.ModelFingerprint{
		Name:         "test-baseline",
		ChannelID:    &chID,
		ChannelName:  "test-channel",
		Model:        "gpt-4",
		ActualModel:  "gpt-4-0613",
		ChannelType:  "openai",
		SampleCount:  5,
		Distribution: []float64{0.1, 0.2, 0.3, 0.2, 0.2},
		Stats: model.FingerprintStats{
			Mean:      10.5,
			Median:    10.0,
			StdDev:    1.2,
			Min:       8,
			Max:       13,
			Unique:    5,
			Mode:      10,
			ModeCount: 2,
		},
		RawData:       []int{8, 10, 10, 12, 13},
		PromptVersion: "v1",
	}

	// Create
	created, err := store.CreateModelFingerprint(ctx, fp)
	if err != nil {
		t.Fatalf("CreateModelFingerprint: %v", err)
	}
	if created.ID == 0 {
		t.Fatal("expected non-zero ID after create")
	}

	// Get by ID
	got, err := store.GetModelFingerprint(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetModelFingerprint: %v", err)
	}
	if got.Name != fp.Name {
		t.Errorf("name: got %q, want %q", got.Name, fp.Name)
	}
	if got.ChannelID == nil || *got.ChannelID != chID {
		t.Errorf("channel_id: got %v, want %d", got.ChannelID, chID)
	}
	if got.Model != fp.Model {
		t.Errorf("model: got %q, want %q", got.Model, fp.Model)
	}
	if len(got.Distribution) != len(fp.Distribution) {
		t.Errorf("distribution len: got %d, want %d", len(got.Distribution), len(fp.Distribution))
	}
	if got.Stats.Mode != fp.Stats.Mode {
		t.Errorf("stats.mode: got %d, want %d", got.Stats.Mode, fp.Stats.Mode)
	}
	if len(got.RawData) != len(fp.RawData) {
		t.Errorf("raw_data len: got %d, want %d", len(got.RawData), len(fp.RawData))
	}
	if got.PromptVersion != "v1" {
		t.Errorf("prompt_version: got %q, want %q", got.PromptVersion, "v1")
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at should not be zero")
	}

	// List
	list, err := store.ListModelFingerprints(ctx)
	if err != nil {
		t.Fatalf("ListModelFingerprints: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("list len: got %d, want 1", len(list))
	}

	// ClearFingerprintChannelID
	if err := store.ClearFingerprintChannelID(ctx, chID); err != nil {
		t.Fatalf("ClearFingerprintChannelID: %v", err)
	}
	cleared, err := store.GetModelFingerprint(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetModelFingerprint after clear: %v", err)
	}
	if cleared.ChannelID != nil {
		t.Errorf("expected channel_id=nil after clear, got %v", cleared.ChannelID)
	}

	// Record still exists
	if cleared.Model != fp.Model {
		t.Errorf("model after clear: got %q, want %q", cleared.Model, fp.Model)
	}

	// Delete
	if err := store.DeleteModelFingerprint(ctx, created.ID); err != nil {
		t.Fatalf("DeleteModelFingerprint: %v", err)
	}
	_, err = store.GetModelFingerprint(ctx, created.ID)
	if err == nil {
		t.Error("expected error after delete, got nil")
	}
}

func TestModelFingerprintNullChannelID(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	store, err := storage.CreateSQLiteStore(filepath.Join(tmp, "fp-null.db"))
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()

	// Create fingerprint with no channel_id (nil)
	fp := &model.ModelFingerprint{
		Name:         "orphan-baseline",
		ChannelID:    nil,
		Model:        "claude-3-5-sonnet",
		ChannelType:  "anthropic",
		SampleCount:  3,
		Distribution: []float64{0.33, 0.33, 0.34},
		Stats: model.FingerprintStats{
			Mean: 5.0, Median: 5.0, StdDev: 0.5,
			Min: 4, Max: 6, Unique: 3, Mode: 5, ModeCount: 1,
		},
		RawData: []int{4, 5, 6},
	}

	created, err := store.CreateModelFingerprint(ctx, fp)
	if err != nil {
		t.Fatalf("CreateModelFingerprint with nil channel_id: %v", err)
	}
	if created.ChannelID != nil {
		t.Errorf("expected nil channel_id, got %v", created.ChannelID)
	}

	// ClearFingerprintChannelID for a channel that has no rows — should be a no-op
	if err := store.ClearFingerprintChannelID(ctx, 99); err != nil {
		t.Errorf("ClearFingerprintChannelID no-op: %v", err)
	}
}

func TestFingerprintTestResultListReturnsMatches(t *testing.T) {
	t.Parallel()

	store, err := storage.CreateSQLiteStore(filepath.Join(t.TempDir(), "fp-results.db"))
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	matchesJSON := `[{"score":0.963,"baseline":{"name":"trusted"}}]`
	if err := store.CreateFingerprintTestResult(context.Background(), &model.FingerprintTestRecord{
		Model:       "gpt-test",
		SampleCount: 100,
		BestScore:   0.963,
		MatchesJSON: matchesJSON,
	}); err != nil {
		t.Fatalf("CreateFingerprintTestResult: %v", err)
	}

	results, err := store.ListFingerprintTestResults(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListFingerprintTestResults: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results len=%d, want 1", len(results))
	}
	if len(results[0].Matches) != 1 {
		t.Fatalf("matches len=%d, want 1", len(results[0].Matches))
	}

	payload, err := json.Marshal(results[0])
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var response map[string]any
	if err := json.Unmarshal(payload, &response); err != nil {
		t.Fatalf("unmarshal result JSON: %v", err)
	}
	if matches, ok := response["matches"].([]any); !ok || len(matches) != 1 {
		t.Fatalf("JSON matches=%v, want one item", response["matches"])
	}
}
