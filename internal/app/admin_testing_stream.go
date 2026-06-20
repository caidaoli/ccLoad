package app

import (
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/testutil"

	"github.com/bytedance/sonic"
	"github.com/gin-gonic/gin"
)

// HandleChannelChat 对话端点：流式上游实时透传，非流式上游归一化为前端 SSE。
// POST /admin/channels/:id/chat
// 请求体与 /test 完全一致，stream=false 时上游走非流式请求。
// 响应始终为 text/event-stream，每条事件包含 delta 文本，结束时发送 [DONE]。
// 错误时发送 data: {"error":"..."} 事件。
func (s *Server) HandleChannelChat(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		writeChatErrorEvent(c, "invalid channel id")
		return
	}

	var testReq testutil.TestChannelRequest
	if err := BindAndValidate(c, &testReq); err != nil {
		writeChatErrorEvent(c, "invalid request")
		return
	}

	cfg, err := s.store.GetConfig(c.Request.Context(), id)
	if err != nil {
		writeChatErrorEvent(c, "channel not found")
		return
	}

	apiKeys, err := s.store.GetAPIKeys(c.Request.Context(), id)
	if err != nil {
		writeChatErrorEvent(c, "failed to load api keys")
		return
	}
	requestAPIKey := strings.TrimSpace(testReq.APIKey)
	if len(apiKeys) == 0 && requestAPIKey == "" {
		writeChatErrorEvent(c, "渠道未配置有效的 API Key")
		return
	}

	keySelection, err := s.selectChannelTestKey(apiKeys, testReq.KeyIndex, requestAPIKey)
	if err != nil {
		writeChatErrorEvent(c, err.Error())
		return
	}

	if !cfg.SupportsModel(testReq.Model) {
		writeChatErrorEvent(c, "模型 "+testReq.Model+" 不在此渠道的支持列表中")
		return
	}

	if strings.TrimSpace(testReq.Content) == "" && len(testReq.Messages) == 0 {
		testReq.Content = s.configService.GetString("channel_test_content", "sonnet 4.0的发布日期是什么")
	}

	// 模型重定向
	if redirectModel, ok := cfg.GetRedirectModel(testReq.Model); ok && redirectModel != "" {
		testReq.Model = redirectModel
	}

	clientProtocol := resolveClientProtocol(cfg, &testReq)
	upstreamProto := resolveTestUpstreamProtocol(cfg, clientProtocol)
	if !supportsRuntimeTestProtocol(clientProtocol, upstreamProto) {
		writeChatErrorEvent(c, fmt.Sprintf("不支持协议转换 %s -> %s", clientProtocol, upstreamProto))
		return
	}

	urls := cfg.GetURLs()
	if len(urls) == 0 {
		writeChatErrorEvent(c, "渠道URL为空")
		return
	}

	var selector *URLSelector
	if len(urls) > 1 && s.urlSelector != nil {
		selector = s.urlSelector
	}
	orderedURLs := orderURLsWithSelector(selector, cfg.ID, urls)

	// 设置 SSE 响应头
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)

	for idx, entry := range orderedURLs {
		ok := s.streamChatWithURL(c, cfg, keySelection.apiKey, &testReq, clientProtocol, entry.url)
		if ok {
			if selector != nil {
				selector.RecordLatency(cfg.ID, entry.url, time.Millisecond)
			}
			return
		}
		if idx == len(orderedURLs)-1 {
			break
		}
		if selector != nil {
			selector.CooldownURL(cfg.ID, entry.url)
		}
	}
}

