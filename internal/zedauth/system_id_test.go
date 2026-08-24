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
