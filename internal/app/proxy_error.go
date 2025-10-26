package app

import (
	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
	"ccLoad/internal/util"
	"context"
)

// ============================================================================
// 错误处理核心函数
// ============================================================================

// handleProxyError 统一错误处理与冷却决策（遵循OCP原则）
// ✅ P2重构: 使用 cooldownManager 统一处理冷却逻辑（DRY原则）
// 返回：(处理动作, 是否需要保存响应信息)
func (s *Server) handleProxyError(ctx context.Context, cfg *model.Config, keyIndex int,
	res *fwResult, err error) (cooldown.Action, bool) {

	var statusCode int
	var errorBody []byte
	var isNetworkError bool

	// 确定状态码、错误体和错误类型
	if err != nil {
		// 网络错误：使用统一分类器
		classifiedStatus, _, shouldRetry := util.ClassifyError(err)
		if !shouldRetry {
			return cooldown.ActionReturnClient, false
		}
		statusCode = classifiedStatus
		errorBody = []byte(err.Error())
		isNetworkError = true // ✅ 标记为网络错误
	} else {
		// HTTP错误
		statusCode = res.Status
		errorBody = res.Body
		isNetworkError = false // ✅ 标记为HTTP错误
	}

	// ✅ P2重构：使用 cooldownManager 统一处理冷却决策
	// 好处：消除重复逻辑，单一职责，便于测试和维护
	action, cooldownErr := s.cooldownManager.HandleError(ctx, cfg.ID, keyIndex, statusCode, errorBody, isNetworkError)
	if cooldownErr != nil {
		// 冷却操作失败（如数据库错误），保守策略：直接返回客户端
		return cooldown.ActionReturnClient, false
	}

	// 根据冷却管理器的决策执行相应动作
	switch action {
	case cooldown.ActionRetryKey:
		// Key级错误：同时标记Key选择器（双重记录，便于选择器快速判断）
		_ = s.keySelector.MarkKeyError(ctx, cfg.ID, keyIndex, statusCode)
		// 精确计数（P1）：记录Key进入冷却
		return action, true

	case cooldown.ActionRetryChannel:
		// 渠道级错误：精确计数（P1）
		return action, true

	default:
		// 客户端错误或未知错误：直接返回
		return action, false
	}
}

// handleNetworkError 处理网络错误
// ✅ P2重构: 从proxy.go提取，遵循SRP原则
func (s *Server) handleNetworkError(
	ctx context.Context,
	cfg *model.Config,
	keyIndex int,
	actualModel string, // ✅ 重定向后的实际模型名称
	selectedKey string,
	duration float64,
	err error,
) (*proxyResult, bool, bool) {
	statusCode, _, _ := util.ClassifyError(err)
	// ✅ 修复：使用 actualModel 而非 reqCtx.originalModel
	s.addLogAsync(buildLogEntry(actualModel, &cfg.ID, statusCode,
		duration, false, selectedKey, nil, err.Error()))

	action, _ := s.handleProxyError(ctx, cfg, keyIndex, nil, err)
	if action == cooldown.ActionReturnClient {
		return &proxyResult{
			status:    statusCode,
			body:      []byte(err.Error()),
			channelID: &cfg.ID,
			message:   truncateErr(err.Error()),
			duration:  duration,
			succeeded: false,
		}, false, false
	}

	// ✅ P0修复 (2025-01-XX): 修复首字节超时不切换渠道的问题
	// 当 handleProxyError 返回 ActionRetryChannel 时，应该立即切换到下一个渠道
	// 而不是继续尝试当前渠道的其他Key
	if action == cooldown.ActionRetryChannel {
		return nil, false, true // 切换到下一个渠道
	}

	return nil, true, false // 继续重试下一个Key
}

// handleProxySuccess 处理代理成功响应（业务逻辑层）
// ✅ P2重构: 使用 cooldownManager 统一管理冷却状态清除
// 注意：与 handleSuccessResponse（HTTP层）不同
func (s *Server) handleProxySuccess(
	ctx context.Context,
	cfg *model.Config,
	keyIndex int,
	actualModel string, // ✅ 重定向后的实际模型名称
	selectedKey string,
	res *fwResult,
	duration float64,
) (*proxyResult, bool, bool) {
	// ✅ P2重构：使用 cooldownManager 清除冷却状态
	_ = s.cooldownManager.ClearChannelCooldown(ctx, cfg.ID)
	_ = s.keySelector.MarkKeySuccess(ctx, cfg.ID, keyIndex)
	// 精确计数（P1）：记录状态恢复

	// 记录成功日志
	// ✅ 修复：使用 actualModel 而非 reqCtx.originalModel
	isStreaming := res.FirstByteTime > 0 // 根据首字节时间判断是否为流式请求
	s.addLogAsync(buildLogEntry(actualModel, &cfg.ID, res.Status,
		duration, isStreaming, selectedKey, res, ""))

	return &proxyResult{
		status:    res.Status,
		header:    res.Header,
		channelID: &cfg.ID,
		message:   "ok",
		duration:  duration,
		succeeded: true,
	}, false, false
}

// handleProxyErrorResponse 处理代理错误响应（业务逻辑层）
// ✅ P2重构: 从proxy.go提取，遵循SRP原则
// 注意：与 handleErrorResponse（HTTP层）不同
func (s *Server) handleProxyErrorResponse(
	ctx context.Context,
	cfg *model.Config,
	keyIndex int,
	actualModel string, // ✅ 重定向后的实际模型名称
	selectedKey string,
	res *fwResult,
	duration float64,
) (*proxyResult, bool, bool) {
	// ✅ 修复：使用 actualModel 而非 reqCtx.originalModel
	isStreaming := res.FirstByteTime > 0 // 根据首字节时间判断是否为流式请求
	s.addLogAsync(buildLogEntry(actualModel, &cfg.ID, res.Status,
		duration, isStreaming, selectedKey, res, ""))

	action, _ := s.handleProxyError(ctx, cfg, keyIndex, res, nil)
	if action == cooldown.ActionReturnClient {
		return &proxyResult{
			status:    res.Status,
			header:    res.Header,
			body:      res.Body,
			channelID: &cfg.ID,
			duration:  duration,
			succeeded: false,
		}, false, false
	}

	if action == cooldown.ActionRetryChannel {
		return nil, false, true // 切换到下一个渠道
	}

	return nil, true, false // 继续重试下一个Key
}