// streamChatWithURL 对单个 URL 发起对话请求并写入前端 SSE。
// 返回 true 表示已成功写入响应（无论是否出错），false 表示应 fallback 到下一个 URL。
func (s *Server) streamChatWithURL(
	c *gin.Context,
	cfg *model.Config,
	apiKey string,
	testReq *testutil.TestChannelRequest,
	clientProtocol, selectedURL string,
) bool {
	req, requestPlan, cancel, err := s.buildTestUpstreamRequest(c.Request.Context(), cfg, apiKey, testReq, clientProtocol, selectedURL)
	if err != nil {
		writeChatErrorEvent(c, err.Error())
		return true
	}
	defer cancel()

	start := time.Now()
	resp, err := s.doUpstreamRequest(cfg, req)
	if err != nil {
		// 网络层错误，尝试 fallback
		return false
	}
	defer func() { _ = resp.Body.Close() }()

	contentType := resp.Header.Get("Content-Type")
	isSSE := strings.Contains(strings.ToLower(contentType), "text/event-stream")

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		msg := extractChatUpstreamError(resp.StatusCode, body)
		writeChatErrorEvent(c, msg)
		return true
	}

	if !isSSE {
		s.streamChatNonStreamResponse(c, resp, requestPlan, testReq, contentType, start)
		return true
	}

	requestPlan.timeout.markFirstStreamContent()

	if clientProtocol == requestPlan.upstreamProtocol {
		// 原生协议：直接透传 SSE，提取 delta 文本
		streamChatNative(c, resp.Body)
	} else {
		// 协议转换：先翻译再透传
		streamChatTranslated(c, resp, requestPlan, testReq, s)
	}
	return true
}

func (s *Server) streamChatNonStreamResponse(
	c *gin.Context,
	resp *http.Response,
	requestPlan *channelTestRequestPlan,
	testReq *testutil.TestChannelRequest,
	contentType string,
	start time.Time,
) {
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		errorMsg := "读取响应失败: " + err.Error()
		if _, timeoutMsg, ok := s.describeChannelTestTimeoutError(start, testReq, requestPlan.timeout, err); ok {
			errorMsg = timeoutMsg
		}
		writeChatErrorEvent(c, errorMsg)
		return
	}

	result := map[string]any{
		"success":     resp.StatusCode >= 200 && resp.StatusCode < 300,
		"status_code": resp.StatusCode,
	}
	result = s.parseTestNonStreamResponse(c.Request.Context(), requestPlan, testReq, resp, contentType, start, respBody, result)
	writeChatNonStreamResult(c, result)
}

func writeChatNonStreamResult(c *gin.Context, result map[string]any) {
	if success, ok := result["success"].(bool); ok && !success {
		msg, _ := result["error"].(string)
		if msg == "" {
			msg = "上游返回错误"
		}
		writeChatErrorEvent(c, msg)
		return
	}

	responseText, _ := result["response_text"].(string)
	if responseText == "" {
		msg, _ := result["error"].(string)
		if msg == "" {
			msg = "上游响应中没有可显示文本"
		}
		writeChatErrorEvent(c, msg)
		return
	}

	state := &chatFrontendStreamState{}
	writeChatFrontendChunks(c, chatChunksFromTextDelta(responseText, state)...)
	writeChatFrontendChunks(c, chatDoneEventChunk())
}

// streamChatNative 原生协议时把上游 SSE 实时透传给前端（提取 delta 文本）。
func streamChatNative(c *gin.Context, body io.Reader) {
	frontendState := &chatFrontendStreamState{}
	_ = streamTransformSSEEvents(c.Request.Context(), body, c.Writer, nil,
		func(rawEvent []byte) ([][]byte, error) {
			return chatFrontendChunksFromSSEEventWithState(rawEvent, frontendState), nil
		},
	)
}

// streamChatTranslated 协议转换时：翻译 SSE 事件后再提取 delta 写给前端。
func streamChatTranslated(c *gin.Context, resp *http.Response, requestPlan *channelTestRequestPlan, testReq *testutil.TestChannelRequest, s *Server) {
	var state any
	frontendState := &chatFrontendStreamState{}
	ctx := c.Request.Context()

	// anyrouter 注入
	if requestPlan.upstreamProtocol == "anthropic" {
		if parsed, err := neturl.Parse(requestPlan.fullURL); err == nil && strings.HasSuffix(parsed.Path, "/v1/messages") {
			requestPlan.requestBody = maybeInjectAnyrouterAdaptiveThinking(&model.Config{}, "/v1/messages", requestPlan.requestBody)
		}
	}

	src := readerWithCloser{Reader: resp.Body, Closer: resp.Body}
	_ = streamTransformSSEEvents(ctx, src, c.Writer,
		nil,
		func(rawEvent []byte) ([][]byte, error) {
			translated, err := s.protocolRegistry.TranslateResponseStream(
				ctx,
				protocol.Protocol(requestPlan.upstreamProtocol),
				protocol.Protocol(requestPlan.clientProtocol),
				testReq.Model,
				requestPlan.clientBody,
				requestPlan.requestBody,
				rawEvent,
				&state,
			)
			if err != nil {
				return nil, err
			}
			var chunks [][]byte
			for _, chunk := range translated {
				chunks = append(chunks, chatFrontendChunksFromSSEEventWithState(chunk, frontendState)...)
			}
			return chunks, nil
		},
	)
}

