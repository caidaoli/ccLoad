package app

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
	"ccLoad/internal/testutil"
	"ccLoad/internal/util"

	"github.com/bytedance/sonic"
	"github.com/gin-gonic/gin"
)

// ==================== 渠道测试功能 ====================
// 从admin.go拆分渠道测试,遵循SRP原则

// HandleChannelTest 测试指定渠道的连通性
func (s *Server) HandleChannelTest(c *gin.Context) {
	// 解析渠道ID
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel id")
		return
	}

	// 解析请求体
	var testReq testutil.TestChannelRequest
	if err := BindAndValidate(c, &testReq); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}

	// 获取渠道配置
	cfg, err := s.store.GetConfig(c.Request.Context(), id)
	if err != nil {
		RespondError(c, http.StatusNotFound, fmt.Errorf("channel not found"))
		return
	}

	// 查询渠道的API Keys
	apiKeys, err := s.store.GetAPIKeys(c.Request.Context(), id)
	if err != nil || len(apiKeys) == 0 {
		RespondJSON(c, http.StatusOK, gin.H{
			"success": false,
			"error":   "渠道未配置有效的 API Key",
		})
		return
	}

	// 验证并选择 Key 索引
	keyIndex := testReq.KeyIndex
	if keyIndex < 0 || keyIndex >= len(apiKeys) {
		keyIndex = 0 // 默认使用第一个 Key
	}

	selectedKey := apiKeys[keyIndex].APIKey

	// 检查模型是否支持
	if !cfg.SupportsModel(testReq.Model) {
		RespondJSON(c, http.StatusOK, gin.H{
			"success":          false,
			"error":            "模型 " + testReq.Model + " 不在此渠道的支持列表中",
			"model":            testReq.Model,
			"supported_models": cfg.GetModels(),
		})
		return
	}

	// 执行测试（传递实际的API Key字符串）
	testResult := s.testChannelAPI(c.Request.Context(), cfg, selectedKey, &testReq)
	// 添加测试的 Key 索引信息到结果中
	testResult["tested_key_index"] = keyIndex
	testResult["total_keys"] = len(apiKeys)

	// [INFO] 修复：根据测试结果应用冷却逻辑
	if success, ok := testResult["success"].(bool); ok && success {
		// 测试成功：清除该Key的冷却状态
		if err := s.store.ResetKeyCooldown(c.Request.Context(), id, keyIndex); err != nil {
			log.Printf("[WARN] 清除Key #%d冷却状态失败: %v", keyIndex, err)
		}

		// ✨ 优化：同时清除渠道级冷却（因为至少有一个Key可用）
		// 设计理念：测试成功证明渠道恢复正常，应立即解除渠道级冷却，避免选择器过滤该渠道
		_ = s.store.ResetChannelCooldown(c.Request.Context(), id)

		// [INFO] 修复：统一使相关缓存失效，确保前端能立即看到状态更新
		s.invalidateChannelRelatedCache(id)
	} else {
		// 🔥 修复：测试失败时应用冷却策略
		// 提取状态码和错误体
		statusCode, _ := testResult["status_code"].(int)
		var errorBody []byte
		if apiError, ok := testResult["api_error"].(map[string]any); ok {
			errorBody, _ = sonic.Marshal(apiError)
		} else if rawResp, ok := testResult["raw_response"].(string); ok {
			errorBody = []byte(rawResp)
		}

		// 提取响应头（用于429错误的精确分类）
		var headers map[string][]string
		if respHeaders, ok := testResult["response_headers"].(map[string]string); ok && statusCode == 429 {
			headers = make(map[string][]string, len(respHeaders))
			for k, v := range respHeaders {
				headers[k] = []string{v}
			}
		}

		// 调用统一冷却管理器处理错误
		action := s.cooldownManager.HandleError(
			c.Request.Context(),
			httpErrorInputFromParts(id, keyIndex, statusCode, errorBody, headers),
		)

		// [INFO] 修复：统一使相关缓存失效，确保前端能立即看到冷却状态更新
		// 无论是Key级冷却还是渠道级冷却，都需要使缓存失效
		s.invalidateChannelRelatedCache(id)

		// 记录冷却决策结果到测试响应中
		var actionStr string
		switch action {
		case cooldown.ActionRetryKey:
			actionStr = "key_cooldown_applied"
		case cooldown.ActionRetryChannel:
			actionStr = "channel_cooldown_applied"
		case cooldown.ActionReturnClient:
			actionStr = "client_error_no_cooldown"
		default:
			actionStr = "unknown_action"
		}
		testResult["cooldown_action"] = actionStr
	}

	RespondJSON(c, http.StatusOK, testResult)
}

