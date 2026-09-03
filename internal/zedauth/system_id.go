package zedauth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
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
			if !os.IsNotExist(err) {
				// 权限不足等同于"这个候选路径不可信"，和文件不存在是同一类事件。
				log.Printf("[WARN] inspect local Zed database %s: %v", path, err)
			}
			continue
		}
		systemID, err := readZedSystemIDDatabase(lookupCtx, path)
		if err != nil {
			// 本机没有可信安装标识不是配置错误：表缺失、Zed 正在运行导致 WAL 只读打不开、
			// 文件损坏都属于同一类事件。省略 x-zed-system-id 让上游决定试用权限，
			// 而不是阻断登录。显式 CCLOAD_ZED_SYSTEM_ID 非法仍然 fail-fast（见上方分支）。
			log.Printf("[WARN] read local Zed system_id from %s: %v", path, err)
			return "", nil
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

func sqliteReadOnlyFileURI(path string) string {
	// Convert only native Windows paths; Unix filenames may contain backslashes.
	if !isWindowsDrivePath(path) && !isWindowsUNCPath(path) {
		return (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String()
	}
	slash := strings.ReplaceAll(path, `\`, `/`)
	if isWindowsDrivePath(slash) {
		slash = "/" + slash
	}
	// UNC 走到这里时 slash 已是 "//server/share/..."，url.URL 会再补一层
	// "file:" + "//"(空 authority) 得到四条斜杠。这是 SQLite 要求的形态：
	// 它只接受空 authority 或 "localhost"，把 server 放进 authority 会被拒绝。
	return (&url.URL{Scheme: "file", Path: slash, RawQuery: "mode=ro"}).String()
}

func isWindowsDrivePath(path string) bool {
	if len(path) < 2 || path[1] != ':' {
		return false
	}
	c := path[0]
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func isWindowsUNCPath(path string) bool {
	return strings.HasPrefix(path, `\\`)
}

func readZedSystemIDDatabase(ctx context.Context, path string) (string, error) {
	database, err := sql.Open("sqlite", sqliteReadOnlyFileURI(path))
	if err != nil {
		return "", err
	}
	defer func() { _ = database.Close() }()
	var raw string
	if err := database.QueryRowContext(ctx, `SELECT value FROM kv_store WHERE key = 'system_id'`).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// "读到了，但这台机器还没有身份"不是故障。上层也会把故障降级成空值，
			// 但会顺带打一条 WARN——全新安装不该产生告警噪音，所以这里单独短路。
			return "", nil
		}
		return "", err
	}
	return NormalizeSystemID(raw)
}
