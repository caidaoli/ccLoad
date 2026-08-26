package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"time"

	"ccLoad/internal/config"
	"ccLoad/internal/model"
	"ccLoad/internal/version"

	"github.com/gin-gonic/gin"
)

// 配置验证常量
const (
	LogRetentionDaysMin      = 1
	LogRetentionDaysMax      = 365
	LogRetentionDaysDisabled = -1 // 永久保留

	maxSettingDurationSeconds = int64((1<<63 - 1) / time.Second)
	maxSettingDurationMinutes = int64((1<<63 - 1) / time.Minute)
	maxSettingDurationHours   = int64((1<<63 - 1) / time.Hour)

	cooldownMinSecondsSettingKey = "cooldown_min_seconds"
	cooldownMaxSecondsSettingKey = "cooldown_max_seconds"
)

var errInvalidSettingCombination = errors.New("invalid setting combination")

type adminSystemSetting struct {
	*model.SystemSetting
	Editable       bool   `json:"editable"`
	DisabledReason string `json:"disabled_reason,omitempty"`
}

const containerImageManagedDisabledReason = "container_image_managed"

func isContainerManagedUpdateSetting(key string) bool {
	if !runningInContainer() {
		return false
	}
	return key == autoUpdateIntervalSettingKey || key == autoUpdateChannelSettingKey
}

func systemSettingForAdmin(setting *model.SystemSetting) adminSystemSetting {
	view := adminSystemSetting{
		SystemSetting: setting,
		Editable:      true,
	}
	if isContainerManagedUpdateSetting(setting.Key) {
		view.Editable = false
		view.DisabledReason = containerImageManagedDisabledReason
	}
	return view
}

func rejectContainerManagedUpdateSetting(c *gin.Context, key string) bool {
	if !isContainerManagedUpdateSetting(key) {
		return false
	}
	RespondErrorMsg(c, http.StatusConflict, "container image updates are managed by image tags; use latest for stable or beta for preview")
	return true
}

func (s *Server) completeCooldownBoundUpdates(ctx context.Context, requested map[string]string) (map[string]string, error) {
	_, updatesMin := requested[cooldownMinSecondsSettingKey]
	_, updatesMax := requested[cooldownMaxSecondsSettingKey]
	if !updatesMin && !updatesMax {
		return requested, nil
	}

	settings, err := s.configService.ListAllSettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("load cooldown bounds: %w", err)
	}
	finalValues := make(map[string]string, 2)
	for _, setting := range settings {
		if setting.Key == cooldownMinSecondsSettingKey || setting.Key == cooldownMaxSecondsSettingKey {
			finalValues[setting.Key] = setting.Value
		}
	}
	for key, value := range requested {
		finalValues[key] = value
	}

	minSeconds, err := strconv.Atoi(finalValues[cooldownMinSecondsSettingKey])
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", cooldownMinSecondsSettingKey, err)
	}
	maxSeconds, err := strconv.Atoi(finalValues[cooldownMaxSecondsSettingKey])
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", cooldownMaxSecondsSettingKey, err)
	}
	if minSeconds > maxSeconds {
		return nil, fmt.Errorf(
			"%w: %s must be <= %s",
			errInvalidSettingCombination,
			cooldownMinSecondsSettingKey,
			cooldownMaxSecondsSettingKey,
		)
	}

	updates := make(map[string]string, len(requested)+1)
	for key, value := range requested {
		updates[key] = value
	}
	updates[cooldownMinSecondsSettingKey] = strconv.Itoa(minSeconds)
	updates[cooldownMaxSecondsSettingKey] = strconv.Itoa(maxSeconds)
	return updates, nil
}

func respondSettingCombinationError(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errInvalidSettingCombination) {
		RespondErrorMsg(c, http.StatusBadRequest, err.Error())
	} else {
		RespondError(c, http.StatusInternalServerError, err)
	}
	return true
}

func settingsRequireRestart(updates map[string]string) bool {
	if len(updates) != 1 {
		return true
	}
	_, hotReloaded := updates[modelMultimodalFallbackSettingKey]
	return !hotReloaded
}

