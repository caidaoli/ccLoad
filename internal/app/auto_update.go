package app

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"

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
	if hours == 0 {
		return 0
	}
	if _, ok := settingDurationFromInt64(int64(hours), time.Hour); !ok {
		log.Printf("[WARN] 无效的 auto_update_interval_hours=%v，已使用默认值 %d", hours, defaultAutoUpdateIntervalHours)
		return defaultAutoUpdateIntervalHours
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
	restart := s.restartFuncSnapshot()
	if restart == nil {
		log.Print("[WARN] 重启函数为空，仅启动版本检查")
	}

	// interval=0 时关闭后台定时检查，但仍保留管理器供手动检测按钮触发完整更新流程。
	var interval time.Duration
	if autoUpdateIntervalHours > 0 {
		interval, _ = settingDurationFromInt64(int64(autoUpdateIntervalHours), time.Hour)
	}
	s.startUpdateManager(
		interval,
		s.configuredReleaseChannel(),
		restart,
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

func (s *Server) startUpdateManager(interval time.Duration, channel version.ReleaseChannel, restart func()) {
	manager, err := version.NewUpdateManager(version.UpdateManagerOptions{
		Interval:       interval,
		Channel:        channel,
		ApplyUpdates:   restart != nil,
		ActiveRequests: s.activeRequestCount,
		Restart:        restart,
	})
	if err != nil {
		log.Printf("[WARN] 更新管理器未启动: %v", err)
		return
	}
	s.updateManager = manager
	if interval > 0 {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			manager.Run(s.baseCtx)
		}()
	} else {
		log.Printf("[INFO] 版本检查已禁用（auto_update_interval_hours=0），仅支持设置页手动检测")
		return
	}
	log.Printf("[INFO] 更新管理器已启用，渠道: %s，检测间隔: %v，自动应用: %t", channel, interval, restart != nil)
}

// HandleManualUpdate 执行一次完整的更新流程：检查、下载、校验、替换并等待空闲后重启。
// 只对当前已生效的变更渠道（auto_update_channel）生效；容器部署直接拒绝。
func (s *Server) HandleManualUpdate(c *gin.Context) {
	if runningInContainer() {
		RespondErrorMsg(c, http.StatusConflict,
			"container image updates are managed by image tags; use latest for stable or beta for preview")
		return
	}
	if s.updateManager == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "update manager is not available")
		return
	}
	if err := s.updateManager.CheckNow(s.baseCtx); err != nil {
		RespondErrorMsg(c, http.StatusBadGateway, err.Error())
		return
	}
	RespondJSON(c, http.StatusOK, s.updateManager.State())
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
