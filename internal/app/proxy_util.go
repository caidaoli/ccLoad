package app

import (
	"bytes"
	"ccLoad/internal/config"
	"ccLoad/internal/model"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bytedance/sonic"
)

// ============================================================================
// 常量定义
// ============================================================================

// 错误类型常量定义
const (
	// HTTP状态码扩展（用于日志记录和监控）
	StatusClientClosedRequest = 499 // 客户端取消请求 (Nginx扩展状态码)
	StatusConnectionReset     = 502 // Connection Reset - 不可重试
	StatusFirstByteTimeout    = 598 // 首字节超时 (自定义状态码，用于触发固定5分钟冷却)

	// ✅ P2-3 修复：内部错误标识符使用负值，避免与HTTP状态码混淆
	// 这些标识符仅用于内部错误分类，不会直接返回给客户端
	ErrCodeNetworkRetryable = -1 // 可重试的网络错误（DNS错误、连接超时等）

	// ✅ P0修复（2025-10-13）：提取常量消除魔法数字
	StreamBufferSize = 32 * 1024 // 流式传输缓冲区大小（32KB，用于大文件传输）

	// ✅ SSE优化（2025-10-17）：SSE专用小缓冲区，降低首Token延迟
	SSEBufferSize = 4 * 1024 // SSE流式传输缓冲区（4KB，优化实时响应）
)

// ============================================================================
// 类型定义
// ============================================================================

// fwResult 转发结果
type fwResult struct {
	Status        int
	Header        http.Header
	Body          []byte         // filled for non-2xx or when needed
	Resp          *http.Response // non-nil only when Status is 2xx to support streaming
	FirstByteTime float64        // 首字节响应时间（秒）
}

// proxyRequestContext 代理请求上下文（封装请求信息，遵循DIP原则）
type proxyRequestContext struct {
	originalModel string
	requestMethod string
	requestPath   string
	rawQuery      string
	body          []byte
	header        http.Header
	isStreaming   bool
}

// proxyResult 代理请求结果
type proxyResult struct {
	status    int
	header    http.Header
	body      []byte
	channelID *int64
	message   string
	duration  float64
	succeeded bool
}

// ✅ P2重构：ErrorAction 已迁移到 cooldown.Action (internal/cooldown/manager.go)
// 统一使用 cooldown.Action 类型，遵循DRY原则

// ============================================================================
// 请求检测工具函数
// ============================================================================

// isGeminiRequest 检测是否为Gemini API请求
// Gemini请求路径特征：包含 /v1beta/ 前缀
// 示例：/v1beta/models/gemini-2.5-flash:streamGenerateContent
func isGeminiRequest(path string) bool {
	return strings.Contains(path, "/v1beta/")
}

// isOpenAIRequest 检测是否为OpenAI API请求
// OpenAI请求路径特征：/v1/chat/completions, /v1/completions, /v1/embeddings 等
// 示例：/v1/chat/completions
func isOpenAIRequest(path string) bool {
	return strings.HasPrefix(path, "/v1/chat/completions") ||
		strings.HasPrefix(path, "/v1/completions") ||
		strings.HasPrefix(path, "/v1/embeddings")
}

// isAnthropicRequest 检测是否为Claude/Anthropic API请求
// 典型路径：/v1/messages、/v1/messages/count_tokens
func isAnthropicRequest(path string) bool {
	return strings.HasPrefix(path, "/v1/messages")
}

// isCodexRequest 检测是否为Codex兼容API请求
// 典型路径：/v1/responses
func isCodexRequest(path string) bool {
	return strings.HasPrefix(path, "/v1/responses")
}

// detectChannelTypeFromPath 根据请求路径推断渠道类型
// 返回空字符串表示未识别出特定渠道类型，沿用默认逻辑
func detectChannelTypeFromPath(path string) string {
	switch {
	case isGeminiRequest(path):
		return "gemini"
	case isOpenAIRequest(path):
		return "openai"
	case isAnthropicRequest(path):
		return "anthropic"
	case isCodexRequest(path):
		return "codex"
	default:
		return ""
	}
}

