// Package cooldown 提供渠道和Key的冷却决策管理
package cooldown

import (
	"context"
	"log"
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
	ActionRetryChannel               // ActionRetryChannel 表示切换到下一个渠道
	ActionReturnClient               // ActionReturnClient 表示直接返回给客户端
)

// NoKeyIndex 表示错误与特定Key无关（网络错误、DNS解析失败等）。
// 用于 HandleError 的 keyIndex 参数。
const NoKeyIndex = -1

// ErrorInput 包含错误处理所需的输入信息。
type ErrorInput struct {
	ChannelID      int64
	ChannelType    string // 渠道类型，用于特定渠道的错误处理策略
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
// 统一管理渠道级和Key级冷却逻辑
// 遵循SRP原则：专注于冷却决策和执行
type Manager struct {
	store        storage.Store
	configGetter ConfigGetter // 可选：优先使用缓存层（性能提升~60%）
}

// NewManager 创建冷却管理器实例
// configGetter: 可选参数，传入nil时降级到store.GetConfig
func NewManager(store storage.Store, configGetter ConfigGetter) *Manager {
	return &Manager{
		store:        store,
		configGetter: configGetter,
	}
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
	var errLevel util.ErrorLevel

	channelID := in.ChannelID
	keyIndex := in.KeyIndex
	statusCode := in.StatusCode
	errorBody := in.ErrorBody

	// 1. 区分网络错误和HTTP错误的分类策略
	var reset1308Time time.Time
	var has1308Time bool
	if in.IsNetworkError {
		// 网络错误默认按"渠道级"处理：这类问题通常是上游/链路/负载，而不是某个Key的固有属性。
		// 继续在同一渠道里换Key只是在浪费重试预算、扩大故障面。
		errLevel = util.ErrorLevelChannel
	} else {
		// HTTP错误: 使用智能分类器(结合响应体内容和headers)
		classification := util.ClassifyHTTPResponseWithMeta(statusCode, in.Headers, errorBody)
		errLevel = classification.Level
		reset1308Time = classification.ResetTime1308
		has1308Time = classification.HasResetTime1308
	}

	// 2. [TARGET] 动态调整:单Key渠道的Key级错误应该直接冷却渠道
	// 设计原则:如果没有其他Key可以重试,Key级错误等同于渠道级错误
	// [WARN] 例外：1308错误保持Key级（因为它有精确时间，后续会特殊处理）
	if errLevel == util.ErrorLevelKey && !has1308Time {
		var config *model.Config
		var err error

		// 优先使用缓存层（如果可用）
		if m.configGetter != nil {
			config, err = m.configGetter.GetConfig(ctx, channelID)
		} else {
			config, err = m.store.GetConfig(ctx, channelID)
		}

		// 查询失败或单Key渠道:直接升级为渠道级错误
		if err != nil || config == nil || config.KeyCount <= 1 {
			errLevel = util.ErrorLevelChannel
		}
	}

	// 4. 根据错误级别执行冷却
	switch errLevel {
	case util.ErrorLevelClient:
		// 客户端错误:不冷却,直接返回
		return ActionReturnClient

	case util.ErrorLevelKey:
		// Key级错误:冷却当前Key,继续尝试其他Key
		if keyIndex != NoKeyIndex {
			// [INFO] 特殊处理: 1308错误自动禁用到指定时间
			if has1308Time {
				// 直接设置冷却时间到指定时刻
				if err := m.store.SetKeyCooldown(ctx, channelID, keyIndex, reset1308Time); err != nil {
					log.Printf("[WARN] Failed to set key cooldown to reset time (channel=%d, key=%d, until=%v): %v",
						channelID, keyIndex, reset1308Time, err)
				} else {
					duration := time.Until(reset1308Time)
					log.Printf("[COOLDOWN] Key冷却(1308): 渠道=%d Key=%d 禁用至 %s (%.1f分钟)",
						channelID, keyIndex, reset1308Time.Format("2006-01-02 15:04:05"), duration.Minutes())
				}
				return ActionRetryKey
			}

			// 默认逻辑: 使用指数退避策略
			_, err := m.store.BumpKeyCooldown(ctx, channelID, keyIndex, time.Now(), statusCode)
			if err != nil {
				// 冷却更新失败是非致命错误
				// 记录日志但不中断请求处理,避免因数据库BUSY导致无限重试
				log.Printf("[WARN] Failed to update key cooldown (channel=%d, key=%d): %v", channelID, keyIndex, err)
			}
		}
		return ActionRetryKey

	case util.ErrorLevelChannel:
		// 渠道级错误:冷却整个渠道,切换到其他渠道
		// [INFO] 特殊处理: 如果有1308精确时间，直接设置（单Key渠道的1308错误会走到这里）
		if has1308Time {
			if err := m.store.SetChannelCooldown(ctx, channelID, reset1308Time); err != nil {
				log.Printf("[WARN] Failed to set channel cooldown to reset time (channel=%d, until=%v): %v",
					channelID, reset1308Time, err)
			} else {
				duration := time.Until(reset1308Time)
				log.Printf("[COOLDOWN] Channel冷却(1308): 渠道=%d 禁用至 %s (%.1f分钟)",
					channelID, reset1308Time.Format("2006-01-02 15:04:05"), duration.Minutes())
			}
			return ActionRetryChannel
		}

		// 默认逻辑: 使用指数退避策略
		_, err := m.store.BumpChannelCooldown(ctx, channelID, time.Now(), statusCode)
		if err != nil {
			// 冷却更新失败是非致命错误
			// 设计原则: 数据库故障不应阻塞用户请求,系统应降级服务
			// 影响: 可能导致短暂的冷却状态不一致,但总比拒绝服务更好
			log.Printf("[WARN] Failed to update channel cooldown (channel=%d): %v", channelID, err)
		}
		return ActionRetryChannel

	default:
		// 未知错误级别:保守策略,直接返回
		return ActionReturnClient
	}
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
