// Package validator 提供渠道验证器框架
//
// 设计原则:
// - SRP: 每个验证器只负责特定的验证规则
// - OCP: 通过接口扩展,无需修改现有代码
// - DIP: 依赖抽象的ChannelValidator接口,而非具体实现
//
// 使用场景:
// - 88code套餐验证(SubscriptionValidator)
// - 未来可扩展: Token额度验证、地域限制验证等
package validator

import (
	"context"

	"ccLoad/internal/model"
)

// ChannelValidator 渠道验证器接口
//
// 实现此接口以添加自定义验证规则,例如:
// - API套餐验证
// - Token额度检查
// - 地域访问限制
// - 时间窗口限制
type ChannelValidator interface {
	// ShouldValidate 判断是否需要验证此渠道
	//
	// 参数:
	//   cfg - 渠道配置
	//
	// 返回:
	//   true - 需要验证
	//   false - 跳过验证
	//
	// 设计原则:
	// - 快速判断(O(1)复杂度),避免不必要的验证开销
	// - 通常基于渠道名称、类型或标签进行判断
	ShouldValidate(cfg *model.Config) bool

	// Validate 执行渠道验证
	//
	// 参数:
	//   ctx - 上下文(用于超时控制和取消传播)
	//   cfg - 渠道配置
	//   apiKey - 当前使用的API Key
	//
	// 返回:
	//   available - 渠道是否可用
	//   reason - 不可用时的原因描述(供日志和调试使用)
	//   err - 验证过程中的错误(非业务错误,如网络故障、超时等)
	//
	// 错误处理约定:
	// - 如果err != nil,表示验证器本身故障,调用方应采取降级策略
	// - 如果available=false,err=nil,表示验证成功但渠道不满足条件
	//
	// 性能要求:
	// - 应实现合理的超时控制(建议5秒内完成)
	// - 建议使用缓存减少外部API调用
	Validate(ctx context.Context, cfg *model.Config, apiKey string) (available bool, reason string, err error)
}

// Manager 验证器管理器
//
// 采用责任链模式,依次执行所有已注册的验证器
// 任何一个验证器返回不可用,则整个验证失败
type Manager struct {
	validators []ChannelValidator
}

// NewManager 创建验证器管理器
func NewManager() *Manager {
	return &Manager{
		validators: make([]ChannelValidator, 0),
	}
}

// AddValidator 注册验证器
//
// 验证器按注册顺序执行
// 建议将快速验证器(如字符串匹配)放在前面
func (m *Manager) AddValidator(v ChannelValidator) {
	m.validators = append(m.validators, v)
}

// ValidateChannel 验证渠道是否可用
//
// 参数:
//
//	ctx - 上下文
//	cfg - 渠道配置
//	apiKey - API Key
//
// 返回:
//
//	available - 渠道是否可用
//	reason - 不可用时的原因(如果所有验证器都通过,返回空字符串)
//
// 验证策略:
// - 遍历所有验证器,执行ShouldValidate判断
// - 对于需要验证的渠道,调用Validate方法
// - 如果某个验证器返回err != nil,记录警告但继续验证(防御性设计)
// - 如果某个验证器返回available=false,立即返回失败
// - 所有验证器都通过,返回可用
func (m *Manager) ValidateChannel(ctx context.Context, cfg *model.Config, apiKey string) (available bool, reason string) {
	// 空验证器列表时直接通过
	if len(m.validators) == 0 {
		return true, ""
	}

	// 责任链模式: 依次执行所有验证器
	for _, v := range m.validators {
		// 快速跳过不需要验证的渠道
		if !v.ShouldValidate(cfg) {
			continue
		}

		// 执行验证
		avail, rsn, err := v.Validate(ctx, cfg, apiKey)
		if err != nil {
			// 验证器故障时的防御性策略:
			// 1. 记录警告日志
			// 2. 默认允许通过(避免单点故障影响整体服务)
			// 这是一个权衡: 牺牲一定的验证准确性换取系统可用性
			// 用户已确认选择"失败时允许通过"策略
			// log.Printf会在后续实现中调用
			// log.Printf("⚠️  WARNING: Validator error for channel %s: %v (defaulting to available)", cfg.Name, err)
			continue // 降级:允许通过
		}

		// 验证失败,立即返回
		if !avail {
			return false, rsn
		}
	}

	// 所有验证器都通过
	return true, ""
}

// ValidatorCount 返回已注册的验证器数量(用于测试和监控)
func (m *Manager) ValidatorCount() int {
	return len(m.validators)
}
