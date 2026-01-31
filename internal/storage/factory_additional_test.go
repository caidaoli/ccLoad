package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsDirWritable(t *testing.T) {
	dir := t.TempDir()
	if !isDirWritable(dir) {
		t.Fatalf("expected dir writable: %s", dir)
	}

	filePath := filepath.Join(dir, "f")
	if err := os.WriteFile(filePath, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file failed: %v", err)
	}
	if isDirWritable(filePath) {
		t.Fatalf("expected file path not writable as dir: %s", filePath)
	}

	if isDirWritable(filepath.Join(dir, "no_such_dir")) {
		t.Fatal("expected non-existent dir not writable")
	}
}

func TestResolveSQLitePath_DefaultAndFallback(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd failed: %v", err)
	}

	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("Chdir failed: %v", err)
	}
	defer func() { _ = os.Chdir(wd) }()

	// 默认：data 目录可创建/可写
	got := resolveSQLitePath()
	if got != filepath.Join("data", "ccload.db") {
		t.Fatalf("resolveSQLitePath()=%q, want %q", got, filepath.Join("data", "ccload.db"))
	}

	// fallback：用同名文件阻止 data 目录创建
	if err := os.RemoveAll("data"); err != nil {
		t.Fatalf("RemoveAll(data) failed: %v", err)
	}
	if err := os.WriteFile("data", []byte("not a dir"), 0o600); err != nil {
		t.Fatalf("write data file failed: %v", err)
	}

	got2 := resolveSQLitePath()
	if !strings.Contains(got2, filepath.Join(os.TempDir(), "ccload")) {
		t.Fatalf("expected fallback path under temp dir, got %q", got2)
	}
}

func TestGetLogSyncDays(t *testing.T) {
	t.Setenv("CCLOAD_SQLITE_LOG_DAYS", "")
	if got := getLogSyncDays(); got != 7 {
		t.Fatalf("default getLogSyncDays=%d, want 7", got)
	}

	t.Setenv("CCLOAD_SQLITE_LOG_DAYS", "0")
	if got := getLogSyncDays(); got != 0 {
		t.Fatalf("getLogSyncDays=%d, want 0", got)
	}

	t.Setenv("CCLOAD_SQLITE_LOG_DAYS", "-1")
	if got := getLogSyncDays(); got != -1 {
		t.Fatalf("getLogSyncDays=%d, want -1", got)
	}

	t.Setenv("CCLOAD_SQLITE_LOG_DAYS", "-2")
	if got := getLogSyncDays(); got != 7 {
		t.Fatalf("invalid getLogSyncDays=%d, want 7", got)
	}

	t.Setenv("CCLOAD_SQLITE_LOG_DAYS", "not-an-int")
	if got := getLogSyncDays(); got != 7 {
		t.Fatalf("invalid getLogSyncDays=%d, want 7", got)
	}
}

func TestNewStore_SQLiteMode_UsesTempCWDDefaultPath(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd failed: %v", err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("Chdir failed: %v", err)
	}
	defer func() { _ = os.Chdir(wd) }()

	t.Setenv("CCLOAD_MYSQL", "")
	t.Setenv("SQLITE_PATH", "")

	s, err := NewStore()
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("Ping failed: %v", err)
	}
}

func TestValidateJournalMode(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"", "WAL"},
		{"WAL", "WAL"},
		{"wal", "WAL"},
		{"DELETE", "DELETE"},
		{"delete", "DELETE"},
		{"TRUNCATE", "TRUNCATE"},
		{"PERSIST", "PERSIST"},
		{"MEMORY", "MEMORY"},
		{"OFF", "OFF"},
	}

	for _, tc := range tests {
		t.Run("mode_"+tc.input, func(t *testing.T) {
			result := validateJournalMode(tc.input)
			if result != tc.expected {
				t.Errorf("validateJournalMode(%q) = %q, want %q", tc.input, result, tc.expected)
			}
		})
	}
}

func TestBuildSQLiteDSN(t *testing.T) {
	dsn := buildSQLiteDSN("/tmp/test.db")
	if !strings.Contains(dsn, "/tmp/test.db") {
		t.Errorf("DSN should contain db path, got %q", dsn)
	}
	if !strings.Contains(dsn, "journal_mode") {
		t.Errorf("DSN should contain journal_mode pragma, got %q", dsn)
	}
	if !strings.Contains(dsn, "busy_timeout") {
		t.Errorf("DSN should contain busy_timeout pragma, got %q", dsn)
	}
}

func TestNewStore_WithExplicitSQLitePath(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "explicit.db")

	t.Setenv("CCLOAD_MYSQL", "")
	t.Setenv("SQLITE_PATH", dbPath)

	s, err := NewStore()
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer func() { _ = s.Close() }()

	// 验证文件存在
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Fatalf("database file not created at %s", dbPath)
	}

	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("Ping failed: %v", err)
	}
}

func TestCreateSQLiteStore(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "test.db")

	store, err := CreateSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("CreateSQLiteStore failed: %v", err)
	}
	defer func() { _ = store.Close() }()

	if err := store.Ping(context.Background()); err != nil {
		t.Fatalf("Ping failed: %v", err)
	}
}

func TestCreateSQLiteStore_CreatesParentDir(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "nested", "deep", "test.db")

	store, err := CreateSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("CreateSQLiteStore failed: %v", err)
	}
	defer func() { _ = store.Close() }()

	// 验证父目录被创建
	if _, err := os.Stat(filepath.Dir(dbPath)); os.IsNotExist(err) {
		t.Fatalf("parent directory not created")
	}
}
