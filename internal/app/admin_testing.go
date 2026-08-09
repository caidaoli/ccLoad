package app

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"mime"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strings"
	"sync/atomic"
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

type channelWebsocketProbeRequest struct {
	URL                string                    `json:"url" binding:"required"`
	APIKey             string                    `json:"api_key" binding:"required"`
	ProxyURL           string                    `json:"proxy_url,omitempty"`
	CustomRequestRules *model.CustomRequestRules `json:"custom_request_rules,omitempty"`
}

func (r *channelWebsocketProbeRequest) Validate() error {
	var err error
	r.URL, err = validateChannelBaseURL(r.URL)
	if err != nil {
		return err
	}
	r.APIKey = strings.TrimSpace(r.APIKey)
	if r.APIKey == "" {
		return errors.New("api_key cannot be empty")
	}
	r.ProxyURL, err = normalizeChannelProxyURL(r.ProxyURL)
	if err != nil {
		return err
	}
	return validateCustomRequestRules(r.CustomRequestRules)
}

type channelWebsocketProbeResult struct {
	Supported bool   `json:"supported"`
	Status    int    `json:"status,omitempty"`
	Error     string `json:"error,omitempty"`
}

// HandleChannelWebsocketProbe 只探测 Codex Responses WebSocket 握手能力。
// 它不发送模型请求，不写日志、冷却或渠道配置。
func (s *Server) HandleChannelWebsocketProbe(c *gin.Context) {
	var probe channelWebsocketProbeRequest
	if err := BindAndValidate(c, &probe); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid websocket probe request: "+err.Error())
		return
	}

	cfg := &model.Config{
		URLs:               model.ChannelURLs{{URL: model.StripExactUpstreamURLMarker(probe.URL), Exact: model.HasExactUpstreamURLMarker(probe.URL)}},
		ProxyURL:           probe.ProxyURL,
		CustomRequestRules: probe.CustomRequestRules,
	}
	testReq := &testutil.TestChannelRequest{Model: "websocket-probe", Stream: true, Content: "probe"}
	fullURL, headers, _, err := (&testutil.CodexTester{}).Build(cfg, probe.APIKey, testReq)
	if err != nil {
		RespondError(c, http.StatusBadRequest, fmt.Errorf("build websocket probe: %w", err))
		return
	}
	upstreamHeaders := make(http.Header)
	copyCodexHTTPHeaders(upstreamHeaders, headers)
	applyHeaderRules(upstreamHeaders, cfg.HeaderRules())
	probeRequest := &http.Request{Header: upstreamHeaders}
	injectCodexHeaders(probeRequest, cfg, probe.APIKey, true)
	copyCodexWebsocketInputHeaders(upstreamHeaders, headers)
	websocketURL, err := codexWebsocketURL(fullURL)
	if err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}

	conn, resp, dialErr := s.codexWebsocketDialer(cfg).DialContext(
		c.Request.Context(), websocketURL, codexWebsocketHeaders(upstreamHeaders),
	)
	if conn != nil {
		_ = conn.Close()
	}
	status := 0
	if resp != nil {
		status = resp.StatusCode
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}
	if dialErr != nil {
		errorMessage := dialErr.Error()
		if resp != nil && resp.Status != "" {
			errorMessage = resp.Status
		}
		RespondJSON(c, http.StatusOK, channelWebsocketProbeResult{
			Supported: false,
			Status:    status,
			Error:     errorMessage,
		})
		return
	}

	RespondJSON(c, http.StatusOK, channelWebsocketProbeResult{
		Supported: true,
		Status:    http.StatusSwitchingProtocols,
	})
}

type channelTestRequestPlan struct {
	clientProtocol    string
	upstreamProtocol  string
	upstreamStreaming bool
	apiKey            string
	xaiOAuth          bool
	xaiConversationID string
	clientTester      testutil.ChannelTester
	clientURL         string
	clientHeaders     http.Header
	fullURL           string
	headers           http.Header
	upstreamHeaders   http.Header
	requestBody       []byte
	clientBody        []byte
	timeout           *channelTestTimeout
	debugCapture      *debugCapture
	antigravityOAuth  bool
}

type channelTestTimeout struct {
	cancel                     context.CancelFunc
	firstByteTimeout           time.Duration
	streamTimeout              time.Duration
	nonStreamTimeout           time.Duration
	firstStreamContentTimer    *time.Timer
	streamTimer                *time.Timer
	firstStreamContentTimedOut atomic.Bool
	streamTimedOut             atomic.Bool
}

func (t *channelTestTimeout) cancelAll() {
	if t == nil {
		return
	}
	if t.firstStreamContentTimer != nil {
		t.firstStreamContentTimer.Stop()
	}
	if t.streamTimer != nil {
		t.streamTimer.Stop()
	}
	if t.cancel != nil {
		t.cancel()
	}
}

func (t *channelTestTimeout) markFirstStreamContent() {
	if t == nil || t.firstStreamContentTimer == nil {
		return
	}
	t.firstStreamContentTimer.Stop()
}

func (t *channelTestTimeout) firstStreamContentTimeoutTriggered() bool {
	return t != nil && t.firstStreamContentTimedOut.Load()
}

func (t *channelTestTimeout) streamTimeoutTriggered() bool {
	return t != nil && t.streamTimedOut.Load()
}

