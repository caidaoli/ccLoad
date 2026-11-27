package app

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"ccLoad/internal/model"
	"ccLoad/internal/util"

	"github.com/tidwall/gjson"
)

/**
 * @brief 压缩探测最多查看 16 KiB 响应前缀，超出时交由正常响应路径处理。
 */
const inferenceCompressionProbeSize = 16 << 10

/**
 * @brief 识别 CodeBuddy 或 Qoder 在首个语义帧中拒绝压缩的响应。
 * @param cfg 当前推理渠道。
 * @param resp 已完成提供商响应归一化的 HTTP 响应，探测后保留已读内容和读取错误。
 * @param onRead 每次收到字节时通知上游有活动；无空闲计时器时为 nil。
 * @return 命中编码拒绝或实测的提供商拒绝时为 true。
 * @note 首个模型帧（含角色/用量元数据）即交回正常转发，避免延迟首包并避免重放已开始生成的请求。
 */
func inferenceCompressionRejected(cfg *model.Config, resp *http.Response, onRead func()) bool {
	status := resp.StatusCode
	if status == http.StatusUnsupportedMediaType {
		return true
	}
	if status != http.StatusBadRequest && status != http.StatusForbidden && (status < 200 || status >= 300) {
		return false
	}
	original := resp.Body
	var prefix []byte
	var readErr error
	defer func() { resp.Body = replayResponseProbe(original, prefix, readErr) }()
	sse, parsed := responseContentTypeIsSSE(resp, false), 0
	var buf [SSEBufferSize]byte
	for len(prefix) < inferenceCompressionProbeSize {
		n, err := original.Read(buf[:min(len(buf), inferenceCompressionProbeSize-len(prefix))])
		if n > 0 && onRead != nil {
			onRead()
		}
		prefix = append(prefix, buf[:n]...)
		readErr = err
		if err != nil && err != io.EOF {
			return false
		}
		if !sse {
			sse, _ = classifySSEPrefix(prefix)
		}
		if sse {
			for parsed < len(prefix) {
				tail := prefix[parsed:]
				end := firstSSEEventEnd(tail)
				if end < 0 {
					if err != io.EOF {
						break
					}
					end = len(tail)
				}
				parsed += end
				event, data := parseSSEEventChunk(normalizeSSEStreamPrefix(tail[:end]))
				if isHeartbeatEventType(event) || event != "error" && len(data) == 0 {
					continue
				}
				if !json.Valid(data) {
					return false
				}
				root := gjson.ParseBytes(data)
				if isHeartbeatEventType(root.Get("type").Str) {
					continue
				}
				return rejectsInferenceEncoding(cfg, status, true, event, root)
			}
		} else {
			data := normalizeSSEStreamPrefix(prefix)
			if json.Valid(data) {
				return rejectsInferenceEncoding(cfg, status, false, "", gjson.ParseBytes(data))
			}
			if err == io.EOF && status >= 400 {
				return isInferenceEncodingRejection("", strings.ToLower(string(data)))
			}
		}
		if err != nil || n == 0 {
			break
		}
	}
	return false
}

/**
 * @brief 校验提供商拒绝，排除认证、额度与普通请求参数错误。
 * @param cfg 当前推理渠道。
 * @param status 外层 HTTP 状态码。
 * @param sse 错误是否出现在 SSE 事件内。
 * @param event SSE 事件名；HTTP JSON 未声明事件。
 * @param root 完整 JSON 负载。
 * @return 错误明确指向编码，或符合实测的流内权限拒绝。
 */
func rejectsInferenceEncoding(cfg *model.Config, status int, sse bool, event string, root gjson.Result) bool {
	if root.Get("choices").IsArray() {
		return false
	}
	var node gjson.Result
	var businessCode int64
	if cfg.UsesCodeBuddyOAuth() {
		node, businessCode = codeBuddyBusinessNode(root, 6)
		if businessCode != 0 && businessCode != 11101 {
			return false
		}
	}
	errNode := root.Get("error")
	if status < 300 {
		status = int(root.Get("status").Int())
		if status == 0 {
			status = int(errNode.Get("status").Int())
		}
		if status == 0 && event != "error" && businessCode == 0 && !isErrorPayload(root.Raw, false) {
			return false
		}
	}
	switch status {
	case http.StatusUnsupportedMediaType:
		return true
	case 0, http.StatusBadRequest, http.StatusForbidden:
	default:
		return false
	}
	if sse && cfg.UsesQoderOAuth() && status == http.StatusForbidden && errNode.Get("code").Str == "permission_denied" {
		message := strings.TrimSpace(errNode.Get("message").Str)
		envelope := gjson.Parse(message)
		if strings.EqualFold(message, "signature invalid") || strings.EqualFold(message, http.StatusText(http.StatusForbidden)) ||
			envelope.Get("statusCode").Str == "FORBIDDEN" && envelope.Get("statusCodeValue").Int() == http.StatusForbidden {
			return true
		}
	}
	for _, detail := range [...]gjson.Result{node, errNode, root} {
		for _, field := range [...]string{"msg", "message", "error"} {
			message := strings.ToLower(detail.Get(field).Str)
			if isInferenceEncodingRejection(detail.Get("code").Str, message) ||
				businessCode == 11101 && strings.Contains(message, "unmarshal chat params failed") &&
					strings.Contains(message, "invalid character") && strings.Contains(message, "looking for beginning of value") {
				return true
			}
		}
	}
	return false
}

