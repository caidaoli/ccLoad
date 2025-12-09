package cooldown

import (
	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/util"
	"context"
	"log"
	"time"
)

// Action 冷却后的建议行动
type Action int

const (
	ActionRetryKey     Action = iota // 重试当前渠道的其他Key
	ActionRetryChannel               // 切换到下一个渠道
	ActionReturnClient               // 直接返回给客户端
)

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
	headers map[string][]string, // 新增headers参数用于429错误分析
) (Action, error) {
	var errLevel util.ErrorLevel

	// 1. 区分网络错误和HTTP错误的分类策略
	if isNetworkError {
		// ✅ 网络错误特殊处理: 区分首字节超时、整体超时以及普通网络波动
		// util.StatusFirstByteTimeout (598) → 渠道级错误（首字节超时，固定1分钟冷却）
		// 504 Gateway Timeout → 渠道级错误（上游整体超时）
		// 其他可重试错误(502等) → 默认Key级错误（可能只是单个Key的连接问题）
		if statusCode == util.StatusFirstByteTimeout || statusCode == 504 {
			errLevel = util.ErrorLevelChannel
		} else {
			errLevel = util.ErrorLevelKey
		}
	} else {
		// HTTP错误: 使用智能分类器(结合响应体内容和headers)

		// 429错误特殊处理
		if statusCode == 429 && headers != nil {
			// 使用增强的Rate Limit分类器
			errLevel = util.ClassifyRateLimitError(headers, errorBody)
		} else {
			// 其他HTTP错误使用标准分类器
			errLevel = util.ClassifyHTTPStatusWithBody(statusCode, errorBody)
		}
	}

	// 2. 🎯 提前检查1308错误（在升级逻辑之前）
	// 1308错误包含精确的重置时间，无论Key级还是Channel级都应该使用
	// ✅ 修复（2025-12-09）：不限制状态码，因为1308可能以不同方式返回：
	//    - HTTP 429 + 错误体包含1308（传统方式）
	//    - HTTP 200 + SSE error事件包含1308（流式响应方式）
	var reset1308Time time.Time
	var has1308Time bool
	if len(errorBody) > 0 {
		reset1308Time, has1308Time = util.ParseResetTimeFrom1308Error(errorBody)
	}

	// 3. 🎯 动态调整:单Key渠道的Key级错误应该直接冷却渠道
	// 设计原则:如果没有其他Key可以重试,Key级错误等同于渠道级错误
	// 优先使用缓存的KeyCount,避免N+1查询(性能提升~60%)
	// ⚠️ 例外：1308错误保持Key级（因为它有精确时间，后续会特殊处理）
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
		return ActionReturnClient, nil

	case util.ErrorLevelKey:
		// Key级错误:冷却当前Key,继续尝试其他Key
		if keyIndex >= 0 {
			// ✅ 特殊处理: 1308错误自动禁用到指定时间
			if has1308Time {
				// 直接设置冷却时间到指定时刻
				if err := m.store.SetKeyCooldown(ctx, channelID, keyIndex, reset1308Time); err != nil {
					log.Printf("⚠️  WARNING: Failed to set key cooldown to reset time (channel=%d, key=%d, until=%v): %v",
						channelID, keyIndex, reset1308Time, err)
				} else {
					duration := time.Until(reset1308Time)
					log.Printf("🔒 Key冷却(1308): 渠道=%d Key=%d 禁用至 %s (%.1f分钟)",
						channelID, keyIndex, reset1308Time.Format("2006-01-02 15:04:05"), duration.Minutes())
				}
				return ActionRetryKey, nil
			}

			// 默认逻辑: 使用指数退避策略
			_, err := m.store.BumpKeyCooldown(ctx, channelID, keyIndex, time.Now(), statusCode)
			if err != nil {
				// 冷却更新失败是非致命错误
				// 记录日志但不中断请求处理,避免因数据库BUSY导致无限重试
				log.Printf("⚠️  WARNING: Failed to update key cooldown (channel=%d, key=%d): %v", channelID, keyIndex, err)
			}
		}
		return ActionRetryKey, nil

	case util.ErrorLevelChannel:
		// 渠道级错误:冷却整个渠道,切换到其他渠道
		// ✅ 特殊处理: 如果有1308精确时间，直接设置（单Key渠道的1308错误会走到这里）
		if has1308Time {
			if err := m.store.SetChannelCooldown(ctx, channelID, reset1308Time); err != nil {
				log.Printf("⚠️  WARNING: Failed to set channel cooldown to reset time (channel=%d, until=%v): %v",
					channelID, reset1308Time, err)
			} else {
				duration := time.Until(reset1308Time)
				log.Printf("🔒 Channel冷却(1308): 渠道=%d 禁用至 %s (%.1f分钟)",
					channelID, reset1308Time.Format("2006-01-02 15:04:05"), duration.Minutes())
			}
			return ActionRetryChannel, nil
		}

		// 默认逻辑: 使用指数退避策略
		_, err := m.store.BumpChannelCooldown(ctx, channelID, time.Now(), statusCode)
		if err != nil {
			// 冷却更新失败是非致命错误
			// 设计原则: 数据库故障不应阻塞用户请求,系统应降级服务
			// 影响: 可能导致短暂的冷却状态不一致,但总比拒绝服务更好
			log.Printf("⚠️  WARNING: Failed to update channel cooldown (channel=%d): %v", channelID, err)
		}
		return ActionRetryChannel, nil

	default:
		// 未知错误级别:保守策略,直接返回
		return ActionReturnClient, nil
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