func chatFrontendChunksFromSSEEvent(rawEvent []byte) [][]byte {
	return chatFrontendChunksFromSSEEventWithState(rawEvent, nil)
}

type chatFrontendStreamState struct {
	thinkTagOpen bool
}

func chatFrontendChunksFromSSEEventWithState(rawEvent []byte, state *chatFrontendStreamState) [][]byte {
	lines := strings.Split(string(rawEvent), "\n")
	chunks := make([][]byte, 0, 1)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			chunks = append(chunks, chatDoneEventChunk())
			continue
		}

		var obj map[string]any
		if err := sonic.Unmarshal([]byte(payload), &obj); err != nil {
			continue
		}
		if thinking := extractSSEThinkingDelta(obj); thinking != "" {
			chunks = append(chunks, chatThinkingEventChunk(thinking))
			continue
		}
		if delta := extractSSEDeltaText(obj); delta != "" {
			chunks = append(chunks, chatChunksFromTextDelta(delta, state)...)
			continue
		}
		if isChatStopEvent(obj) {
			chunks = append(chunks, chatDoneEventChunk())
			continue
		}
		if errMsg, _, matched := extractSSEErrorMessage(obj); matched {
			if errMsg == "" {
				errMsg = "上游返回错误"
			}
			chunks = append(chunks, chatErrorEventChunk(errMsg))
		}
	}
	return chunks
}

func chatChunksFromTextDelta(delta string, state *chatFrontendStreamState) [][]byte {
	if state == nil {
		if thinking, text := splitThinkTaggedText(delta); thinking != "" {
			chunks := [][]byte{chatThinkingEventChunk(thinking)}
			if text != "" {
				chunks = append(chunks, chatDeltaEventChunk(text))
			}
			return chunks
		}
		return [][]byte{chatDeltaEventChunk(delta)}
	}

	chunks := make([][]byte, 0, 1)
	remaining := delta
	for remaining != "" {
		if state.thinkTagOpen {
			closeIdx, closeLen := findThinkCloseTag(remaining)
			if closeIdx < 0 {
				chunks = appendNonEmptyThinkingChunk(chunks, remaining)
				return chunks
			}
			chunks = appendNonEmptyThinkingChunk(chunks, remaining[:closeIdx])
			remaining = remaining[closeIdx+closeLen:]
			state.thinkTagOpen = false
			continue
		}

		openIdx, openLen := findThinkOpenTag(remaining)
		if openIdx < 0 {
			chunks = appendNonEmptyDeltaChunk(chunks, remaining)
			return chunks
		}
		chunks = appendNonEmptyDeltaChunk(chunks, remaining[:openIdx])
		remaining = remaining[openIdx+openLen:]
		state.thinkTagOpen = true
	}
	return chunks
}

func appendNonEmptyThinkingChunk(chunks [][]byte, text string) [][]byte {
	if text == "" {
		return chunks
	}
	return append(chunks, chatThinkingEventChunk(text))
}

func appendNonEmptyDeltaChunk(chunks [][]byte, text string) [][]byte {
	if text == "" {
		return chunks
	}
	return append(chunks, chatDeltaEventChunk(text))
}

func findThinkOpenTag(text string) (idx int, length int) {
	return findFirstTag(text, []string{"<think>", "<thinking>"})
}

func findThinkCloseTag(text string) (idx int, length int) {
	return findFirstTag(text, []string{"</think>", "</thinking>"})
}

func findFirstTag(text string, tags []string) (idx int, length int) {
	bestIdx := -1
	bestLen := 0
	for _, tag := range tags {
		pos := strings.Index(text, tag)
		if pos < 0 {
			continue
		}
		if bestIdx < 0 || pos < bestIdx {
			bestIdx = pos
			bestLen = len(tag)
		}
	}
	return bestIdx, bestLen
}