// 测试渠道API连通性
func (s *Server) testChannelAPI(reqCtx context.Context, cfg *model.Config, apiKey string, testReq *testutil.TestChannelRequest) map[string]any {
	// 设置默认测试内容（从配置读取）
	if strings.TrimSpace(testReq.Content) == "" {
		testReq.Content = s.configService.GetString("channel_test_content", "sonnet 4.0的发布日期是什么")
	}

	// [INFO] 修复：应用模型重定向逻辑（与正常代理流程保持一致）
	originalModel := testReq.Model
	actualModel := originalModel

	// 检查模型重定向
	if redirectModel, ok := cfg.GetRedirectModel(originalModel); ok && redirectModel != "" {
		actualModel = redirectModel
		log.Printf("[RELOAD] [测试-模型重定向] 渠道ID=%d, 原始模型=%s, 重定向模型=%s", cfg.ID, originalModel, actualModel)
	}

	// 如果模型发生重定向，更新测试请求中的模型名称
	if actualModel != originalModel {
		testReq.Model = actualModel
		log.Printf("[INFO] [测试-请求体修改] 渠道ID=%d, 修改后模型=%s", cfg.ID, actualModel)
	}

	// 选择并规范化渠道类型
	channelType := util.NormalizeChannelType(testReq.ChannelType)
	var tester testutil.ChannelTester
	switch channelType {
	case "codex":
		tester = &testutil.CodexTester{}
	case "openai":
		tester = &testutil.OpenAITester{}
	case "gemini":
		tester = &testutil.GeminiTester{}
	case "anthropic":
		tester = &testutil.AnthropicTester{}
	default:
		tester = &testutil.AnthropicTester{}
	}

	// 构建请求（传递实际的API Key和重定向后的模型）
	fullURL, baseHeaders, body, err := tester.Build(cfg, apiKey, testReq)
	if err != nil {
		return map[string]any{"success": false, "error": "构造测试请求失败: " + err.Error()}
	}

	// 创建HTTP请求
	ctx, cancel := context.WithTimeout(reqCtx, 2*time.Minute)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", fullURL, bytes.NewReader(body))
	if err != nil {
		return map[string]any{"success": false, "error": "创建HTTP请求失败: " + err.Error()}
	}

	// 设置基础请求头
	for k, vs := range baseHeaders {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	// 添加/覆盖自定义请求头
	for key, value := range testReq.Headers {
		req.Header.Set(key, value)
	}

	// 发送请求
	start := time.Now()
	resp, err := s.client.Do(req)
	if err != nil {
		return map[string]any{
			"success":     false,
			"error":       "网络请求失败: " + err.Error(),
			"duration_ms": time.Since(start).Milliseconds(),
		}
	}
	defer func() { _ = resp.Body.Close() }()

	// 判断是否为SSE响应，以及是否请求了流式
	contentType := resp.Header.Get("Content-Type")
	isEventStream := strings.Contains(strings.ToLower(contentType), "text/event-stream")

	// 通用结果初始化
	result := map[string]any{
		"success":     resp.StatusCode >= 200 && resp.StatusCode < 300,
		"status_code": resp.StatusCode,
	}

	parseNonStreamResponse := func(bodyBytes []byte) map[string]any {
		// duration_ms 统一表示完整响应总耗时（含读取响应体）
		result["duration_ms"] = time.Since(start).Milliseconds()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			// 成功：委托给 tester 解析
			parsed := tester.Parse(resp.StatusCode, bodyBytes)
			for k, v := range parsed {
				result[k] = v
			}

			// 补齐成本信息（与代理计费口径一致：使用归一化后的可计费inputTokens）
			usageParser := newJSONUsageParser(channelType)
			_ = usageParser.Feed(bodyBytes)
			billableInput, output, cacheRead, _ := usageParser.GetUsage()
			if billableInput+output+cacheRead > 0 {
				result["cost_usd"] = util.CalculateCostDetailed(
					testReq.Model,
					billableInput,
					output,
					cacheRead,
					usageParser.Cache5mInputTokens,
					usageParser.Cache1hInputTokens,
				)
			}

			result["message"] = "API测试成功"
			return result
		}

		// 错误：统一解析
		var errorMsg string
		var apiError map[string]any
		if err := sonic.Unmarshal(bodyBytes, &apiError); err == nil {
			if errInfo, ok := apiError["error"].(map[string]any); ok {
				if msg, ok := errInfo["message"].(string); ok {
					errorMsg = msg
				} else if typeStr, ok := errInfo["type"].(string); ok {
					errorMsg = typeStr
				}
			}
			result["api_error"] = apiError
		} else {
			result["raw_response"] = string(bodyBytes)
		}
		if errorMsg == "" {
			errorMsg = "API返回错误状态: " + resp.Status
		}
		result["error"] = errorMsg
		return result
	}

	// 附带响应头与类型，便于排查（不含请求头以避免泄露）
	if len(resp.Header) > 0 {
		hdr := make(map[string]string, len(resp.Header))
		for k, vs := range resp.Header {
			if len(vs) == 1 {
				hdr[k] = vs[0]
			} else if len(vs) > 1 {
				hdr[k] = strings.Join(vs, "; ")
			}
		}
		result["response_headers"] = hdr
	}
	if contentType != "" {
		result["content_type"] = contentType
	}

	if isEventStream {
		// 流式解析（SSE）。无论状态码是否2xx，都尽量读取并回显上游返回内容。
		var rawBuilder strings.Builder
		var textBuilder strings.Builder
		var lastErrMsg string
		var lastUsage map[string]any
		dataLineCount := 0
		firstByteCaptured := false

		// [DRY] 复用代理链路的SSE usage解析器，保证tokens/成本口径一致
		usageParser := newSSEUsageParser(channelType)

		scanner := bufio.NewScanner(resp.Body)
		// 提高扫描缓冲，避免长行截断
		buf := make([]byte, 0, 1024*1024)
		scanner.Buffer(buf, 16*1024*1024)

		for scanner.Scan() {
			// first_byte_duration_ms 表示从请求发起到读取到首个响应字节的时间
			if !firstByteCaptured {
				firstByteCaptured = true
				result["first_byte_duration_ms"] = time.Since(start).Milliseconds()
			}

			line := scanner.Text()
			// 给usage解析器喂原始行（补回换行符），它依赖空行判断事件结束
			if err := usageParser.Feed([]byte(line + "\n")); err != nil {
				log.Printf("[WARN] SSE usage解析失败: %v", err)
			}

			rawBuilder.WriteString(line)
			rawBuilder.WriteString("\n")

			// SSE 行通常以 "data:" 开头
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			dataLineCount++
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" || data == "[DONE]" {
				continue
			}

			var obj map[string]any
			if err := sonic.Unmarshal([]byte(data), &obj); err != nil {
				// 非JSON数据，忽略
				continue
			}

			// 记录最后一个usage（一般出现在message_start/message_delta/response.completed等事件）
			if usage := extractUsage(obj); usage != nil {
				lastUsage = usage
			}

			// OpenAI: choices[0].delta.content
			if choices, ok := obj["choices"].([]any); ok && len(choices) > 0 {
				if choice, ok := choices[0].(map[string]any); ok {
					if delta, ok := choice["delta"].(map[string]any); ok {
						if content, ok := delta["content"].(string); ok && content != "" {
							textBuilder.WriteString(content)
							continue
						}
					}
				}
			}

			// Gemini: candidates[0].content.parts[0].text
			if candidates, ok := obj["candidates"].([]any); ok && len(candidates) > 0 {
				if candidate, ok := candidates[0].(map[string]any); ok {
					if content, ok := candidate["content"].(map[string]any); ok {
						if parts, ok := content["parts"].([]any); ok && len(parts) > 0 {
							if part, ok := parts[0].(map[string]any); ok {
								if text, ok := part["text"].(string); ok && text != "" {
									textBuilder.WriteString(text)
									continue
								}
							}
						}
					}
				}
			}

			// Anthropic: type == content_block_delta 且 delta.text 为增量
			if typ, ok := obj["type"].(string); ok {
				if typ == "content_block_delta" {
					if delta, ok := obj["delta"].(map[string]any); ok {
						if tx, ok := delta["text"].(string); ok && tx != "" {
							textBuilder.WriteString(tx)
							continue
						}
					}
				}
				// Codex: type == response.output_text.delta 且 delta 直接是文本
				if typ == "response.output_text.delta" {
					if delta, ok := obj["delta"].(string); ok && delta != "" {
						textBuilder.WriteString(delta)
						continue
					}
				}
			}

			// 错误事件通用: data 中包含 error 字段或 message
			if errObj, ok := obj["error"].(map[string]any); ok {
				if msg, ok := errObj["message"].(string); ok && msg != "" {
					lastErrMsg = msg
				} else if typeStr, ok := errObj["type"].(string); ok && typeStr != "" {
					lastErrMsg = typeStr
				}
				// 记录完整错误对象
				result["api_error"] = obj
				continue
			}
			if msg, ok := obj["message"].(string); ok && msg != "" {
				lastErrMsg = msg
				result["api_error"] = obj
				continue
			}
		}

		if err := scanner.Err(); err != nil {
			result["duration_ms"] = time.Since(start).Milliseconds()
			result["error"] = "读取流式响应失败: " + err.Error()
			result["raw_response"] = rawBuilder.String()
			return result
		}
		// 容错：部分上游错误地返回 text/event-stream 但实际是完整 JSON。
		// 若未发现任何 SSE data 行，按非流式响应解析，避免“测试成功但无 response_text”。
		if dataLineCount == 0 {
			return parseNonStreamResponse([]byte(rawBuilder.String()))
		}

		result["duration_ms"] = time.Since(start).Milliseconds()

		if textBuilder.Len() > 0 {
			result["response_text"] = textBuilder.String()
		}
		result["raw_response"] = rawBuilder.String()

		// 补齐tokens与成本信息（用于前端表格展示）
		billableInput, output, cacheRead, _ := usageParser.GetUsage()
		if lastUsage != nil {
			result["api_response"] = map[string]any{"usage": lastUsage}
		} else if billableInput+output+cacheRead > 0 {
			result["api_response"] = map[string]any{
				"usage": map[string]any{
					"input_tokens":                billableInput,
					"output_tokens":               output,
					"cache_read_input_tokens":     cacheRead,
					"cache_creation_input_tokens": 0,
				},
			}
		}

		if billableInput+output+cacheRead > 0 {
			costUSD := util.CalculateCostDetailed(
				testReq.Model,
				billableInput,
				output,
				cacheRead,
				usageParser.Cache5mInputTokens,
				usageParser.Cache1hInputTokens,
			)
			result["cost_usd"] = costUSD
		}

		if lastErrMsg != "" {
			// 软错误检测：HTTP 200但SSE流中包含错误事件（如余额不足、配额耗尽等）
			result["success"] = false
			result["error"] = lastErrMsg
		} else if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			result["message"] = "API测试成功（流式）"
		} else {
			if lastErrMsg == "" {
				lastErrMsg = "API返回错误状态: " + resp.Status
			}
			result["error"] = lastErrMsg
		}
		return result
	}

	// 非流式或非SSE响应：按原逻辑读取完整响应（即便前端请求了流式，但上游未返回SSE，也按普通响应处理，确保能展示完整错误体）
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return map[string]any{
			"success":     false,
			"error":       "读取响应失败: " + err.Error(),
			"duration_ms": time.Since(start).Milliseconds(),
			"status_code": resp.StatusCode,
		}
	}
	return parseNonStreamResponse(respBody)
}
