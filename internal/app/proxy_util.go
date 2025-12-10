package app

import (
	"bytes"
	"ccLoad/internal/model"
	"ccLoad/internal/util"
	"compress/gzip"
	"context"
	"fmt"
	"io"
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
	// 499状态码有两种来源，分别处理
	// 1. 下游客户端取消（context.Canceled）→ ErrorLevelClient，不重试
	// 2. 上游API返回HTTP 499响应 → ErrorLevelChannel，重试其他渠道
	// 分类逻辑详见 internal/util/classifier.go
	StatusClientClosedRequest = 499                         // 客户端取消请求 (Nginx扩展状态码)
	StatusConnectionReset     = 502                         // Connection Reset - 不可重试
	StatusFirstByteTimeout    = util.StatusFirstByteTimeout // 首字节超时（自定义状态码，用于触发1分钟冷却）

	// 内部错误标识符使用负值，避免与HTTP状态码混淆
	// 这些标识符仅用于内部错误分类，不会直接返回给客户端
	ErrCodeNetworkRetryable = -1 // 可重试的网络错误（DNS错误、连接超时等）

	// 提取常量消除魔法数字
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

	// Token统计（2025-11新增，从SSE响应中提取）
	InputTokens              int
	OutputTokens             int
	CacheReadInputTokens     int
	CacheCreationInputTokens int

	// 流传输诊断信息（2025-12新增）
	StreamDiagMsg string // 流中断/不完整时的诊断消息，合并到成功日志的Message字段

	// ✅ SSE错误事件（2025-12新增）
	// 用于捕获SSE流中的error事件（如1308错误），在流结束后触发冷却逻辑
	// 虽然HTTP状态码是200，但error事件表示实际上发生了错误
	SSEErrorEvent []byte // SSE流中检测到的最后一个error事件的完整JSON
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
	tokenHash     string // Token哈希值（用于统计，2025-11新增）
	tokenID       int64  // Token ID（用于日志记录，2025-12新增，0表示未使用token）
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

// ErrorAction 已迁移到 cooldown.Action (internal/cooldown/manager.go)
// 统一使用 cooldown.Action 类型，遵循DRY原则

// ============================================================================
// 请求检测工具函数
// ============================================================================

// detectChannelTypeFromPath 根据请求路径推断渠道类型
// 使用 util.DetectChannelTypeFromPath 统一检测，遵循DRY原则
// 返回空字符串表示未识别出特定渠道类型，沿用默认逻辑
func detectChannelTypeFromPath(path string) string {
	return util.DetectChannelTypeFromPath(path)
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
	// 根据API类型设置不同的认证头（使用统一的渠道类型检测）
	channelType := detectChannelTypeFromPath(requestPath)

	switch channelType {
	case util.ChannelTypeGemini:
		// Gemini API: 仅使用 x-goog-api-key
		req.Header.Set("x-goog-api-key", apiKey)
	case util.ChannelTypeOpenAI:
		// OpenAI API: 仅使用 Authorization Bearer
		req.Header.Set("Authorization", "Bearer "+apiKey)
	default:
		// Claude/Anthropic/Codex API: 同时设置两个头
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
}

// filterAndWriteResponseHeaders 过滤并写回响应头（DRY）
// Go Transport 仅自动解压 gzip（当 DisableCompression=false 且请求无 Accept-Encoding 时）
// 对于 br/deflate 等其他编码，必须保留 Content-Encoding 让客户端自行解压
func filterAndWriteResponseHeaders(w http.ResponseWriter, hdr http.Header) {
	contentEncoding := hdr.Get("Content-Encoding")
	// 仅当 Transport 已自动解压 gzip 时才移除编码头（此时 hdr 中已无 Content-Encoding）
	// 若存在非 gzip 编码，必须透传让客户端处理
	skipContentEncoding := contentEncoding == "" || strings.EqualFold(contentEncoding, "gzip")

	for k, vs := range hdr {
		if strings.EqualFold(k, "Connection") ||
			strings.EqualFold(k, "Content-Length") ||
			strings.EqualFold(k, "Transfer-Encoding") {
			continue
		}
		if strings.EqualFold(k, "Content-Encoding") && skipContentEncoding {
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
func buildLogEntry(originalModel string, channelID int64, statusCode int,
	duration float64, isStreaming bool, apiKeyUsed string, authTokenID int64,
	res *fwResult, errMsg string) *model.LogEntry {

	entry := &model.LogEntry{
		Time:        model.JSONTime{Time: time.Now()},
		Model:       originalModel,
		ChannelID:   channelID,
		StatusCode:  statusCode,
		Duration:    duration,
		IsStreaming: isStreaming,
		APIKeyUsed:  apiKeyUsed,
		AuthTokenID: authTokenID,
	}

	if errMsg != "" {
		entry.Message = truncateErr(errMsg)
	} else if res != nil {
		if statusCode >= 200 && statusCode < 300 {
			// ✅ 2025-12: 流传输诊断信息优先于 "ok"
			if res.StreamDiagMsg != "" {
				entry.Message = res.StreamDiagMsg
			} else {
				entry.Message = "ok"
			}
		} else {
			msg := fmt.Sprintf("upstream status %d", statusCode)
			if len(res.Body) > 0 {
				msg = fmt.Sprintf("%s: %s", msg, truncateErr(safeBodyToString(res.Body)))
			}
			entry.Message = msg
		}

		// 流式请求记录首字节响应时间
		if isStreaming && res.FirstByteTime > 0 {
			entry.FirstByteTime = res.FirstByteTime
		}

		// Token统计（2025-11新增，从SSE响应中提取）
		entry.InputTokens = res.InputTokens
		entry.OutputTokens = res.OutputTokens
		entry.CacheReadInputTokens = res.CacheReadInputTokens
		entry.CacheCreationInputTokens = res.CacheCreationInputTokens

		// 成本计算（2025-11新增，基于token统计）
		if res.InputTokens > 0 || res.OutputTokens > 0 || res.CacheReadInputTokens > 0 || res.CacheCreationInputTokens > 0 {
			entry.Cost = util.CalculateCost(
				originalModel,
				res.InputTokens,
				res.OutputTokens,
				res.CacheReadInputTokens,
				res.CacheCreationInputTokens,
			)
		}
	} else {
		entry.Message = "unknown"
	}

	return entry
}

// truncateErr 截断错误信息到512字符（防止日志过长）
func truncateErr(s string) string {
	const maxLen = 512
	s = strings.TrimSpace(s)
	if len(s) > maxLen {
		return s[:maxLen]
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
	if vs, ok := q["timeout_ms"]; ok && len(vs) > 0 && vs[0] != "" {
		if ms, err := strconv.Atoi(vs[0]); err == nil && ms > 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	if vs, ok := q["timeout_s"]; ok && len(vs) > 0 && vs[0] != "" {
		if sec, err := strconv.Atoi(vs[0]); err == nil && sec > 0 {
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

// ============================================================================
// Gemini相关工具函数
// ============================================================================

// formatModelDisplayName 将模型ID转换为友好的显示名称
func formatModelDisplayName(modelID string) string {
	// 简单的格式化:移除日期后缀,首字母大写
	// 例如:gemini-2.5-flash → Gemini 2.5 Flash
	parts := strings.Split(modelID, "-")
	var words []string
	for _, part := range parts {
		// 跳过日期格式(8位纯数字)
		if len(part) == 8 {
			if _, err := strconv.Atoi(part); err == nil {
				continue
			}
		}
		// 首字母大写
		if len(part) > 0 {
			words = append(words, strings.ToUpper(string(part[0]))+part[1:])
		}
	}
	return strings.Join(words, " ")
}