func newChannelTester(protocolName string) testutil.ChannelTester {
	switch util.NormalizeProtocol(protocolName) {
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

func resolveClientProtocol(testReq *testutil.TestChannelRequest) string {
	if protocolName := strings.TrimSpace(testReq.ClientProtocol); protocolName != "" {
		return strings.ToLower(protocolName)
	}
	return ""
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

func (s *Server) newChannelTestTimeoutContextWithTimeouts(parent context.Context, stream bool, timeouts protocolTimeoutConfig) (context.Context, *channelTestTimeout) {
	ctx, cancel := context.WithCancel(parent)
	timeout := &channelTestTimeout{
		cancel:           cancel,
		firstByteTimeout: timeouts.FirstByteTimeout,
		streamTimeout:    timeouts.StreamTimeout,
		nonStreamTimeout: timeouts.NonStreamTimeout,
	}

	if stream {
		if timeouts.StreamTimeout > 0 {
			timeout.streamTimer = time.AfterFunc(timeouts.StreamTimeout, func() {
				timeout.streamTimedOut.Store(true)
				cancel()
			})
		}
		if timeouts.FirstByteTimeout > 0 {
			timeout.firstStreamContentTimer = time.AfterFunc(timeouts.FirstByteTimeout, func() {
				timeout.firstStreamContentTimedOut.Store(true)
				cancel()
			})
		}
		return ctx, timeout
	}

	if timeouts.NonStreamTimeout > 0 {
		timeoutCtx, timeoutCancel := context.WithTimeout(ctx, timeouts.NonStreamTimeout)
		timeout.cancel = func() {
			timeoutCancel()
			cancel()
		}
		return timeoutCtx, timeout
	}

	return ctx, timeout
}

func (s *Server) describeChannelTestTimeoutError(start time.Time, testReq *testutil.TestChannelRequest, timeout *channelTestTimeout, err error) (int, string, bool) {
	durationSec := time.Since(start).Seconds()
	if timeout.firstStreamContentTimeoutTriggered() {
		threshold := timeout.firstByteTimeout
		if threshold == 0 {
			threshold = s.firstByteTimeout
		}
		return util.StatusFirstByteTimeout,
			fmt.Sprintf("流式请求首个有效内容超时: upstream first valid stream content timeout after %.2fs (threshold=%v): %v", durationSec, threshold, err),
			true
	}
	if timeout.streamTimeoutTriggered() {
		return util.StatusStreamIncomplete,
			fmt.Sprintf("流式请求总超时: upstream stream timeout after %.2fs (threshold=%v): %v", durationSec, timeout.streamTimeout, err),
			true
	}
	if !testReq.Stream && timeout != nil && timeout.nonStreamTimeout > 0 && errors.Is(err, context.DeadlineExceeded) {
		threshold := timeout.nonStreamTimeout
		if threshold == 0 {
			threshold = s.nonStreamTimeout
		}
		return http.StatusGatewayTimeout,
			fmt.Sprintf("非流式请求超时: upstream timeout after %.2fs (threshold=%v): %v", durationSec, threshold, err),
			true
	}
	return 0, "", false
}

func testStreamParserHasFirstContent(parser usageParser) bool {
	return parser != nil && (parser.GetLastError() != nil || parser.HasStreamOutput())
}

func markTestFirstStreamContent(requestPlan *channelTestRequestPlan, result map[string]any, start time.Time) {
	if requestPlan == nil {
		return
	}
	if _, exists := result["first_byte_duration_ms"]; !exists {
		result["first_byte_duration_ms"] = time.Since(start).Milliseconds()
	}
	requestPlan.timeout.markFirstStreamContent()
}

// patchUpstreamTestFields 将上游测试器拥有的顶层字段覆盖到协议转换结果。
// upstreamBody 已应用 TestChannelRequest，因此显式请求选项必须优先于转换器默认值。
func patchUpstreamTestFields(translatedBody, upstreamBody []byte, upstreamProtocol string) []byte {
	var keys []string
	switch upstreamProtocol {
	case "anthropic":
		keys = []string{"system"}
	case "codex":
		keys = []string{
			"instructions",
			"reasoning",
			"include",
			"text",
			"tool_choice",
			"client_metadata",
			"prompt_cache_key",
		}
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

	for _, key := range keys {
		if val, ok := upstream[key]; ok {
			translated[key] = val
		} else {
			delete(translated, key)
		}
	}

	result, err := sonic.ConfigStd.Marshal(translated)
	if err != nil {
		return translatedBody
	}
	return result
}

func (s *Server) buildChannelTestRequestPlan(
	cfgForBuild *model.Config,
	apiKey string,
	testReq *testutil.TestChannelRequest,
	clientProtocol, upstreamProtocol string,
) (*channelTestRequestPlan, error) {
	clientTester := newChannelTester(clientProtocol)

	fullURL, headers, body, err := clientTester.Build(cfgForBuild, apiKey, testReq)
	if err != nil {
		return nil, err
	}

	plan := &channelTestRequestPlan{
		clientProtocol:   clientProtocol,
		upstreamProtocol: upstreamProtocol,
		apiKey:           apiKey,
		clientTester:     clientTester,
		clientURL:        fullURL,
		clientHeaders:    cloneHeaders(headers),
		fullURL:          fullURL,
		headers:          headers,
		requestBody:      body,
		clientBody:       body,
		antigravityOAuth: cfgForBuild.UsesAntigravityOAuth(),
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
		channelTestClientRequestPath(clientProtocol, testReq),
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

	// 协议转换负责消息和工具的格式变换；上游测试器负责系统提示、请求选项和协议默认值。
	translatedBody = patchUpstreamTestFields(translatedBody, upstreamBody, upstreamProtocol)

	plan.fullURL = upstreamURL
	plan.headers = cloneHeaders(upstreamHeaders)
	plan.requestBody = translatedBody
	return plan, nil
}

func channelTestClientRequestPath(protocolName string, testReq *testutil.TestChannelRequest) string {
	switch protocol.Protocol(protocolName) {
	case protocol.OpenAI:
		return "/v1/chat/completions"
	case protocol.Anthropic:
		return "/v1/messages"
	case protocol.Codex:
		return "/v1/responses"
	case protocol.Gemini:
		action := ":generateContent"
		if testReq != nil && testReq.Stream {
			action = ":streamGenerateContent"
		}
		return "/v1beta/models/test" + action
	default:
		return ""
	}
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
		result["success"] = false
		result["error"] = "读取流式响应失败: " + scanner.Err().Error()
		return result
	}
	if collector.dataLineCount == 0 {
		result["success"] = false
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
		log.Printf("[WARN] invalid channel test request: %v", err)
		RespondErrorMsg(c, http.StatusBadRequest, "invalid request")
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
	runtimeCfg, keySelection, err := s.prepareChannelTestAuth(
		c.Request.Context(), cfg, apiKeys, testReq.KeyIndex, strings.TrimSpace(testReq.APIKey),
	)
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
	testResult := s.executeChannelTestWithCooldown(c.Request.Context(), runtimeCfg, keySelection.keyIndex, keySelection.requestCredential, &testReq, keySelection.updatePersistedCooldown)
	s.persistDetectionLog(c.Request.Context(), detectionLogFromResult(cfg, model.LogSourceManualTest, requestedModel, channelTestActualModel(testResult, testReq.Model), keySelection.apiKey, c.ClientIP(), testReq.ThinkingEffort, testResult))
	testResult["tested_key_index"] = keySelection.keyIndex
	testResult["total_keys"] = len(apiKeys)

	RespondJSON(c, http.StatusOK, testResult)
}

func channelTestActualModel(result map[string]any, fallback string) string {
	if actualModel, _ := result["actual_model"].(string); strings.TrimSpace(actualModel) != "" {
		return actualModel
	}
	return fallback
}

type channelTestKeySelection struct {
	keyIndex                int
	apiKey                  string
	requestCredential       string
	updatePersistedCooldown bool
}

func (s *Server) prepareChannelTestAuth(
	ctx context.Context,
	cfg *model.Config,
	apiKeys []*model.APIKey,
	requestedKeyIndex int,
	requestAPIKey string,
) (*model.Config, channelTestKeySelection, error) {
	cfg = s.withOAuthBaseURLOverride(cfg)
	if cfg != nil && cfg.UsesCodexOAuth() {
		credential, err := s.codexCredentials.credential(ctx, cfg, false)
		if err != nil {
			return nil, channelTestKeySelection{}, fmt.Errorf("加载 Codex OAuth 凭证失败: %w", err)
		}
		runtimeCfg := cfg.Clone()
		runtimeCfg.CodexAccessToken = credential.AccessToken
		runtimeCfg.CodexAccountID = credential.AccountID
		return runtimeCfg, channelTestKeySelection{
			keyIndex:                cooldown.NoKeyIndex,
			updatePersistedCooldown: true,
		}, nil
	}
	if cfg != nil && cfg.UsesAntigravityOAuth() {
		credential, err := s.antigravityCredentials.credential(ctx, cfg, false)
		if err != nil {
			return nil, channelTestKeySelection{}, fmt.Errorf("加载 Antigravity OAuth 凭证失败: %w", err)
		}
		runtimeCfg := cfg.Clone()
		runtimeCfg.AntigravityAccessToken = credential.AccessToken
		runtimeCfg.AntigravityProjectID = credential.ProjectID
		return runtimeCfg, channelTestKeySelection{
			keyIndex:                cooldown.NoKeyIndex,
			updatePersistedCooldown: true,
		}, nil
	}
	if cfg != nil && cfg.UsesXAIOAuth() {
		credential, err := s.xaiCredentials.credential(ctx, cfg, false)
		if err != nil {
			return nil, channelTestKeySelection{}, fmt.Errorf("加载 xAI OAuth 凭证失败: %w", err)
		}
		return cfg.Clone(), channelTestKeySelection{
			keyIndex:                cooldown.NoKeyIndex,
			requestCredential:       credential.AccessToken,
			updatePersistedCooldown: true,
		}, nil
	}
	if cfg != nil && cfg.UsesAnthropicOAuth() {
		credential, err := s.anthropicCredentials.credential(ctx, cfg, false)
		if err != nil {
			return nil, channelTestKeySelection{}, fmt.Errorf("加载 Anthropic OAuth 凭证失败: %w", err)
		}
		return cfg.Clone(), channelTestKeySelection{
			keyIndex: cooldown.NoKeyIndex, requestCredential: credential.AccessToken,
			updatePersistedCooldown: true,
		}, nil
	}

	if len(apiKeys) == 0 && requestAPIKey == "" {
		return nil, channelTestKeySelection{}, errors.New("渠道未配置有效的 API Key")
	}
	selection, err := s.selectChannelTestKey(apiKeys, requestedKeyIndex, requestAPIKey)
	return cfg, selection, err
}

func (s *Server) selectChannelTestKey(apiKeys []*model.APIKey, requestedKeyIndex int, requestAPIKey string) (channelTestKeySelection, error) {
	if requestAPIKey != "" {
		matchedKey, ok := findAPIKeyByIndex(apiKeys, requestedKeyIndex)
		return channelTestKeySelection{
			keyIndex:                requestedKeyIndex,
			apiKey:                  requestAPIKey,
			requestCredential:       requestAPIKey,
			updatePersistedCooldown: ok && matchedKey.APIKey == requestAPIKey,
		}, nil
	}

	// 显式优于隐式：调用方指定了 key_index 就严格使用该 Key（无视冷却状态）。
	// 既往的"冷却时静默回退到其他可用 Key"会导致 tested_key_index 与请求不一致，
	// 让用户困惑（点了 key 0 却测了 key 4）。要测全部冷却中的渠道，请显式指定 key_index 或调用方自行选择。
	requestedKey, ok := findAPIKeyByIndex(apiKeys, requestedKeyIndex)
	if !ok {
		return channelTestKeySelection{}, fmt.Errorf("未找到 Key #%d", requestedKeyIndex)
	}
	return channelTestKeySelection{
		keyIndex:                requestedKey.KeyIndex,
		apiKey:                  requestedKey.APIKey,
		requestCredential:       requestedKey.APIKey,
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
	actualModel := channelTestActualModel(result, testReq.Model)
	if success, ok := result["success"].(bool); ok && success {
		if updatePersistedCooldown {
			if keyIndex != cooldown.NoKeyIndex {
				if err := s.store.ResetKeyCooldown(ctx, cfg.ID, keyIndex); err != nil {
					log.Printf("[WARN] 清除Key #%d冷却状态失败: %v", keyIndex, err)
				}
			}
			if err := s.store.ResetChannelCooldown(ctx, cfg.ID); err != nil {
				log.Printf("[WARN] 清除渠道冷却状态失败: %v", err)
			}
			if actualModel != "" {
				if err := s.store.ResetModelCooldown(ctx, cfg.ID, actualModel); err != nil {
					log.Printf("[WARN] 清除模型 %s 冷却状态失败: %v", actualModel, err)
				}
			}
			s.invalidateChannelRelatedCache(cfg.ID)
		}
		return result
	}

	if limited, _ := result["rpm_limited"].(bool); limited {
		result["cooldown_action"] = "rpm_limited_no_cooldown"
		return result
	}
	if limited, _ := result["concurrency_limited"].(bool); limited {
		result["cooldown_action"] = "concurrency_limited_no_cooldown"
		return result
	}

	if !updatePersistedCooldown {
		result["cooldown_action"] = "request_key_no_cooldown"
		return result
	}

	statusCode, errorBody, headers := buildTestFailureClassificationInput(result)
	input := httpErrorInputFromParts(cfg.ID, keyIndex, statusCode, errorBody, headers)
	if upstreamStatusCode, ok := getResultInt(result["status_code"]); ok {
		input.UpstreamStatusCode = upstreamStatusCode
	}
	action := s.applyCooldownDecision(
		ctx,
		cfg,
		cooldownInputForModel(input, actualModel),
	)

	switch action {
	case cooldown.ActionRetryKey:
		result["cooldown_action"] = "key_cooldown_applied"
	case cooldown.ActionRetryModel:
		result["cooldown_action"] = "model_cooldown_applied"
	case cooldown.ActionRetryChannel:
		result["cooldown_action"] = "channel_cooldown_applied"
	case cooldown.ActionReturnClient:
		result["cooldown_action"] = "client_error_no_cooldown"
	default:
		result["cooldown_action"] = "unknown_action"
	}

	return result
}

func channelRPMExceededTestResult(start time.Time, retryAfter time.Duration) map[string]any {
	retryAfterMs := int64(retryAfter / time.Millisecond)
	if retryAfter > 0 && retryAfterMs == 0 {
		retryAfterMs = 1
	}
	return map[string]any{
		"success":        false,
		"error":          "渠道已达到RPM限制",
		"status_code":    http.StatusTooManyRequests,
		"duration_ms":    time.Since(start).Milliseconds(),
		"rpm_limited":    true,
		"retry_after_ms": retryAfterMs,
	}
}

func channelConcurrencyExceededTestResult(start time.Time, err error) map[string]any {
	active, limit, _ := channelConcurrencyLimit(err)
	return map[string]any{
		"success":             false,
		"error":               "渠道已达到并发限制",
		"status_code":         http.StatusTooManyRequests,
		"duration_ms":         time.Since(start).Milliseconds(),
		"concurrency_limited": true,
		"active_concurrency":  active,
		"max_concurrency":     limit,
	}
}

func resolveConfiguredURLUpstreamProtocols(
	cfg *model.Config,
	entry model.ChannelURL,
	clientProtocol string,
) []string {
	client := protocol.Protocol(clientProtocol)
	candidates, _ := protocolCandidatesForURL(
		entry,
		cfg.GetProtocolTransformMode(),
		client,
		channelTestRequestFamily(client),
		localUpstreamProtocolOrder(cfg.URLs),
	)
	upstreamProtocols := make([]string, len(candidates))
	for idx, candidate := range candidates {
		upstreamProtocols[idx] = string(candidate)
	}
	return upstreamProtocols
}

func configuredURLAt(cfg *model.Config, index int, runtimeURL string) model.ChannelURL {
	if cfg == nil {
		return model.ChannelURL{
			URL:   model.StripExactUpstreamURLMarker(runtimeURL),
			Exact: model.HasExactUpstreamURLMarker(runtimeURL),
		}
	}
	return configuredURLFrom(cfg.URLs, index, runtimeURL)
}

// 测试渠道API连通性
func (s *Server) testChannelAPI(reqCtx context.Context, cfg *model.Config, apiKey string, testReq *testutil.TestChannelRequest) map[string]any {
	// 设置默认测试内容（从配置读取）
	if strings.TrimSpace(testReq.Content) == "" {
		testReq.Content = configuredChannelTestContent(s.configService)
	}

	clientProtocol := resolveClientProtocol(testReq)

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
	switch cfg.GetProtocolTransformMode() {
	case model.ProtocolTransformModeAuto:
		orderedURLs = prioritizeAutomaticProtocolURLs(orderedURLs, cfg.URLs)
	case model.ProtocolTransformModeLocal:
		orderedURLs = prioritizeDeclaredProtocolURLs(orderedURLs, cfg.URLs)
	}

	var lastResult map[string]any
	for idx, entry := range orderedURLs {
		upstreamProtocols := resolveConfiguredURLUpstreamProtocols(
			cfg, configuredURLAt(cfg, entry.idx, entry.url), clientProtocol,
		)
		if len(upstreamProtocols) == 0 {
			lastResult = map[string]any{
				"success":  false,
				"error":    fmt.Sprintf("URL 不支持当前协议 %s", clientProtocol),
				"base_url": entry.url,
			}
			continue
		}

		capabilityExhausted := false
		for protocolIdx, upstreamProtocol := range upstreamProtocols {
			lastResult = s.testChannelAPIWithURLForProtocol(
				reqCtx, cfg, apiKey, testReq, clientProtocol, upstreamProtocol, entry.url,
			)
			lastResult["base_url"] = entry.url
			success, _ := lastResult["success"].(bool)
			if success {
				if selector != nil {
					latency := pickURLSelectorLatency(lastResult)
					selector.RecordLatency(cfg.ID, entry.url, latency)
				}
				return lastResult
			}
			if !isChannelTestProtocolEndpointMissing(lastResult) {
				break
			}
			capabilityExhausted = protocolIdx == len(upstreamProtocols)-1
		}
		if idx == len(orderedURLs)-1 {
			break
		}
		if capabilityExhausted && cfg.GetProtocolTransformMode() != model.ProtocolTransformModeUpstream {
			continue
		}

		continueFallback, shouldCooldown := shouldFallbackToNextURL(lastResult)
		if shouldCooldown && selector != nil {
			selector.CooldownURL(cfg.ID, entry.url)
		}
		if !continueFallback {
			return lastResult
		}
	}

	if lastResult != nil {
		return lastResult
	}
	return map[string]any{"success": false, "error": "渠道测试失败: 未找到可用URL"}
}

func (s *Server) testChannelAPIWithURLForProtocol(
	reqCtx context.Context,
	cfg *model.Config,
	apiKey string,
	testReq *testutil.TestChannelRequest,
	clientProtocol, upstreamProtocol, selectedURL string,
) (result map[string]any) {
	attemptReq := *testReq
	attemptReq.Model = s.resolveFinalUpstreamModel(cfg, testReq.Model, upstreamProtocol)
	if attemptReq.Model != testReq.Model {
		log.Printf("[INFO] [测试-请求体修改] 渠道ID=%d, 修改后模型=%s", cfg.ID, attemptReq.Model)
	}
	testReq = &attemptReq
	defer func() {
		if result == nil {
			return
		}
		result["client_protocol"] = clientProtocol
		result["upstream_protocol"] = upstreamProtocol
		result["actual_model"] = attemptReq.Model
	}()

	start := time.Now()
	var (
		req              *http.Request
		requestPlan      *channelTestRequestPlan
		cancel           context.CancelFunc
		capacityRelease  func()
		websocketSession *codexUpstreamWebsocketSession
		err              error
	)
	if testReq.WaitForCapacity {
		var cfgForBuild *model.Config
		cfgForBuild, requestPlan, err = s.buildTestUpstreamRequestPlan(
			cfg, apiKey, testReq, clientProtocol, upstreamProtocol, selectedURL,
		)
		if err == nil {
			capacityRelease, err = s.waitForUpstreamRequest(reqCtx, cfg)
		}
		if err == nil {
			req, cancel, err = s.newTestUpstreamRequest(reqCtx, cfgForBuild, testReq, requestPlan)
		}
		if err != nil && capacityRelease != nil {
			capacityRelease()
		}
	} else {
		req, requestPlan, cancel, err = s.buildTestUpstreamRequestForProtocol(
			reqCtx, cfg, apiKey, testReq, clientProtocol, upstreamProtocol, selectedURL,
		)
	}
	if err != nil {
		result := map[string]any{
			"success":     false,
			"error":       err.Error(),
			"duration_ms": time.Since(start).Milliseconds(),
		}
		if isAutomaticProtocolTranslationFailure(cfg, err) {
			result["protocol_capability_missing"] = true
		}
		return result
	}
	defer cancel()
	ctx := req.Context()
	useNativeCodexWebsocket := cfg.Websockets && !requestPlan.xaiOAuth && testReq.Stream &&
		clientProtocol == string(protocol.Codex) && requestPlan.upstreamProtocol == string(protocol.Codex)
	if useNativeCodexWebsocket {
		copyCodexWebsocketInputHeaders(req.Header, requestPlan.upstreamHeaders)
		preparedBody, prepareErr := buildCodexWebsocketRequestBody(requestPlan.requestBody)
		if prepareErr != nil {
			if capacityRelease != nil {
				capacityRelease()
			}
			return map[string]any{
				"success":     false,
				"error":       prepareErr.Error(),
				"duration_ms": time.Since(start).Milliseconds(),
			}
		}
		websocketURL, websocketURLErr := codexWebsocketURL(requestPlan.fullURL)
		if websocketURLErr != nil {
			if capacityRelease != nil {
				capacityRelease()
			}
			return map[string]any{
				"success":     false,
				"error":       websocketURLErr.Error(),
				"duration_ms": time.Since(start).Milliseconds(),
			}
		}
		debugRequest := req.Clone(ctx)
		debugRequest.Method = "WEBSOCKET"
		debugRequest.URL, err = neturl.Parse(websocketURL)
		if err != nil {
			if capacityRelease != nil {
				capacityRelease()
			}
			return map[string]any{
				"success":     false,
				"error":       "解析 WebSocket URL 失败: " + err.Error(),
				"duration_ms": time.Since(start).Milliseconds(),
			}
		}
		requestPlan.fullURL = websocketURL
		requestPlan.requestBody = preparedBody
		requestPlan.debugCapture = s.captureDebugRequest(debugRequest, preparedBody)
		websocketSession = newCodexUpstreamWebsocketSession(
			s.bodyLimits.maxForPath("/v1/responses"),
			s.upstreamConnectionMaxAge,
			newCodexWebsocketFailureTracker(),
		)
		defer websocketSession.Close()
	}

	// 发送请求
	var resp *http.Response
	if useNativeCodexWebsocket {
		resp, err = s.doChannelTestCodexWebsocket(ctx, cfg, websocketSession, req, requestPlan.requestBody, capacityRelease)
	} else if capacityRelease != nil {
		resp, err = s.doReservedUpstreamRequest(cfg, req, capacityRelease, nil)
	} else {
		resp, err = s.doUpstreamRequest(cfg, req)
	}
	if err != nil {
		if errors.Is(err, ErrChannelRPMExceeded) {
			result := channelRPMExceededTestResult(start, channelRPMRetryAfter(err))
			result["is_streaming"] = testReq.Stream
			return attachTestDebugData(requestPlan, nil, result)
		}
		if errors.Is(err, ErrChannelConcurrencyExceeded) {
			result := channelConcurrencyExceededTestResult(start, err)
			result["is_streaming"] = testReq.Stream
			return attachTestDebugData(requestPlan, nil, result)
		}
		errorMsg := "网络请求失败: " + err.Error()
		statusCode := 0
		if timeoutStatus, timeoutMsg, ok := s.describeChannelTestTimeoutError(start, testReq, requestPlan.timeout, err); ok {
			errorMsg = timeoutMsg
			statusCode = timeoutStatus
		}
		result := map[string]any{
			"success":     false,
			"error":       errorMsg,
			"duration_ms": time.Since(start).Milliseconds(),
		}
		if statusCode > 0 {
			result["status_code"] = statusCode
		}
		result["is_streaming"] = testReq.Stream
		return attachTestDebugData(requestPlan, nil, result)
	}
	s.persistCodexPassiveUsage(ctx, cfg, resp)
	defer func() { _ = resp.Body.Close() }()
	if isAnthropicOAuthMessagesRequest(
		cfg, protocol.Protocol(requestPlan.upstreamProtocol), req.URL.Path,
	) {
		if decodeErr := decodeAnthropicResponse(resp); decodeErr != nil {
			return attachTestDebugData(requestPlan, resp, map[string]any{
				"success": false, "error": "解码 Anthropic 测试响应失败: " + decodeErr.Error(),
				"duration_ms": time.Since(start).Milliseconds(), "status_code": resp.StatusCode,
				"is_streaming": testReq.Stream,
			})
		}
	}
	if requestPlan.debugCapture != nil {
		requestPlan.debugCapture.wrapResponseBody(resp)
	}

	// 判断是否为SSE响应，以及是否请求了流式
	contentType := resp.Header.Get("Content-Type")
	isEventStream := responseIsSSE(resp, requestPlan.upstreamStreaming)

	// 通用结果初始化
	result = map[string]any{
		"success":      resp.StatusCode >= 200 && resp.StatusCode < 300,
		"status_code":  resp.StatusCode,
		"is_streaming": testReq.Stream,
	}
	if useNativeCodexWebsocket {
		result["transport"] = "websocket"
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			result["websocket_handshake_status"] = http.StatusSwitchingProtocols
		}
	}

	// 始终返回上游请求原始数据，便于调试排查（不依赖 debug_log_enabled）
	result["upstream_request_url"] = requestPlan.fullURL
	result["upstream_request_headers"] = maskSensitiveHeaderMap(flattenHeader(req.Header))
	result["upstream_request_body"] = string(requestPlan.requestBody)
	if effort := testRequestThinkingEffort(testReq, requestPlan); effort != "" {
		result["thinking_effort"] = effort
	}

	// 附带响应头与类型，便于排查（不含请求头以避免泄露）
	if len(resp.Header) > 0 {
		result["response_headers"] = flattenHeader(resp.Header)
	}
	if contentType != "" {
		result["content_type"] = contentType
	}

	if isEventStream {
		if requestPlan.clientProtocol != requestPlan.upstreamProtocol || requestPlan.antigravityOAuth {
			return attachTestDebugData(requestPlan, resp, s.parseTestTranslatedSSEResponse(ctx, requestPlan, testReq, resp, start, result))
		}
		return attachTestDebugData(requestPlan, resp, s.parseTestNativeSSEResponse(ctx, requestPlan, testReq, resp, contentType, start, result))
	}

	// 非流式或非SSE响应：按原逻辑读取完整响应（即便前端请求了流式，但上游未返回SSE，也按普通响应处理，确保能展示完整错误体）
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		errorMsg := "读取响应失败: " + err.Error()
		statusCode := resp.StatusCode
		if timeoutStatus, timeoutMsg, ok := s.describeChannelTestTimeoutError(start, testReq, requestPlan.timeout, err); ok {
			errorMsg = timeoutMsg
			statusCode = timeoutStatus
		}
		return attachTestDebugData(requestPlan, resp, map[string]any{
			"success":      false,
			"error":        errorMsg,
			"duration_ms":  time.Since(start).Milliseconds(),
			"status_code":  statusCode,
			"is_streaming": testReq.Stream,
		})
	}
	return attachTestDebugData(requestPlan, resp, s.parseTestNonStreamResponse(ctx, requestPlan, testReq, resp, contentType, start, respBody, result))
}

func (s *Server) doChannelTestCodexWebsocket(
	ctx context.Context,
	cfg *model.Config,
	session *codexUpstreamWebsocketSession,
	req *http.Request,
	body []byte,
	capacityRelease func(),
) (*http.Response, error) {
	if capacityRelease == nil {
		resp, _, _, err := s.doCodexWebsocketRequest(
			ctx, cfg, session, req, body, nil, nil, req.URL.String(),
		)
		return resp, err
	}

	resp, _, _, err := session.roundTrip(
		ctx,
		cfg,
		s.codexWebsocketDialer(cfg),
		req,
		body,
		nil,
		nil,
		s.skipTLSVerify,
		s.codexWebsocketTimeouts(),
		req.URL.String(),
		func(headers http.Header) {
			if len(headers) == 0 {
				return
			}
			s.persistCodexPassiveUsage(ctx, cfg, &http.Response{
				StatusCode: http.StatusOK,
				Header:     headers,
			})
		},
	)
	if err != nil || resp == nil || resp.Body == nil {
		capacityRelease()
		return resp, err
	}
	resp.Body = &releaseOnCloseReadCloser{ReadCloser: resp.Body, release: capacityRelease}
	return resp, nil
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
		upstreamBody := bodyBytes
		translatedRequestBody := requestPlan.requestBody
		if requestPlan.antigravityOAuth && len(bodyBytes) > 0 {
			var unwrapErr error
			upstreamBody, unwrapErr = unwrapAntigravityResponse(bodyBytes)
			if unwrapErr != nil {
				result["success"] = false
				result["error"] = "解包 Antigravity 测试响应失败: " + unwrapErr.Error()
				result["raw_response"] = string(bodyBytes)
				return result
			}
			translatedRequestBody, unwrapErr = unwrapAntigravityRequest(requestPlan.requestBody)
			if unwrapErr != nil {
				result["success"] = false
				result["error"] = "解包 Antigravity 测试请求失败: " + unwrapErr.Error()
				return result
			}
			parseBody = upstreamBody
		}
		if (requestPlan.clientProtocol != requestPlan.upstreamProtocol || requestPlan.antigravityOAuth) && len(bodyBytes) > 0 {
			translatedBody, translateErr := s.protocolRegistry.TranslateResponseNonStream(
				ctx,
				protocol.Protocol(requestPlan.upstreamProtocol),
				protocol.Protocol(requestPlan.clientProtocol),
				testReq.Model,
				requestPlan.clientBody,
				translatedRequestBody,
				upstreamBody,
			)
			if translateErr != nil {
				result["success"] = false
				result["error"] = "转换测试响应失败: " + translateErr.Error()
				result["raw_response"] = string(bodyBytes)
				return result
			}
			parseBody = translatedBody
			translatedHeader := resp.Header.Clone()
			translatedHeader.Set("Content-Type", "application/json")
			translatedHeader.Del("Content-Encoding")
			requestPlan.debugCapture.captureTranslatedResponseMeta(resp.StatusCode, translatedHeader)
			requestPlan.debugCapture.captureTranslatedResponse(translatedBody)
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
		_ = usageParser.Feed(upstreamBody)
		populateTestNormalizedUsageAndCost(result, testReq, usageParser)

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

func (s *Server) buildTestUpstreamRequestPlan(
	cfg *model.Config,
	apiKey string,
	testReq *testutil.TestChannelRequest,
	clientProtocol, upstreamProtocol, selectedURL string,
) (*model.Config, *channelTestRequestPlan, error) {
	cfgForBuild := cfg.Clone()
	cfgForBuild.URLs = model.ChannelURLs{{
		URL:   model.StripExactUpstreamURLMarker(selectedURL),
		Exact: model.HasExactUpstreamURLMarker(selectedURL),
	}}

	requestPlan, err := s.buildChannelTestRequestPlan(cfgForBuild, apiKey, testReq, clientProtocol, upstreamProtocol)
	if err != nil {
		return nil, nil, fmt.Errorf("构造测试请求失败: %w", err)
	}

	requestPath := extractRequestPath(requestPlan.fullURL)
	if parsed, parseErr := neturl.Parse(requestPlan.fullURL); parseErr == nil {
		requestPath = parsed.Path
	}
	upstreamProtocolValue := protocol.Protocol(requestPlan.upstreamProtocol)
	xaiResponsesRequest := isXAIOAuthResponsesRequest(cfgForBuild, upstreamProtocolValue, requestPath)
	if xaiResponsesRequest {
		requestPlan.fullURL = buildXAIResponsesURL(selectedURL, "")
		requestPath = extractRequestPath(requestPlan.fullURL)
	}
	if isAnthropicOAuthMessagesRequest(cfgForBuild, upstreamProtocolValue, requestPath) {
		requestPlan.fullURL = buildAnthropicOAuthURL(selectedURL, requestPath, "")
	}
	requestedStreaming := isStreamingRequest(requestPath, requestPlan.requestBody)
	requestPlan.requestBody, err = s.prepareTranslatedUpstreamBody(
		cfgForBuild, upstreamProtocolValue, requestPath, requestPlan.requestBody, requestPlan.clientBody, requestPlan.headers, false,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("finalize test request body: %w", err)
	}
	if xaiResponsesRequest {
		requestPlan.xaiConversationID = testReq.ResolveSessionID()
		requestPlan.requestBody, err = finalizeXAIResponsesBody(
			requestPlan.requestBody,
			testReq.Model,
			requestPlan.xaiConversationID,
			selectedURL,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("finalize xAI test request body: %w", err)
		}
		requestPlan.xaiOAuth = true
		requestPlan.upstreamStreaming = true
	} else if cfgForBuild.UsesAntigravityOAuth() {
		requestPlan.upstreamStreaming = requestedStreaming
		requestPlan.fullURL, err = antigravityUpstreamURL(selectedURL, requestedStreaming)
		if err != nil {
			return nil, nil, err
		}
	} else {
		requestPlan.upstreamStreaming = isStreamingRequest(requestPath, requestPlan.requestBody)
	}
	if upstreamProtocolValue == protocol.Codex && !requestPlan.xaiOAuth {
		sessionID := testReq.ResolveSessionID()
		requestPlan.requestBody = injectCodexPromptCacheKey(requestPlan.requestBody, sessionID)
		ensureCodexSessionHeader(requestPlan.headers, sessionID)
	}
	return cfgForBuild, requestPlan, nil
}

func (s *Server) newTestUpstreamRequest(
	reqCtx context.Context,
	cfgForBuild *model.Config,
	testReq *testutil.TestChannelRequest,
	requestPlan *channelTestRequestPlan,
) (*http.Request, context.CancelFunc, error) {
	ctx, timeout := s.newChannelTestTimeoutContextWithTimeouts(reqCtx, testReq.Stream, s.resolveProtocolTimeouts(protocol.TransformPlan{
		UpstreamProtocol: protocol.Protocol(requestPlan.upstreamProtocol),
	}))
	requestPlan.timeout = timeout
	req, err := http.NewRequestWithContext(ctx, "POST", requestPlan.fullURL, bytes.NewReader(requestPlan.requestBody))
	if err != nil {
		timeout.cancelAll()
		return nil, nil, fmt.Errorf("创建HTTP请求失败: %w", err)
	}

	sourceHeaders := cloneHeaders(requestPlan.headers)
	for key, value := range testReq.Headers {
		sourceHeaders.Set(key, value)
	}
	requestPlan.upstreamHeaders = sourceHeaders
	requestProtocol := protocol.Protocol(requestPlan.upstreamProtocol)
	if requestProtocol == protocol.Codex {
		copyCodexHTTPHeaders(req.Header, sourceHeaders)
	} else {
		for k, vs := range sourceHeaders {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
	}
	applyHeaderRules(req.Header, cfgForBuild.HeaderRules())
	if requestPlan.xaiOAuth {
		injectXAIResponsesHeaders(req, requestPlan.apiKey, requestPlan.xaiConversationID)
	} else if requestProtocol == protocol.Codex {
		injectCodexHeaders(req, cfgForBuild, requestPlan.apiKey, requestPlan.upstreamStreaming)
	} else if cfgForBuild.UsesAntigravityOAuth() {
		injectAntigravityOAuthHeaders(req, cfgForBuild)
	} else if isAnthropicOAuthMessagesRequest(cfgForBuild, requestProtocol, req.URL.Path) {
		injectAnthropicOAuthHeaders(req, cfgForBuild, requestPlan.apiKey, requestPlan.requestBody)
	}
	requestPlan.debugCapture = s.captureDebugRequest(req, requestPlan.requestBody)
	if requestPlan.clientProtocol != requestPlan.upstreamProtocol {
		originalHeaders := cloneHeaders(requestPlan.clientHeaders)
		for key, value := range testReq.Headers {
			originalHeaders.Set(key, value)
		}
		requestPlan.debugCapture.markProtocolTransform(extractRequestPath(requestPlan.clientURL), originalHeaders, requestPlan.clientBody)
	}

	return req, timeout.cancelAll, nil
}

func (s *Server) buildTestUpstreamRequestForProtocol(
	reqCtx context.Context,
	cfg *model.Config,
	apiKey string,
	testReq *testutil.TestChannelRequest,
	clientProtocol, upstreamProtocol, selectedURL string,
) (*http.Request, *channelTestRequestPlan, context.CancelFunc, error) {
	cfgForBuild, requestPlan, err := s.buildTestUpstreamRequestPlan(cfg, apiKey, testReq, clientProtocol, upstreamProtocol, selectedURL)
	if err != nil {
		return nil, nil, nil, err
	}
	req, cancel, err := s.newTestUpstreamRequest(reqCtx, cfgForBuild, testReq, requestPlan)
	if err != nil {
		return nil, nil, nil, err
	}
	return req, requestPlan, cancel, nil
}

func attachTestDebugData(requestPlan *channelTestRequestPlan, resp *http.Response, result map[string]any) map[string]any {
	if result == nil || requestPlan == nil || requestPlan.debugCapture == nil {
		return result
	}
	result["debug_data"] = requestPlan.debugCapture.buildEntry(resp)
	return result
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
	translatedWriter := http.ResponseWriter(recorder)
	if requestPlan.debugCapture != nil {
		translatedWriter = requestPlan.debugCapture.wrapTranslatedResponseWriter(recorder)
	}
	filterAndWriteResponseHeaders(translatedWriter, resp.Header)
	translatedWriter.WriteHeader(resp.StatusCode)
	var rawUpstreamBuf bytes.Buffer
	upstreamTee := io.TeeReader(resp.Body, &rawUpstreamBuf)
	streamReader := readerWithCloser{Reader: upstreamTee, Closer: resp.Body}
	firstContentCaptured := false
	upstreamParser := newSSEUsageParser(requestPlan.upstreamProtocol)
	var translatedComplete bool
	var state any

	streamErr := streamTransformSSEEventsUntil(
		ctx,
		streamReader,
		translatedWriter,
		func(rawEvent []byte) error {
			if len(rawEvent) == 0 {
				return nil
			}
			parserEvent := rawEvent
			if requestPlan.antigravityOAuth {
				var err error
				parserEvent, err = unwrapAntigravitySSEEvent(rawEvent)
				if err != nil {
					return err
				}
			}
			if err := upstreamParser.Feed(parserEvent); err != nil {
				log.Printf("[WARN] SSE 内容解析失败: %v", err)
			}
			if !firstContentCaptured && testStreamParserHasFirstContent(upstreamParser) {
				firstContentCaptured = true
				markTestFirstStreamContent(requestPlan, result, start)
			}
			return nil
		},
		func(rawEvent []byte) ([][]byte, error) {
			translatedRequestBody := requestPlan.requestBody
			if requestPlan.antigravityOAuth {
				var err error
				rawEvent, err = unwrapAntigravitySSEEvent(rawEvent)
				if err != nil {
					return nil, err
				}
				translatedRequestBody, err = unwrapAntigravityRequest(requestPlan.requestBody)
				if err != nil {
					return nil, err
				}
			}
			chunks, err := s.protocolRegistry.TranslateResponseStream(
				ctx,
				protocol.Protocol(requestPlan.upstreamProtocol),
				protocol.Protocol(requestPlan.clientProtocol),
				testReq.Model,
				requestPlan.clientBody,
				translatedRequestBody,
				rawEvent,
				&state,
			)
			if err != nil {
				return nil, err
			}
			if !translatedComplete && translatedStreamChunksComplete(protocol.Protocol(requestPlan.clientProtocol), chunks) {
				translatedComplete = true
			}
			return chunks, nil
		},
		func() bool {
			return upstreamParser.IsStreamComplete() && translatedComplete
		},
	)
	if streamErr != nil {
		errorMsg := "读取流式响应失败: " + streamErr.Error()
		statusCode := resp.StatusCode
		if timeoutStatus, timeoutMsg, ok := s.describeChannelTestTimeoutError(start, testReq, requestPlan.timeout, streamErr); ok {
			errorMsg = timeoutMsg
			statusCode = timeoutStatus
		}
		result["success"] = false
		result["status_code"] = statusCode
		result["duration_ms"] = time.Since(start).Milliseconds()
		result["error"] = errorMsg
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
// 覆盖：
//   - 顶层 error 对象/字符串（Anthropic/Chat 风格）
//   - OpenAI Responses：type=response.failed 或 response.status=failed / response.error
func extractSSEErrorMessage(obj map[string]any) (msg string, raw map[string]any, matched bool) {
	if msg, raw, matched = errorMessageFromObject(obj["error"], obj); matched {
		return msg, raw, true
	}
	if errStr, ok := obj["error"].(string); ok {
		if trimmed := strings.TrimSpace(errStr); trimmed != "" {
			return trimmed, obj, true
		}
	}
	if typ, _ := obj["type"].(string); typ == "response.failed" {
		if resp, ok := obj["response"].(map[string]any); ok {
			if msg, raw, matched = errorMessageFromObject(resp["error"], obj); matched {
				return msg, raw, true
			}
		}
		return "response.failed", obj, true
	}
	if resp, ok := obj["response"].(map[string]any); ok {
		if status, _ := resp["status"].(string); strings.EqualFold(strings.TrimSpace(status), "failed") {
			if msg, raw, matched = errorMessageFromObject(resp["error"], obj); matched {
				return msg, raw, true
			}
			return "response status failed", obj, true
		}
		if msg, raw, matched = errorMessageFromObject(resp["error"], obj); matched {
			return msg, raw, true
		}
	}
	if m, ok := obj["message"].(string); ok && m != "" {
		return m, obj, true
	}
	return "", nil, false
}

func errorMessageFromObject(v any, raw map[string]any) (msg string, out map[string]any, matched bool) {
	errObj, ok := v.(map[string]any)
	if !ok {
		return "", nil, false
	}
	if m, ok := errObj["message"].(string); ok && m != "" {
		return m, raw, true
	}
	if t, ok := errObj["type"].(string); ok && t != "" {
		return t, raw, true
	}
	if code, ok := errObj["code"].(string); ok && code != "" {
		return code, raw, true
	}
	return "", raw, true
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
	if lastUsage != nil {
		result["api_response"] = map[string]any{"usage": lastUsage}
	}
	usage, ok := normalizedTestUsage(usageParser)
	if ok {
		result["usage"] = usage
		if lastUsage == nil {
			result["api_response"] = map[string]any{"usage": usage}
		}
	}
	populateTestNormalizedUsageAndCost(result, testReq, usageParser)
}

func normalizedTestUsage(parser usageParser) (map[string]any, bool) {
	input, output, cacheRead, cacheCreation := parser.GetUsage()
	cache5m, cache1h, _ := parser.GetCacheBreakdown()
	reasoningTokens := parser.GetReasoningTokens()
	if input+output+cacheRead+cacheCreation+cache5m+cache1h+reasoningTokens == 0 {
		return nil, false
	}
	return map[string]any{
		"input_tokens":                input,
		"output_tokens":               output,
		"reasoning_tokens":            reasoningTokens,
		"cache_read_input_tokens":     cacheRead,
		"cache_creation_input_tokens": cacheCreation,
		"cache_5m_input_tokens":       cache5m,
		"cache_1h_input_tokens":       cache1h,
	}, true
}

func populateTestNormalizedUsageAndCost(result map[string]any, testReq *testutil.TestChannelRequest, parser usageParser) {
	usage, ok := normalizedTestUsage(parser)
	if ok {
		result["usage"] = usage
	}
	if effort := parser.GetThinkingEffort(); effort != "" {
		result["thinking_effort"] = effort
	}

	billableInput, output, cacheRead, _ := parser.GetUsage()
	cache5m, cache1h, _ := parser.GetCacheBreakdown()
	if billableInput+output+cacheRead > 0 {
		result["cost_usd"] = util.CalculateCostDetailed(
			testReq.Model,
			billableInput,
			output,
			cacheRead,
			cache5m,
			cache1h,
		) + parser.GetToolCostUSD()
	} else if toolCost := parser.GetToolCostUSD(); toolCost > 0 {
		result["cost_usd"] = toolCost
	}
}

func testRequestThinkingEffort(testReq *testutil.TestChannelRequest, requestPlan *channelTestRequestPlan) string {
	if requestPlan != nil {
		if effort := extractThinkingEffortFromJSON(requestPlan.requestBody); effort != "" {
			return effort
		}
	}
	if testReq == nil {
		return ""
	}
	return normalizeThinkingEffort(testReq.ThinkingEffort)
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
	firstContentCaptured := false

	// [DRY] 复用代理链路的SSE usage解析器，保证tokens/成本口径一致
	usageParser := newSSEUsageParser(requestPlan.upstreamProtocol)

	scanner := bufio.NewScanner(resp.Body)
	buf := make([]byte, 0, 1024*1024)
	scanner.Buffer(buf, 16*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		collector.consumeLine(line, usageParser)
		if !firstContentCaptured && testStreamParserHasFirstContent(usageParser) {
			firstContentCaptured = true
			markTestFirstStreamContent(requestPlan, result, start)
		}
		if usageParser.IsStreamComplete() {
			break
		}
	}

	if err := scanner.Err(); err != nil && !usageParser.IsStreamComplete() {
		errorMsg := "读取流式响应失败: " + err.Error()
		statusCode := resp.StatusCode
		if timeoutStatus, timeoutMsg, ok := s.describeChannelTestTimeoutError(start, testReq, requestPlan.timeout, err); ok {
			errorMsg = timeoutMsg
			statusCode = timeoutStatus
		}
		result["success"] = false
		result["status_code"] = statusCode
		result["duration_ms"] = time.Since(start).Milliseconds()
		result["error"] = errorMsg
		result["raw_response"] = collector.rawResponse()
		return result
	}
	if timeoutStatus, timeoutMsg, ok := s.describeChannelTestTimeoutError(start, testReq, requestPlan.timeout, ctx.Err()); ok {
		result["success"] = false
		result["status_code"] = timeoutStatus
		result["duration_ms"] = time.Since(start).Milliseconds()
		result["error"] = timeoutMsg
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

	if timeoutStatus, timeoutMsg, ok := s.describeChannelTestTimeoutError(start, testReq, requestPlan.timeout, ctx.Err()); ok {
		result["success"] = false
		result["status_code"] = timeoutStatus
		result["error"] = timeoutMsg
		return result
	}

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
	if util.IsModelScopedHTTPStatus(statusCode) || util.IsModelScopedStreamFailure(statusCode) {
		return false, false
	}

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

func isChannelTestProtocolEndpointMissing(result map[string]any) bool {
	if missing, _ := result["protocol_capability_missing"].(bool); missing {
		return true
	}
	statusCode, ok := getResultInt(result["status_code"])
	if !ok {
		return false
	}
	_, errorBody, _ := buildTestFailureClassificationInput(result)
	return util.ShouldFallbackProtocol(statusCode, errorBody)
}

func isAutomaticProtocolTranslationFailure(cfg *model.Config, err error) bool {
	if cfg == nil || cfg.GetProtocolTransformMode() != model.ProtocolTransformModeAuto || err == nil {
		return false
	}
	var translationErr *protocol.RequestTranslationError
	return errors.As(err, &translationErr)
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
