package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/testutil"
	"ccLoad/internal/util"
	"ccLoad/internal/version"
	"ccLoad/internal/xaiauth"

	"github.com/bytedance/sonic"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	imageGenerationPath                = "/v1/images/generations"
	directImageGenerationPath          = "/images/generations"
	imageGenerationDiagnosticBodyLimit = 64 << 10
	imageGenerationAPIImages           = "images"
	imageGenerationAPIChatCompletions  = "chat_completions"
)

type imageGenerationTestRequest struct {
	GenerationAPI string `json:"generation_api"`
	Model         string `json:"model"`
	Prompt        string `json:"prompt"`
	Size          string `json:"size,omitempty"`
	Quality       string `json:"quality,omitempty"`
	Background    string `json:"background,omitempty"`
	OutputFormat  string `json:"output_format,omitempty"`
	KeyIndex      *int   `json:"key_index,omitempty"`
}

func (r *imageGenerationTestRequest) Validate() error {
	if r == nil {
		return errors.New("request is required")
	}
	r.Model = strings.TrimSpace(r.Model)
	r.Prompt = strings.TrimSpace(r.Prompt)
	r.GenerationAPI = strings.ToLower(strings.TrimSpace(r.GenerationAPI))
	r.Size = strings.ToLower(strings.TrimSpace(r.Size))
	r.Quality = strings.ToLower(strings.TrimSpace(r.Quality))
	r.Background = strings.ToLower(strings.TrimSpace(r.Background))
	r.OutputFormat = strings.ToLower(strings.TrimSpace(r.OutputFormat))
	if r.Model == "" || len(r.Model) > 191 {
		return errors.New("model is required and must not exceed 191 characters")
	}
	if r.Prompt == "" || len(r.Prompt) > 32*1024 {
		return errors.New("prompt is required and must not exceed 32 KiB")
	}
	if !oneOf(r.GenerationAPI, imageGenerationAPIImages, imageGenerationAPIChatCompletions) {
		return errors.New("generation_api must be images or chat_completions")
	}
	if r.GenerationAPI == imageGenerationAPIChatCompletions {
		if !validChatImageGenerationSize(r.Size) {
			return errors.New("invalid Chat Completions image size")
		}
		if !oneOf(r.Quality, "", "auto") || !oneOf(r.Background, "", "auto") || !oneOf(r.OutputFormat, "", "auto") {
			return errors.New("chat completions image generation does not support quality, background, or output_format")
		}
	} else if !validImageGenerationSize(r.Size) && !validChatImageGenerationSize(r.Size) {
		return errors.New("invalid Images API image size")
	}
	if !validImageGenerationOption(r.Quality) {
		return errors.New("invalid image quality")
	}
	if !oneOf(r.Background, "", "auto", "opaque", "transparent") {
		return errors.New("invalid image background")
	}
	if !oneOf(r.OutputFormat, "", "auto", "png", "jpeg", "webp") {
		return errors.New("invalid image output format")
	}
	return nil
}

func validImageGenerationSize(value string) bool {
	if value == "" || value == "auto" {
		return true
	}
	widthText, heightText, ok := strings.Cut(value, "x")
	if !ok || strings.Contains(heightText, "x") {
		return false
	}
	width, widthErr := strconv.Atoi(widthText)
	height, heightErr := strconv.Atoi(heightText)
	return widthErr == nil && heightErr == nil && width >= 64 && width <= 8192 && height >= 64 && height <= 8192
}

func validChatImageGenerationSize(value string) bool {
	if value == "" || value == "auto" {
		return true
	}
	aspectRatio, imageSize, ok := strings.Cut(value, "@")
	return ok && oneOf(aspectRatio, "1:1", "16:9", "9:16", "3:2", "2:3") && oneOf(imageSize, "1k", "2k")
}

func validImageGenerationOption(value string) bool {
	if len(value) > 32 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' || char == '_' {
			continue
		}
		return false
	}
	return true
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

