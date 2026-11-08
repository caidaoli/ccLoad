package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
	"ccLoad/internal/util"

	"github.com/tidwall/gjson"
)

/**
 * @brief 成功响应只探测有限开头；文本到达后最多增加 250ms 首字延迟
 */
const (
	successProbeBytes = 4 * 1024               ///< 原始前缀缓冲上限，单位字节
	successProbeWait  = 250 * time.Millisecond ///< 首段文本后的额外等待上限
)

/**
 * @brief 探测窗口耗尽时放行，保留尚未完成的底层读取
 */
var errSuccessProbeExpired = errors.New("success response probe window expired")

/**
 * @brief 通过通道转交读取结果，避免探测超时后并发读取同一个响应体
 */
type successProbeRead struct {
	data []byte ///< 读取 goroutine 所有的缓冲区，经通道交给请求 goroutine
	err  error  ///< 与这批字节同时返回的上游错误，不能丢弃
}

/**
 * @brief 仅在探测窗口内异步读取，超时后的首次 Read 接收同一个待完成结果
 * @note Read 与探测状态只由请求 goroutine 操作；Close 必须在最后一次 Read 返回后调用
 */
type successProbeBody struct {
	io.ReadCloser
	deadline <-chan time.Time      ///< nil 表示探测窗口尚未开始或已结束
	pending  chan successProbeRead ///< 超时后保留待读取结果，由正常转发接续消费
}

/**
 * @brief 读取原始字节；探测超时不取消上游、不丢失字节或读取错误
 * @param dst 接收缓冲区
 * @return 已读取字节数
 * @return 探测窗口耗尽返回 errSuccessProbeExpired，否则保留上游读取错误
 */
func (r *successProbeBody) Read(dst []byte) (int, error) {
	if r.pending == nil {
		if r.deadline == nil {
			return r.ReadCloser.Read(dst)
		}
		r.pending = make(chan successProbeRead, 1)
		result := r.pending
		go func() {
			buf := make([]byte, len(dst))
			n, err := r.ReadCloser.Read(buf)
			result <- successProbeRead{data: buf[:n], err: err}
		}()
	}
	select {
	case result := <-r.pending:
		n := copy(dst, result.data)
		if n < len(result.data) {
			result.data = result.data[n:]
			r.pending <- result
			return n, nil
		}
		r.pending = nil
		return n, result.err
	case <-r.deadline:
		r.deadline = nil
		return 0, errSuccessProbeExpired
	}
}

/**
 * @brief 关闭上游并等待待完成读取退出，避免返回后继续访问响应捕获缓冲区
 * @return 底层关闭错误
 */
func (r *successProbeBody) Close() error {
	err := r.ReadCloser.Close()
	if r.pending != nil {
		<-r.pending
		r.pending = nil
	}
	return err
}

/**
 * @brief 在下游提交前探测显式配置的成功响应错误，未命中则原样重放全部字节
 * @param reqCtx 请求上下文与超时控制
 * @param resp 上游成功响应，Body 会被替换为可重放读取器
 * @param timed 底层有界等待读取器，必须位于读取统计包装器内侧
 * @param rules 已过滤的成功状态及正文规则
 * @param stats 当前读取统计
 * @param upstreamProtocol 上游协议，用于流首字节语义识别
 * @return 命中且可执行时返回未提交的失败结果，否则返回 nil
 */
func probeConfiguredSuccessResponse(reqCtx *requestContext, resp *http.Response, timed *successProbeBody, rules *model.CooldownDetectionRules, stats *streamReadStats, upstreamProtocol string) *fwResult {
	original := resp.Body
	var prefix []byte
	var readErr error
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
		timed.deadline = nil
		resp.Body = replayResponseProbe(original, prefix, readErr)
	}()
	parser := newSSEUsageParser(upstreamProtocol)
	buf := make([]byte, 2048)
	for len(prefix) < successProbeBytes {
		n, err := resp.Body.Read(buf[:min(len(buf), successProbeBytes-len(prefix))])
		if errors.Is(err, errSuccessProbeExpired) {
			break
		}
		readErr = err
		prefix = append(prefix, buf[:n]...)
		isSSE, _ := classifySSEPrefix(prefix)
		isSSE = isSSE || responseContentTypeIsSSE(resp, false)
		if isSSE {
			_ = parser.Feed(buf[:n])
			if shouldMarkUpstreamFirstByte(parser) {
				markFirstStreamResponse(reqCtx, stats)
			}
			if parser.GetLastError() != nil {
				break
			}
		}
		message, complete, nonText := successResponseProbeText(prefix, isSSE, err == io.EOF)
		if nonText {
			break
		}
		if message != "" && (err == nil || err == io.EOF) && reqCtx.ctx.Err() == nil {
			body, _ := json.Marshal(map[string]string{"message": message})
			evaluation := cooldown.EvaluateCooldownDetectionRules(rules, cooldown.DetectionInput{StatusCode: resp.StatusCode, ErrorBody: body}, time.Now())
			if evaluation.Actionable {
				header := resp.Header.Clone()
				header.Set("Content-Type", "application/json")
				header.Del("Content-Length")
				header.Del("Content-Encoding")
				result := &fwResult{
					Status: util.StatusSSEError, UpstreamStatus: resp.StatusCode,
					Header: header, Body: body, ConfiguredDetection: &evaluation,
					FirstByteTime: responseFirstByteSec(reqCtx, stats), BytesReceived: stats.totalBytes,
				}
				var usage usageParser = parser
				if !isSSE {
					usage = newJSONUsageParser(upstreamProtocol)
					_ = usage.Feed(prefix)
				}
				populateFWResultFromUsageParser(result, usage)
				return result
			}
		}
		if complete || (isSSE && parser.IsStreamComplete()) || err != nil || n == 0 {
			break
		}
		if timer == nil && (message != "" || !isSSE) {
			timer = time.NewTimer(successProbeWait)
			timed.deadline = timer.C
		}
	}
	return nil
}

