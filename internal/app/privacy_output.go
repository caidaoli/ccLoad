package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"ccLoad/internal/privacy"

	"github.com/tidwall/gjson"
)

/**
 * @brief 在协议转换和用量统计之后还原客户端输出。
 * @details 可能跨帧的占位符后缀延迟当前事件及后续事件，保证并行内容通道的事件顺序。
 */
type privacyResponseWriter struct {
	http.ResponseWriter
	session       *privacy.Session // masking session: what this request sent upstream
	observer      *privacy.Session
	pending       []byte
	frames        []*privacyOutputFrame
	channels      map[string]*privacyOutputChannel
	detailAliases map[string]string ///< 同一 Chat choice 内同时出现的 reasoning detail ID 与索引。
	queuedBytes   int
	modeKnown     bool
	streaming     bool
	emitted       bool
	finished      bool
	err           error
}

func newPrivacyResponseWriter(w http.ResponseWriter, session *privacy.Session) *privacyResponseWriter {
	output := &privacyResponseWriter{ResponseWriter: w, channels: make(map[string]*privacyOutputChannel)}
	if session != nil {
		output.session, output.observer = session, session.ObserveRestoration()
	}
	return output
}

func (w *privacyResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *privacyResponseWriter) WriteHeader(status int) {
	if w.observer != nil {
		w.Header().Del("Content-Length")
	}
	w.ResponseWriter.WriteHeader(status)
	if status >= http.StatusOK && responseHeaderWriteError(w.ResponseWriter) == nil {
		w.emitted = true
	}
}

func (w *privacyResponseWriter) Write(data []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.finished {
		return 0, io.ErrClosedPipe
	}
	if w.observer == nil {
		n, err := w.ResponseWriter.Write(data)
		w.emitted = w.emitted || n > 0
		return n, err
	}
	w.Header().Del("Content-Length")
	w.pending = append(w.pending, data...)
	w.detectMode()
	if w.streaming {
		for {
			end := firstSSEEventEnd(w.pending)
			if end < 0 {
				break
			}
			frame := bytes.Clone(w.pending[:end])
			w.pending = w.pending[end:]
			if w.err = w.addFrame(frame); w.err != nil {
				return 0, w.err
			}
		}
		if len(w.pending)+w.queuedBytes > maxSSEEventBytes {
			w.err = errors.New("privacy response events exceed size limit")
			return 0, w.err
		}
	}
	return len(data), nil
}

func (w *privacyResponseWriter) detectMode() {
	if w.modeKnown {
		return
	}
	contentType := strings.ToLower(w.Header().Get("Content-Type"))
	if strings.Contains(contentType, "text/event-stream") {
		w.streaming, w.modeKnown = true, true
		return
	}
	if strings.Contains(contentType, "json") {
		w.modeKnown = true
		return
	}
	data := bytes.TrimSpace(w.pending)
	if len(data) == 0 {
		return
	}
	// The Responses WebSocket bridge can receive SSE before a content type is
	// set. Do not mistake a Write ending inside "data:" for a JSON response.
	for _, prefix := range []string{"data:", "event:", "id:", "retry:", ":"} {
		if bytes.HasPrefix(data, []byte(prefix)) {
			w.streaming, w.modeKnown = true, true
			return
		}
		if bytes.HasPrefix([]byte(prefix), data) {
			return
		}
	}
	w.modeKnown = true
}

func (w *privacyResponseWriter) Flush() { _ = w.FlushError() }

func (w *privacyResponseWriter) FlushError() error {
	if w.err == nil && w.emitted {
		err := http.NewResponseController(w.ResponseWriter).Flush()
		if errors.Is(err, http.ErrNotSupported) {
			return err
		}
		w.err = err
	}
	return w.err
}

func (w *privacyResponseWriter) SetWriteDeadline(deadline time.Time) error {
	return http.NewResponseController(w.ResponseWriter).SetWriteDeadline(deadline)
}

/**
 * @brief 刷新完整响应并释放截断流中无法匹配的占位符前缀。
 * @return 解析、还原或写入失败时的错误；成功为 nil。
 * @note WebSocket 桥接需要先完成此步骤，再读取终态。
 */
