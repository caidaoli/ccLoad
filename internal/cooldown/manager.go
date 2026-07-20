// Package cooldown 提供渠道和Key的冷却决策管理
package cooldown

import (
	"context"
	"log"
	"strings"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/util"
)

// Action 表示冷却后的建议行动类型。
type Action int

// Action 常量定义冷却后的建议行动。
const (
	ActionRetryKey     Action = iota // ActionRetryKey 表示重试当前渠道的其他Key
	ActionRetryModel                 // ActionRetryModel 表示当前模型在该渠道不可用，切换渠道
	ActionRetryChannel               // ActionRetryChannel 表示切换到下一个渠道
	ActionReturnClient               // ActionReturnClient 表示直接返回给客户端
)

// NoKeyIndex 表示错误与特定Key无关（网络错误、DNS解析失败等）。
// 用于 HandleError 的 keyIndex 参数。
const NoKeyIndex = -1

// ErrorInput 包含错误处理所需的输入信息。
type ErrorInput struct {
	ChannelID      int64
	ChannelType    string   // 渠道类型，用于特定渠道的错误处理策略
	Model          string   // 实际发送给上游的模型名
	ChannelModels  []string // 该渠道可实际发送的模型键，用于判断模型资源是否全部冷却
	KeyIndex       int
	StatusCode     int
	ErrorBody      []byte
	IsNetworkError bool
	Headers        map[string][]string
}

// ConfigGetter 获取渠道配置的接口（支持缓存）
// 设计原则：接口隔离，cooldown包不依赖具体的cache实现
type ConfigGetter interface {
	GetConfig(ctx context.Context, channelID int64) (*model.Config, error)
}

// Manager 冷却管理器
// 统一管理 Key、模型和渠道冷却逻辑
// 遵循SRP原则：专注于冷却决策和执行
type Manager struct {
	store        storage.Store
	configGetter ConfigGetter // 可选：优先使用缓存层（性能提升~60%）
}

type cooldownDecision struct {
	action                  Action
	keyCooldownUntil        time.Time
	hasKeyCooldownUntil     bool
	keyCooldownReason       string
	model                   string
	modelScoped             bool
	modelCooldownUntil      time.Time
	channelCooldownUntil    time.Time
	hasChannelCooldownUntil bool
	channelCooldownReason   string
}

// NewManager 创建冷却管理器实例
// configGetter: 可选参数，传入nil时降级到store.GetConfig
func NewManager(store storage.Store, configGetter ConfigGetter) *Manager {
	return &Manager{
		store:        store,
		configGetter: configGetter,
	}
}

