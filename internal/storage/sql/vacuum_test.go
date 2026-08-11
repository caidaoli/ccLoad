package sql_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"

	_ "modernc.org/sqlite"
)

func TestCleanupLogsBeforeLeavesFreelistForReuse(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "cleanup_vacuum.db")
	store, err := storage.CreateSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	defer func() { _ = store.Close() }()

	now := time.Now()
	message := strings.Repeat("x", 2048)
	logs := make([]*model.LogEntry, 0, 1000)
	for i := 0; i < cap(logs); i++ {
		logs = append(logs, &model.LogEntry{
			Time:       model.JSONTime{Time: now.Add(-2 * time.Hour)},
			Model:      "gpt-4",
			StatusCode: 200,
			Message:    message,
			LogSource:  model.LogSourceProxy,
		})
	}
	if err := store.BatchAddLogs(ctx, logs); err != nil {
		t.Fatalf("BatchAddLogs failed: %v", err)
	}

	if err := store.CleanupLogsBefore(ctx, now.Add(-time.Hour)); err != nil {
		t.Fatalf("CleanupLogsBefore failed: %v", err)
	}

	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	defer func() { _ = db.Close() }()

	var remaining int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM logs").Scan(&remaining); err != nil {
		t.Fatalf("count logs: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("expected all logs deleted, got %d", remaining)
	}

	assertSQLiteFreelistNotEmpty(t, ctx, db)
}

func TestDeleteConfigLeavesFreelistForReuse(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "delete_config_freelist.db")
	store, err := storage.CreateSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	defer func() { _ = store.Close() }()

	channel, err := store.CreateConfig(ctx, &model.Config{
		Name:    "delete-with-fragmented-logs",
		URLs:    model.ChannelURLs{{URL: "https://api.example.com"}},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	now := time.Now()
	message := strings.Repeat("x", 2048)
	logs := make([]*model.LogEntry, 0, 1000)
	for i := 0; i < cap(logs); i++ {
		logs = append(logs, &model.LogEntry{
			Time:       model.JSONTime{Time: now},
			Model:      "gpt-4",
			ChannelID:  channel.ID,
			StatusCode: 200,
			Message:    message,
			LogSource:  model.LogSourceProxy,
		})
	}
	if err := store.BatchAddLogs(ctx, logs); err != nil {
		t.Fatalf("BatchAddLogs failed: %v", err)
	}
	if err := store.DeleteConfig(ctx, channel.ID); err != nil {
		t.Fatalf("DeleteConfig failed: %v", err)
	}

	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	defer func() { _ = db.Close() }()
	assertSQLiteFreelistNotEmpty(t, ctx, db)
}

func TestTruncateDebugLogsLeavesFreelistForReuse(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "truncate_debug_logs_freelist.db")
	store, err := storage.CreateSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	defer func() { _ = store.Close() }()

	body := []byte(strings.Repeat("x", 4096))
	for i := int64(1); i <= 256; i++ {
		if err := store.AddDebugLog(ctx, &model.DebugLogEntry{
			LogID:       i,
			ReqURL:      "https://api.example.com",
			ReqHeaders:  "{}",
			ReqBody:     body,
			RespHeaders: "{}",
		}); err != nil {
			t.Fatalf("AddDebugLog(%d) failed: %v", i, err)
		}
	}
	if err := store.TruncateDebugLogs(ctx); err != nil {
		t.Fatalf("TruncateDebugLogs failed: %v", err)
	}

	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	defer func() { _ = db.Close() }()
	assertSQLiteFreelistNotEmpty(t, ctx, db)
}

func assertSQLiteFreelistNotEmpty(t testing.TB, ctx context.Context, db *sql.DB) {
	t.Helper()

	var freePages int
	if err := db.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&freePages); err != nil {
		t.Fatalf("query freelist_count: %v", err)
	}
	if freePages == 0 {
		t.Fatal("expected deleted pages to remain on the freelist for reuse")
	}
}