func (w *privacyResponseWriter) Finish() error {
	if w.finished {
		return w.err
	}
	w.finished = true
	if w.err != nil || w.observer == nil {
		return w.err
	}
	w.detectMode()
	if !w.streaming {
		if len(w.pending) == 0 {
			return nil
		}
		restored, err := w.restoreJSONBody()
		if err != nil && !json.Valid(w.pending) {
			// Plain-text and HTML error bodies are not JSON documents.
			restored, err = []byte(w.observer.RestoreText(string(w.pending))), nil
		}
		w.err = err
		if w.err == nil {
			w.err = w.writeOutput(restored)
		}
		w.pending = nil
		return w.err
	}
	if len(w.pending) > 0 {
		frame := bytes.Clone(w.pending)
		w.pending = nil
		w.err = w.addFrame(frame)
	}
	if w.err == nil {
		w.releaseChannels("")
		w.err = w.drainFrames()
	}
	return w.err
}

/**
 * @brief 还原 JSON 响应，包括 Gemini 普通响应与 Vertex JSON 数组中的跨 part 占位符。
 * @return 用于下游的响应 JSON。
 * @return 解析或还原失败时的错误；成功为 nil。
 */
func (w *privacyResponseWriter) restoreJSONBody() ([]byte, error) {
	candidates := gjson.GetBytes(w.pending, "candidates")
	chunkArray := bytes.HasPrefix(bytes.TrimSpace(w.pending), []byte("["))
	if (!candidates.IsArray() && !chunkArray) || !json.Valid(w.pending) {
		return w.observer.RestoreJSON(w.pending)
	}

	frame := &privacyOutputFrame{data: w.pending}
	var err error
	if chunkArray {
		gjson.ParseBytes(w.pending).ForEach(func(_, chunk gjson.Result) bool {
			if candidates := chunk.Get("candidates"); candidates.IsArray() {
				err = w.collectGeminiFields(frame, candidates)
			}
			return err == nil
		})
	} else {
		err = w.collectGeminiFields(frame, candidates)
	}
	if err != nil {
		return nil, err
	}
	if len(frame.fields) == 0 {
		return w.observer.RestoreJSON(w.pending)
	}
	w.releaseChannels("")
	if !frame.needsRestoration() {
		return w.pending, nil
	}
	return frame.restoreJSON(w.observer)
}

func (w *privacyResponseWriter) writeOutput(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	w.Header().Del("Content-Length")
	n, err := w.ResponseWriter.Write(data)
	if n > 0 {
		w.emitted = true
	}
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}

type privacyOutputFrame struct {
	wire   []byte
	data   []byte
	fields []*privacyOutputField
	holds  int
}

/**
 * @brief 响应帧中的一个解码字符串字段。
 */
type privacyOutputField struct {
	frame        *privacyOutputFrame          ///< 所属帧。
	index        int                          ///< 字符串在帧 JSON 中的字节位置。
	value        string                       ///< 上游发送的已解码文本。
	edits        []privacyOutputEdit          ///< 刷新前要应用的前向还原。
	lengthChange int                          ///< 编辑对可见文本字节数的净变化。
	reasoning    bool                         ///< 文本可绑定当前 part 或后续事件中的签名。
	signature    *privacyOutputFieldSignature ///< 绑定同一 part 的签名；无签名时为 nil。
}

/**
 * @brief 支持按 part 单独回放的签名及逆编辑。
 */
type privacyOutputFieldSignature struct {
	value string                  ///< 上游原签名。
	index int                     ///< 签名字符串在帧 JSON 中的字节位置。
	edits []privacy.ReasoningEdit ///< 回放此 part 需要的逆编辑。
}

type privacyOutputEdit struct {
	start, end int
	text       string
}

type privacyOutputPiece struct {
	field      *privacyOutputField
	start, end int
}

/**
 * @brief 一个 SSE 文本通道的跨帧状态。
 */
type privacyOutputChannel struct {
	pending         []privacyOutputPiece    ///< 尚未确定是否匹配占位符的字段片段。
	observer        *privacy.Session        ///< 所属调用方的还原观测会话。
	tail            string                  ///< 可能成为占位符的文本后缀。
	kind            privacyOutputFieldKind  ///< 文本用途，决定是否转义或记录签名回放补丁。
	reasoningLength int                     ///< 签名思考已处理的可见文本 UTF-8 字节数。
	upstreamLength  int                     ///< 签名思考收到的上游文本 UTF-8 字节数。
	reasoningEdits  []privacy.ReasoningEdit ///< 回放签名思考时要逆转的替换。
	// These are the lexical state of the inner tool JSON before tail. They
	// must span deltas: a fragment need not start at an opening quotation mark.
	quoted, escaped bool ///< 内层工具 JSON 的字符串与转义状态。
}

