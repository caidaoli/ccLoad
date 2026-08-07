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
	autoUpdateIntervalSettingKey   = "auto_update_interval_hours"
	autoUpdateChannelSettingKey    = "auto_update_channel"
)

func runningInContainer() bool {
	return os.Getenv("CCLOAD_CONTAINER") == "1"
}

func normalizeAutoUpdateIntervalHours(hours int) int {
	if hours < 0 {
		log.Printf("[WARN] 无效的 auto_update_interval_hours=%v（必须 >= 0），已设为 0（禁用版本检查和自动更新）", hours)
		return 0
	}
	return hours
}

// StartUpdateManager starts the single release check loop and optional update application.
func (s *Server) StartUpdateManager() {
	if runningInContainer() {
		log.Print("[INFO] 容器镜像通过镜像标签更新，版本检查和进程内自动更新均已禁用")
		return
	}

	autoUpdateIntervalHours := normalizeAutoUpdateIntervalHours(
		s.configService.GetInt(autoUpdateIntervalSettingKey, defaultAutoUpdateIntervalHours),
	)
	if autoUpdateIntervalHours == 0 {
		log.Print("[INFO] 版本检查和自动更新未启用（auto_update_interval_hours=0）")
		return
	}

	applyUpdates := RestartFunc != nil
	if !applyUpdates {
		log.Print("[WARN] RestartFunc 为空，仅启动版本检查")
	}

	s.startUpdateManager(
		time.Duration(autoUpdateIntervalHours)*time.Hour,
		s.configuredReleaseChannel(),
		applyUpdates,
	)
}

func (s *Server) configuredReleaseChannel() version.ReleaseChannel {
	value := s.configService.GetString(autoUpdateChannelSettingKey, string(defaultAutoUpdateChannel))
	channel, err := version.ParseReleaseChannel(value)
	if err != nil {
		log.Printf("[WARN] 无效的 auto_update_channel=%q，使用 stable: %v", value, err)
		return defaultAutoUpdateChannel
	}
	return channel
}

func (s *Server) startUpdateManager(interval time.Duration, channel version.ReleaseChannel, applyUpdates bool) {
	manager, err := version.NewUpdateManager(version.UpdateManagerOptions{
		Interval:       interval,
		Channel:        channel,
		ApplyUpdates:   applyUpdates,
		ActiveRequests: s.activeRequestCount,
		Restart:        RestartFunc,
	})
	if err != nil {
		log.Printf("[WARN] 更新管理器未启动: %v", err)
		return
	}
	s.updateManager = manager

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		manager.Run(s.baseCtx)
	}()
	log.Printf("[INFO] 更新管理器已启用，渠道: %s，检测间隔: %v，自动应用: %t", channel, interval, applyUpdates)
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
