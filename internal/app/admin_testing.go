package app

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"html"
	"io"
	"log"
	"mime"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strings"
	"time"

	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/testutil"
	"ccLoad/internal/util"

	"github.com/bytedance/sonic"
	"github.com/gin-gonic/gin"
)

// ==================== 渠道测试功能 ====================
// 从admin.go拆分渠道测试,遵循SRP原则

// HandleChannelTest 测试指定渠道的连通性
func (s *Server) HandleChannelTest(c *gin.Context) {
	s.handleChannelTestRequest(c, false)
}

// HandleChannelURLTest 测试指定渠道的单个 URL。
func (s *Server) HandleChannelURLTest(c *gin.Context) {
	s.handleChannelTestRequest(c, true)
}

type channelTestRequestPlan struct {
	clientProtocol   string
	upstreamProtocol string
	clientTester     testutil.ChannelTester
	fullURL          string
	headers          http.Header
	requestBody      []byte
	clientBody       []byte
}

func newChannelTester(protocolName string) testutil.ChannelTester {
	switch util.NormalizeChannelType(protocolName) {
	case "codex":
		return &testutil.CodexTester{}
	case "openai":
		return &testutil.OpenAITester{}
	case "gemini":
		return &testutil.GeminiTester{}
	case "anthropic":
		return &testutil.AnthropicTester{}
	default:
		return &testutil.AnthropicTester{}
	}
}

func resolveClientProtocol(cfg *model.Config, testReq *testutil.TestChannelRequest) string {
	if protocolName := strings.TrimSpace(testReq.ProtocolTransform); protocolName != "" {
		return strings.ToLower(protocolName)
	}
	if protocolName := strings.TrimSpace(testReq.ChannelType); protocolName != "" {
		return strings.ToLower(protocolName)
	}
	return cfg.GetChannelType()
}

func extractRequestPath(fullURL string) string {
	parsed, err := neturl.Parse(fullURL)
	if err != nil {
		return ""
	}
	path := parsed.EscapedPath()
	if path == "" {
		path = parsed.Path
	}
	if parsed.RawQuery != "" {
		return path + "?" + parsed.RawQuery
	}
	return path
}