/**
 * @brief SSE 通道对还原文本的处理方式。
 */
type privacyOutputFieldKind uint8

/**
 * @brief 普通文本、工具参数和可回放的签名思考通道。
 */
const (
	privacyOutputText            privacyOutputFieldKind = iota ///< 普通显示文本。
	privacyOutputArguments                                     ///< 字符串化 JSON 工具参数。
	privacyOutputSignedReasoning                               ///< 可能在后续帧收到签名的思考文本。
	privacyOutputGeminiThought                                 ///< 签名绑定每个 part 的 Gemini 思考文本。
)

/**
 * @brief 收集帧内可见字段，处理通道终态并写出已准备好的帧。
 * @param wire 保留原始行和 SSE 事件元数据的帧。
 * @return 事件超出缓冲限制、签名回放补丁编码失败或下游写入失败时的错误；成功为 nil。
 * @note 将未就绪的帧留在队列，等待后续帧完成占位符。
 */
func (w *privacyResponseWriter) addFrame(wire []byte) error {
	frame := &privacyOutputFrame{wire: wire}
	w.frames = append(w.frames, frame)
	w.queuedBytes += len(wire)
	event, data := parseSSEEventChunk(wire)
	frame.data = data
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		w.releaseChannels("")
	} else if gjson.ValidBytes(data) {
		root := gjson.ParseBytes(data)
		if err := w.collectFields(frame, root); err != nil {
			return err
		}
		w.releaseTerminalChannels(event, root)
	}
	if w.queuedBytes > maxSSEEventBytes {
		return errors.New("privacy response events exceed size limit")
	}
	return w.drainFrames()
}

/**
 * @brief 将可见字段送入对应的跨帧还原通道。
 * @param frame 字段所属的帧。
 * @param value JSON 字段，仅字符串可进入文本通道。
 * @param channel 客户端拼接时使用的内容通道标识。
 * @param kind 文本字段用途。
 * @param signature 字段所属 part 的签名，缺失时为零值。
 * @return 超出签名回放预算时的错误；成功为 nil。
 * @details 工具调用可能插在连续文本之间，挂起的占位符前缀仍留在原通道。
 * @note 可将帧保留到下一个片段或通道终态。
 */
func (w *privacyResponseWriter) collectField(frame *privacyOutputFrame, value gjson.Result, channel string, kind privacyOutputFieldKind, signature gjson.Result) error {
	if value.Type != gjson.String {
		return nil
	}
	field := &privacyOutputField{frame: frame, index: value.Index, value: privacy.JSONString(value), reasoning: kind == privacyOutputSignedReasoning}
	if signature.Type == gjson.String && signature.Str != "" {
		field.signature = &privacyOutputFieldSignature{value: privacy.JSONString(signature), index: signature.Index}
		field.reasoning = true
	}
	frame.fields = append(frame.fields, field)
	state := w.channels[channel]
	if state == nil {
		state = &privacyOutputChannel{kind: kind, observer: w.observer}
		w.channels[channel] = state
	}
	return state.feed(field)
}

/**
 * @brief 结束已收到供应商签名的思考通道，为签名附上回放补丁。
 * @param frame 签名字段所属的帧。
 * @param value 供应商签名字段。
 * @param channel 签名对应的思考通道标识。
 * @return 回放补丁无法编码时的错误；成功为 nil。
 * @note 只有非空签名结束通道。独立的签名与未修改的思考保持原字节。
 */
func (w *privacyResponseWriter) collectReasoningSignature(frame *privacyOutputFrame, value gjson.Result, channel string) error {
	state := w.channels[channel]
	if state == nil || value.Type != gjson.String || value.Str == "" {
		return nil
	}
	state.release()
	delete(w.channels, channel)
	if len(state.reasoningEdits) == 0 {
		return nil
	}
	signature := privacy.JSONString(value)
	wrapped, err := privacy.WrapReasoningSignature(signature, state.reasoningEdits)
	if err != nil {
		return err
	}
	frame.fields = append(frame.fields, &privacyOutputField{
		frame: frame, index: value.Index, value: signature,
		edits: []privacyOutputEdit{{end: len(signature), text: wrapped}},
	})
	return nil
}

