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

func TestCleanupLogsBeforeRunsIncrementalVacuumForSQLite(t *testing.T) {
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

	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := store.CleanupLogsBefore(ctx, now.Add(-time.Hour)); err != nil {
		t.Fatalf("CleanupLogsBefore failed: %v", err)
	}

	var remaining int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM logs").Scan(&remaining); err != nil {
		t.Fatalf("count logs: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("expected all logs deleted, got %d", remaining)
	}

	var freePages int
	if err := db.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&freePages); err != nil {
		t.Fatalf("query freelist_count: %v", err)
	}
	if freePages != 0 {
		t.Fatalf("expected incremental_vacuum to release free pages, got freelist_count=%d", freePages)
	}
}