// commitSettingUpdates 将持久化与运行态发布绑定成同一个有序操作。
// 只要包含多模态映射，就串行化整个提交，避免并发请求让数据库终值与运行态快照错序。
func (s *Server) commitSettingUpdates(updates map[string]string, persist func() error) (bool, error) {
	value, updatesMultimodalFallback := updates[modelMultimodalFallbackSettingKey]
	var multimodalFallbackModels map[string]string
	if updatesMultimodalFallback {
		parsed, err := parseMultimodalFallbackModels(value)
		if err != nil {
			return false, fmt.Errorf("parse validated multimodal fallback setting: %w", err)
		}
		multimodalFallbackModels = parsed
		s.multimodalFallbackUpdateMu.Lock()
		defer s.multimodalFallbackUpdateMu.Unlock()
	}

	if err := persist(); err != nil {
		return false, err
	}
	if updatesMultimodalFallback {
		s.setMultimodalFallbackModels(multimodalFallbackModels)
	}
	return settingsRequireRestart(updates), nil
}

// AdminListSettings 获取所有配置项
// GET /admin/settings
func (s *Server) AdminListSettings(c *gin.Context) {
	settings, err := s.configService.ListAllSettings(c.Request.Context())
	if err != nil {
		log.Printf("[ERROR] AdminListSettings 失败: %v", err)
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	if settings == nil {
		settings = make([]*model.SystemSetting, 0)
	}
	views := make([]adminSystemSetting, 0, len(settings))
	for _, setting := range settings {
		views = append(views, systemSettingForAdmin(setting))
	}
	RespondJSON(c, http.StatusOK, views)
}

// AdminGetSetting 获取单个配置项
// GET /admin/settings/:key
func (s *Server) AdminGetSetting(c *gin.Context) {
	key := c.Param("key")
	if key == "" {
		RespondErrorMsg(c, http.StatusBadRequest, "missing setting key")
		return
	}

	// 管理接口必须返回持久化后的最新值，不能复用等待重启的运行时缓存。
	setting, err := s.configService.GetSettingFresh(c.Request.Context(), key)
	if errors.Is(err, model.ErrSettingNotFound) {
		RespondErrorMsg(c, http.StatusNotFound, fmt.Sprintf("setting not found: %s", key))
		return
	}
	if err != nil {
		log.Printf("[ERROR] AdminGetSetting key=%s 失败: %v", key, err)
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	// 配置项变更频率极低，允许浏览器缓存 5 分钟
	c.Header("Cache-Control", "private, max-age=300")
	RespondJSON(c, http.StatusOK, systemSettingForAdmin(setting))
}

// AdminUpdateSetting 更新配置项
// PUT /admin/settings/:key
func (s *Server) AdminUpdateSetting(c *gin.Context) {
	key := c.Param("key")
	if key == "" {
		RespondErrorMsg(c, http.StatusBadRequest, "missing setting key")
		return
	}
	if rejectContainerManagedUpdateSetting(c, key) {
		return
	}
	var req SettingUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, fmt.Sprintf("invalid request: %v", err))
		return
	}

	// 验证值的合法性
	setting := s.configService.GetSetting(key)
	if setting == nil {
		RespondErrorMsg(c, http.StatusNotFound, fmt.Sprintf("setting not found: %s", key))
		return
	}

	if err := validateSettingValue(key, setting.ValueType, req.Value); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, fmt.Sprintf("invalid value for type %s: %v", setting.ValueType, err))
		return
	}

	updates, err := s.completeCooldownBoundUpdates(c.Request.Context(), map[string]string{key: req.Value})
	if respondSettingCombinationError(c, err) {
		return
	}

	// 冷却上下限必须作为一个有效快照原子写入，其他设置保持单项更新。
	restartRequired, err := s.commitSettingUpdates(updates, func() error {
		if len(updates) == 1 {
			return s.configService.UpdateSetting(c.Request.Context(), key, req.Value)
		}
		return s.configService.BatchUpdateSettings(c.Request.Context(), updates)
	})
	if err != nil {
		log.Printf("[ERROR] AdminUpdateSetting key=%s 失败: %v", key, err)
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	message := "配置已保存并生效"
	if restartRequired {
		message = "配置已保存，程序将在2秒后重启"
	}
	RespondJSON(c, http.StatusOK, gin.H{
		"message": message,
		"key":     key,
		"value":   req.Value,
	})

	if restartRequired {
		go s.triggerRestart()
	}
}