/**
 * @brief 提取有限响应前缀中的回答文本；SSE 仅解析完整事件并拼接增量
 * @param body 已接收的原始响应前缀
 * @param isSSE 是否按 SSE 解析
 * @param eof 上游是否已正常结束，允许解析无空行结尾的最后事件
 * @return 回答文本或纯文本正文
 * @return 非流式 JSON 已完整，或上游已结束
 * @return 已出现工具调用或其他非文本输出，应停止探测
 */
func successResponseProbeText(body []byte, isSSE, eof bool) (string, bool, bool) {
	if !isSSE {
		trimmed := bytes.TrimSpace(body)
		if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
			if !json.Valid(trimmed) {
				return "", false, false
			}
			text, nonText := successResponseJSONText(gjson.ParseBytes(trimmed), false)
			return text, true, nonText
		}
		return string(body), eof, false
	}
	var text strings.Builder
	events := strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n\n")
	if !eof {
		events = events[:len(events)-1]
	}
	for _, event := range events {
		_, data := parseSSEEventChunk([]byte(event))
		if !json.Valid(data) {
			continue
		}
		delta, nonText := successResponseJSONText(gjson.ParseBytes(data), true)
		if nonText {
			return "", false, true
		}
		text.WriteString(delta)
	}
	return text.String(), eof, false
}

/**
 * @brief 从 OpenAI、Anthropic、Responses、Gemini JSON 中提取回答，排除工具与思考输出
 * @param payload 单个 JSON 响应或 SSE 事件
 * @param streaming 是否仅提取增量字段
 * @return 回答文本；没有回答时为空
 * @return 存在非文本语义输出，调用方必须放行而不再探测
 */
func successResponseJSONText(payload gjson.Result, streaming bool) (string, bool) {
	path := "choices.0.message"
	if streaming {
		path = "choices.0.delta"
	}
	message := payload.Get(path)
	if len(message.Get("tool_calls").Array()) > 0 || message.Get("function_call").IsObject() ||
		message.Get("reasoning_content").String() != "" || message.Get("reasoning").String() != "" || message.Get("refusal").String() != "" {
		return "", true
	}
	if content := message.Get("content"); content.Type == gjson.String {
		return content.String(), false
	}
	if streaming {
		typ := payload.Get("type").String()
		switch typ {
		case "content_block_start":
			block := payload.Get("content_block")
			return block.Get("text").String(), block.Get("type").String() != "text"
		case "content_block_delta":
			delta := payload.Get("delta")
			return delta.Get("text").String(), delta.Get("type").String() != "text_delta"
		case "response.output_text.delta":
			return payload.Get("delta").String(), false
		case "response.output_text.done", "response.completed", "response.done":
			return "", false
		case "response.output_item.done":
			return "", payload.Get("item.type").String() != "message"
		case "response.content_part.done":
			return "", payload.Get("part.type").String() != "output_text"
		}
		if strings.HasPrefix(typ, "response.") || typ == "codex.rate_limits" {
			return "", !isResponsesPreOutputEvent(typ, payload.Raw)
		}
	}
	var text strings.Builder
	for _, part := range payload.Get("candidates.0.content.parts").Array() {
		if part.Get("thought").Bool() || part.Get("functionCall").Exists() || part.Get("inlineData").Exists() {
			return "", true
		}
		text.WriteString(part.Get("text").String())
	}
	if !streaming {
		blocks := payload.Get("content").Array()
		for _, item := range payload.Get("output").Array() {
			if item.Get("type").String() != "message" {
				return "", true
			}
			blocks = append(blocks, item.Get("content").Array()...)
		}
		for _, block := range blocks {
			typ := block.Get("type").String()
			if typ != "text" && typ != "output_text" {
				return "", true
			}
			text.WriteString(block.Get("text").String())
		}
	}
	return text.String(), false
}
