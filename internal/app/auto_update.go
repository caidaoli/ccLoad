package app

import (
	"log"
	"time"

	"ccLoad/internal/version"
)

const defaultAutoUpdateIntervalHours = 12

func normalizeAutoUpdateIntervalHours(hours int) int {
	if hours < 0 {
		log.Printf("[WARN] 无效的 auto_update_interval_hours=%v（必须 >= 0），已设为 0（禁用自动更新）", hours)
		return 0
	}
	return hours
}

// StartAutoUpdateLoop starts the configured auto-update loop after RestartFunc is injected.
func (s *Server) StartAutoUpdateLoop() {
	autoUpdateIntervalHours := normalizeAutoUpdateIntervalHours(
		s.configService.GetInt("auto_update_interval_hours", defaultAutoUpdateIntervalHours),
	)
	s.startAutoUpdateLoop(time.Duration(autoUpdateIntervalHours) * time.Hour)
}

func (s *Server) startAutoUpdateLoop(interval time.Duration) {
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
	log.Printf("[INFO] 自动更新已启用，检测间隔: %v", interval)
}

func (s *Server) activeRequestCount() int {
	if s == nil || s.activeRequests == nil {
		return 0
	}
	return s.activeRequests.Count()
}