func privacyOutputIndex(value gjson.Result, fallback int) string {
	if value.Exists() {
		return value.String()
	}
	return strconv.Itoa(fallback)
}

/**
 * @brief 根据 OpenRouter 提供的索引或 ID 区分 reasoning detail 的分片。
 * @param choice 已包含分隔符的 Chat choice 通道。
 * @param detail 当前 detail 对象。
 * @param position 未提供索引和 ID 时使用的帧内数组位置。
 * @return 已包含分隔符的 detail 通道名称。
 * @note 收到同时带索引和 ID 的事件后，将两种标识解析为同一个通道。
 */
func (w *privacyResponseWriter) reasoningDetailChannel(choice string, detail gjson.Result, position int) string {
	prefix := choice + "reasoning_details/"
	index := detail.Get("index")
	hasIndex := index.Type == gjson.Number
	indexKey := prefix + "index/" + strconv.Itoa(position) + "/"
	if hasIndex {
		indexKey = prefix + "index/" + index.Raw + "/"
	}
	indexChannel := indexKey
	if alias := w.detailAliases[indexKey]; alias != "" {
		indexChannel = alias
	}

	id := detail.Get("id")
	if id.Type != gjson.String || id.Str == "" {
		return indexChannel
	}
	idKey := prefix + "id/" + id.Str + "/"
	idChannel := w.detailAliases[idKey]
	if idChannel == "" {
		idChannel = idKey
	}
	if !hasIndex {
		return idChannel
	}

	channel := indexChannel
	if w.channels[idChannel+"text"] != nil || w.channels[idChannel+"summary"] != nil {
		channel = idChannel
	}
	if w.detailAliases == nil {
		w.detailAliases = make(map[string]string)
	}
	w.detailAliases[indexKey], w.detailAliases[idKey] = channel, channel
	return channel
}

/**
 * @brief 按客户端协议提取可能跨事件拼接的文本字段。
 * @param frame 字段所属的帧。
 * @param root 帧中 JSON 负载的根节点。
 * @return 超出签名回放预算或补丁无法编码时的错误；成功为 nil。
 * @details 自带非空签名的独立 Claude 思考块由单帧 JSON 还原处理。
 */
