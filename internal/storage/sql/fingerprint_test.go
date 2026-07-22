package sql_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

func newFingerprintForStorageTest(id int64) *model.ModelFingerprint {
	return &model.ModelFingerprint{
		ID:            id,
		Name:          "replicated-baseline",
		Model:         "gpt-test",
		SampleCount:   3,
		Distribution:  []float64{0.5, 0.25, 0.25},
		Stats:         model.FingerprintStats{Mean: 2, Median: 2, Min: 1, Max: 3, Unique: 3, Mode: 1, ModeCount: 1},
		RawData:       []int{1, 2, 3},
		PromptVersion: "v1",
		CreatedAt:     model.JSONTime{Time: time.Unix(1_700_000_000, 0)},
		UpdatedAt:     model.JSONTime{Time: time.Unix(1_700_000_100, 0)},
	}
}

func TestModelFingerprintCreatePreservesExplicitIDAndTimestamps(t *testing.T) {
	t.Parallel()

	store, err := storage.CreateSQLiteStore(filepath.Join(t.TempDir(), "fp-explicit-id.db"))
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	want := newFingerprintForStorageTest(42)
	created, err := store.CreateModelFingerprint(context.Background(), want)
	if err != nil {
		t.Fatalf("CreateModelFingerprint: %v", err)
	}
	if created.ID != want.ID {
		t.Fatalf("id=%d, want %d", created.ID, want.ID)
	}
	if !created.CreatedAt.Equal(want.CreatedAt.Time) || !created.UpdatedAt.Equal(want.UpdatedAt.Time) {
		t.Fatalf("timestamps=(%v,%v), want (%v,%v)", created.CreatedAt.Time, created.UpdatedAt.Time, want.CreatedAt.Time, want.UpdatedAt.Time)
	}
}

func TestFingerprintTestResultCreateSetsAndPreservesID(t *testing.T) {
	t.Parallel()

	store, err := storage.CreateSQLiteStore(filepath.Join(t.TempDir(), "fp-result-id.db"))
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	generated := &model.FingerprintTestRecord{Model: "generated", MatchesJSON: `[]`}
	if err := store.CreateFingerprintTestResult(context.Background(), generated); err != nil {
		t.Fatalf("CreateFingerprintTestResult generated: %v", err)
	}
	if generated.ID == 0 {
		t.Fatal("generated id was not written back to record")
	}

	explicit := &model.FingerprintTestRecord{
		ID:          42,
		Model:       "replicated",
		MatchesJSON: `[]`,
		CreatedAt:   model.JSONTime{Time: time.Unix(1_700_000_200, 0)},
	}
	if err := store.CreateFingerprintTestResult(context.Background(), explicit); err != nil {
		t.Fatalf("CreateFingerprintTestResult explicit: %v", err)
	}

	results, err := store.ListFingerprintTestResults(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListFingerprintTestResults: %v", err)
	}
	var found *model.FingerprintTestRecord
	for _, result := range results {
		if result.ID == explicit.ID {
			found = result
			break
		}
	}
	if found == nil {
		t.Fatalf("explicit id %d not found in %#v", explicit.ID, results)
	}
	if !found.CreatedAt.Equal(explicit.CreatedAt.Time) {
		t.Fatalf("created_at=%v, want %v", found.CreatedAt.Time, explicit.CreatedAt.Time)
	}
}

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
	distribution := []float64{0, 0.25, 0.5, 0.25}
	if err := store.CreateFingerprintTestResult(context.Background(), &model.FingerprintTestRecord{
		Model:        "gpt-test",
		SampleCount:  100,
		BestScore:    0.963,
		MatchesJSON:  matchesJSON,
		Distribution: distribution,
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
	if len(results[0].Distribution) != len(distribution) {
		t.Fatalf("distribution len=%d, want %d", len(results[0].Distribution), len(distribution))
	}
	for i := range distribution {
		if results[0].Distribution[i] != distribution[i] {
			t.Fatalf("distribution[%d]=%v, want %v", i, results[0].Distribution[i], distribution[i])
		}
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
	if got, ok := response["distribution"].([]any); !ok || len(got) != len(distribution) {
		t.Fatalf("JSON distribution=%v, want %v", response["distribution"], distribution)
	}
}