// HandleChannelImageGeneration 通过指定渠道和 Key 调用显式选择的生图接口。
func (s *Server) HandleChannelImageGeneration(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel id")
		return
	}

	var imageReq imageGenerationTestRequest
	if err := BindAndValidate(c, &imageReq); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid image generation request")
		return
	}

	cfg, err := s.store.GetConfig(c.Request.Context(), id)
	if err != nil {
		RespondError(c, http.StatusNotFound, fmt.Errorf("channel not found"))
		return
	}
	if imageReq.GenerationAPI == imageGenerationAPIImages && cfg.UsesOAuth() && !cfg.UsesXAIOAuth() && !cfg.UsesCodexOAuth() {
		RespondJSON(c, http.StatusOK, gin.H{
			"success": false,
			"error":   "Images API 测试仅支持 API Key、Codex 或 xAI 凭证渠道；Antigravity 请切换到 Chat Completions",
		})
		return
	}
	if imageReq.GenerationAPI == imageGenerationAPIImages {
		if cfg.UsesXAIOAuth() {
			responsesImage := xaiSupportsImageGeneration(s.resolveFinalUpstreamModel(cfg, imageReq.Model, util.ProtocolOpenAI))
			if !responsesImage && !validChatImageGenerationSize(imageReq.Size) {
				RespondErrorMsg(c, http.StatusBadRequest, "xAI Images API size must use an aspect-ratio and 1K/2K value")
				return
			}
			if !responsesImage && (!oneOf(imageReq.Quality, "", "auto") || !oneOf(imageReq.Background, "", "auto") || !oneOf(imageReq.OutputFormat, "", "auto")) {
				RespondErrorMsg(c, http.StatusBadRequest, "xAI Images API does not support quality, background, or output_format")
				return
			}
		} else if !validImageGenerationSize(imageReq.Size) {
			RespondErrorMsg(c, http.StatusBadRequest, "Images API size must use pixel dimensions")
			return
		}
	}
	modelLookup, modelSupported := imageGenerationModelLookup(cfg, imageReq.Model)
	if !modelSupported {
		RespondJSON(c, http.StatusOK, gin.H{
			"success":          false,
			"error":            "模型 " + imageReq.Model + " 不在此渠道的支持列表中",
			"model":            imageReq.Model,
			"supported_models": cfg.GetModels(),
		})
		return
	}
	if imageReq.GenerationAPI == imageGenerationAPIImages && (cfg.UsesXAIOAuth() || cfg.UsesCodexOAuth()) {
		actualModel := s.resolveFinalUpstreamModel(cfg, modelLookup, util.ProtocolOpenAI)
		if cfg.UsesXAIOAuth() {
			if !xaiSupportsImageGeneration(actualModel) {
				if _, supported := canonicalXAIImageModel(actualModel); supported {
					// xAI 原生 Images 模型继续走 /images/generations。
				} else {
					RespondJSON(c, http.StatusOK, gin.H{
						"success":      false,
						"error":        xaiImageUnsupportedModelError(actualModel).Error(),
						"model":        imageReq.Model,
						"actual_model": actualModel,
					})
					return
				}
			}
		} else if _, supported := canonicalCodexImageModel(actualModel); !supported {
			RespondJSON(c, http.StatusOK, gin.H{
				"success":      false,
				"error":        codexImageUnsupportedModelError(actualModel).Error(),
				"model":        imageReq.Model,
				"actual_model": actualModel,
			})
			return
		}
	}

	apiKeys, err := s.store.GetAPIKeys(c.Request.Context(), id)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	runtimeCfg, keySelection, err := s.prepareChannelTestAuth(
		c.Request.Context(), cfg, apiKeys, imageReq.Model, imageReq.KeyIndex, "", oauthCredentialUseCurrent,
	)
	if err != nil {
		RespondJSON(c, http.StatusOK, gin.H{
			"success":    false,
			"error":      err.Error(),
			"total_keys": len(apiKeys),
		})
		return
	}
	runtimeCfg = s.imageGenerationRuntimeConfig(runtimeCfg, imageReq.GenerationAPI)

	result := s.testChannelImageGenerationByAPI(c.Request.Context(), runtimeCfg, keySelection.requestCredential, &imageReq)
	statusCode, _ := getResultInt(result["status_code"])
	if cfg.UsesOAuth() && statusCode == http.StatusUnauthorized {
		refreshedCfg, refreshedSelection, handled, refreshErr := s.prepareRejectedOAuthChannelTestAuth(
			c.Request.Context(), cfg, oauthCredentialTestAccessToken(cfg, runtimeCfg, keySelection),
		)
		if !handled && refreshErr == nil {
			refreshErr = errors.New("OAuth credential refresh is unavailable")
		}
		if refreshErr != nil {
			message := "刷新 OAuth 凭证失败: " + refreshErr.Error()
			if original := strings.TrimSpace(getResultString(result, "error")); original != "" {
				message = original + "; " + message
			}
			result["error"] = message
		} else {
			runtimeCfg = s.imageGenerationRuntimeConfig(refreshedCfg, imageReq.GenerationAPI)
			keySelection = refreshedSelection
			result = s.testChannelImageGenerationByAPI(
				c.Request.Context(), runtimeCfg, keySelection.requestCredential, &imageReq,
			)
		}
	}
	testReq := imageGenerationChannelTestRequest(&imageReq)
	if clientCanceled, _ := result["client_canceled"].(bool); clientCanceled {
		result["cooldown_action"] = "client_error_no_cooldown"
	} else {
		result = s.applyChannelTestResultCooldown(
			c.Request.Context(), runtimeCfg, keySelection.keyIndex, testReq,
			keySelection.updatePersistedCooldown, result,
		)
	}
	result["tested_key_index"] = keySelection.keyIndex
	result["total_keys"] = len(apiKeys)
	result["generation_api"] = imageReq.GenerationAPI
	s.persistDetectionLog(c.Request.Context(), detectionLogFromResult(
		cfg, model.LogSourceManualTest, model.RoutingModelName(imageReq.Model),
		channelTestActualModel(result, imageReq.Model), keySelection.apiKey,
		c.ClientIP(), "", result,
	))
	delete(result, "debug_data")

	RespondJSON(c, http.StatusOK, result)
}

func (s *Server) imageGenerationRuntimeConfig(cfg *model.Config, generationAPI string) *model.Config {
	if generationAPI == imageGenerationAPIImages {
		if cfg != nil && cfg.UsesCodexOAuth() {
			return withCodexImageGenerationRuntime(cfg)
		}
		return s.withXAIImageGenerationRuntime(cfg)
	}
	return cfg
}