func (w *privacyResponseWriter) collectFields(frame *privacyOutputFrame, root gjson.Result) error {
	for index, choice := range root.Get("choices").Array() {
		channel := "chat/" + privacyOutputIndex(choice.Get("index"), index) + "/"
		delta := choice.Get("delta")
		collect := func(value gjson.Result, key string, kind privacyOutputFieldKind) {
			_ = w.collectField(frame, value, channel+key, kind, gjson.Result{}) // 普通文本和工具参数没有签名回放预算。
		}
		for _, key := range []string{"content", "reasoning_content", "reasoning", "refusal"} {
			collect(delta.Get(key), key, privacyOutputText)
		}
		for partIndex, part := range delta.Get("content").Array() {
			key := "content/" + strconv.Itoa(partIndex)
			if part.Get("type").String() == "thinking" {
				if err := w.collectField(frame, part.Get("thinking"), channel+key+"/thinking", privacyOutputText, part.Get("signature")); err != nil {
					return err
				}
			}
			collect(part.Get("text"), key, privacyOutputText)
		}
		for detailIndex, detail := range delta.Get("reasoning_details").Array() {
			field := "text"
			detailType := detail.Get("type").String()
			if detailType != "" && detailType != "reasoning.text" && detailType != "reasoning.summary" {
				continue
			}
			base := w.reasoningDetailChannel(channel, detail, detailIndex)
			switch detailType {
			case "":
				if detail.Get("summary").Type == gjson.String || detail.Get(field).Type != gjson.String && w.channels[base+field] == nil {
					field = "summary"
				}
			case "reasoning.summary":
				field = "summary"
			}
			if err := w.collectField(frame, detail.Get(field), base+field, privacyOutputSignedReasoning, gjson.Result{}); err != nil {
				return err
			}
			if err := w.collectReasoningSignature(frame, detail.Get("signature"), base+field); err != nil {
				return err
			}
		}
		for toolIndex, tool := range delta.Get("tool_calls").Array() {
			collect(tool.Get("function.arguments"), "tool/"+privacyOutputIndex(tool.Get("index"), toolIndex), privacyOutputArguments)
		}
		collect(delta.Get("function_call.arguments"), "function", privacyOutputArguments)
	}
	if err := w.collectGeminiFields(frame, root.Get("candidates")); err != nil {
		return err
	}
	index := privacyOutputIndex(root.Get("index"), 0)
	channel := "anthropic/" + index + "/"
	switch root.Get("type").String() {
	case "content_block_start":
		block := root.Get("content_block")
		_ = w.collectField(frame, block.Get("text"), channel+"text", privacyOutputText, gjson.Result{})
		if block.Get("type").String() == "thinking" && block.Get("signature").String() == "" {
			return w.collectField(frame, block.Get("thinking"), channel+"thinking", privacyOutputSignedReasoning, gjson.Result{})
		}
	case "content_block_delta":
		delta := root.Get("delta")
		_ = w.collectField(frame, delta.Get("text"), channel+"text", privacyOutputText, gjson.Result{})
		_ = w.collectField(frame, delta.Get("partial_json"), channel+"partial_json", privacyOutputArguments, gjson.Result{})
		switch delta.Get("type").String() {
		case "thinking_delta":
			return w.collectField(frame, delta.Get("thinking"), channel+"thinking", privacyOutputSignedReasoning, gjson.Result{})
		case "signature_delta":
			return w.collectReasoningSignature(frame, delta.Get("signature"), channel+"thinking")
		}
	case "response.output_text.delta", "response.refusal.delta", "response.reasoning_text.delta", "response.reasoning_summary_text.delta", "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		event := root.Get("type").String()
		item := root.Get("output_index").String()
		if !root.Get("output_index").Exists() {
			item = root.Get("item_id").String()
		}
		channel := "responses/" + item + "/" + root.Get("content_index").String() + "/" + root.Get("summary_index").String() + "/" + event
		kind := privacyOutputText
		if event == "response.function_call_arguments.delta" {
			kind = privacyOutputArguments
		}
		return w.collectField(frame, root.Get("delta"), channel, kind, gjson.Result{})
	}
	return nil
}

/**
 * @brief 按 Gemini 候选拼接正文与思考，各签名仍绑定对应的 part。
 * @param frame 当前响应或事件。
 * @param candidates Gemini 响应中的候选数组。
 * @return 超出 part 签名回放预算时的错误；成功为 nil。
 * @note 候选的签名 part 和工具调用可以出现在占位符分片之间；收到 finishReason 后结束该候选。
 */
func (w *privacyResponseWriter) collectGeminiFields(frame *privacyOutputFrame, candidates gjson.Result) error {
	for candidateIndex, candidate := range candidates.Array() {
		channel := "gemini/" + privacyOutputIndex(candidate.Get("index"), candidateIndex) + "/"
		for _, part := range candidate.Get("content.parts").Array() {
			key, kind := "text", privacyOutputText
			var signature gjson.Result
			if part.Get("thought").Type == gjson.True {
				key, kind = "thought", privacyOutputGeminiThought
				signature = part.Get("thoughtSignature")
				if signature.Type != gjson.String || signature.Str == "" {
					signature = part.Get("thought_signature")
				}
			}
			if err := w.collectField(frame, part.Get("text"), channel+key, kind, signature); err != nil {
				return err
			}
		}
		if candidate.Get("finishReason").String() != "" {
			w.releaseChannels(channel)
		}
	}
	return nil
}

func (w *privacyResponseWriter) releaseTerminalChannels(event string, root gjson.Result) {
	for index, choice := range root.Get("choices").Array() {
		if finish := choice.Get("finish_reason"); finish.Exists() && finish.Type != gjson.Null {
			w.releaseChannels("chat/" + privacyOutputIndex(choice.Get("index"), index) + "/")
		}
	}
	if kind := root.Get("type").String(); kind != "" {
		event = kind
	}
	switch event {
	case "content_block_stop":
		w.releaseChannels("anthropic/" + privacyOutputIndex(root.Get("index"), 0) + "/")
	case "message_stop", "error", "response.completed", "response.done", "response.incomplete", "response.failed":
		w.releaseChannels("")
	case "response.output_item.done":
		item := root.Get("output_index").String()
		if !root.Get("output_index").Exists() {
			item = root.Get("item.id").String()
		}
		w.releaseChannels("responses/" + item + "/")
	}
}