// isStreamingRequest 检测是否为流式请求
// 支持多种API的流式标识方式：
// - Gemini: 路径包含 :streamGenerateContent
// - Claude/OpenAI: 请求体中 stream=true
func isStreamingRequest(path string, body []byte) bool {
	// Gemini流式请求特征：路径包含 :streamGenerateContent
	if strings.Contains(path, ":streamGenerateContent") {
		return true
	}

	// Claude/OpenAI流式请求特征：请求体中 stream=true
	var reqModel struct {
		Stream bool `json:"stream"`
	}
	_ = sonic.Unmarshal(body, &reqModel)
	return reqModel.Stream
}

// ============================================================================
// 错误分类工具函数
// ============================================================================

// classifyError 分类错误类型，返回状态码和是否应该重试
// ✅ P0修复（2025-10-13）：移除缓存机制，简化逻辑（KISS原则）
// 性能优化：快速路径 + 类型断言，99%的错误在快速路径返回
func classifyError(err error) (statusCode int, shouldRetry bool) {
	if err == nil {
		return 200, false
	}

	// 快速路径1：优先检查最常见的错误类型（避免字符串操作）
	// Context canceled - 客户端主动取消，不应重试（最常见）
	if errors.Is(err, context.Canceled) {
		return StatusClientClosedRequest, false
	}

	// ⚠️ Context deadline exceeded 需要区分两种情况：
	// 1. 客户端超时（来自客户端设置的超时）- 不应重试
	// 2. 上游服务器响应慢导致的超时 - 应该重试其他渠道
	// ✅ P0修复 (2025-10-13): 默认将DeadlineExceeded视为上游超时（可重试）
	// 设计原则：
	// - 客户端主动取消通常是context.Canceled，而不是DeadlineExceeded
	// - 保守策略：宁可多重试（提升可用性），也不要漏掉上游超时（导致可用性下降）
	// - 兼容性：不依赖特定的错误消息格式，适配Go不同版本和HTTP客户端实现
	if errors.Is(err, context.DeadlineExceeded) {
		// 所有DeadlineExceeded错误默认为上游超时，应该重试其他渠道
		return 504, true // ✅ Gateway Timeout，触发渠道切换
	}

	// 快速路径2：检查系统级错误（使用类型断言替代字符串匹配）
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return 504, false // Gateway Timeout
		}
	}

	// 慢速路径：字符串匹配（<1%的错误会到达这里）
	return classifyErrorByString(err.Error())
}

// classifyErrorByString 通过字符串匹配分类网络错误
// ✅ P0修复（2025-10-13）：提取独立函数，遵循SRP原则
func classifyErrorByString(errStr string) (int, bool) {
	errLower := strings.ToLower(errStr)

	// Connection reset by peer - 不应重试
	if strings.Contains(errLower, "connection reset by peer") ||
		strings.Contains(errLower, "broken pipe") {
		return StatusConnectionReset, false
	}

	// Connection refused - 应该重试其他渠道
	if strings.Contains(errLower, "connection refused") {
		return 502, true
	}

	// 其他常见的网络连接错误也应该重试
	if strings.Contains(errLower, "no such host") ||
		strings.Contains(errLower, "host unreachable") ||
		strings.Contains(errLower, "network unreachable") ||
		strings.Contains(errLower, "connection timeout") ||
		strings.Contains(errLower, "no route to host") {
		return 502, true
	}

	// ✅ P2-3 修复：使用负值错误码，避免与HTTP状态码混淆
	// 其他网络错误 - 可以重试
	return ErrCodeNetworkRetryable, true
}

// ============================================================================
// URL和请求构建工具函数
// ============================================================================

// buildUpstreamURL 构建上游完整URL（KISS）
func buildUpstreamURL(cfg *model.Config, requestPath, rawQuery string) string {
	upstreamURL := strings.TrimRight(cfg.URL, "/") + requestPath
	if rawQuery != "" {
		upstreamURL += "?" + rawQuery
	}
	return upstreamURL
}