func extractSSEThinkingDelta(obj map[string]any) string {
	if choices, ok := obj["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if delta, ok := choice["delta"].(map[string]any); ok {
				if reasoning, ok := delta["reasoning_content"].(string); ok && reasoning != "" {
					return reasoning
				}
			}
		}
	}

	if candidates, ok := obj["candidates"].([]any); ok && len(candidates) > 0 {
		if candidate, ok := candidates[0].(map[string]any); ok {
			if content, ok := candidate["content"].(map[string]any); ok {
				if parts, ok := content["parts"].([]any); ok && len(parts) > 0 {
					if part, ok := parts[0].(map[string]any); ok {
						if thought, _ := part["thought"].(bool); thought {
							if text, ok := part["text"].(string); ok && text != "" {
								return text
							}
						}
					}
				}
			}
		}
	}

	if typ, _ := obj["type"].(string); typ == "content_block_delta" {
		if delta, ok := obj["delta"].(map[string]any); ok {
			if thinking, ok := delta["thinking"].(string); ok && thinking != "" {
				return thinking
			}
		}
	}
	if typ, _ := obj["type"].(string); typ == "response.reasoning_summary_text.delta" {
		if delta, ok := obj["delta"].(string); ok && delta != "" {
			return delta
		}
	}
	return ""
}

func splitThinkTaggedText(text string) (thinking string, answer string) {
	trimmed := strings.TrimSpace(text)
	for _, tag := range []string{"think", "thinking"} {
		openTag := "<" + tag + ">"
		closeTag := "</" + tag + ">"
		if !strings.HasPrefix(trimmed, openTag) || !strings.Contains(trimmed, closeTag) {
			continue
		}
		end := strings.Index(trimmed, closeTag)
		if end < 0 {
			continue
		}
		thinking = strings.TrimSpace(trimmed[len(openTag):end])
		answer = strings.TrimSpace(trimmed[end+len(closeTag):])
		return thinking, answer
	}
	return "", text
}

func isChatStopEvent(obj map[string]any) bool {
	typ, _ := obj["type"].(string)
	return typ == "message_stop" || typ == "response.completed"
}

func writeChatFrontendChunks(c *gin.Context, chunks ...[]byte) {
	for _, chunk := range chunks {
		if len(chunk) == 0 {
			continue
		}
		if _, err := c.Writer.Write(chunk); err != nil {
			return
		}
		c.Writer.Flush()
	}
}

func chatThinkingEventChunk(thinking string) []byte {
	return []byte("data: " + jsonMustMarshalString(map[string]any{"thinking_delta": thinking}) + "\n\n")
}

func chatDeltaEventChunk(delta string) []byte {
	return []byte("data: " + jsonMustMarshalString(map[string]any{"delta": delta}) + "\n\n")
}

func chatDoneEventChunk() []byte {
	return []byte("data: [DONE]\n\n")
}

func chatErrorEventChunk(msg string) []byte {
	return []byte("data: " + jsonMustMarshalString(map[string]any{"error": msg}) + "\n\n")
}

// writeChatErrorEvent 写错误事件并刷新（通过 gin.Context，尚未写 SSE 头时也能用）。
func writeChatErrorEvent(c *gin.Context, msg string) {
	// 若 SSE 头已写出，直接用 ResponseWriter；否则先写头
	w := c.Writer
	if !c.Writer.Written() {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("X-Accel-Buffering", "no")
		c.Status(http.StatusOK)
	}
	writeChatErrorEventWriter(w, msg)
}

func writeChatErrorEventWriter(w http.ResponseWriter, msg string) {
	_, _ = w.Write(chatErrorEventChunk(msg))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// jsonMustMarshalString 序列化为 JSON 字符串，失败返回空对象字符串。
func jsonMustMarshalString(v any) string {
	b, err := sonic.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// extractChatUpstreamError 从非流式错误响应提取可读消息。
func extractChatUpstreamError(statusCode int, body []byte) string {
	if len(body) > 0 {
		var obj map[string]any
		if err := sonic.Unmarshal(body, &obj); err == nil {
			if msg := extractTestAPIErrorMessage(obj); msg != "" {
				return msg
			}
		}
		if snippet := strings.TrimSpace(string(body)); len(snippet) > 0 && len(snippet) <= 300 {
			return snippet
		}
	}
	return fmt.Sprintf("上游返回错误 HTTP %d", statusCode)
}