/**
 * @brief 结束指定协议或 choice 下的通道，并释放同期使用的 detail 别名。
 * @param prefix 通道前缀；为空时结束全部通道。
 */
func (w *privacyResponseWriter) releaseChannels(prefix string) {
	for key, channel := range w.channels {
		if strings.HasPrefix(key, prefix) {
			channel.release()
			delete(w.channels, key)
		}
	}
	for key := range w.detailAliases {
		if strings.HasPrefix(key, prefix) {
			delete(w.detailAliases, key)
		}
	}
}

func (c *privacyOutputChannel) release() {
	c.observer.RestoreText(c.tail)
	c.clearPending()
}

func (c *privacyOutputChannel) clearPending() {
	for _, piece := range c.pending {
		piece.field.frame.holds--
	}
	c.pending, c.tail = nil, ""
}

/**
 * @brief 还原已确定的占位符，将可能跨帧的余留前缀留到下一个片段。
 * @param field 当前事件中的解码文本字段。
 * @return 超出签名回放预算时的错误；成功为 nil。
 * @note 更新跨帧状态和帧编辑；签名思考仅保留可见字节偏移与占位符。
 */
func (c *privacyOutputChannel) feed(field *privacyOutputField) error {
	if c.kind == privacyOutputSignedReasoning {
		if len(field.value) > privacy.MaxReasoningTextBytes-c.upstreamLength {
			return privacy.ErrInvalidReasoningSignature
		}
		c.upstreamLength += len(field.value)
	} else if field.signature != nil && len(field.value) > privacy.MaxReasoningTextBytes {
		return privacy.ErrInvalidReasoningSignature
	}

	pieces := append(c.pending, privacyOutputPiece{field: field, end: len(field.value)})
	tailLength := len(c.tail)
	text := c.tail + field.value
	c.clearPending()
	position, observed, lengthChange := 0, 0, 0
	for position < len(text) {
		length, incomplete := privacyOutputPlaceholder(text[position:])
		if incomplete {
			break
		}
		if length > 0 {
			c.observer.RestoreText(text[observed:position])
			token := text[position : position+length]
			if c.kind == privacyOutputGeminiThought && position < tailLength && privacyOutputMixedSignatures(pieces, position, position+length) {
				c.observer.ObserveUnrestoredPlaceholder()
			} else if restored, found := c.observer.RestorePlaceholder(token); found && restored != token {
				if c.kind == privacyOutputArguments && c.quoted {
					escaped, _ := json.Marshal(restored)
					restored = string(escaped[1 : len(escaped)-1])
				}
				if c.kind == privacyOutputSignedReasoning {
					offset := c.reasoningLength + position + lengthChange
					if len(c.reasoningEdits) == privacy.MaxReasoningEdits || len(restored) > privacy.MaxReasoningTextBytes-offset {
						return privacy.ErrInvalidReasoningSignature
					}
					c.reasoningEdits = append(c.reasoningEdits, privacy.ReasoningEdit{
						Offset: offset, Length: len(restored), Placeholder: strings.Clone(token),
					})
					lengthChange += len(restored) - length
				}
				if err := privacyOutputReplace(pieces, position, token, restored); err != nil {
					return err
				}
			}
			position += length
			observed = position
			continue
		}
		if c.kind == privacyOutputArguments {
			if c.escaped {
				c.escaped = false
			} else if c.quoted && text[position] == '\\' {
				c.escaped = true
			} else if text[position] == '"' {
				c.quoted = !c.quoted
			}
		}
		position++
	}
	c.observer.RestoreText(text[observed:position])
	if c.kind == privacyOutputSignedReasoning {
		if len(text)+lengthChange > privacy.MaxReasoningTextBytes-c.reasoningLength {
			return privacy.ErrInvalidReasoningSignature
		}
		c.reasoningLength += position + lengthChange
	}
	if position < len(text) {
		c.tail = text[position:]
		offset := 0
		for _, piece := range pieces {
			length := piece.end - piece.start
			if offset+length > position {
				if offset < position {
					piece.start += position - offset
				}
				c.pending = append(c.pending, piece)
				piece.field.frame.holds++
			}
			offset += length
		}
	}
	return nil
}

