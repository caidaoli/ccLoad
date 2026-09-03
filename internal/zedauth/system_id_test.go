package zedauth

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestResolveSystemIDUsesConfiguredInstallationIdentity(t *testing.T) {
	t.Setenv(SystemIDEnv, "9D4B8C17-12AE-4091-96BC-1A79CE2DE601")
	systemID, err := ResolveSystemID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if systemID != "9d4b8c17-12ae-4091-96bc-1a79ce2de601" {
		t.Fatalf("system_id = %q", systemID)
	}
}

func TestReadZedSystemIDDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`CREATE TABLE kv_store (key TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO kv_store(key, value) VALUES ('system_id', '9d4b8c17-12ae-4091-96bc-1a79ce2de601')`); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	systemID, err := readZedSystemIDDatabase(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if systemID != "9d4b8c17-12ae-4091-96bc-1a79ce2de601" {
		t.Fatalf("system_id = %q", systemID)
	}
}

func TestReadZedSystemIDDatabaseMissingSystemIDIsOptional(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`CREATE TABLE kv_store (key TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT`); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	systemID, err := readZedSystemIDDatabase(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if systemID != "" {
		t.Fatalf("system_id = %q, want empty", systemID)
	}
}

func TestSqliteReadOnlyFileURIUsesWindowsDrivePathNotAuthority(t *testing.T) {
	t.Parallel()
	got := sqliteReadOnlyFileURI(`C:\Users\chenx\AppData\Local\Zed\db\0-global\db.sqlite`)
	want := "file:///C:/Users/chenx/AppData/Local/Zed/db/0-global/db.sqlite?mode=ro"
	if got != want {
		t.Fatalf("sqlite URI = %q, want %q", got, want)
	}
}

func TestSqliteReadOnlyFileURIEncodesUnixSpaces(t *testing.T) {
	t.Parallel()
	got := sqliteReadOnlyFileURI("/Users/foo/Library/Application Support/Zed/db/0-global/db.sqlite")
	want := "file:///Users/foo/Library/Application%20Support/Zed/db/0-global/db.sqlite?mode=ro"
	if got != want {
		t.Fatalf("sqlite URI = %q, want %q", got, want)
	}
}

func TestSqliteReadOnlyFileURIPreservesUnixBackslashes(t *testing.T) {
	t.Parallel()
	got := sqliteReadOnlyFileURI(`/tmp/Zed\profile/db.sqlite`)
	want := "file:///tmp/Zed%5Cprofile/db.sqlite?mode=ro"
	if got != want {
		t.Fatalf("sqlite URI = %q, want %q", got, want)
	}
}

func TestSqliteReadOnlyFileURIUsesEmptyAuthorityForUNC(t *testing.T) {
	// SQLite 只接受空 authority 或 "localhost"。UNC 路径 \\server\share 必须
	// 产出 file:////server/share/...(4 条斜杠:file:// + 空 authority + //server/share),
	// 把 server 放进 authority 会被 SQLite 拒绝。
	t.Parallel()
	got := sqliteReadOnlyFileURI(`\\server\share\Zed\db\0-global\db.sqlite`)
	want := "file:////server/share/Zed/db/0-global/db.sqlite?mode=ro"
	if got != want {
		t.Fatalf("sqlite URI = %q, want %q", got, want)
	}
}
