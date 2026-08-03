package app

import (
	"log"
	"os"
	"time"

	"ccLoad/internal/version"
)

const (
	defaultAutoUpdateIntervalHours = 12
	defaultAutoUpdateChannel       = version.ReleaseChannelStable
)

func normalizeAutoUpdateIntervalHours(hours int) int {
	if hours < 0 {
		log.Printf("[WARN] 无效的 auto_update_interval_hours=%v（必须 >= 0），已设为 0（禁用自动更新）", hours)
		return 0
	}
	return hours
}

// StartAutoUpdateLoop starts the configured auto-update loop after RestartFunc is injected.
func (s *Server) StartAutoUpdateLoop() {
	inContainer := os.Getenv("CCLOAD_CONTAINER") == "1"
	if inContainer && os.Getenv("CCLOAD_ALLOW_SELF_UPDATE") != "1" {
		log.Print("[INFO] 容器镜像禁用进程内自动更新；请拉取新的稳定版镜像")
		return
	}
	if inContainer {
		log.Print("[WARN] 已通过 CCLOAD_ALLOW_SELF_UPDATE=1 启用容器进程内自动更新")
	}
	autoUpdateIntervalHours := normalizeAutoUpdateIntervalHours(
		s.configService.GetInt("auto_update_interval_hours", defaultAutoUpdateIntervalHours),
	)
	s.startAutoUpdateLoop(
		time.Duration(autoUpdateIntervalHours)*time.Hour,
		s.configuredReleaseChannel(),
	)
}

// StartVersionChecker starts update notifications using the configured release channel.
func (s *Server) StartVersionChecker() {
	channel := s.configuredReleaseChannel()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := version.RunChecker(s.baseCtx, channel); err != nil {
			log.Printf("[WARN] 版本检测未启动: %v", err)
		}
	}()
}

func (s *Server) configuredReleaseChannel() version.ReleaseChannel {
	value := s.configService.GetString("auto_update_channel", string(defaultAutoUpdateChannel))
	channel, err := version.ParseReleaseChannel(value)
	if err != nil {
		log.Printf("[WARN] 无效的 auto_update_channel=%q，使用 stable: %v", value, err)
		return defaultAutoUpdateChannel
	}
	return channel
}

func (s *Server) startAutoUpdateLoop(interval time.Duration, channel version.ReleaseChannel) {
	if interval <= 0 {
		log.Print("[INFO] 自动更新未启用（auto_update_interval_hours=0）")
		return
	}
	if RestartFunc == nil {
		log.Print("[WARN] 自动更新未启动：RestartFunc 为空")
		return
	}

	updater, err := version.NewAutoUpdater(version.AutoUpdateOptions{
		Interval:       interval,
		Channel:        channel,
		ActiveRequests: s.activeRequestCount,
		Restart:        RestartFunc,
	})
	if err != nil {
		log.Printf("[WARN] 自动更新未启动: %v", err)
		return
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		updater.Run(s.baseCtx)
	}()
	log.Printf("[INFO] 自动更新已启用，渠道: %s，检测间隔: %v", channel, interval)
}

func (s *Server) activeRequestCount() int {
	if s == nil {
		return 0
	}
	// 自动更新关心的是所有已经进入代理处理流程的客户端请求，包含仍在等待
	// Responses 会话锁的请求。activeRequests 只表示已经开始的上游尝试，不能
	// 再拿来判断服务是否空闲。
	return len(s.concurrencySem)
}