func cloneHeaders(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

func (s *Server) buildChannelTestRequestPlan(
	cfgForBuild *model.Config,
	apiKey string,
	testReq *testutil.TestChannelRequest,
	clientProtocol string,
) (*channelTestRequestPlan, error) {
	upstreamProtocol := cfgForBuild.GetChannelType()
	clientTester := newChannelTester(clientProtocol)

	fullURL, headers, body, err := clientTester.Build(cfgForBuild, apiKey, testReq)
	if err != nil {
		return nil, err
	}

	plan := &channelTestRequestPlan{
		clientProtocol:   clientProtocol,
		upstreamProtocol: upstreamProtocol,
		clientTester:     clientTester,
		fullURL:          fullURL,
		headers:          headers,
		requestBody:      body,
		clientBody:       body,
	}

	if clientProtocol == upstreamProtocol {
		return plan, nil
	}
	if s == nil || s.protocolRegistry == nil {
		return nil, fmt.Errorf("protocol registry unavailable for transform %s -> %s", clientProtocol, upstreamProtocol)
	}

	upstreamTester := newChannelTester(upstreamProtocol)
	upstreamURL, upstreamHeaders, _, err := upstreamTester.Build(cfgForBuild, apiKey, testReq)
	if err != nil {
		return nil, err
	}

	transformPlan, err := protocol.BuildTransformPlan(
		protocol.Protocol(clientProtocol),
		protocol.Protocol(upstreamProtocol),
		extractRequestPath(fullURL),
		extractRequestPath(upstreamURL),
		body,
		body,
		testReq.Model,
		testReq.Model,
		testReq.Stream,
	)
	if err != nil {
		return nil, err
	}

	translatedBody, err := s.protocolRegistry.TranslateRequest(
		transformPlan.ClientProtocol,
		transformPlan.UpstreamProtocol,
		transformPlan.RequestModel(),
		transformPlan.TranslatedBody,
		transformPlan.Streaming,
	)
	if err != nil {
		return nil, err
	}

	plan.fullURL = upstreamURL
	plan.headers = cloneHeaders(upstreamHeaders)
	plan.requestBody = translatedBody
	return plan, nil
}

func parseTestStreamResponseBytes(
	raw []byte,
	parseProtocol string,
	statusCode int,
	result map[string]any,
	testReq *testutil.TestChannelRequest,
) map[string]any {
	var rawBuilder strings.Builder
	var textBuilder strings.Builder
	var lastErrMsg string
	var lastUsage map[string]any
	dataLineCount := 0

	usageParser := newSSEUsageParser(parseProtocol)
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	buf := make([]byte, 0, 1024*1024)
	scanner.Buffer(buf, 16*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if err := usageParser.Feed([]byte(line + "\n")); err != nil {
			log.Printf("[WARN] SSE usage解析失败: %v", err)
		}

		rawBuilder.WriteString(line)
		rawBuilder.WriteString("\n")

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
			continue
		}

		if usage := extractUsage(obj); usage != nil {
			lastUsage = usage
		}
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
		if typ, ok := obj["type"].(string); ok {
			if typ == "content_block_delta" {
				if delta, ok := obj["delta"].(map[string]any); ok {
					if tx, ok := delta["text"].(string); ok && tx != "" {
						textBuilder.WriteString(tx)
						continue
					}
				}
			}
			if typ == "response.output_text.delta" {
				if delta, ok := obj["delta"].(string); ok && delta != "" {
					textBuilder.WriteString(delta)
					continue
				}
			}
		}
		if errObj, ok := obj["error"].(map[string]any); ok {
			if msg, ok := errObj["message"].(string); ok && msg != "" {
				lastErrMsg = msg
			} else if typeStr, ok := errObj["type"].(string); ok && typeStr != "" {
				lastErrMsg = typeStr
			}
			result["api_error"] = obj
			continue
		}
		if errMsg, ok := obj["error"].(string); ok && strings.TrimSpace(errMsg) != "" {
			lastErrMsg = strings.TrimSpace(errMsg)
			result["api_error"] = obj
			continue
		}
		if msg, ok := obj["message"].(string); ok && msg != "" {
			lastErrMsg = msg
			result["api_error"] = obj
			continue
		}
	}

	result["raw_response"] = rawBuilder.String()
	if scanner.Err() != nil {
		result["error"] = "读取流式响应失败: " + scanner.Err().Error()
		return result
	}
	if dataLineCount == 0 {
		result["error"] = summarizeUnexpectedTestResponse("text/event-stream", raw)
		return result
	}
	if textBuilder.Len() > 0 {
		result["response_text"] = textBuilder.String()
	}

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
		result["cost_usd"] = util.CalculateCostDetailed(
			testReq.Model,
			billableInput,
			output,
			cacheRead,
			usageParser.Cache5mInputTokens,
			usageParser.Cache1hInputTokens,
		)
	}

	if lastErrMsg != "" {
		result["success"] = false
		result["error"] = lastErrMsg
	} else if statusCode >= 200 && statusCode < 300 {
		result["message"] = "API测试成功（流式）"
	} else {
		result["error"] = "API返回错误状态: " + http.StatusText(statusCode)
	}

	return result
}