func (s *Server) testChannelImageGenerationByAPI(
	ctx context.Context,
	cfg *model.Config,
	apiKey string,
	imageReq *imageGenerationTestRequest,
) map[string]any {
	if imageReq.GenerationAPI == imageGenerationAPIChatCompletions {
		result := s.testChannelAPI(ctx, cfg, apiKey, imageGenerationChannelTestRequest(imageReq))
		return normalizeChatCompletionsImageResult(result)
	}
	return s.testChannelImageGeneration(ctx, cfg, apiKey, imageReq)
}

func imageGenerationChannelTestRequest(imageReq *imageGenerationTestRequest) *testutil.TestChannelRequest {
	testReq := &testutil.TestChannelRequest{
		Model:          imageReq.Model,
		Content:        imageReq.Prompt,
		ClientProtocol: util.ProtocolOpenAI,
		Stream:         false,
	}
	if imageReq.GenerationAPI == imageGenerationAPIChatCompletions {
		testReq.ImageGeneration = chatCompletionsImageOptions(imageReq.Size)
	}
	return testReq
}

func chatCompletionsImageOptions(size string) *testutil.ImageGenerationOptions {
	options := &testutil.ImageGenerationOptions{}
	normalizedSize := strings.ToLower(strings.TrimSpace(size))
	if normalizedSize == "" || normalizedSize == "auto" {
		return options
	}
	aspectRatio, imageSize, ok := strings.Cut(normalizedSize, "@")
	if !ok {
		return options
	}
	options.AspectRatio = aspectRatio
	options.ImageSize = strings.ToUpper(imageSize)
	return options
}

func normalizeChatCompletionsImageResult(result map[string]any) map[string]any {
	if result == nil {
		return map[string]any{"success": false, "error": "渠道测试失败: 上游返回空结果"}
	}
	delete(result, "cost_usd")
	success, _ := result["success"].(bool)
	if !success {
		return result
	}
	apiResponse, _ := result["api_response"].(map[string]any)
	images := extractChatCompletionsImageData(apiResponse)
	if len(images) == 0 {
		result["success"] = false
		result["error"] = "Chat Completions 响应中没有可显示图片"
		delete(result, "message")
		captureNormalizedImageFailureDebug(result, apiResponse)
		return result
	}
	result["images"] = images
	result["message"] = "图片生成成功"
	if outputFormat := imageOutputFormatFromMIMEType(getResultString(images[0], "mime_type")); outputFormat != "" {
		result["output_format"] = outputFormat
	}
	// api_response 与 upstream_response_body 都包含同一份 base64；返回规范化
	// images 即可，避免响应和检测日志成倍膨胀。
	delete(result, "api_response")
	delete(result, "upstream_response_body")
	delete(result, "raw_response")
	return result
}

func captureNormalizedImageFailureDebug(result map[string]any, apiResponse map[string]any) {
	debugEntry, _ := result["debug_data"].(*model.DebugLogEntry)
	if debugEntry == nil {
		return
	}
	if len(debugEntry.RespBody) == 0 {
		if upstreamBody := getResultString(result, "upstream_response_body"); upstreamBody != "" {
			debugEntry.RespBody = []byte(imageGenerationDiagnosticBody([]byte(upstreamBody)))
		}
	}
	if len(debugEntry.TranslatedRespBody) == 0 && apiResponse != nil {
		if translatedBody, err := sonic.Marshal(apiResponse); err == nil {
			debugEntry.TranslatedRespBody = []byte(imageGenerationDiagnosticBody(translatedBody))
		}
	}
}

func extractChatCompletionsImageData(apiResponse map[string]any) []map[string]any {
	if apiResponse == nil {
		return nil
	}
	choices, _ := apiResponse["choices"].([]any)
	images := make([]map[string]any, 0, len(choices))
	for _, rawChoice := range choices {
		choice, _ := rawChoice.(map[string]any)
		if choice == nil {
			continue
		}
		for _, containerKey := range []string{"message", "delta"} {
			container, _ := choice[containerKey].(map[string]any)
			if container == nil {
				continue
			}
			before := len(images)
			appendChatCompletionsImages(&images, container["images"])
			if len(images) == before {
				appendChatCompletionsContentImages(&images, container["content"])
			}
			if len(images) > before {
				break
			}
		}
	}
	return images
}

func appendChatCompletionsImages(images *[]map[string]any, raw any) {
	items, _ := raw.([]any)
	for _, rawItem := range items {
		item, _ := rawItem.(map[string]any)
		if item == nil {
			continue
		}
		imageURL, _ := item["image_url"].(map[string]any)
		url, _ := imageURL["url"].(string)
		if url == "" {
			url, _ = item["url"].(string)
		}
		if image, ok := normalizeChatCompletionsImageURL(url); ok {
			*images = append(*images, image)
		}
	}
}