// buildUpstreamRequest 创建带上下文的HTTP请求
func buildUpstreamRequest(ctx context.Context, method, upstreamURL string, body []byte) (*http.Request, error) {
	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	u, err := neturl.Parse(upstreamURL)
	if err != nil {
		return nil, err
	}
	return http.NewRequestWithContext(ctx, method, u.String(), bodyReader)
}

// copyRequestHeaders 复制请求头，跳过认证相关（DRY）
func copyRequestHeaders(dst *http.Request, src http.Header) {
	for k, vs := range src {
		// 不透传认证头（由上游注入）
		if strings.EqualFold(k, "Authorization") || strings.EqualFold(k, "X-Api-Key") {
			continue
		}
		// 不透传 Accept-Encoding，避免上游返回 br/gzip 压缩导致错误体乱码
		// 让 Go Transport 自动设置并透明解压 gzip（DisableCompression=false）
		if strings.EqualFold(k, "Accept-Encoding") {
			continue
		}
		for _, v := range vs {
			dst.Header.Add(k, v)
		}
	}
	if dst.Header.Get("Accept") == "" {
		dst.Header.Set("Accept", "application/json")
	}
}

// injectAPIKeyHeaders 按路径类型注入API Key头（Gemini vs Claude）
// 参数简化：直接接受API Key字符串，由调用方从KeySelector获取
func injectAPIKeyHeaders(req *http.Request, apiKey string, requestPath string) {
	// 根据API类型设置不同的认证头
	if isGeminiRequest(requestPath) {
		// Gemini API: 仅使用 x-goog-api-key
		req.Header.Set("x-goog-api-key", apiKey)
	} else if isOpenAIRequest(requestPath) {
		// OpenAI API: 仅使用 Authorization Bearer
		req.Header.Set("Authorization", "Bearer "+apiKey)
	} else {
		// Claude/Anthropic API: 同时设置两个头
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
}

// filterAndWriteResponseHeaders 过滤并写回响应头（DRY）
func filterAndWriteResponseHeaders(w http.ResponseWriter, hdr http.Header) {
	for k, vs := range hdr {
		// 过滤不应向客户端透传的头
		if strings.EqualFold(k, "Connection") ||
			strings.EqualFold(k, "Content-Length") ||
			strings.EqualFold(k, "Transfer-Encoding") ||
			strings.EqualFold(k, "Content-Encoding") { // 避免上游压缩头与实际解压后的body不一致
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
}

// ============================================================================
// 模型和路径解析工具函数
// ============================================================================

// extractModelFromPath 从URL路径中提取模型名称
// 支持格式：/models/{model}:method 或 /models/{model}
func extractModelFromPath(path string) string {
	// 查找 "/models/" 子串
	modelsPrefix := "/models/"
	idx := strings.Index(path, modelsPrefix)
	if idx == -1 {
		return ""
	}

	// 提取 "/models/" 之后的部分
	start := idx + len(modelsPrefix)
	remaining := path[start:]

	// 查找模型名称的结束位置（遇到 : 或 / 或字符串结尾）
	end := len(remaining)
	for i, ch := range remaining {
		if ch == ':' || ch == '/' {
			end = i
			break
		}
	}

	return remaining[:end]
}

// prepareRequestBody 准备请求体（处理模型重定向）
// 遵循SRP原则：单一职责 - 仅负责模型重定向和请求体准备
func prepareRequestBody(cfg *model.Config, reqCtx *proxyRequestContext) (actualModel string, bodyToSend []byte) {
	actualModel = reqCtx.originalModel

	// 检查模型重定向
	if len(cfg.ModelRedirects) > 0 {
		if redirectModel, ok := cfg.ModelRedirects[reqCtx.originalModel]; ok && redirectModel != "" {
			actualModel = redirectModel
		}
	}

	bodyToSend = reqCtx.body

	// 如果模型发生重定向，修改请求体
	if actualModel != reqCtx.originalModel {
		var reqData map[string]any
		if err := sonic.Unmarshal(reqCtx.body, &reqData); err == nil {
			reqData["model"] = actualModel
			if modifiedBody, err := sonic.Marshal(reqData); err == nil {
				bodyToSend = modifiedBody
			}
		}
	}

	return actualModel, bodyToSend
}

// ============================================================================
// 日志和字符串处理工具函数
// ============================================================================

// buildLogEntry 构建日志条目（消除重复代码，遵循DRY原则）
func buildLogEntry(originalModel string, channelID *int64, statusCode int,
	duration float64, isStreaming bool, apiKeyUsed string,
	res *fwResult, errMsg string) *model.LogEntry {

	entry := &model.LogEntry{
		Time:        model.JSONTime{Time: time.Now()},
		Model:       originalModel,
		ChannelID:   channelID,
		StatusCode:  statusCode,
		Duration:    duration,
		IsStreaming: isStreaming,
		APIKeyUsed:  apiKeyUsed,
	}

	if errMsg != "" {
		entry.Message = truncateErr(errMsg)
	} else if res != nil {
		if statusCode >= 200 && statusCode < 300 {
			entry.Message = "ok"
		} else {
			msg := fmt.Sprintf("upstream status %d", statusCode)
			if len(res.Body) > 0 {
				msg = fmt.Sprintf("%s: %s", msg, truncateErr(safeBodyToString(res.Body)))
			}
			entry.Message = msg
		}

		// 流式请求记录首字节响应时间
		if isStreaming && res.FirstByteTime > 0 {
			entry.FirstByteTime = &res.FirstByteTime
		}
	} else {
		entry.Message = "unknown"
	}

	return entry
}

// truncateErr 截断错误信息到指定长度
func truncateErr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > config.LogErrorTruncateLength {
		return s[:config.LogErrorTruncateLength]
	}
	return s
}

// safeBodyToString 安全地将响应体转换为字符串，处理可能的gzip压缩
func safeBodyToString(data []byte) string {
	if len(data) == 0 {
		return ""
	}

	// 检查gzip魔数 (0x1f, 0x8b)
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		// 尝试解压gzip
		if decompressed, err := decompressGzip(data); err == nil {
			return string(decompressed)
		}
		// 解压失败，返回友好提示
		return "[compressed error response]"
	}

	// 非压缩数据，直接转换
	return string(data)
}

// decompressGzip 解压gzip数据
func decompressGzip(data []byte) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	return io.ReadAll(reader)
}