func (m *Manager) classifyDecision(in ErrorInput) cooldownDecision {
	var errLevel util.ErrorLevel

	statusCode := in.StatusCode
	errorBody := in.ErrorBody

	decision := cooldownDecision{
		action: ActionReturnClient,
	}

	// 1. 区分网络错误和HTTP错误的分类策略
	if in.IsNetworkError {
		// 网络错误默认按"渠道级"处理：这类问题通常是上游/链路/负载，而不是某个Key的固有属性。
		// 继续在同一渠道里换Key只是在浪费重试预算、扩大故障面。
		errLevel = util.ErrorLevelChannel
	} else {
		// HTTP错误: 使用智能分类器(结合响应体内容和headers)
		classification := util.ClassifyHTTPResponseWithMeta(statusCode, in.Headers, errorBody)
		errLevel = classification.Level
		decision.keyCooldownUntil = classification.KeyCooldownUntil
		decision.hasKeyCooldownUntil = classification.HasKeyCooldownUntil
		decision.keyCooldownReason = classification.KeyCooldownReason
		decision.channelCooldownUntil = classification.ChannelCooldownUntil
		decision.hasChannelCooldownUntil = classification.HasChannelCooldownUntil
		decision.channelCooldownReason = classification.ChannelCooldownReason

		if decision.hasKeyCooldownUntil && decision.keyCooldownReason == "model_cooldown" {
			decision.model = strings.TrimSpace(in.Model)
			if decision.model == "" {
				decision.model = strings.TrimSpace(classification.Model)
			}
			decision.modelScoped = true
			decision.modelCooldownUntil = decision.keyCooldownUntil
			// 模型级故障需要切换渠道，但不得冷却 Key 或整个渠道。
			errLevel = util.ErrorLevelChannel
		} else if classification.ModelScoped {
			decision.model = strings.TrimSpace(in.Model)
			if decision.model == "" {
				decision.model = strings.TrimSpace(classification.Model)
			}
			if decision.model != "" {
				decision.modelScoped = true
				decision.modelCooldownUntil = time.Now().Add(util.DefaultModelCooldownDuration)
			}
		} else if errLevel == util.ErrorLevelChannel &&
			!decision.hasChannelCooldownUntil &&
			util.IsModelScopedHTTPStatus(statusCode) {
			decision.model = strings.TrimSpace(in.Model)
			if decision.model != "" {
				decision.modelScoped = true
				decision.modelCooldownUntil = time.Now().Add(util.DefaultModelCooldownDuration)
			}
		}
	}

	// 2. 仅给出动作决策（不产生副作用）
	if decision.modelScoped {
		decision.action = ActionRetryModel
		return decision
	}
	switch errLevel {
	case util.ErrorLevelClient:
		decision.action = ActionReturnClient
	case util.ErrorLevelKey:
		decision.action = ActionRetryKey
	case util.ErrorLevelChannel:
		decision.action = ActionRetryChannel
	default:
		decision.action = ActionReturnClient
	}

	return decision
}

// DecideAction 仅做错误分类和动作决策，不写入任何冷却状态。
func (m *Manager) DecideAction(ctx context.Context, in ErrorInput) Action {
	return m.classifyDecision(in).action
}