func appendChatCompletionsContentImages(images *[]map[string]any, raw any) {
	items, _ := raw.([]any)
	for _, rawItem := range items {
		item, _ := rawItem.(map[string]any)
		if item == nil || !strings.EqualFold(getResultString(item, "type"), "image_url") {
			continue
		}
		imageURL, _ := item["image_url"].(map[string]any)
		if image, ok := normalizeChatCompletionsImageURL(getResultString(imageURL, "url")); ok {
			*images = append(*images, image)
		}
	}
}

func normalizeChatCompletionsImageURL(raw string) (map[string]any, bool) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, false
	}
	comma := strings.IndexByte(value, ',')
	if comma < 0 || !strings.HasPrefix(strings.ToLower(value), "data:") {
		return map[string]any{"url": value}, true
	}
	metadata := strings.TrimSpace(value[len("data:"):comma])
	lowerMetadata := strings.ToLower(metadata)
	if !strings.HasPrefix(lowerMetadata, "image/") || !strings.HasSuffix(lowerMetadata, ";base64") {
		return nil, false
	}
	mimeType := strings.TrimSpace(metadata[:len(metadata)-len(";base64")])
	base64Data := strings.TrimSpace(value[comma+1:])
	if mimeType == "" || base64Data == "" {
		return nil, false
	}
	return map[string]any{"b64_json": base64Data, "mime_type": strings.ToLower(mimeType)}, true
}

func imageOutputFormatFromMIMEType(mimeType string) string {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/jpeg", "image/jpg":
		return "jpeg"
	case "image/png":
		return "png"
	case "image/webp":
		return "webp"
	default:
		return ""
	}
}

// withXAIImageGenerationRuntime keeps xAI chat traffic on the CLI endpoint while
// routing this Images request to the public API endpoint used by CLIProxyAPI.
func (s *Server) withXAIImageGenerationRuntime(cfg *model.Config) *model.Config {
	if cfg == nil || !cfg.UsesXAIOAuth() {
		return cfg
	}
	runtimeCfg := cfg.Clone()
	// CLIProxyAPI routes xAI media and hosted image tools to the public API.
	// The CLI chat endpoint is only for ordinary OAuth chat and drops
	// image_generation before request validation.
	runtimeCfg.URLs = model.ChannelURLs{{URL: xaiauth.APIBaseURL, Protocols: []string{util.ProtocolOpenAI}}}
	runtimeCfg.ProtocolTransformMode = model.ProtocolTransformModeUpstream
	return runtimeCfg
}

func withCodexImageGenerationRuntime(cfg *model.Config) *model.Config {
	if cfg == nil || !cfg.UsesCodexOAuth() {
		return cfg
	}
	responsesURL := codexUpstreamURL
	if len(cfg.URLs) > 0 && strings.TrimSpace(cfg.URLs[0].URL) != "" {
		responsesURL = strings.TrimSpace(cfg.URLs[0].URL)
	}
	baseURL := strings.TrimRight(responsesURL, "/")
	baseURL = strings.TrimSuffix(baseURL, "/responses")
	runtimeCfg := cfg.Clone()
	runtimeCfg.URLs = model.ChannelURLs{{
		URL:       baseURL + directImageGenerationPath,
		Exact:     true,
		Protocols: []string{util.ProtocolOpenAI},
	}}
	runtimeCfg.ProtocolTransformMode = model.ProtocolTransformModeUpstream
	return runtimeCfg
}

func (s *Server) testChannelImageGeneration(
	ctx context.Context,
	cfg *model.Config,
	apiKey string,
	imageReq *imageGenerationTestRequest,
) map[string]any {
	urls := cfg.GetURLs()
	if len(urls) == 0 {
		return map[string]any{"success": false, "error": "渠道URL为空"}
	}

	var selector *URLSelector
	if len(urls) > 1 && s.urlSelector != nil {
		selector = s.urlSelector
	}
	orderedURLs := orderChannelAttemptURLs(selector, cfg, urls)

	var lastResult map[string]any
	for idx, entry := range orderedURLs {
		configuredURL := configuredURLAt(cfg, entry.idx, entry.url)
		candidates, _ := protocolCandidatesForURL(
			configuredURL,
			cfg.GetProtocolTransformMode(),
			protocol.OpenAI,
			protocol.RequestFamilyImages,
			localUpstreamProtocolOrder(cfg.URLs),
		)
		if len(candidates) == 0 || candidates[0] != protocol.OpenAI {
			if lastResult == nil || isImageProtocolCapabilityMissing(lastResult) {
				lastResult = map[string]any{
					"success":                     false,
					"error":                       "URL 不支持 OpenAI Images API",
					"base_url":                    entry.url,
					"protocol_capability_missing": true,
				}
			}
			continue
		}

		lastResult = s.testChannelImageGenerationWithURL(ctx, cfg, apiKey, imageReq, entry.url)
		lastResult["base_url"] = entry.url
		if success, _ := lastResult["success"].(bool); success {
			if selector != nil {
				selector.RecordLatency(cfg.ID, entry.url, pickURLSelectorLatency(lastResult))
			}
			return lastResult
		}
		if clientCanceled, _ := lastResult["client_canceled"].(bool); clientCanceled {
			return lastResult
		}

		hasNextURL := idx < len(orderedURLs)-1
		if !hasNextURL {
			break
		}
		continueFallback, shouldCooldown := shouldFallbackToNextURL(lastResult)
		if shouldCooldown && selector != nil {
			selector.CooldownURL(cfg.ID, entry.url)
		}
		if !continueFallback {
			break
		}
	}

	if lastResult != nil {
		return lastResult
	}
	return map[string]any{"success": false, "error": "渠道测试失败: 未找到支持 OpenAI Images API 的URL"}
}