// AdminResetSetting 重置配置为默认值
// POST /admin/settings/:key/reset
func (s *Server) AdminResetSetting(c *gin.Context) {
	key := c.Param("key")
	if key == "" {
		RespondErrorMsg(c, http.StatusBadRequest, "missing setting key")
		return
	}
	if rejectContainerManagedUpdateSetting(c, key) {
		return
	}
	// 获取默认值
	setting := s.configService.GetSetting(key)
	if setting == nil {
		RespondErrorMsg(c, http.StatusNotFound, fmt.Sprintf("setting not found: %s", key))
		return
	}

	if err := validateSettingValue(key, setting.ValueType, setting.DefaultValue); err != nil {
		RespondErrorMsg(c, http.StatusInternalServerError, fmt.Sprintf("invalid default value for %s: %v", key, err))
		return
	}
	updates, err := s.completeCooldownBoundUpdates(c.Request.Context(), map[string]string{key: setting.DefaultValue})
	if respondSettingCombinationError(c, err) {
		return
	}
	restartRequired, err := s.commitSettingUpdates(updates, func() error {
		if len(updates) == 1 {
			return s.configService.UpdateSetting(c.Request.Context(), key, setting.DefaultValue)
		}
		return s.configService.BatchUpdateSettings(c.Request.Context(), updates)
	})
	if err != nil {
		log.Printf("[ERROR] AdminResetSetting key=%s 失败: %v", key, err)
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	message := "配置已重置并生效"
	if restartRequired {
		message = "配置已重置为默认值，程序将在2秒后重启"
	}
	RespondJSON(c, http.StatusOK, gin.H{
		"message": message,
		"key":     key,
		"value":   setting.DefaultValue,
	})

	if restartRequired {
		go s.triggerRestart()
	}
}

// AdminBatchUpdateSettings 批量更新配置(事务保护)
// POST /admin/settings/batch
func (s *Server) AdminBatchUpdateSettings(c *gin.Context) {
	var req map[string]string
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, fmt.Sprintf("invalid request: %v", err))
		return
	}

	if len(req) == 0 {
		RespondErrorMsg(c, http.StatusBadRequest, "no settings to update")
		return
	}

	// 验证所有配置
	for key, value := range req {
		if rejectContainerManagedUpdateSetting(c, key) {
			return
		}
		setting := s.configService.GetSetting(key)
		if setting == nil {
			RespondErrorMsg(c, http.StatusBadRequest, fmt.Sprintf("unknown setting: %s", key))
			return
		}

		if err := validateSettingValue(key, setting.ValueType, value); err != nil {
			RespondErrorMsg(c, http.StatusBadRequest, fmt.Sprintf("invalid value for %s: %v", key, err))
			return
		}
	}
	updates, err := s.completeCooldownBoundUpdates(c.Request.Context(), req)
	if respondSettingCombinationError(c, err) {
		return
	}

	// 批量更新(事务保护)
	restartRequired, err := s.commitSettingUpdates(updates, func() error {
		return s.configService.BatchUpdateSettings(c.Request.Context(), updates)
	})
	if err != nil {
		log.Printf("[ERROR] AdminBatchUpdateSettings 失败: %v", err)
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	message := fmt.Sprintf("已保存 %d 项配置，已生效", len(req))
	if restartRequired {
		message = fmt.Sprintf("已保存 %d 项配置，程序将在2秒后重启", len(req))
		log.Printf("[INFO] 已批量更新 %d 项配置（需重启）", len(req))
	} else {
		log.Printf("[INFO] 已批量更新 %d 项配置（已热生效）", len(req))
	}

	RespondJSON(c, http.StatusOK, gin.H{
		"message": message,
	})

	if restartRequired {
		go s.triggerRestart()
	}
}