// HandleError 统一错误处理与冷却决策
// 将proxy_error.go中的handleProxyError逻辑提取到专用模块
//
// 输入:
//   - ChannelID / KeyIndex: 目标渠道与Key（KeyIndex=NoKeyIndex 表示与特定Key无关）
//   - StatusCode / ErrorBody / Headers: 上游错误信息（Headers 用于 429 限流范围分析）
//   - IsNetworkError: 是否为网络错误（与HTTP错误区分）
//
// 返回:
//   - Action: 建议采取的行动
func (m *Manager) HandleError(ctx context.Context, in ErrorInput) Action {
	decision := m.classifyDecision(in)
	channelID := in.ChannelID
	keyIndex := in.KeyIndex
	statusCode := in.StatusCode

	// 4. 根据错误级别执行冷却
	switch decision.action {
	case ActionReturnClient:
		// 客户端错误:不冷却,直接返回
		return ActionReturnClient

	case ActionRetryKey:
		// Key级错误:冷却当前Key,继续尝试其他Key
		if keyIndex != NoKeyIndex {
			// [INFO] 特殊处理: 已知Key配额错误自动禁用到指定时间
			if decision.hasKeyCooldownUntil {
				// 直接设置冷却时间到指定时刻
				if err := m.store.SetKeyCooldown(ctx, channelID, keyIndex, decision.keyCooldownUntil); err != nil {
					log.Printf("[WARN] 按重置时间设置 Key 冷却失败 (channel=%d, key=%d, until=%v): %v",
						channelID, keyIndex, decision.keyCooldownUntil, err)
				} else {
					duration := time.Until(decision.keyCooldownUntil)
					log.Printf("[COOLDOWN] Key冷却(%s): 渠道=%d Key=%d 禁用至 %s (%.1f分钟)",
						decision.keyCooldownReason, channelID, keyIndex,
						decision.keyCooldownUntil.Format("2006-01-02 15:04:05"), duration.Minutes())
				}
			} else {
				// 默认逻辑: 使用指数退避策略
				_, err := m.store.BumpKeyCooldown(ctx, channelID, keyIndex, time.Now(), statusCode)
				if err != nil {
					// 冷却更新失败是非致命错误
					// 记录日志但不中断请求处理,避免因数据库BUSY导致无限重试
					log.Printf("[WARN] 更新 Key 冷却失败 (channel=%d, key=%d): %v", channelID, keyIndex, err)
				}
			}
		}
		if m.promoteExhaustedResources(ctx, in) {
			return ActionRetryChannel
		}
		return ActionRetryKey

	case ActionRetryModel:
		if decision.model == "" {
			log.Printf("[WARN] 收到 model_cooldown 但缺少模型名，跳过持久化 (channel=%d)", channelID)
			return ActionRetryModel
		}
		if err := m.store.SetModelCooldown(ctx, channelID, decision.model, decision.modelCooldownUntil); err != nil {
			log.Printf("[WARN] 设置模型冷却失败 (channel=%d, model=%s, until=%v): %v",
				channelID, decision.model, decision.modelCooldownUntil, err)
		} else {
			duration := time.Until(decision.modelCooldownUntil)
			log.Printf("[COOLDOWN] 模型冷却: 渠道=%d 模型=%s 禁用至 %s (%.1f分钟)",
				channelID, decision.model,
				decision.modelCooldownUntil.Format("2006-01-02 15:04:05"), duration.Minutes())
		}
		if m.promoteExhaustedResources(ctx, in) {
			return ActionRetryChannel
		}
		return ActionRetryModel

	case ActionRetryChannel:
		// 渠道级错误:冷却整个渠道,切换到其他渠道
		if decision.hasChannelCooldownUntil {
			if err := m.store.SetChannelCooldown(ctx, channelID, decision.channelCooldownUntil); err != nil {
				log.Printf("[WARN] 按重置时间设置渠道冷却失败 (channel=%d, until=%v): %v",
					channelID, decision.channelCooldownUntil, err)
			} else {
				duration := time.Until(decision.channelCooldownUntil)
				log.Printf("[COOLDOWN] 渠道冷却(%s): 渠道=%d 禁用至 %s (%.1f分钟)",
					decision.channelCooldownReason, channelID,
					decision.channelCooldownUntil.Format("2006-01-02 15:04:05"), duration.Minutes())
			}
			return ActionRetryChannel
		}

		// 默认逻辑: 使用指数退避策略
		_, err := m.store.BumpChannelCooldown(ctx, channelID, time.Now(), statusCode)
		if err != nil {
			// 冷却更新失败是非致命错误
			// 设计原则: 数据库故障不应阻塞用户请求,系统应降级服务
			// 影响: 可能导致短暂的冷却状态不一致,但总比拒绝服务更好
			log.Printf("[WARN] 更新渠道冷却失败 (channel=%d): %v", channelID, err)
		}
		return ActionRetryChannel

	default:
		// 未知错误级别:保守策略,直接返回
		return ActionReturnClient
	}
}

func (m *Manager) promoteExhaustedResources(ctx context.Context, in ErrorInput) bool {
	now := time.Now()
	keyUntil, allKeysCooled := m.allEnabledKeysCooldownUntil(ctx, in.ChannelID, now)
	modelUntil, allModelsCooled := m.allConfiguredModelsCooldownUntil(ctx, in, now)
	if !allKeysCooled && !allModelsCooled {
		return false
	}

	channelUntil := keyUntil
	reason := "all_keys_cooled"
	if allModelsCooled {
		if !allKeysCooled || modelUntil.After(channelUntil) {
			channelUntil = modelUntil
		}
		reason = "all_models_cooled"
	}
	if allKeysCooled && allModelsCooled {
		reason = "all_models_and_keys_cooled"
	}

	if err := m.store.SetChannelCooldown(ctx, in.ChannelID, channelUntil); err != nil {
		log.Printf("[WARN] 资源耗尽后设置渠道冷却失败 (channel=%d, reason=%s, until=%v): %v",
			in.ChannelID, reason, channelUntil, err)
	} else {
		log.Printf("[COOLDOWN] 渠道冷却(%s): 渠道=%d 禁用至 %s",
			reason, in.ChannelID, channelUntil.Format("2006-01-02 15:04:05"))
	}
	return true
}