// ============================================================================
// 超时和参数解析工具函数
// ============================================================================

// parseTimeout 从query参数或header中解析超时时间
func parseTimeout(q map[string][]string, h http.Header) time.Duration {
	// 优先 query: timeout_ms / timeout_s
	if v := first(q, "timeout_ms"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	if v := first(q, "timeout_s"); v != "" {
		if sec, err := strconv.Atoi(v); err == nil && sec > 0 {
			return time.Duration(sec) * time.Second
		}
	}
	// header 兜底
	if v := h.Get("x-timeout-ms"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	if v := h.Get("x-timeout-s"); v != "" {
		if sec, err := strconv.Atoi(v); err == nil && sec > 0 {
			return time.Duration(sec) * time.Second
		}
	}
	return 0
}

// first 获取query参数的第一个值
func first(q map[string][]string, k string) string {
	if vs, ok := q[k]; ok && len(vs) > 0 {
		return vs[0]
	}
	return ""
}

// ============================================================================
// Gemini相关工具函数
// ============================================================================

// formatModelDisplayName 将模型ID转换为友好的显示名称
func formatModelDisplayName(modelID string) string {
	// 简单的格式化：移除日期后缀，首字母大写
	// 例如：gemini-2.5-flash → Gemini 2.5 Flash
	parts := strings.Split(modelID, "-")
	var words []string
	for _, part := range parts {
		// 跳过日期格式（8位数字）
		if len(part) == 8 && isNumeric(part) {
			continue
		}
		// 首字母大写
		if len(part) > 0 {
			words = append(words, strings.ToUpper(string(part[0]))+part[1:])
		}
	}
	return strings.Join(words, " ")
}

// isNumeric 检查字符串是否全是数字
func isNumeric(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
