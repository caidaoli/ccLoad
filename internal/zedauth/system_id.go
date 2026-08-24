package zedauth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// SystemIDEnv overrides the local Zed installation identity lookup.
const SystemIDEnv = "CCLOAD_ZED_SYSTEM_ID"

// NormalizeSystemID validates and canonicalizes a Zed installation UUID.
func NormalizeSystemID(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	parsed, err := uuid.Parse(raw)
	if err != nil || parsed == uuid.Nil {
		return "", errors.New("zed system_id must be a non-zero UUID")
	}
	return parsed.String(), nil
}

// ResolveSystemID returns the Zed installation identity from an explicit
// deployment setting or the local Zed client database. An empty result means
// this host has no trustworthy installation identity.
func ResolveSystemID(ctx context.Context) (string, error) {
	if raw, configured := os.LookupEnv(SystemIDEnv); configured {
		systemID, err := NormalizeSystemID(raw)
		if err != nil || systemID == "" {
			if err == nil {
				err = errors.New("zed system_id is empty")
			}
			return "", fmt.Errorf("invalid %s: %w", SystemIDEnv, err)
		}
		return systemID, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for _, path := range localZedSystemIDDatabases() {
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", fmt.Errorf("inspect local Zed database: %w", err)
		}
		systemID, err := readZedSystemIDDatabase(lookupCtx, path)
		if err != nil {
			return "", fmt.Errorf("read local Zed system_id: %w", err)
		}
		return systemID, nil
	}
	return "", nil
}

func localZedSystemIDDatabases() []string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		if home != "" {
			return []string{filepath.Join(home, "Library", "Application Support", "Zed", "db", "0-global", "db.sqlite")}
		}
	case "windows":
		if localAppData := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); localAppData != "" {
			return []string{filepath.Join(localAppData, "Zed", "db", "0-global", "db.sqlite")}
		}
	default:
		if dataHome := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); dataHome != "" {
			return []string{filepath.Join(dataHome, "zed", "db", "0-global", "db.sqlite")}
		}
		if home != "" {
			return []string{filepath.Join(home, ".local", "share", "zed", "db", "0-global", "db.sqlite")}
		}
	}
	return nil
}

func readZedSystemIDDatabase(ctx context.Context, path string) (string, error) {
	databaseURL := (&url.URL{Scheme: "file", Path: path}).String() + "?mode=ro"
	database, err := sql.Open("sqlite", databaseURL)
	if err != nil {
		return "", err
	}
	defer func() { _ = database.Close() }()
	var raw string
	if err := database.QueryRowContext(ctx, `SELECT value FROM kv_store WHERE key = 'system_id'`).Scan(&raw); err != nil {
		return "", err
	}
	return NormalizeSystemID(raw)
}