func (s *Server) handleChannelTestRequest(c *gin.Context, requireBaseURL bool) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel id")
		return
	}

	var testReq testutil.TestChannelRequest
	if err := BindAndValidate(c, &testReq); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}

	forcedBaseURL := strings.TrimSpace(testReq.BaseURL)
	if requireBaseURL {
		if forcedBaseURL == "" {
			RespondErrorMsg(c, http.StatusBadRequest, "base_url is required for /admin/channels/:id/test-url")
			return
		}
	} else if forcedBaseURL != "" {
		RespondErrorMsg(c, http.StatusBadRequest, "base_url is not supported on /admin/channels/:id/test; use /admin/channels/:id/test-url")
		return
	}

	cfg, err := s.store.GetConfig(c.Request.Context(), id)
	if err != nil {
		RespondError(c, http.StatusNotFound, fmt.Errorf("channel not found"))
		return
	}
	if forcedBaseURL != "" {
		normalizedBaseURL, err := validateChannelBaseURL(forcedBaseURL)
		if err != nil {
			RespondErrorMsg(c, http.StatusBadRequest, "invalid base_url: "+err.Error())
			return
		}
		testReq.BaseURL = normalizedBaseURL
	}

	apiKeys, err := s.store.GetAPIKeys(c.Request.Context(), id)
	if err != nil || len(apiKeys) == 0 {
		RespondJSON(c, http.StatusOK, gin.H{
			"success": false,
			"error":   "渠道未配置有效的 API Key",
		})
		return
	}

	keyIndex := testReq.KeyIndex
	if keyIndex < 0 || keyIndex >= len(apiKeys) {
		keyIndex = 0
	}

	selectedKey := apiKeys[keyIndex].APIKey

	if !cfg.SupportsModel(testReq.Model) {
		RespondJSON(c, http.StatusOK, gin.H{
			"success":          false,
			"error":            "模型 " + testReq.Model + " 不在此渠道的支持列表中",
			"model":            testReq.Model,
			"supported_models": cfg.GetModels(),
		})
		return
	}

	requestedModel := testReq.Model
	testResult := s.executeChannelTest(c.Request.Context(), cfg, keyIndex, selectedKey, &testReq)
	s.persistDetectionLog(c.Request.Context(), detectionLogFromResult(cfg, model.LogSourceManualTest, requestedModel, testReq.Model, selectedKey, c.ClientIP(), 0, testResult))
	testResult["tested_key_index"] = keyIndex
	testResult["total_keys"] = len(apiKeys)

	RespondJSON(c, http.StatusOK, testResult)
}

func (s *Server) executeChannelTest(ctx context.Context, cfg *model.Config, keyIndex int, apiKey string, testReq *testutil.TestChannelRequest) map[string]any {
	result := s.testChannelAPI(ctx, cfg, apiKey, testReq)
	if success, ok := result["success"].(bool); ok && success {
		if err := s.store.ResetKeyCooldown(ctx, cfg.ID, keyIndex); err != nil {
			log.Printf("[WARN] 清除Key #%d冷却状态失败: %v", keyIndex, err)
		}
		if err := s.store.ResetChannelCooldown(ctx, cfg.ID); err != nil {
			log.Printf("[WARN] 清除渠道冷却状态失败: %v", err)
		}
		s.invalidateChannelRelatedCache(cfg.ID)
		return result
	}

	statusCode, errorBody, headers := buildTestFailureClassificationInput(result)
	action := s.cooldownManager.HandleError(
		ctx,
		httpErrorInputFromParts(cfg.ID, keyIndex, statusCode, errorBody, headers),
	)
	s.invalidateChannelRelatedCache(cfg.ID)

	switch action {
	case cooldown.ActionRetryKey:
		result["cooldown_action"] = "key_cooldown_applied"
	case cooldown.ActionRetryChannel:
		result["cooldown_action"] = "channel_cooldown_applied"
	case cooldown.ActionReturnClient:
		result["cooldown_action"] = "client_error_no_cooldown"
	default:
		result["cooldown_action"] = "unknown_action"
	}

	return result
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

	clientProtocol := resolveClientProtocol(cfg, testReq)
	if !cfg.SupportsProtocol(clientProtocol) {
		return map[string]any{
			"success": false,
			"error":   fmt.Sprintf("渠道不支持协议转换 %s", clientProtocol),
		}
	}

	urls := cfg.GetURLs()
	if forcedBaseURL := strings.TrimSpace(testReq.BaseURL); forcedBaseURL != "" {
		urls = []string{forcedBaseURL}
	}
	if len(urls) == 0 {
		return map[string]any{"success": false, "error": "渠道URL为空"}
	}

	var selector *URLSelector
	if len(urls) > 1 && s != nil && s.urlSelector != nil {
		selector = s.urlSelector
	}
	orderedURLs := orderURLsWithSelector(selector, cfg.ID, urls)

	var lastResult map[string]any
	for idx, entry := range orderedURLs {
		attemptResult := s.testChannelAPIWithURL(reqCtx, cfg, apiKey, testReq, clientProtocol, entry.url)
		attemptResult["base_url"] = entry.url
		success, _ := attemptResult["success"].(bool)
		if success {
			if selector != nil {
				latency := pickURLSelectorLatency(attemptResult)
				selector.RecordLatency(cfg.ID, entry.url, latency)
			}
			return attemptResult
		}

		lastResult = attemptResult
		if idx == len(orderedURLs)-1 {
			break
		}

		continueFallback, shouldCooldown := shouldFallbackToNextURL(attemptResult)
		if shouldCooldown && selector != nil {
			selector.CooldownURL(cfg.ID, entry.url)
		}
		if !continueFallback {
			return attemptResult
		}
	}

	if lastResult != nil {
		return lastResult
	}
	return map[string]any{"success": false, "error": "渠道测试失败: 未找到可用URL"}
}

