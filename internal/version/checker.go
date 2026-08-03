// Package version 提供版本检测服务
package version

import (
	"context"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// 检测间隔
	checkInterval = 4 * time.Hour
	// 请求超时
	requestTimeout = 10 * time.Second
)

// GitHubRelease describes the release resolved from GitHub's latest redirect.
type GitHubRelease struct {
	TagName    string
	HTMLURL    string
	Prerelease bool
}

// Checker 版本检测器
type Checker struct {
	mu            sync.RWMutex
	latestVersion string
	releaseURL    string
	hasUpdate     bool
	lastCheck     time.Time
	client        *http.Client
	sources       []ReleaseSource
	channel       ReleaseChannel
}

// 全局检测器实例
var checker = &Checker{
	client: &http.Client{Timeout: requestTimeout},
}

// RunChecker runs version checks until ctx is canceled.
func RunChecker(ctx context.Context, channel ReleaseChannel) error {
	parsedChannel, err := ParseReleaseChannel(string(channel))
	if err != nil {
		return err
	}

	checker.mu.Lock()
	checker.channel = parsedChannel
	checker.mu.Unlock()

	checker.checkContext(ctx)
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			checker.checkContext(ctx)
		}
	}
}

// check 执行版本检测
func (c *Checker) check() {
	c.checkContext(context.Background())
}

func (c *Checker) checkContext(ctx context.Context) {
	c.mu.RLock()
	sources := c.sources
	client := c.client
	channel := c.channel
	c.mu.RUnlock()

	if len(sources) == 0 {
		var err error
		sources, err = releaseSources(os.Getenv("CCLOAD_RELEASE_BASE_URL"))
		if err != nil {
			log.Printf("[VersionChecker] 发布源配置错误: %v", err)
			return
		}
	}

	release, err := resolveLatestRelease(ctx, client, sources, channel)
	if err != nil {
		log.Printf("[VersionChecker] 请求发布源失败: %v", err)
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.latestVersion = release.TagName
	c.releaseURL = release.HTMLURL
	c.lastCheck = time.Now()

	// 比较版本
	c.hasUpdate = compareSemanticVersions(release.TagName, Version) > 0

	if c.hasUpdate {
		log.Printf("[VersionChecker] 发现新版本: %s -> %s", Version, release.TagName)
	}
}

// normalizeVersion 标准化版本号（去掉v前缀）
func normalizeVersion(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}

// GetUpdateInfo 获取更新信息
func GetUpdateInfo() (hasUpdate bool, latestVersion, releaseURL string) {
	checker.mu.RLock()
	defer checker.mu.RUnlock()
	return checker.hasUpdate, checker.latestVersion, checker.releaseURL
}
