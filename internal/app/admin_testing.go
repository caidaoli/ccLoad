package app

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/testutil"
	"ccLoad/internal/util"

	"github.com/bytedance/sonic"
	"github.com/gin-gonic/gin"
)

// ==================== 渠道测试功能 ====================
// ✅ P1重构 (2025-10-28): 从admin.go拆分渠道测试,遵循SRP原则

func (s *Server) handleChannelTest(c *gin.Context) {
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
	modelSupported := false
	for _, model := range cfg.Models {
		if model == testReq.Model {
			modelSupported = true
			break
		}
	}
	if !modelSupported {
		RespondJSON(c, http.StatusOK, gin.H{
			"success":          false,
			"error":            "模型 " + testReq.Model + " 不在此渠道的支持列表中",
			"model":            testReq.Model,
			"supported_models": cfg.Models,
		})
		return
	}

	// 执行测试（传递实际的API Key字符串）
	testResult := s.testChannelAPI(cfg, selectedKey, &testReq)
	// 添加测试的 Key 索引信息到结果中
	testResult["tested_key_index"] = keyIndex
	testResult["total_keys"] = len(apiKeys)

	// ✅ 修复：测试成功时清除该Key的冷却状态
	if success, ok := testResult["success"].(bool); ok && success {
		if err := s.store.ResetKeyCooldown(c.Request.Context(), id, keyIndex); err != nil {
			util.SafePrintf("⚠️  警告: 清除Key #%d冷却状态失败: %v", keyIndex, err)
		}

		// ✨ 优化：同时清除渠道级冷却（因为至少有一个Key可用）
		// 设计理念：测试成功证明渠道恢复正常，应立即解除渠道级冷却，避免选择器过滤该渠道
		_ = s.store.ResetChannelCooldown(c.Request.Context(), id)

		// 精确计数（P1）：记录状态恢复
	}

	RespondJSON(c, http.StatusOK, testResult)
}

// 测试渠道API连通性
func (s *Server) testChannelAPI(cfg *model.Config, apiKey string, testReq *testutil.TestChannelRequest) map[string]any {
	// ✅ 修复：应用模型重定向逻辑（与正常代理流程保持一致）
	originalModel := testReq.Model
	actualModel := originalModel

	// 检查模型重定向
	if len(cfg.ModelRedirects) > 0 {
		if redirectModel, ok := cfg.ModelRedirects[originalModel]; ok && redirectModel != "" {
			actualModel = redirectModel
			util.SafePrintf("🔄 [测试-模型重定向] 渠道ID=%d, 原始模型=%s, 重定向模型=%s", cfg.ID, originalModel, actualModel)
		}
	}

	// 如果模型发生重定向，更新测试请求中的模型名称
	if actualModel != originalModel {
		testReq.Model = actualModel
		util.SafePrintf("✅ [测试-请求体修改] 渠道ID=%d, 修改后模型=%s", cfg.ID, actualModel)
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
	duration := time.Since(start)
	if err != nil {
		return map[string]any{"success": false, "error": "网络请求失败: " + err.Error(), "duration_ms": duration.Milliseconds()}
	}
	defer resp.Body.Close()

	// 判断是否为SSE响应，以及是否请求了流式
	contentType := resp.Header.Get("Content-Type")
	isEventStream := strings.Contains(strings.ToLower(contentType), "text/event-stream")

	// 通用结果初始化
	result := map[string]any{
		"success":     resp.StatusCode >= 200 && resp.StatusCode < 300,
		"status_code": resp.StatusCode,
		"duration_ms": duration.Milliseconds(),
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

		scanner := bufio.NewScanner(resp.Body)
		// 提高扫描缓冲，避免长行截断
		buf := make([]byte, 0, 1024*1024)
		scanner.Buffer(buf, 16*1024*1024)

		for scanner.Scan() {
			line := scanner.Text()
			rawBuilder.WriteString(line)
			rawBuilder.WriteString("\n")

			// SSE 行通常以 "data:" 开头
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" || data == "[DONE]" {
				continue
			}

			var obj map[string]any
			if err := sonic.Unmarshal([]byte(data), &obj); err != nil {
				// 非JSON数据，忽略
				continue
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
			result["error"] = "读取流式响应失败: " + err.Error()
			result["raw_response"] = rawBuilder.String()
			return result
		}

		if textBuilder.Len() > 0 {
			result["response_text"] = textBuilder.String()
		}
		result["raw_response"] = rawBuilder.String()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
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
		return map[string]any{"success": false, "error": "读取响应失败: " + err.Error(), "duration_ms": duration.Milliseconds(), "status_code": resp.StatusCode}
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// 成功：委托给 tester 解析
		parsed := tester.Parse(resp.StatusCode, respBody)
		for k, v := range parsed {
			result[k] = v
		}
		result["message"] = "API测试成功"
	} else {
		// 错误：统一解析
		var errorMsg string
		var apiError map[string]any
		if err := sonic.Unmarshal(respBody, &apiError); err == nil {
			if errInfo, ok := apiError["error"].(map[string]any); ok {
				if msg, ok := errInfo["message"].(string); ok {
					errorMsg = msg
				} else if typeStr, ok := errInfo["type"].(string); ok {
					errorMsg = typeStr
				}
			}
			result["api_error"] = apiError
		} else {
			result["raw_response"] = string(respBody)
		}
		if errorMsg == "" {
			errorMsg = "API返回错误状态: " + resp.Status
		}
		result["error"] = errorMsg
	}

	return result
}
