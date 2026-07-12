// Package version 提供版本检测服务
package version

import (
	"context"
	"errors"
	"fmt"
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
	TagName string
	HTMLURL string
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
}

// 全局检测器实例
var checker = &Checker{
	client: &http.Client{Timeout: requestTimeout},
}

// StartChecker 启动版本检测服务
func StartChecker() {
	// 启动时立即检测一次
	go func() {
		checker.check()
		// 定时检测
		ticker := time.NewTicker(checkInterval)
		defer ticker.Stop()
		for range ticker.C {
			checker.check()
		}
	}()
}

// check 执行版本检测
func (c *Checker) check() {
	sources := c.sources
	if len(sources) == 0 {
		var err error
		sources, err = releaseSources(os.Getenv("CCLOAD_RELEASE_BASE_URL"))
		if err != nil {
			log.Printf("[VersionChecker] 发布源配置错误: %v", err)
			return
		}
	}

	var release GitHubRelease
	var sourceErrors []error
	for _, source := range sources {
		var err error
		release, err = fetchLatestRelease(context.Background(), c.client, source.LatestURL)
		if err == nil {
			break
		}
		sourceErrors = append(sourceErrors, fmt.Errorf("%s: %w", source.Name, err))
	}
	if release.TagName == "" {
		err := errors.Join(sourceErrors...)
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