/**
 * @brief 区分明确的编码或解压错误与普通请求参数错误。
 * @param code 上游标明的结构化错误码，缺失时为空。
 * @param message 转为小写的上游错误文案。
 * @return 文案或错误码指向请求体编码失败时为 true。
 */
func isInferenceEncodingRejection(code, message string) bool {
	switch code {
	case "unsupported_content_encoding", "invalid_content_encoding", "decompression_failed":
		return true
	}
	if strings.Contains(message, "content-encoding") || strings.Contains(message, "content encoding") {
		return strings.Contains(message, "unsupported") || strings.Contains(message, "not supported") || strings.Contains(message, "invalid")
	}
	return strings.Contains(message, "failed to decompress") || strings.Contains(message, "cannot decompress") ||
		strings.Contains(message, "gzip: invalid header") || strings.Contains(message, "zstd: invalid magic number")
}

/**
 * @brief 在正常读取明文响应时确认模型已完成该请求。
 * @param cfg 当前推理渠道。
 * @param resp 待观察的明文 SSE 响应，保持正文与读取时序。
 * @param onSuccess 读到有效模型结果与正常终态时同步调用一次。
 */
func observePlainInferenceResponse(cfg *model.Config, resp *http.Response, onSuccess func()) {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		body := &plainInferenceResponseBody{
			ReadCloser: resp.Body,
			parser:     newSSEUsageParser(util.ProtocolOpenAI),
			onSuccess:  onSuccess,
			codeBuddy:  cfg.UsesCodeBuddyOAuth(),
			qoder:      cfg.UsesQoderOAuth(),
		}
		body.parser.observeEvent = body.observeEvent
		resp.Body = body
	}
}

/**
 * @brief 透传正常读取，不延迟角色或模型输出的首包。
 */
type plainInferenceResponseBody struct {
	io.ReadCloser
	parser    *sseUsageParser ///< 读取失败或得到确定的模型结果后停止观察。
	onSuccess func()          ///< 与正常 Read 在同一 goroutine 执行。
	codeBuddy bool            ///< CodeBuddy 业务码可能置于原始负载外层。
	qoder     bool            ///< Qoder 允许无内容的截断与过滤终态。
	failed    bool            ///< 明文重放遇到提供商业务错误。
	result    bool            ///< 已确认 Chat 有实际输出或合法的 Qoder 空终态。
	finished  bool            ///< 已确认有合法 finish_reason 的 Chat 选项。
}

/**
 * @brief 原样返回读取字节和错误，探测不发起额外读取。
 * @param buf 调用方提供的读取缓冲区。
 * @return 上游实际返回的字节数。
 * @return 上游实际返回的读取错误。
 */
func (b *plainInferenceResponseBody) Read(buf []byte) (int, error) {
	n, err := b.ReadCloser.Read(buf)
	if p := b.parser; p != nil {
		switch {
		case err != nil && err != io.EOF, p.Feed(buf[:n]) != nil, b.failed, p.GetLastError() != nil:
			b.parser = nil
		case p.IsStreamEnded(), err == io.EOF:
			b.parser = nil
			if b.result && (p.IsStreamEnded() || b.finished) {
				b.onSuccess()
			}
		}
	}
	return n, err
}

/**
 * @brief 在 SSE 解析器解码事件时区分错误、实际 Chat 输出和模型收尾。
 * @param data 解析后的事件原始 JSON，用于读取 CodeBuddy 外层业务码。
 * @param event 已解码的事件对象。
 */
func (b *plainInferenceResponseBody) observeEvent(data string, event map[string]any) {
	if b.codeBuddy {
		if _, code := codeBuddyBusinessNode(gjson.Parse(data), 6); code != 0 {
			b.failed = true
			return
		}
	}
	if choices, ok := event["choices"].([]any); ok {
		b.result = b.result || !isOpenAIStreamMetadata(event)
		b.finished = b.finished || openAIStreamPayloadComplete(event)
		if b.qoder && !b.result {
			for _, item := range choices {
				choice, _ := item.(map[string]any)
				switch choice["finish_reason"] {
				case "length", "content_filter":
					b.result = true
					return
				}
			}
		}
	}
}