func (s *Server) testChannelAPIWithURL(
	reqCtx context.Context,
	cfg *model.Config,
	apiKey string,
	testReq *testutil.TestChannelRequest,
	clientProtocol, selectedURL string,
) map[string]any {
	// 仅构造测试请求必需字段，避免复制带锁 Config 结构体。
	cfgForBuild := &model.Config{
		ID:           cfg.ID,
		Name:         cfg.Name,
		ChannelType:  cfg.ChannelType,
		URL:          selectedURL,
		ModelEntries: append([]model.ModelEntry(nil), cfg.ModelEntries...),
	}

	requestPlan, err := s.buildChannelTestRequestPlan(cfgForBuild, apiKey, testReq, clientProtocol)
	if err != nil {
		return map[string]any{"success": false, "error": "构造测试请求失败: " + err.Error()}
	}

	// 创建HTTP请求
	ctx, cancel := context.WithTimeout(reqCtx, 2*time.Minute)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", requestPlan.fullURL, bytes.NewReader(requestPlan.requestBody))
	if err != nil {
		return map[string]any{"success": false, "error": "创建HTTP请求失败: " + err.Error()}
	}

	// 设置基础请求头
	for k, vs := range requestPlan.headers {
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
			parseBody := bodyBytes
			if requestPlan.clientProtocol != requestPlan.upstreamProtocol {
				translatedBody, translateErr := s.protocolRegistry.TranslateResponseNonStream(
					ctx,
					protocol.Protocol(requestPlan.upstreamProtocol),
					protocol.Protocol(requestPlan.clientProtocol),
					testReq.Model,
					requestPlan.clientBody,
					requestPlan.requestBody,
					bodyBytes,
				)
				if translateErr != nil {
					result["success"] = false
					result["error"] = "转换测试响应失败: " + translateErr.Error()
					result["raw_response"] = string(bodyBytes)
					return result
				}
				parseBody = translatedBody
			}

			// 成功：委托给客户端协议 tester 解析
			parsed := requestPlan.clientTester.Parse(resp.StatusCode, parseBody)
			for k, v := range parsed {
				result[k] = v
			}

			if success, ok := result["success"].(bool); !ok || success {
				if _, ok := result["api_response"]; !ok {
					result["success"] = false
					result["error"] = summarizeUnexpectedTestResponse(contentType, bodyBytes)
					if _, hasRaw := result["raw_response"]; !hasRaw {
						result["raw_response"] = string(bodyBytes)
					}
					delete(result, "message")
					return result
				}
			}

			// 补齐成本信息（与代理计费口径一致：使用归一化后的可计费inputTokens）
			usageParser := newJSONUsageParser(requestPlan.upstreamProtocol)
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

			if success, ok := result["success"].(bool); !ok || success {
				result["message"] = "API测试成功"
			}
			return result
		}

		// 错误：统一解析
		var errorMsg string
		var apiError map[string]any
		if err := sonic.Unmarshal(bodyBytes, &apiError); err == nil {
			errorMsg = extractTestAPIErrorMessage(apiError)
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
		if requestPlan.clientProtocol != requestPlan.upstreamProtocol {
			recorder := httptest.NewRecorder()
			firstByteCaptured := false
			var state any

			streamErr := streamTransformSSEEvents(
				ctx,
				resp.Body,
				recorder,
				func(rawEvent []byte) error {
					if !firstByteCaptured && len(rawEvent) > 0 {
						firstByteCaptured = true
						result["first_byte_duration_ms"] = time.Since(start).Milliseconds()
					}
					return nil
				},
				func(rawEvent []byte) ([][]byte, error) {
					return s.protocolRegistry.TranslateResponseStream(
						ctx,
						protocol.Protocol(requestPlan.upstreamProtocol),
						protocol.Protocol(requestPlan.clientProtocol),
						testReq.Model,
						requestPlan.clientBody,
						requestPlan.requestBody,
						rawEvent,
						&state,
					)
				},
			)
			if streamErr != nil {
				result["duration_ms"] = time.Since(start).Milliseconds()
				result["error"] = "读取流式响应失败: " + streamErr.Error()
				return result
			}

			result["duration_ms"] = time.Since(start).Milliseconds()
			return parseTestStreamResponseBytes(recorder.Body.Bytes(), requestPlan.clientProtocol, resp.StatusCode, result, testReq)
		}

		// 流式解析（SSE）。无论状态码是否2xx，都尽量读取并回显上游返回内容。
		var rawBuilder strings.Builder
		var textBuilder strings.Builder
		var lastErrMsg string
		var lastUsage map[string]any
		dataLineCount := 0
		firstByteCaptured := false

		// [DRY] 复用代理链路的SSE usage解析器，保证tokens/成本口径一致
		usageParser := newSSEUsageParser(requestPlan.upstreamProtocol)

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
			if errMsg, ok := obj["error"].(string); ok && strings.TrimSpace(errMsg) != "" {
				lastErrMsg = strings.TrimSpace(errMsg)
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

func buildTestFailureClassificationInput(result map[string]any) (statusCode int, errorBody []byte, headers map[string][]string) {
	statusCode, _ = getResultInt(result["status_code"])

	hasStructuredAPIError := false
	if apiError, ok := result["api_error"].(map[string]any); ok {
		errorBody, _ = sonic.Marshal(apiError)
		hasStructuredAPIError = true
	} else if rawResp, ok := result["raw_response"].(string); ok {
		errorBody = []byte(rawResp)
	} else if errMsg, ok := result["error"].(string); ok {
		errorBody = []byte(errMsg)
	}

	switch h := result["response_headers"].(type) {
	case map[string]string:
		headers = make(map[string][]string, len(h))
		for k, v := range h {
			headers[k] = []string{v}
		}
	case map[string]any:
		headers = make(map[string][]string, len(h))
		for k, v := range h {
			if vs, ok := v.(string); ok {
				headers[k] = []string{vs}
			}
		}
	}

	// 上游测试会保留真实HTTP状态码，但冷却分类器需要内部软错误码才能正确识别
	// “HTTP 200 + 结构化 error 对象”本质上不是成功，只是上游把错误塞进了响应体。
	if statusCode >= 200 && statusCode < 300 && hasStructuredAPIError {
		if _, is1308 := util.ParseResetTimeFrom1308Error(errorBody); is1308 {
			statusCode = util.StatusQuotaExceeded
		} else {
			statusCode = util.StatusSSEError
		}
	}

	return statusCode, errorBody, headers
}

func shouldFallbackToNextURL(result map[string]any) (continueFallback bool, shouldCooldown bool) {
	if _, hasStatus := getResultInt(result["status_code"]); !hasStatus {
		errMsg, _ := result["error"].(string)
		if strings.HasPrefix(errMsg, "网络请求失败:") || strings.HasPrefix(errMsg, "读取响应失败:") {
			return true, true
		}
		return false, false
	}

	statusCode, errorBody, headers := buildTestFailureClassificationInput(result)

	classification := util.ClassifyHTTPResponseWithMeta(statusCode, headers, errorBody)
	switch classification.Level {
	case util.ErrorLevelChannel:
		return true, true
	case util.ErrorLevelNone:
		// 软错误场景：2xx 但业务层已标记 success=false，继续换URL尝试。
		if statusCode >= 200 && statusCode < 300 {
			return true, true
		}
		return false, false
	default:
		return false, false
	}
}

func pickURLSelectorLatency(result map[string]any) time.Duration {
	if ms, ok := getResultInt64(result["first_byte_duration_ms"]); ok && ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	if ms, ok := getResultInt64(result["duration_ms"]); ok && ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return time.Millisecond
}

func getResultInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	default:
		return 0, false
	}
}

func extractTestAPIErrorMessage(apiError map[string]any) string {
	if apiError == nil {
		return ""
	}

	switch errValue := apiError["error"].(type) {
	case string:
		if msg := strings.TrimSpace(errValue); msg != "" {
			return msg
		}
	case map[string]any:
		if msg, ok := errValue["message"].(string); ok && strings.TrimSpace(msg) != "" {
			return strings.TrimSpace(msg)
		}
		if nested, ok := errValue["error"].(string); ok && strings.TrimSpace(nested) != "" {
			return strings.TrimSpace(nested)
		}
		if typeStr, ok := errValue["type"].(string); ok && strings.TrimSpace(typeStr) != "" {
			return strings.TrimSpace(typeStr)
		}
	}

	if msg, ok := apiError["message"].(string); ok && strings.TrimSpace(msg) != "" {
		return strings.TrimSpace(msg)
	}

	return ""
}

func summarizeUnexpectedTestResponse(contentType string, bodyBytes []byte) string {
	body := strings.TrimSpace(string(bodyBytes))
	if body == "" {
		if ct := strings.TrimSpace(contentType); ct != "" {
			return "上游返回空响应体: " + ct
		}
		return "上游返回空响应体"
	}

	if looksLikeHTMLResponse(contentType, body) {
		if heading := extractHTMLTagText(body, "h1"); heading != "" {
			return heading
		}
		if title := extractHTMLTagText(body, "title"); title != "" {
			return title
		}
	}

	if snippet := normalizeUnexpectedResponseText(stripHTMLTags(body)); snippet != "" {
		return snippet
	}
	if ct := strings.TrimSpace(contentType); ct != "" {
		return "上游返回了非预期响应: " + ct
	}
	return "上游返回了非预期响应"
}

func looksLikeHTMLResponse(contentType, body string) bool {
	if ct := strings.TrimSpace(contentType); ct != "" {
		if mediaType, _, err := mime.ParseMediaType(ct); err == nil {
			switch strings.ToLower(mediaType) {
			case "text/html", "application/xhtml+xml":
				return true
			}
		}
	}

	bodyLower := strings.ToLower(body)
	return strings.Contains(bodyLower, "<!doctype html") ||
		strings.Contains(bodyLower, "<html") ||
		strings.Contains(bodyLower, "<body") ||
		strings.Contains(bodyLower, "<title")
}

func extractHTMLTagText(body, tag string) string {
	tagLower := strings.ToLower(tag)
	bodyLower := strings.ToLower(body)
	openIdx := strings.Index(bodyLower, "<"+tagLower)
	if openIdx < 0 {
		return ""
	}

	contentStart := strings.Index(bodyLower[openIdx:], ">")
	if contentStart < 0 {
		return ""
	}
	contentStart += openIdx + 1

	closeIdx := strings.Index(bodyLower[contentStart:], "</"+tagLower+">")
	if closeIdx < 0 {
		return ""
	}

	return normalizeUnexpectedResponseText(stripHTMLTags(body[contentStart : contentStart+closeIdx]))
}

func stripHTMLTags(body string) string {
	var builder strings.Builder
	builder.Grow(len(body))

	inTag := false
	for _, r := range body {
		switch r {
		case '<':
			inTag = true
		case '>':
			if inTag {
				inTag = false
				builder.WriteByte(' ')
			}
		default:
			if !inTag {
				builder.WriteRune(r)
			}
		}
	}

	return html.UnescapeString(builder.String())
}

func normalizeUnexpectedResponseText(text string) string {
	text = strings.TrimSpace(strings.Join(strings.Fields(text), " "))
	if text == "" {
		return ""
	}

	const maxRunes = 200
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	return string(runes[:maxRunes]) + "..."
}

func getResultInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		return int64(n), true
	default:
		return 0, false
	}
}