func (m *Manager) allEnabledKeysCooldownUntil(ctx context.Context, channelID int64, now time.Time) (time.Time, bool) {
	keys, err := m.store.GetAPIKeys(ctx, channelID)
	if err != nil {
		log.Printf("[WARN] 查询 Key 冷却状态失败 (channel=%d): %v", channelID, err)
		return time.Time{}, false
	}

	var earliest time.Time
	enabledCount := 0
	for _, key := range keys {
		if key == nil || key.Disabled {
			continue
		}
		enabledCount++
		until := time.Unix(key.CooldownUntil, 0)
		if !until.After(now) {
			return time.Time{}, false
		}
		if earliest.IsZero() || until.Before(earliest) {
			earliest = until
		}
	}
	return earliest, enabledCount > 0 && !earliest.IsZero()
}

func (m *Manager) allConfiguredModelsCooldownUntil(ctx context.Context, in ErrorInput, now time.Time) (time.Time, bool) {
	models := normalizeModelKeys(in.ChannelModels)
	if len(models) == 0 {
		models = m.configuredModelKeys(ctx, in.ChannelID)
	}
	if len(models) == 0 {
		return time.Time{}, false
	}

	cooldowns, err := m.store.GetAllModelCooldowns(ctx)
	if err != nil {
		log.Printf("[WARN] 查询模型冷却状态失败 (channel=%d): %v", in.ChannelID, err)
		return time.Time{}, false
	}
	channelCooldowns := cooldowns[in.ChannelID]
	var earliest time.Time
	for _, modelName := range models {
		until, exists := channelCooldowns[modelName]
		if !exists || !until.After(now) {
			return time.Time{}, false
		}
		if earliest.IsZero() || until.Before(earliest) {
			earliest = until
		}
	}
	return earliest, !earliest.IsZero()
}

func (m *Manager) configuredModelKeys(ctx context.Context, channelID int64) []string {
	var (
		cfg *model.Config
		err error
	)
	if m.configGetter != nil {
		cfg, err = m.configGetter.GetConfig(ctx, channelID)
	} else {
		cfg, err = m.store.GetConfig(ctx, channelID)
	}
	if err != nil || cfg == nil {
		if err != nil {
			log.Printf("[WARN] 查询渠道模型失败 (channel=%d): %v", channelID, err)
		}
		return nil
	}

	models := make([]string, 0, len(cfg.ModelEntries))
	for _, entry := range cfg.ModelEntries {
		modelName := strings.TrimSpace(entry.RedirectModel)
		if modelName == "" {
			modelName = strings.TrimSpace(entry.Model)
		}
		models = append(models, modelName)
	}
	return normalizeModelKeys(models)
}

func normalizeModelKeys(models []string) []string {
	seen := make(map[string]struct{}, len(models))
	normalized := make([]string, 0, len(models))
	for _, modelName := range models {
		modelName = strings.TrimSpace(modelName)
		if modelName == "" {
			continue
		}
		if _, exists := seen[modelName]; exists {
			continue
		}
		seen[modelName] = struct{}{}
		normalized = append(normalized, modelName)
	}
	return normalized
}

// ClearChannelCooldown 清除渠道冷却状态
// 简化成功后的冷却清除逻辑
func (m *Manager) ClearChannelCooldown(ctx context.Context, channelID int64) error {
	return m.store.ResetChannelCooldown(ctx, channelID)
}

// ClearKeyCooldown 清除Key冷却状态
// 简化成功后的冷却清除逻辑
func (m *Manager) ClearKeyCooldown(ctx context.Context, channelID int64, keyIndex int) error {
	return m.store.ResetKeyCooldown(ctx, channelID, keyIndex)
}

// ClearModelCooldown 清除指定渠道的实际上游模型冷却。
func (m *Manager) ClearModelCooldown(ctx context.Context, channelID int64, model string) error {
	return m.store.ResetModelCooldown(ctx, channelID, model)
}
