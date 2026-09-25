package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	anthropicCLIVersionSyncURL      = "https://api.github.com/repos/anthropics/claude-code/releases/latest"
	anthropicCLIVersionSyncInterval = time.Hour
	anthropicCLIVersionSyncTimeout  = 30 * time.Second
	anthropicCLIVersionMaxBodyBytes = 1 << 20
)

var anthropicRuntimeCLIVersion atomic.Pointer[string]

type anthropicLatestRelease struct {
	TagName string `json:"tag_name"`
}

func anthropicEffectiveCLIVersion() string {
	if version := anthropicRuntimeCLIVersion.Load(); version != nil && *version != "" {
		return *version
	}
	return anthropicCLIVersion
}

// StartAnthropicCLIVersionSync keeps generated Claude Code wire versions above
// the built-in floor. A failed fetch leaves the last known version untouched.
func (s *Server) StartAnthropicCLIVersionSync() {
	if s == nil || s.isShuttingDown.Load() {
		return
	}
	s.anthropicCLIVersionSyncOnce.Do(func() {
		cachePath := anthropicCLIVersionCachePath()
		if err := loadAnthropicCLIVersionCache(cachePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("[WARN] Claude Code CLI 版本缓存加载失败: %v", err)
		}
		if raw, present := os.LookupEnv("CCLOAD_ANTHROPIC_CLI_VERSION_SYNC"); present {
			enabled, err := strconv.ParseBool(strings.TrimSpace(raw))
			if err != nil {
				log.Printf("[WARN] 无效的 CCLOAD_ANTHROPIC_CLI_VERSION_SYNC=%q: %v", raw, err)
			} else if !enabled {
				return
			}
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			client := &http.Client{Timeout: anthropicCLIVersionSyncTimeout}
			refresh := func() {
				ctx, cancel := context.WithTimeout(s.baseCtx, anthropicCLIVersionSyncTimeout)
				defer cancel()
				previous := anthropicEffectiveCLIVersion()
				if err := refreshAnthropicCLIVersion(ctx, client); err != nil {
					if !errors.Is(err, context.Canceled) {
						log.Printf("[WARN] Claude Code CLI 版本同步失败: %v", err)
					}
				} else if current := anthropicEffectiveCLIVersion(); current != previous {
					if err := saveAnthropicCLIVersionCache(cachePath, current); err != nil {
						log.Printf("[WARN] Claude Code CLI 版本缓存写入失败: %v", err)
					}
				}
			}
			refresh()
			ticker := time.NewTicker(anthropicCLIVersionSyncInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					refresh()
				case <-s.shutdownCh:
					return
				}
			}
		}()
	})
}

func refreshAnthropicCLIVersion(ctx context.Context, client *http.Client) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, anthropicCLIVersionSyncURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ccLoad-Claude-Code-Version-Sync")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return errors.New("GitHub latest release returned HTTP " + resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, anthropicCLIVersionMaxBodyBytes+1))
	if err != nil {
		return err
	}
	if len(body) > anthropicCLIVersionMaxBodyBytes {
		return errors.New("GitHub latest release response is too large")
	}
	var release anthropicLatestRelease
	if err := json.Unmarshal(body, &release); err != nil {
		return err
	}
	version := strings.TrimPrefix(strings.TrimSpace(release.TagName), "v")
	if _, ok := parseAnthropicCLIVersion(version); !ok || !anthropicCLIVersionGTE(version, anthropicCLIVersion) {
		return errors.New("latest release has an unsupported Claude Code version")
	}
	for {
		current := anthropicRuntimeCLIVersion.Load()
		currentVersion := anthropicCLIVersion
		if current != nil && *current != "" {
			currentVersion = *current
		}
		if anthropicCLIVersionGTE(currentVersion, version) {
			return nil
		}
		versionCopy := version
		if anthropicRuntimeCLIVersion.CompareAndSwap(current, &versionCopy) {
			log.Printf("[INFO] Claude Code CLI wire 版本已更新: %s", version)
			return nil
		}
	}
}

func anthropicCLIVersionCachePath() string {
	if path := strings.TrimSpace(os.Getenv("CCLOAD_ANTHROPIC_CLI_VERSION_CACHE")); path != "" {
		return path
	}
	return filepath.Join(filepath.Dir(modelCatalogCachePath()), "anthropic-cli-version.json")
}

func loadAnthropicCLIVersionCache(path string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cached struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &cached); err != nil {
		return err
	}
	version := strings.TrimSpace(cached.Version)
	if _, ok := parseAnthropicCLIVersion(version); !ok || !anthropicCLIVersionGTE(version, anthropicCLIVersion) {
		return errors.New("cached Claude Code CLI version is invalid")
	}
	for {
		current := anthropicRuntimeCLIVersion.Load()
		if current != nil && anthropicCLIVersionGTE(*current, version) {
			return nil
		}
		versionCopy := version
		if anthropicRuntimeCLIVersion.CompareAndSwap(current, &versionCopy) {
			return nil
		}
	}
}

func saveAnthropicCLIVersionCache(path, version string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".anthropic-cli-version-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err := fmt.Fprintf(file, "{\"version\":%q}\n", version); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