/**
 * @brief 检查跨 part 占位符是否同时涉及带签名和无签名的 Gemini 思考。
 * @param pieces 拼接后文本的原始字段及其片段范围。
 * @param start 占位符起始字节位置。
 * @param end 占位符结束字节位置。
 * @return 部分原片无法在签名中保存逆编辑时为 true。
 */
func privacyOutputMixedSignatures(pieces []privacyOutputPiece, start, end int) bool {
	signed, unsigned := false, false
	offset := 0
	for _, piece := range pieces {
		next := offset + piece.end - piece.start
		if max(start, offset) < min(end, next) {
			signed = signed || piece.field.signature != nil
			unsigned = unsigned || piece.field.signature == nil
		}
		if next >= end {
			break
		}
		offset = next
	}
	return signed && unsigned
}

/**
 * @brief 将还原值写到占位符起始处，再删除各后续字段里的占位符续片。
 * @param pieces 当前文本在通道中涉及的原始字段及位置。
 * @param start 占位符在当前文本中的起始字节位置。
 * @param token 完整占位符。
 * @param restored 占位符的可见明文。
 * @return 超出 part 签名回放预算时的错误；成功为 nil。
 * @note 每个 part 签名只记录其所属 part 内发生的替换。
 */
func privacyOutputReplace(pieces []privacyOutputPiece, start int, token, restored string) error {
	end := start + len(token)
	offset := 0
	for _, piece := range pieces {
		length := piece.end - piece.start
		left, right := max(start, offset), min(end, offset+length)
		if left < right {
			edit := privacyOutputEdit{
				start: piece.start + left - offset,
				end:   piece.start + right - offset,
				text:  restored,
			}
			if sig := piece.field.signature; sig != nil {
				offset := edit.start + piece.field.lengthChange
				if len(sig.edits) == privacy.MaxReasoningEdits || len(restored) > privacy.MaxReasoningTextBytes-offset {
					return privacy.ErrInvalidReasoningSignature
				}
				replay := privacy.ReasoningEdit{
					Offset: offset, Length: len(restored), Placeholder: strings.Clone(token),
				}
				if left != start || right != end {
					replay.TokenStart, replay.TokenEnd = left-start, right-start
				}
				sig.edits = append(sig.edits, replay)
			}
			piece.field.edits = append(piece.field.edits, edit)
			piece.field.lengthChange += len(restored) - (edit.end - edit.start)
			restored = ""
		}
		offset += length
		if offset >= end {
			break
		}
	}
	return nil
}

const privacyPlaceholderPrefix = "{{CCLOAD_"

// Return a complete token's length, or whether the entire input could still
// become a token. The bounded grammar avoids holding arbitrary model prose.
func privacyOutputPlaceholder(text string) (int, bool) {
	if len(text) == 0 || text[0] != '{' {
		return 0, false
	}
	const prefix = privacyPlaceholderPrefix
	incomplete := false
	for kindLength := 1; kindLength <= 24; kindLength++ {
		length := len(prefix) + kindLength + 1 + 16 + 2
		matches := true
		for index := 0; index < min(len(text), length); index++ {
			char := text[index]
			switch {
			case index < len(prefix):
				matches = char == prefix[index]
			case index == len(prefix):
				matches = char >= 'A' && char <= 'Z'
			case index < len(prefix)+kindLength:
				matches = char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_'
			case index == len(prefix)+kindLength:
				matches = char == '_'
			case index < length-2:
				matches = char >= '0' && char <= '9' || char >= 'a' && char <= 'f'
			default:
				matches = char == '}'
			}
			if !matches {
				break
			}
		}
		if matches {
			if len(text) >= length {
				return length, false
			}
			incomplete = true
		}
	}
	return 0, incomplete
}

