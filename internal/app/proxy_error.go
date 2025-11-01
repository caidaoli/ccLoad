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
// 使用 cooldownManager 统一处理冷却逻辑（DRY原则）
// 返回：(处理动作, 是否需要保存响应信息)
func (s *Server) handleProxyError(ctx context.Context, cfg *model.Config, keyIndex int,
	res *fwResult, err error) (cooldown.Action, bool) {

	var statusCode int
	var errorBody []byte
	var isNetworkError bool
	var headers map[string][]string // 提取响应头用于429错误分析

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
		headers = nil         // 网络错误无响应头
	} else {
		// HTTP错误
		statusCode = res.Status
		errorBody = res.Body
		isNetworkError = false // ✅ 标记为HTTP错误
		headers = res.Header   // 提取响应头用于429分析
	}

	// 使用 cooldownManager 统一处理冷却决策
	// 好处：消除重复逻辑，单一职责，便于测试和维护
	// manager.HandleError 现在不返回错误（日志记录方式）
	// 因此这里不再需要检查 cooldownErr，直接使用 action 即可
	action, _ := s.cooldownManager.HandleError(ctx, cfg.ID, keyIndex, statusCode, errorBody, isNetworkError, headers)

	// 根据冷却管理器的决策执行相应动作
	switch action {
	case cooldown.ActionRetryKey:
		// Key级错误：立即刷新相关缓存
		s.InvalidateAPIKeysCache(cfg.ID)
		s.invalidateCooldownCache()
		return action, true

	case cooldown.ActionRetryChannel:
		// 渠道级错误：刷新渠道与冷却缓存，确保下次选择避开问题渠道
		s.InvalidateChannelListCache()
		s.InvalidateAPIKeysCache(cfg.ID)
		s.invalidateCooldownCache()
		return action, true

	default:
		// 客户端错误或未知错误：直接返回
		return action, false
	}
}

// handleNetworkError 处理网络错误
// 从proxy.go提取，遵循SRP原则
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
	s.AddLogAsync(buildLogEntry(actualModel, &cfg.ID, statusCode,
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

	// 修复首字节超时不切换渠道的问题
	// 当 handleProxyError 返回 ActionRetryChannel 时，应该立即切换到下一个渠道
	// 而不是继续尝试当前渠道的其他Key
	if action == cooldown.ActionRetryChannel {
		return nil, false, true // 切换到下一个渠道
	}

	return nil, true, false // 继续重试下一个Key
}

// handleProxySuccess 处理代理成功响应（业务逻辑层）
// 使用 cooldownManager 统一管理冷却状态清除
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
	// 使用 cooldownManager 清除冷却状态
	// 记录清除失败但不中断成功响应
	// 设计原则: 清除失败不应影响用户请求成功，但需要记录用于监控
	if err := s.cooldownManager.ClearChannelCooldown(ctx, cfg.ID); err != nil {
		_ = err // 忽略清除失败，不影响成功响应
	}
	if err := s.cooldownManager.ClearKeyCooldown(ctx, cfg.ID, keyIndex); err != nil {
		_ = err // 忽略清除失败，不影响成功响应
	}

	// 冷却状态已恢复，刷新相关缓存避免下次命中过期数据
	s.InvalidateChannelListCache()
	s.InvalidateAPIKeysCache(cfg.ID)
	s.invalidateCooldownCache()

	// 精确计数：记录状态恢复

	// 记录成功日志
	// ✅ 修复：使用 actualModel 而非 reqCtx.originalModel
	isStreaming := res.FirstByteTime > 0 // 根据首字节时间判断是否为流式请求
	s.AddLogAsync(buildLogEntry(actualModel, &cfg.ID, res.Status,
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
// 从proxy.go提取，遵循SRP原则
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

	// ✅ 日志改进 (2025-10-28): 明确标识上游返回的499错误
	errMsg := ""
	if res.Status == 499 {
		errMsg = "upstream returned 499 (not client cancel)"
	}

	s.AddLogAsync(buildLogEntry(actualModel, &cfg.ID, res.Status,
		duration, isStreaming, selectedKey, res, errMsg))

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