// validateSettingValue 验证配置值的合法性
func validateSettingValue(key, valueType, value string) error {
	if key == globalCooldownDetectionRulesSettingKey {
		_, err := parseGlobalCooldownDetectionRules(value)
		return err
	}
	if key == modelMultimodalFallbackSettingKey {
		_, err := parseMultimodalFallbackModels(value)
		return err
	}

	switch valueType {
	case "int":
		intVal, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("not a valid integer")
		}
		// 按配置项定义具体约束
		switch key {
		case "max_key_retries":
			if intVal < 1 {
				return fmt.Errorf("max_key_retries must be >= 1")
			}
		case "log_retention_days":
			if intVal != LogRetentionDaysDisabled && (intVal < LogRetentionDaysMin || intVal > LogRetentionDaysMax) {
				return fmt.Errorf("log_retention_days must be %d (永久) or %d-%d", LogRetentionDaysDisabled, LogRetentionDaysMin, LogRetentionDaysMax)
			}
		case autoUpdateIntervalSettingKey:
			if intVal < 0 || int64(intVal) > maxSettingDurationHours {
				return fmt.Errorf("%s must be between 0 and %d", key, maxSettingDurationHours)
			}
		case responsesWebsocketSessionTTLSetting:
			if intVal < 0 || int64(intVal) > maxSettingDurationMinutes {
				return fmt.Errorf("%s must be between 0 and %d", key, maxSettingDurationMinutes)
			}
		case responsesWebsocketMaxSessionsSetting,
			responsesWebsocketMaxTranscriptBytesSetting,
			responsesWebsocketMaxConnectionsSetting,
			responsesWebsocketMaxConnectionsPerTokenSetting:
			if intVal < 0 {
				return fmt.Errorf("%s must be >= 0", key)
			}
		case "success_rate_penalty_weight":
			if intVal < 0 {
				return fmt.Errorf("%s must be >= 0", key)
			}
		case "health_score_window_minutes":
			if intVal < 1 || int64(intVal) > maxSettingDurationMinutes {
				return fmt.Errorf("%s must be between 1 and %d", key, maxSettingDurationMinutes)
			}
		case "health_score_update_interval":
			if intVal < 1 || int64(intVal) > maxSettingDurationSeconds {
				return fmt.Errorf("%s must be between 1 and %d", key, maxSettingDurationSeconds)
			}
		case "health_min_confident_sample", "ttfb_min_confident_sample":
			if intVal < 1 {
				return fmt.Errorf("%s must be >= 1", key)
			}
		case "debug_log_retention_minutes":
			if intVal < 1 || intVal > 1440 {
				return fmt.Errorf("debug_log_retention_minutes must be between 1 and 1440")
			}
		case "auto_refresh_interval_seconds":
			if intVal < 0 || int64(intVal) > maxSettingDurationSeconds {
				return fmt.Errorf("%s must be between 0 and %d", key, maxSettingDurationSeconds)
			}
		case "max_concurrency",
			"max_body_bytes",
			"max_image_body_bytes",
			"cooldown_auth_seconds",
			"cooldown_server_seconds",
			"cooldown_timeout_seconds",
			"cooldown_rate_limit_seconds",
			cooldownMaxSecondsSettingKey,
			cooldownMinSecondsSettingKey:
			if intVal < 1 || int64(intVal) > maxSettingDurationSeconds {
				return fmt.Errorf("%s must be between 1 and %d", key, maxSettingDurationSeconds)
			}
		default:
			if intVal < -1 {
				return fmt.Errorf("value must be >= -1")
			}
		}

	case "float":
		floatVal, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return fmt.Errorf("not a valid number")
		}
		if math.IsNaN(floatVal) || math.IsInf(floatVal, 0) {
			return fmt.Errorf("must be a finite number")
		}
		switch key {
		case "channel_check_interval_hours", "model_catalog_sync_interval_hours":
			if floatVal < 0 || floatVal > float64(maxSettingDurationHours) {
				return fmt.Errorf("%s must be between 0 and %d", key, maxSettingDurationHours)
			}
		case "ttfb_penalty_weight", "ttfb_max_slow_ratio":
			if floatVal < 0 {
				return fmt.Errorf("%s must be >= 0", key)
			}
		}

	case "bool":
		if _, ok := parseSettingBool(value); !ok {
			return fmt.Errorf("must be true/false or 1/0")
		}

	case "duration":
		intVal, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("duration must be an integer (seconds)")
		}
		if intVal < 0 || int64(intVal) > maxSettingDurationSeconds {
			return fmt.Errorf("duration must be between 0 and %d seconds", maxSettingDurationSeconds)
		}

	case "string":
		switch key {
		case config.CodexBaseURLSettingKey,
			config.XAIBaseURLSettingKey,
			config.AntigravityURLSettingKey,
			config.AnthropicBaseURLSettingKey:
			return validateOptionalOAuthBaseURL(value)
		case "log_channel_click_action":
			if value != "edit" && value != "navigate" {
				return fmt.Errorf("log_channel_click_action must be edit or navigate")
			}
		case "auto_update_channel":
			_, err := version.ParseReleaseChannel(value)
			return err
		case "channel_test_content":
			if strings.TrimSpace(value) == "" {
				return fmt.Errorf("channel_test_content must not be blank")
			}
		case "channel_stats_range":
			if !validChannelStatsRange(value) {
				return fmt.Errorf("invalid channel_stats_range")
			}
		}

	case "json":
		if key != "antigravity_sensitive_words" {
			return fmt.Errorf("unknown JSON setting: %s", key)
		}
		return validateJSONStringArray(value)

	default:
		return fmt.Errorf("unknown value type: %s", valueType)
	}

	return nil
}