func (w *privacyResponseWriter) drainFrames() error {
	for len(w.frames) > 0 && w.frames[0].holds == 0 {
		frame := w.frames[0]
		data, err := frame.restore(w.observer)
		if err != nil {
			return err
		}
		if err := w.writeOutput(data); err != nil {
			return err
		}
		w.queuedBytes -= len(frame.wire)
		w.frames[0] = nil
		w.frames = w.frames[1:]
	}
	return nil
}

/**
 * @brief 判断事件是否包含尚待扫描的占位符或跨帧编辑。
 * @return JSON 需要继续还原时为 true。
 */
func (f *privacyOutputFrame) needsRestoration() bool {
	return privacy.MayContainPlaceholder(f.data) || slices.ContainsFunc(f.fields, func(field *privacyOutputField) bool {
		return len(field.edits) > 0
	})
}

/**
 * @brief 将已跨帧还原的字段和其他 JSON 字段写回 SSE 事件。
 * @param observer 所属调用方的还原观测会话。
 * @return 保留 SSE 元数据的原始或已还原事件。
 * @return JSON 无法还原时的错误；成功为 nil。
 */
func (f *privacyOutputFrame) restore(observer *privacy.Session) ([]byte, error) {
	if !f.needsRestoration() {
		return f.wire, nil
	}
	if !gjson.ValidBytes(f.data) {
		observer.RestoreText(string(f.data))
		return f.wire, nil
	}
	updated, err := f.restoreJSON(observer)
	if err != nil {
		return nil, fmt.Errorf("restore privacy response event: %w", err)
	}
	if bytes.Equal(updated, f.data) {
		return f.wire, nil
	}
	return privacyOutputEventData(f.wire, updated), nil
}

/**
 * @brief 应用横跨 part 或多个帧的文本替换与各 part 的签名回放补丁。
 * @param observer 所属调用方的还原观测会话。
 * @return 已还原的 JSON。
 * @return 签名补丁或 JSON 无法写回时的错误；成功为 nil。
 */
func (f *privacyOutputFrame) restoreJSON(observer *privacy.Session) ([]byte, error) {
	remaining := privacy.MaxReasoningTextBytes
	for _, field := range f.fields {
		if field.reasoning {
			remaining -= len(field.value) + field.lengthChange
			if remaining < 0 {
				return nil, privacy.ErrInvalidReasoningSignature
			}
		}
	}
	// Channel deltas were restored and observed after concatenation. Supply
	// their final values so the JSON pass only observes the remaining fields.
	var fields map[int]string
	if len(f.fields) > 0 {
		fields = make(map[int]string, len(f.fields))
	}
	for _, field := range f.fields {
		if len(field.edits) == 0 {
			fields[field.index] = field.value
			continue
		}
		var restored strings.Builder
		offset := 0
		for _, edit := range field.edits {
			restored.WriteString(field.value[offset:edit.start])
			restored.WriteString(edit.text)
			offset = edit.end
		}
		restored.WriteString(field.value[offset:])
		fields[field.index] = restored.String()
		if sig := field.signature; sig != nil {
			wrapped, err := privacy.WrapReasoningSignature(sig.value, sig.edits)
			if err != nil {
				return nil, err
			}
			fields[sig.index] = wrapped
		}
	}
	return observer.RestoreJSONWithFields(f.data, fields)
}

// Replace only data lines. Preserve comments, IDs, event names, retry fields and
// CRLF framing. Every newline in the rewritten JSON must still have a data:
// prefix, otherwise SSE would discard the remaining JSON lines.
func privacyOutputEventData(wire, data []byte) []byte {
	var output bytes.Buffer
	wrote := false
	for _, line := range bytes.SplitAfter(wire, []byte{'\n'}) {
		if !bytes.HasPrefix(line, []byte("data:")) {
			output.Write(line)
			continue
		}
		if wrote {
			continue
		}
		wrote = true
		prefix := len("data:")
		if len(line) > prefix && line[prefix] == ' ' {
			prefix++
		}
		ending := "\n"
		if bytes.HasSuffix(line, []byte("\r\n")) {
			ending = "\r\n"
		}
		for index, dataLine := range bytes.Split(data, []byte{'\n'}) {
			if index > 0 {
				output.WriteString(ending)
			}
			output.Write(line[:prefix])
			output.Write(dataLine)
		}
		if bytes.HasSuffix(line, []byte{'\n'}) {
			output.WriteString(ending)
		}
	}
	return output.Bytes()
}
