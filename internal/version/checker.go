// Package version 提供版本检测服务
package version

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// GitHub API 地址
	githubReleaseAPI = "https://api.github.com/repos/caidaoli/ccLoad/releases/latest"
	// 检测间隔
	checkInterval = 4 * time.Hour
	// 请求超时
	requestTimeout = 10 * time.Second
)

// GitHubRelease GitHub release API 响应结构
type GitHubRelease struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
}

// Checker 版本检测器
type Checker struct {
	mu            sync.RWMutex
	latestVersion string
	releaseURL    string
	hasUpdate     bool
	lastCheck     time.Time
	client        *http.Client
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
	req, err := http.NewRequest(http.MethodGet, githubReleaseAPI, nil)
	if err != nil {
		log.Printf("[VersionChecker] 创建请求失败: %v", err)
		return
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "ccLoad/"+Version)

	resp, err := c.client.Do(req)
	if err != nil {
		log.Printf("[VersionChecker] 请求GitHub失败: %v", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		log.Printf("[VersionChecker] GitHub返回非200状态: %d", resp.StatusCode)
		return
	}

	var release GitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		log.Printf("[VersionChecker] 解析响应失败: %v", err)
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.latestVersion = release.TagName
	c.releaseURL = release.HTMLURL
	c.lastCheck = time.Now()

	// 比较版本
	current := normalizeVersion(Version)
	latest := normalizeVersion(release.TagName)
	c.hasUpdate = current != "" && latest != "" && current != latest

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