func validateJSONStringArray(value string) error {
	var items []string
	if err := json.Unmarshal([]byte(value), &items); err != nil {
		return fmt.Errorf("must be a JSON string array: %w", err)
	}
	if items == nil {
		return fmt.Errorf("must be a non-null JSON string array")
	}
	return nil
}

func validChannelStatsRange(value string) bool {
	switch value {
	case "today", "yesterday", "day_before_yesterday", "this_week", "last_week", "this_month", "last_month":
		return true
	default:
		return false
	}
}

func validateOptionalOAuthBaseURL(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if separator := strings.Index(value, "://"); separator >= 0 {
		correction := value[separator+3:]
		if strings.HasPrefix(correction, "http://") || strings.HasPrefix(correction, "https://") {
			return fmt.Errorf("URL contains a duplicated scheme; use %s", correction)
		}
	}
	parsed, err := neturl.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("must be empty or a valid HTTP(S) URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("URL scheme must be http or https")
	}
	if parsed.User != nil {
		return fmt.Errorf("URL must not contain user info")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("URL must not contain a query or fragment")
	}
	return nil
}

// SetRestartFunc 注入当前 Server 的重启函数（由 main 包提供，避免循环依赖）。
// 回调在锁外执行，允许重启流程并发访问 Server 而不会死锁。
func (s *Server) SetRestartFunc(fn func()) {
	if s == nil {
		return
	}
	s.restartMu.Lock()
	s.restartFunc = fn
	s.restartMu.Unlock()
}

func (s *Server) restartFuncSnapshot() func() {
	if s == nil {
		return nil
	}
	s.restartMu.RLock()
	fn := s.restartFunc
	s.restartMu.RUnlock()
	return fn
}

// triggerRestart 触发程序重启
// 依赖优雅关闭语义：触发 SIGTERM 后，HTTP 服务器应完成当前请求再退出。
func (s *Server) triggerRestart() {
	log.Print("[INFO] 配置变更触发重启...")

	fn := s.restartFuncSnapshot()
	if fn == nil {
		log.Printf("[ERROR] 重启函数为空，重启已跳过")
		return
	}
	fn()
}
