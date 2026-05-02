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

// resolveTestUpstreamProtocol 测试链路专用：跳过 ProtocolTransforms 白名单，
// 仅按 ProtocolTransformMode 决定上游协议（upstream→client 直通；local→渠道原生触发翻译）。
// 让测试者无需先把协议加入 ProtocolTransforms 列表即可验证任意 client 协议下的渠道行为。
func resolveTestUpstreamProtocol(cfg *model.Config, clientProtocol string) string {
	clientProtocol = strings.ToLower(strings.TrimSpace(clientProtocol))
	if clientProtocol == "" || !util.IsValidChannelType(clientProtocol) {
		return cfg.GetChannelType()
	}
	if cfg.GetProtocolTransformMode() == model.ProtocolTransformModeUpstream {
		return clientProtocol
	}
	return cfg.GetChannelType()
}

func cloneHeaders(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

// flattenHeader 将 http.Header 扁平化为字符串 map（多值用 "; " 拼接，空值跳过）。
func flattenHeader(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vs := range h {
		switch len(vs) {
		case 0:
			continue
		case 1:
			out[k] = vs[0]
		default:
			out[k] = strings.Join(vs, "; ")
		}
	}
	return out
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

// patchUpstreamSystemPrompt 将协议转换后的请求体中的 system prompt
// 替换为上游协议模板定义的 system prompt，确保发送内容匹配上游 API 预期。
func patchUpstreamSystemPrompt(translatedBody, upstreamBody []byte, upstreamProtocol string) []byte {
	var key string
	switch upstreamProtocol {
	case "anthropic":
		key = "system"
	case "codex":
		key = "instructions"
	default:
		return translatedBody
	}

	var translated, upstream map[string]any
	if err := sonic.Unmarshal(translatedBody, &translated); err != nil {
		return translatedBody
	}
	if err := sonic.Unmarshal(upstreamBody, &upstream); err != nil {
		return translatedBody
	}

	if val, ok := upstream[key]; ok {
		translated[key] = val
	} else {
		delete(translated, key)
	}

	result, err := sonic.Marshal(translated)
	if err != nil {
		return translatedBody
	}
	return result
}

func supportsRuntimeTestProtocol(clientProtocol, upstreamProtocol string) bool {
	if clientProtocol == "" || upstreamProtocol == "" {
		return false
	}
	if !util.IsValidChannelType(clientProtocol) || !util.IsValidChannelType(upstreamProtocol) {
		return false
	}
	if clientProtocol == upstreamProtocol {
		return true
	}
	return protocol.SupportsTransform(protocol.Protocol(clientProtocol), protocol.Protocol(upstreamProtocol))
}

func (s *Server) buildChannelTestRequestPlan(
	cfgForBuild *model.Config,
	apiKey string,
	testReq *testutil.TestChannelRequest,
	clientProtocol string,
) (*channelTestRequestPlan, error) {
	upstreamProtocol := resolveTestUpstreamProtocol(cfgForBuild, clientProtocol)
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
	upstreamURL, upstreamHeaders, upstreamBody, err := upstreamTester.Build(cfgForBuild, apiKey, testReq)
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

	// system prompt 用上游协议模板的版本替换：
	// 协议转换验证的是消息/工具的格式变换，system prompt 需匹配上游 API 预期。
	translatedBody = patchUpstreamSystemPrompt(translatedBody, upstreamBody, upstreamProtocol)

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
	collector := newTestSSECollector()
	usageParser := newSSEUsageParser(parseProtocol)
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	buf := make([]byte, 0, 1024*1024)
	scanner.Buffer(buf, 16*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		collector.consumeLine(line, usageParser)
	}

	result["raw_response"] = collector.rawResponse()
	if scanner.Err() != nil {
		result["error"] = "读取流式响应失败: " + scanner.Err().Error()
		return result
	}
	if collector.dataLineCount == 0 {
		result["error"] = summarizeUnexpectedTestResponse("text/event-stream", raw)
		return result
	}
	collector.applyResult(result)
	populateTestSSEUsageAndCost(result, testReq, usageParser, collector.lastUsage)

	if collector.lastErrMsg != "" {
		result["success"] = false
		result["error"] = collector.lastErrMsg
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
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	requestAPIKey := strings.TrimSpace(testReq.APIKey)
	if len(apiKeys) == 0 && requestAPIKey == "" {
		RespondJSON(c, http.StatusOK, gin.H{
			"success": false,
			"error":   "渠道未配置有效的 API Key",
		})
		return
	}

	keySelection, err := s.selectChannelTestKey(cfg, apiKeys, testReq.KeyIndex, requestAPIKey)
	if err != nil {
		RespondJSON(c, http.StatusOK, gin.H{
			"success":    false,
			"error":      err.Error(),
			"total_keys": len(apiKeys),
		})
		return
	}

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
	testResult := s.executeChannelTestWithCooldown(c.Request.Context(), cfg, keySelection.keyIndex, keySelection.apiKey, &testReq, keySelection.updatePersistedCooldown)
	s.persistDetectionLog(c.Request.Context(), detectionLogFromResult(cfg, model.LogSourceManualTest, requestedModel, testReq.Model, keySelection.apiKey, c.ClientIP(), 0, testResult))
	testResult["tested_key_index"] = keySelection.keyIndex
	testResult["total_keys"] = len(apiKeys)

	RespondJSON(c, http.StatusOK, testResult)
}

type channelTestKeySelection struct {
	keyIndex                int
	apiKey                  string
	updatePersistedCooldown bool
}

func (s *Server) selectChannelTestKey(cfg *model.Config, apiKeys []*model.APIKey, requestedKeyIndex int, requestAPIKey string) (channelTestKeySelection, error) {
	if requestAPIKey != "" {
		matchedKey, ok := findAPIKeyByIndex(apiKeys, requestedKeyIndex)
		return channelTestKeySelection{
			keyIndex:                requestedKeyIndex,
			apiKey:                  requestAPIKey,
			updatePersistedCooldown: ok && matchedKey.APIKey == requestAPIKey,
		}, nil
	}

	now := time.Now()
	if requestedKey, ok := findAPIKeyByIndex(apiKeys, requestedKeyIndex); ok && !requestedKey.IsCoolingDown(now) {
		return channelTestKeySelection{
			keyIndex:                requestedKey.KeyIndex,
			apiKey:                  requestedKey.APIKey,
			updatePersistedCooldown: true,
		}, nil
	}

	keyIndex, apiKey, err := s.keySelector.SelectAvailableKey(cfg.ID, apiKeys, nil)
	if err != nil {
		return channelTestKeySelection{}, fmt.Errorf("无可用 API Key（全部处于冷却中）")
	}
	return channelTestKeySelection{
		keyIndex:                keyIndex,
		apiKey:                  apiKey,
		updatePersistedCooldown: true,
	}, nil
}

func findAPIKeyByIndex(apiKeys []*model.APIKey, keyIndex int) (*model.APIKey, bool) {
	for _, apiKey := range apiKeys {
		if apiKey != nil && apiKey.KeyIndex == keyIndex {
			return apiKey, true
		}
	}
	return nil, false
}

func (s *Server) executeChannelTest(ctx context.Context, cfg *model.Config, keyIndex int, apiKey string, testReq *testutil.TestChannelRequest) map[string]any {
	return s.executeChannelTestWithCooldown(ctx, cfg, keyIndex, apiKey, testReq, true)
}

func (s *Server) executeChannelTestWithCooldown(ctx context.Context, cfg *model.Config, keyIndex int, apiKey string, testReq *testutil.TestChannelRequest, updatePersistedCooldown bool) map[string]any {
	result := s.testChannelAPI(ctx, cfg, apiKey, testReq)
	if success, ok := result["success"].(bool); ok && success {
		if updatePersistedCooldown {
			if err := s.store.ResetKeyCooldown(ctx, cfg.ID, keyIndex); err != nil {
				log.Printf("[WARN] 清除Key #%d冷却状态失败: %v", keyIndex, err)
			}
			if err := s.store.ResetChannelCooldown(ctx, cfg.ID); err != nil {
				log.Printf("[WARN] 清除渠道冷却状态失败: %v", err)
			}
			s.invalidateChannelRelatedCache(cfg.ID)
		}
		return result
	}

	if !updatePersistedCooldown {
		result["cooldown_action"] = "request_key_no_cooldown"
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
	upstreamProto := resolveTestUpstreamProtocol(cfg, clientProtocol)
	if !supportsRuntimeTestProtocol(clientProtocol, upstreamProto) {
		return map[string]any{
			"success": false,
			"error":   fmt.Sprintf("不支持协议转换 %s -> %s", clientProtocol, upstreamProto),
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
	req, requestPlan, cancel, err := s.buildTestUpstreamRequest(reqCtx, cfg, apiKey, testReq, clientProtocol, selectedURL)
	if err != nil {
		return map[string]any{"success": false, "error": err.Error()}
	}
	defer cancel()
	ctx := req.Context()

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

	// 始终返回上游请求原始数据，便于调试排查（不依赖 debug_log_enabled）
	result["upstream_request_url"] = requestPlan.fullURL
	result["upstream_request_headers"] = maskSensitiveHeaderMap(flattenHeader(req.Header))
	result["upstream_request_body"] = string(requestPlan.requestBody)

	// 附带响应头与类型，便于排查（不含请求头以避免泄露）
	if len(resp.Header) > 0 {
		result["response_headers"] = flattenHeader(resp.Header)
	}
	if contentType != "" {
		result["content_type"] = contentType
	}

	if isEventStream {
		if requestPlan.clientProtocol != requestPlan.upstreamProtocol {
			return s.parseTestTranslatedSSEResponse(ctx, requestPlan, testReq, resp, start, result)
		}
		return s.parseTestNativeSSEResponse(ctx, requestPlan, testReq, resp, contentType, start, result)
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
	return s.parseTestNonStreamResponse(ctx, requestPlan, testReq, resp, contentType, start, respBody, result)
}

// parseTestNonStreamResponse 解析非流式响应（成功/失败两路），写入 result 并返回。
// 提取自 testChannelAPIWithURL 内嵌闭包，行为保持不变。
func (s *Server) parseTestNonStreamResponse(
	ctx context.Context,
	requestPlan *channelTestRequestPlan,
	testReq *testutil.TestChannelRequest,
	resp *http.Response,
	contentType string,
	start time.Time,
	bodyBytes []byte,
	result map[string]any,
) map[string]any {
	result["duration_ms"] = time.Since(start).Milliseconds()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		parseBody := bodyBytes
		if requestPlan.clientProtocol != requestPlan.upstreamProtocol && len(bodyBytes) > 0 {
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

		result["upstream_response_body"] = string(bodyBytes)

		if success, ok := result["success"].(bool); !ok || success {
			result["message"] = "API测试成功"
		}
		return result
	}

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
	result["upstream_response_body"] = string(bodyBytes)
	return result
}

// buildTestUpstreamRequest 构造测试用上游 HTTP 请求（含 plan 构造、anyrouter 注入、body/header 规则）。
// 返回的 cancel 必须由调用者 defer。
func (s *Server) buildTestUpstreamRequest(
	reqCtx context.Context,
	cfg *model.Config,
	apiKey string,
	testReq *testutil.TestChannelRequest,
	clientProtocol, selectedURL string,
) (*http.Request, *channelTestRequestPlan, context.CancelFunc, error) {
	cfgForBuild := &model.Config{
		ID:                    cfg.ID,
		Name:                  cfg.Name,
		ChannelType:           cfg.ChannelType,
		ProtocolTransformMode: cfg.ProtocolTransformMode,
		ProtocolTransforms:    cfg.ProtocolTransforms,
		URL:                   selectedURL,
		ModelEntries:          append([]model.ModelEntry(nil), cfg.ModelEntries...),
		CustomRequestRules:    cfg.CustomRequestRules,
	}

	requestPlan, err := s.buildChannelTestRequestPlan(cfgForBuild, apiKey, testReq, clientProtocol)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("构造测试请求失败: %w", err)
	}

	// anyrouter 渠道：为 /v1/messages 自动注入 adaptive thinking（与代理链路保持一致）
	if requestPlan.upstreamProtocol == "anthropic" {
		if parsed, perr := neturl.Parse(requestPlan.fullURL); perr == nil && strings.HasSuffix(parsed.Path, "/v1/messages") {
			requestPlan.requestBody = maybeInjectAnyrouterAdaptiveThinking(cfgForBuild, "/v1/messages", requestPlan.requestBody)
		}
	}

	// 渠道级自定义请求体规则（与代理链路一致，仅对 JSON body 生效）
	requestPlan.requestBody = applyBodyRules(requestPlan.headers.Get("Content-Type"), requestPlan.requestBody, cfgForBuild.BodyRules())

	ctx, cancel := context.WithTimeout(reqCtx, 2*time.Minute)
	req, err := http.NewRequestWithContext(ctx, "POST", requestPlan.fullURL, bytes.NewReader(requestPlan.requestBody))
	if err != nil {
		cancel()
		return nil, nil, nil, fmt.Errorf("创建HTTP请求失败: %w", err)
	}

	for k, vs := range requestPlan.headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	for key, value := range testReq.Headers {
		req.Header.Set(key, value)
	}
	applyHeaderRules(req.Header, cfgForBuild.HeaderRules())

	return req, requestPlan, cancel, nil
}

// parseTestTranslatedSSEResponse 处理需要跨协议翻译的 SSE 响应分支。
func (s *Server) parseTestTranslatedSSEResponse(
	ctx context.Context,
	requestPlan *channelTestRequestPlan,
	testReq *testutil.TestChannelRequest,
	resp *http.Response,
	start time.Time,
	result map[string]any,
) map[string]any {
	recorder := httptest.NewRecorder()
	var rawUpstreamBuf bytes.Buffer
	upstreamTee := io.TeeReader(resp.Body, &rawUpstreamBuf)
	firstByteCaptured := false
	var state any

	streamErr := streamTransformSSEEvents(
		ctx,
		upstreamTee,
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
		result["upstream_response_body"] = rawUpstreamBuf.String()
		return result
	}

	result["duration_ms"] = time.Since(start).Milliseconds()
	result["upstream_response_body"] = rawUpstreamBuf.String()
	return parseTestStreamResponseBytes(recorder.Body.Bytes(), requestPlan.clientProtocol, resp.StatusCode, result, testReq)
}

// extractSSEDeltaText 从 SSE 单事件 JSON 对象提取增量文本（覆盖 OpenAI/Gemini/Anthropic/Codex）。
// 返回空字符串表示该事件无文本增量。
func extractSSEDeltaText(obj map[string]any) string {
	// OpenAI: choices[0].delta.content
	if choices, ok := obj["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if delta, ok := choice["delta"].(map[string]any); ok {
				if content, ok := delta["content"].(string); ok && content != "" {
					return content
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
							return text
						}
					}
				}
			}
		}
	}
	// Anthropic / Codex by event type
	typ, _ := obj["type"].(string)
	switch typ {
	case "content_block_delta":
		if delta, ok := obj["delta"].(map[string]any); ok {
			if tx, ok := delta["text"].(string); ok && tx != "" {
				return tx
			}
		}
	case "response.output_text.delta":
		if delta, ok := obj["delta"].(string); ok && delta != "" {
			return delta
		}
	}
	return ""
}

// extractSSEErrorMessage 从事件对象识别错误。
// matched=true 表示当前事件携带错误对象，msg 为人类可读消息（可能为空），raw 用于 api_error 字段。
func extractSSEErrorMessage(obj map[string]any) (msg string, raw map[string]any, matched bool) {
	if errObj, ok := obj["error"].(map[string]any); ok {
		if m, ok := errObj["message"].(string); ok && m != "" {
			return m, obj, true
		}
		if t, ok := errObj["type"].(string); ok && t != "" {
			return t, obj, true
		}
		return "", obj, true
	}
	if errStr, ok := obj["error"].(string); ok {
		if trimmed := strings.TrimSpace(errStr); trimmed != "" {
			return trimmed, obj, true
		}
	}
	if m, ok := obj["message"].(string); ok && m != "" {
		return m, obj, true
	}
	return "", nil, false
}

type testSSECollector struct {
	rawBuilder    strings.Builder
	textBuilder   strings.Builder
	lastErrMsg    string
	lastUsage     map[string]any
	lastAPIError  map[string]any
	dataLineCount int
}

func newTestSSECollector() *testSSECollector {
	return &testSSECollector{}
}

func (c *testSSECollector) consumeLine(line string, usageParser *sseUsageParser) {
	if err := usageParser.Feed([]byte(line + "\n")); err != nil {
		log.Printf("[WARN] SSE usage解析失败: %v", err)
	}

	c.rawBuilder.WriteString(line)
	c.rawBuilder.WriteString("\n")

	if !strings.HasPrefix(line, "data:") {
		return
	}

	c.dataLineCount++
	data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if data == "" || data == "[DONE]" {
		return
	}

	var obj map[string]any
	if err := sonic.Unmarshal([]byte(data), &obj); err != nil {
		return
	}

	if usage := extractUsage(obj); usage != nil {
		c.lastUsage = usage
	}
	if text := extractSSEDeltaText(obj); text != "" {
		c.textBuilder.WriteString(text)
		return
	}
	if msg, raw, matched := extractSSEErrorMessage(obj); matched {
		if msg != "" {
			c.lastErrMsg = msg
		}
		c.lastAPIError = raw
	}
}

func (c *testSSECollector) applyResult(result map[string]any) {
	if c.textBuilder.Len() > 0 {
		result["response_text"] = c.textBuilder.String()
	}
	if c.lastAPIError != nil {
		result["api_error"] = c.lastAPIError
	}
}

func (c *testSSECollector) rawResponse() string {
	return c.rawBuilder.String()
}

func populateTestSSEUsageAndCost(
	result map[string]any,
	testReq *testutil.TestChannelRequest,
	usageParser *sseUsageParser,
	lastUsage map[string]any,
) {
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
}

// parseTestNativeSSEResponse 处理客户端协议与上游协议一致时的原生 SSE 解析。
func (s *Server) parseTestNativeSSEResponse(
	ctx context.Context,
	requestPlan *channelTestRequestPlan,
	testReq *testutil.TestChannelRequest,
	resp *http.Response,
	contentType string,
	start time.Time,
	result map[string]any,
) map[string]any {
	collector := newTestSSECollector()
	firstByteCaptured := false

	// [DRY] 复用代理链路的SSE usage解析器，保证tokens/成本口径一致
	usageParser := newSSEUsageParser(requestPlan.upstreamProtocol)

	scanner := bufio.NewScanner(resp.Body)
	buf := make([]byte, 0, 1024*1024)
	scanner.Buffer(buf, 16*1024*1024)

	for scanner.Scan() {
		if !firstByteCaptured {
			firstByteCaptured = true
			result["first_byte_duration_ms"] = time.Since(start).Milliseconds()
		}

		line := scanner.Text()
		collector.consumeLine(line, usageParser)
	}

	if err := scanner.Err(); err != nil {
		result["duration_ms"] = time.Since(start).Milliseconds()
		result["error"] = "读取流式响应失败: " + err.Error()
		result["raw_response"] = collector.rawResponse()
		return result
	}
	// 容错：部分上游错误地返回 text/event-stream 但实际是完整 JSON。
	// 若未发现任何 SSE data 行，按非流式响应解析。
	if collector.dataLineCount == 0 {
		return s.parseTestNonStreamResponse(ctx, requestPlan, testReq, resp, contentType, start, []byte(collector.rawResponse()), result)
	}

	result["duration_ms"] = time.Since(start).Milliseconds()
	collector.applyResult(result)
	result["raw_response"] = collector.rawResponse()
	result["upstream_response_body"] = collector.rawResponse()
	populateTestSSEUsageAndCost(result, testReq, usageParser, collector.lastUsage)

	if collector.lastErrMsg != "" {
		// 软错误：HTTP 200 但 SSE 流携带错误事件（余额不足、配额耗尽等）
		result["success"] = false
		result["error"] = collector.lastErrMsg
	} else if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		result["message"] = "API测试成功（流式）"
	} else {
		result["error"] = "API返回错误状态: " + resp.Status
	}
	return result
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
