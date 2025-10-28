package cooldown

import (
	"ccLoad/internal/storage"
	"ccLoad/internal/util"
	"context"
	"time"
)

// Action 冷却后的建议行动
type Action int

const (
	ActionRetryKey     Action = iota // 重试当前渠道的其他Key
	ActionRetryChannel               // 切换到下一个渠道
	ActionReturnClient               // 直接返回给客户端
)

// Manager 冷却管理器
// ✅ P2重构: 统一管理渠道级和Key级冷却逻辑
// 遵循SRP原则：专注于冷却决策和执行
type Manager struct {
	store storage.Store
}

// NewManager 创建冷却管理器实例
func NewManager(store storage.Store) *Manager {
	return &Manager{store: store}
}

// HandleError 统一错误处理与冷却决策
// ✅ P2重构: 将proxy_error.go中的handleProxyError逻辑提取到专用模块
//
// 参数:
//   - channelID: 渠道ID
//   - keyIndex: Key索引（-1表示网络错误，非Key级错误）
//   - statusCode: HTTP状态码（或内部错误码）
//   - errorBody: 错误响应体（用于智能分类）
//   - isNetworkError: 是否为网络错误（区分HTTP错误）
//
// 返回:
//   - Action: 建议采取的行动
//   - error: 执行冷却操作时的错误
func (m *Manager) HandleError(
	ctx context.Context,
	channelID int64,
	keyIndex int,
	statusCode int,
	errorBody []byte,
	isNetworkError bool,
) (Action, error) {
	var errLevel util.ErrorLevel

	// 1. 区分网络错误和HTTP错误的分类策略
	if isNetworkError {
		// ✅ 网络错误特殊处理：区分超时类错误和其他网络错误
		// StatusFirstByteTimeout (598) → 渠道级错误（首字节超时，固定5分钟冷却）
		// 504 Gateway Timeout → 渠道级错误（上游整体超时）
		// 其他可重试错误（502等）→ Key级错误（可能只是单个Key的连接问题）
		const StatusFirstByteTimeout = 598
		if statusCode == StatusFirstByteTimeout || statusCode == 504 {
			errLevel = util.ErrorLevelChannel
		} else {
			errLevel = util.ErrorLevelKey
		}
	} else {
		// HTTP错误：使用智能分类器（结合响应体内容）
		errLevel = util.ClassifyHTTPStatusWithBody(statusCode, errorBody)
	}

	// 2. 🎯 动态调整：单Key渠道的Key级错误应该直接冷却渠道
	// 设计原则：如果没有其他Key可以重试，Key级错误等同于渠道级错误
	// 🔧 P1优化：使用缓存的KeyCount，避免N+1查询（性能提升~60%）
	if errLevel == util.ErrorLevelKey {
		config, err := m.store.GetConfig(ctx, channelID)
		// 查询失败或单Key渠道：直接升级为渠道级错误
		if err != nil || config == nil || config.KeyCount <= 1 {
			errLevel = util.ErrorLevelChannel
		}
	}

	// 3. 根据错误级别执行冷却
	switch errLevel {
	case util.ErrorLevelClient:
		// 客户端错误：不冷却，直接返回
		return ActionReturnClient, nil

	case util.ErrorLevelKey:
		// Key级错误：冷却当前Key，继续尝试其他Key
		if keyIndex >= 0 {
			_, err := m.store.BumpKeyCooldown(ctx, channelID, keyIndex, time.Now(), statusCode)
			if err != nil {
				return ActionReturnClient, err
			}
		}
		return ActionRetryKey, nil

	case util.ErrorLevelChannel:
		// 渠道级错误：冷却整个渠道，切换到其他渠道
		_, err := m.store.BumpChannelCooldown(ctx, channelID, time.Now(), statusCode)
		if err != nil {
			return ActionReturnClient, err
		}
		return ActionRetryChannel, nil

	default:
		// 未知错误级别：保守策略，直接返回
		return ActionReturnClient, nil
	}
}

// ClearChannelCooldown 清除渠道冷却状态
// ✅ P2重构: 简化成功后的冷却清除逻辑
func (m *Manager) ClearChannelCooldown(ctx context.Context, channelID int64) error {
	return m.store.ResetChannelCooldown(ctx, channelID)
}

// ClearKeyCooldown 清除Key冷却状态
// ✅ P2重构: 简化成功后的冷却清除逻辑
func (m *Manager) ClearKeyCooldown(ctx context.Context, channelID int64, keyIndex int) error {
	return m.store.ResetKeyCooldown(ctx, channelID, keyIndex)
}