func isImageProtocolCapabilityMissing(result map[string]any) bool {
	missing, _ := result["protocol_capability_missing"].(bool)
	return missing
}

func (s *Server) testChannelImageGenerationWithURL(
	parent context.Context,
	cfg *model.Config,
	apiKey string,
	imageReq *imageGenerationTestRequest,
	selectedURL string,
) map[string]any {
	start := time.Now()
	modelLookup, _ := imageGenerationModelLookup(cfg, imageReq.Model)
	actualModel := s.resolveFinalUpstreamModel(cfg, modelLookup, util.ProtocolOpenAI)
	if cfg.UsesCodexOAuth() {
		canonicalModel, supported := canonicalCodexImageModel(actualModel)
		if !supported {
			modelErr := codexImageUnsupportedModelError(actualModel)
			result := imageGenerationErrorResult(start, modelErr)
			result["error"] = modelErr.Error()
			return annotateImageGenerationResult(result, actualModel)
		}
		actualModel = canonicalModel
	}
	if cfg.UsesXAIOAuth() && xaiSupportsImageGeneration(actualModel) {
		return s.testXAIResponsesImageGeneration(parent, cfg, apiKey, imageReq, selectedURL, actualModel)
	}
	body, err := imageGenerationRequestBody(cfg, actualModel, imageReq)
	if err != nil {
		result := imageGenerationErrorResult(start, err)
		result["error"] = err.Error()
		return annotateImageGenerationResult(result, actualModel)
	}
	if cfg.UsesXAIOAuth() {
		actualModel, _ = canonicalXAIImageModel(actualModel)
	}
	body = applyBodyRules("application/json", body, cfg.BodyRules())
	actualModel = resolveModelAfterBodyRules(actualModel, cfg.BodyRules())

	ctx, timeout := s.newChannelTestTimeoutContextWithTimeouts(parent, false, s.resolveProtocolTimeouts(protocol.TransformPlan{
		UpstreamProtocol: protocol.OpenAI,
	}))
	defer timeout.cancelAll()

	requestPath := imageGenerationPath
	if cfg.UsesXAIOAuth() || cfg.UsesCodexOAuth() {
		requestPath = directImageGenerationPath
	}
	upstreamURL := buildUpstreamURL(selectedURL, requestPath, "")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		return imageGenerationErrorResult(start, fmt.Errorf("创建HTTP请求失败: %w", err))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", version.OutboundUserAgent())
	if !cfg.UsesOAuth() {
		injectAPIKeyHeaders(req, apiKey, util.ProtocolOpenAI)
	}
	applyHeaderRules(req.Header, cfg.HeaderRules())
	if cfg.UsesCodexOAuth() {
		injectCodexHeaders(req, cfg, apiKey, false)
	} else if cfg.UsesXAIOAuth() {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	// 部分兼容网关会解压响应体却保留 Content-Encoding: gzip。
	// 强制请求 identity，避免 net/http 再次自动解压并报 gzip: invalid header。
	req.Header.Set("Accept-Encoding", "identity")

	debugCapture := s.captureDebugRequest(req, body)
	resp, err := s.doUpstreamRequest(cfg, req)
	if err != nil {
		if debugCapture != nil {
			debugCapture.captureUpstreamError(err)
		}
		result := imageGenerationErrorResult(start, err)
		if errors.Is(err, ErrChannelRPMExceeded) {
			result = channelRPMExceededTestResult(start, channelRPMRetryAfter(err))
		} else if errors.Is(err, ErrChannelConcurrencyExceeded) {
			result = channelConcurrencyExceededTestResult(start, err)
		} else if errors.Is(err, context.DeadlineExceeded) {
			result["status_code"] = http.StatusGatewayTimeout
			result["error"] = "非流式请求超时: " + err.Error()
		} else if errors.Is(err, context.Canceled) {
			result["status_code"] = util.StatusClientClosedRequest
			result["error"] = "客户端已取消请求"
			result["client_canceled"] = true
		}
		if debugCapture != nil {
			result["debug_data"] = debugCapture.buildEntry(nil)
		}
		return annotateImageGenerationResult(result, actualModel)
	}
	if resp == nil || resp.Body == nil {
		return annotateImageGenerationResult(imageGenerationErrorResult(start, errors.New("上游返回空响应")), actualModel)
	}
	defer func() { _ = resp.Body.Close() }()

	firstByteDuration := time.Since(start).Milliseconds()
	if debugCapture != nil {
		debugCapture.captureResponseMeta(resp)
	}
	responseBody, readErr := readLimitedImageGenerationResponse(resp.Body, s.bodyLimits.maxForPath(imageGenerationPath))
	result := map[string]any{
		"success":                false,
		"status_code":            resp.StatusCode,
		"duration_ms":            time.Since(start).Milliseconds(),
		"first_byte_duration_ms": firstByteDuration,
		"is_streaming":           false,
		"client_protocol":        util.ProtocolOpenAI,
		"upstream_protocol":      util.ProtocolOpenAI,
		"response_headers":       flattenHeader(resp.Header),
	}
	if readErr != nil {
		switch {
		case errors.Is(readErr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
			result["status_code"] = http.StatusGatewayTimeout
			result["error"] = "非流式请求超时: " + readErr.Error()
		case errors.Is(readErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
			result["status_code"] = util.StatusClientClosedRequest
			result["error"] = "客户端已取消请求"
			result["client_canceled"] = true
		default:
			result["error"] = readErr.Error()
		}
	} else {
		requestedOutputFormat := imageReq.OutputFormat
		if cfg.UsesXAIOAuth() {
			requestedOutputFormat = ""
		}
		parseImageGenerationResponse(result, resp, responseBody, requestedOutputFormat)
	}
	// 成功响应可能包含数十 MiB 的 base64。不要再复制进诊断字段；
	// 失败响应才需要原始 body 供错误分类和手工排障。
	if success, _ := result["success"].(bool); !success && len(responseBody) > 0 {
		diagnosticBody := imageGenerationDiagnosticBody(responseBody)
		if _, hasStructuredError := result["api_error"]; !hasStructuredError {
			result["raw_response"] = diagnosticBody
		}
		if debugCapture != nil && debugCapture.respBuf != nil {
			_, _ = debugCapture.respBuf.Write([]byte(diagnosticBody))
		}
	}
	result["duration_ms"] = time.Since(start).Milliseconds()
	if debugCapture != nil {
		result["debug_data"] = debugCapture.buildEntry(resp)
	}
	return annotateImageGenerationResult(result, actualModel)
}

func imageGenerationRequestBody(cfg *model.Config, actualModel string, imageReq *imageGenerationTestRequest) ([]byte, error) {
	if cfg != nil && cfg.UsesXAIOAuth() {
		return xaiImageGenerationRequestBody(actualModel, imageReq)
	}
	payload := map[string]any{
		"model":  actualModel,
		"prompt": imageReq.Prompt,
	}
	if imageReq.Size != "" && imageReq.Size != "auto" {
		payload["size"] = imageReq.Size
	}
	if imageReq.Quality != "" && imageReq.Quality != "auto" {
		payload["quality"] = imageReq.Quality
	}
	if imageReq.Background != "" && imageReq.Background != "auto" {
		payload["background"] = imageReq.Background
	}
	if imageReq.OutputFormat != "" && imageReq.OutputFormat != "auto" {
		payload["output_format"] = imageReq.OutputFormat
	}
	return sonic.Marshal(payload)
}

func (s *Server) testXAIResponsesImageGeneration(
	parent context.Context,
	cfg *model.Config,
	apiKey string,
	imageReq *imageGenerationTestRequest,
	selectedURL string,
	actualModel string,
) map[string]any {
	start := time.Now()
	source := map[string]any{"model": actualModel, "prompt": imageReq.Prompt, "stream": true}
	for key, value := range map[string]string{
		"size": imageReq.Size, "quality": imageReq.Quality,
		"background": imageReq.Background, "output_format": imageReq.OutputFormat,
	} {
		if value != "" && value != "auto" {
			source[key] = value
		}
	}
	raw, err := sonic.Marshal(source)
	if err != nil {
		return annotateImageGenerationResult(imageGenerationErrorResult(start, err), actualModel)
	}
	body, err := buildXAIImagesResponsesRequest(raw, actualModel)
	if err != nil {
		result := imageGenerationErrorResult(start, err)
		result["error"] = err.Error()
		return annotateImageGenerationResult(result, actualModel)
	}
	ctx, timeout := s.newChannelTestTimeoutContextWithTimeouts(parent, false, s.resolveProtocolTimeouts(protocol.TransformPlan{
		UpstreamProtocol: protocol.Codex,
	}))
	defer timeout.cancelAll()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, buildUpstreamURL(selectedURL, "/responses", ""), bytes.NewReader(body))
	if err != nil {
		return annotateImageGenerationResult(imageGenerationErrorResult(start, err), actualModel)
	}
	injectXAIAPIResponsesHeaders(req, apiKey)
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := s.doUpstreamRequest(cfg, req)
	if err != nil {
		result := imageGenerationErrorResult(start, err)
		result["error"] = err.Error()
		return annotateImageGenerationResult(result, actualModel)
	}
	if resp == nil || resp.Body == nil {
		return annotateImageGenerationResult(imageGenerationErrorResult(start, errors.New("上游返回空响应")), actualModel)
	}
	defer func() { _ = resp.Body.Close() }()
	result := map[string]any{
		"success": false, "status_code": resp.StatusCode,
		"duration_ms": time.Since(start).Milliseconds(), "first_byte_duration_ms": time.Since(start).Milliseconds(),
		"is_streaming": false, "client_protocol": util.ProtocolOpenAI, "upstream_protocol": util.ProtocolCodex,
		"response_headers": flattenHeader(resp.Header),
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		responseBody, readErr := readLimitedImageGenerationResponse(resp.Body, s.bodyLimits.maxForPath(imageGenerationPath))
		if readErr != nil {
			result["error"] = readErr.Error()
		} else {
			parseImageGenerationResponse(result, resp, responseBody, imageReq.OutputFormat)
		}
		return annotateImageGenerationResult(result, actualModel)
	}
	wrapCodexSSEResponseBody(resp, protocol.Codex, true)
	collector := newCodexNonStreamCollector(newSSEUsageParser(string(protocol.Codex)))
	streamErr := streamTransformSSEEventsUntil(ctx, resp.Body, discardHTTPResponseWriter{}, collector.consume,
		func([]byte) ([][]byte, error) { return nil, nil }, collector.doneForXAIImages)
	if collector.err != nil {
		streamErr = collector.err
	}
	if streamErr != nil {
		result["error"] = streamErr.Error()
		return annotateImageGenerationResult(result, actualModel)
	}
	terminal := collector.patchedTerminal()
	if gjson.GetBytes(terminal, "type").String() != "response.completed" {
		result["error"] = "xAI Responses image generation did not complete"
		return annotateImageGenerationResult(result, actualModel)
	}
	responseBody := gjson.GetBytes(terminal, "response")
	if !responseBody.Exists() {
		result["error"] = "xAI Responses completed event missing response"
		return annotateImageGenerationResult(result, actualModel)
	}
	imagesBody, err := buildOpenAIImagesResponseFromXAIResponses([]byte(responseBody.Raw), raw)
	if err != nil {
		result["error"] = err.Error()
		return annotateImageGenerationResult(result, actualModel)
	}
	parseImageGenerationResponse(result, &http.Response{StatusCode: http.StatusOK, Status: "200 OK"}, imagesBody, imageReq.OutputFormat)
	result["duration_ms"] = time.Since(start).Milliseconds()
	result["first_byte_duration_ms"] = result["duration_ms"]
	return annotateImageGenerationResult(result, actualModel)
}

func canonicalCodexImageModel(raw string) (string, bool) {
	modelName := strings.TrimSpace(raw)
	if slash := strings.LastIndex(modelName, "/"); slash >= 0 && slash < len(modelName)-1 {
		modelName = strings.TrimSpace(modelName[slash+1:])
	}
	switch strings.ToLower(modelName) {
	case "gpt-image-1.5":
		return "gpt-image-1.5", true
	case "gpt-image-2":
		return "gpt-image-2", true
	default:
		return "", false
	}
}

func codexImageUnsupportedModelError(modelName string) error {
	return fmt.Errorf("模型 %s 不受 Codex Images API 支持；可用模型: gpt-image-1.5, gpt-image-2", modelName)
}

func xaiImageGenerationRequestBody(actualModel string, imageReq *imageGenerationTestRequest) ([]byte, error) {
	modelName, ok := canonicalXAIImageModel(actualModel)
	if !ok {
		return nil, xaiImageUnsupportedModelError(actualModel)
	}
	payload := map[string]any{
		"model":           modelName,
		"prompt":          imageReq.Prompt,
		"response_format": "b64_json",
		"aspect_ratio":    xaiImageAspectRatio(imageReq.Size),
		"resolution":      xaiImageResolution(imageReq.Size),
	}
	return sonic.Marshal(payload)
}

func xaiImageUnsupportedModelError(modelName string) error {
	return fmt.Errorf(
		"xAI Images API 不支持模型 %s；可用模型: %s, %s, %s",
		modelName, xaiImageModelDefault, xaiImageModelQuality, xaiImageModel20,
	)
}

func imageGenerationModelLookup(cfg *model.Config, requestedModel string) (string, bool) {
	if cfg == nil {
		return requestedModel, false
	}
	if cfg.SupportsModel(requestedModel) {
		return requestedModel, true
	}
	if cfg.UsesCodexOAuth() {
		canonical, ok := canonicalCodexImageModel(requestedModel)
		if ok && cfg.SupportsModel(canonical) {
			return canonical, true
		}
		return requestedModel, false
	}
	if !cfg.UsesXAIOAuth() {
		return requestedModel, false
	}
	canonical, ok := canonicalXAIImageModel(requestedModel)
	if !ok || !cfg.SupportsModel(canonical) {
		return requestedModel, false
	}
	return canonical, true
}

func canonicalXAIImageModel(raw string) (string, bool) {
	modelName := strings.TrimSpace(raw)
	prefix := ""
	if slash := strings.LastIndex(modelName, "/"); slash >= 0 && slash < len(modelName)-1 {
		prefix = strings.ToLower(strings.TrimSpace(modelName[:slash]))
		modelName = strings.TrimSpace(modelName[slash+1:])
	}
	if prefix != "" && prefix != "xai" && prefix != "x-ai" && prefix != "grok" {
		return "", false
	}
	switch strings.ToLower(modelName) {
	case xaiImageModelDefault:
		return xaiImageModelDefault, true
	case xaiImageModelQuality:
		return xaiImageModelQuality, true
	case xaiImageModel20:
		return xaiImageModel20, true
	default:
		return "", false
	}
}

func xaiImageAspectRatio(size string) string {
	if aspectRatio, _, ok := strings.Cut(strings.ToLower(strings.TrimSpace(size)), "@"); ok {
		return aspectRatio
	}
	switch strings.ToLower(strings.TrimSpace(size)) {
	case "1792x1024", "16:9":
		return "16:9"
	case "1024x1792", "9:16":
		return "9:16"
	case "1536x1024", "3:2":
		return "3:2"
	case "1024x1536", "2:3":
		return "2:3"
	default:
		return "1:1"
	}
}

func xaiImageResolution(size string) string {
	if _, resolution, ok := strings.Cut(strings.ToLower(strings.TrimSpace(size)), "@"); ok {
		return resolution
	}
	if strings.Contains(strings.ToLower(strings.TrimSpace(size)), "2048") {
		return "2k"
	}
	return "1k"
}

func readLimitedImageGenerationResponse(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return body, fmt.Errorf("读取响应失败: %w", err)
	}
	if int64(len(body)) > limit {
		return body[:limit], fmt.Errorf("生图响应超过 %d 字节上限", limit)
	}
	return body, nil
}

func imageGenerationDiagnosticBody(body []byte) string {
	if len(body) <= imageGenerationDiagnosticBodyLimit {
		return string(body)
	}
	return string(body[:imageGenerationDiagnosticBodyLimit]) + "\n... response truncated"
}

func parseImageGenerationResponse(
	result map[string]any,
	resp *http.Response,
	body []byte,
	requestedOutputFormat string,
) {
	var apiResponse map[string]any
	if err := sonic.Unmarshal(body, &apiResponse); err != nil {
		result["error"] = "上游返回了无效的 JSON 响应"
		return
	}
	if rawError, exists := apiResponse["error"]; exists && rawError != nil {
		message := extractTestAPIErrorMessage(apiResponse)
		if message == "" {
			message = "上游返回了结构化错误"
		}
		result["error"] = message
		result["api_error"] = apiResponse
		return
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		message := extractTestAPIErrorMessage(apiResponse)
		if message == "" {
			message = "API返回错误状态: " + resp.Status
		}
		result["error"] = message
		result["api_error"] = apiResponse
		return
	}

	images := extractImageGenerationData(apiResponse)
	if len(images) == 0 {
		result["error"] = "上游响应中没有可显示图片"
		return
	}
	result["success"] = true
	result["message"] = "图片生成成功"
	result["images"] = images
	for _, key := range []string{"created", "background", "output_format", "quality", "size", "usage"} {
		if value, ok := apiResponse[key]; ok {
			result[key] = value
		}
	}
	if _, ok := result["output_format"]; !ok && requestedOutputFormat != "" && requestedOutputFormat != "auto" {
		result["output_format"] = requestedOutputFormat
	}
	if _, ok := result["output_format"]; !ok {
		if outputFormat := imageOutputFormatFromMIMEType(getResultString(images[0], "mime_type")); outputFormat != "" {
			result["output_format"] = outputFormat
		}
	}
}

func extractImageGenerationData(apiResponse map[string]any) []map[string]any {
	rawImages, ok := apiResponse["data"].([]any)
	if !ok {
		return nil
	}
	images := make([]map[string]any, 0, len(rawImages))
	for _, rawImage := range rawImages {
		image, ok := rawImage.(map[string]any)
		if !ok {
			continue
		}
		url, _ := image["url"].(string)
		base64JSON, _ := image["b64_json"].(string)
		if strings.TrimSpace(url) == "" && strings.TrimSpace(base64JSON) == "" {
			continue
		}
		normalized := map[string]any{}
		if url != "" {
			normalized["url"] = url
		}
		if base64JSON != "" {
			normalized["b64_json"] = base64JSON
			if mimeType := imageMIMETypeFromBase64(base64JSON); mimeType != "" {
				normalized["mime_type"] = mimeType
			}
		}
		if revisedPrompt, _ := image["revised_prompt"].(string); revisedPrompt != "" {
			normalized["revised_prompt"] = revisedPrompt
		}
		images = append(images, normalized)
	}
	return images
}

func imageMIMETypeFromBase64(encoded string) string {
	decoder := base64.NewDecoder(base64.StdEncoding, strings.NewReader(strings.TrimSpace(encoded)))
	header := make([]byte, 16)
	n, err := decoder.Read(header)
	if n == 0 || (err != nil && !errors.Is(err, io.EOF)) {
		return ""
	}
	switch http.DetectContentType(header[:n]) {
	case "image/png":
		return "image/png"
	case "image/jpeg":
		return "image/jpeg"
	case "image/webp":
		return "image/webp"
	case "image/gif":
		return "image/gif"
	default:
		return ""
	}
}

func imageGenerationErrorResult(start time.Time, err error) map[string]any {
	return map[string]any{
		"success":           false,
		"error":             "网络请求失败: " + err.Error(),
		"duration_ms":       time.Since(start).Milliseconds(),
		"is_streaming":      false,
		"client_protocol":   util.ProtocolOpenAI,
		"upstream_protocol": util.ProtocolOpenAI,
	}
}

func annotateImageGenerationResult(result map[string]any, actualModel string) map[string]any {
	result["actual_model"] = actualModel
	result["client_protocol"] = util.ProtocolOpenAI
	result["upstream_protocol"] = util.ProtocolOpenAI
	result["is_streaming"] = false
	return result
}
