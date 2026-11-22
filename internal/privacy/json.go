package privacy

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

var protocolKeys = map[string]bool{
	"model": true, "id": true, "ids": true, "type": true, "role": true, "name": true,
	"namespace": true, "server_label": true, "server_name": true,
	"signature": true, "thoughtSignature": true, "thought_signature": true,
	"encrypted_content": true, "data": true, "url": true, "image_url": true,
	"media_type": true, "mime_type": true, "mimeType": true, "prompt_cache_key": true,
	"reasoning_effort": true, "effort": true, "stop_reason": true, "finish_reason": true,
	"status": true, "object": true, "event": true,
}

var definitionKeys = map[string]bool{
	"tools": true, "tool_choice": true, "functions": true, "function_call": true,
	"toolConfig": true, "tool_config": true, "response_format": true,
	"reasoning": true, "include": true, "output_format": true, "output_config": true,
	"generationConfig": true, "generation_config": true, "stream_options": true,
	"client_metadata": true,
}

func identifierKey(key string) bool {
	return strings.HasSuffix(key, "_id") || strings.HasSuffix(key, "_ids") || strings.HasSuffix(key, "Id") || strings.HasSuffix(key, "Ids")
}

type jsonContext struct {
	key, event       string
	data, definition bool
	root             bool
	toolResult       bool
	textKeys         bool
	reasoning        reasoningJSONPosition
}

/**
 * @brief 允许协议思考块出现的 JSON 位置
 */
type reasoningJSONPosition uint8

/**
 * @brief 记录协议容器直系关系，避免普通对象凭同名 type 进入签名回放
 */
const (
	reasoningJSONEnvelope  reasoningJSONPosition = iota + 1 ///< 包裹的请求或响应
	reasoningJSONChoice                                     ///< Chat choices 数组元素
	reasoningJSONMessage                                    ///< Claude 或 Chat 的消息及 Chat delta
	reasoningJSONCandidate                                  ///< Gemini candidates 数组元素
	reasoningJSONContent                                    ///< Gemini 消息的 content 或 contents 元素
	reasoningJSONBlock                                      ///< 在协议位置的可见思考块
	reasoningJSONRootItem                                   ///< 顶层数组元素，可为独立思考块或完整 Gemini 响应
)

/**
 * @brief 确定子字段是否处在原生或转换后的协议结构内
 * @param parent 当前对象的协议位置
 * @param key 子字段的名字
 * @param event 当前对象的 type
 * @return 子字段对应的协议位置；普通字段返回零
 */
func reasoningPosition(parent jsonContext, key, event string) reasoningJSONPosition {
	if parent.root || parent.reasoning == reasoningJSONEnvelope || parent.reasoning == reasoningJSONRootItem {
		switch key {
		case "request", "response":
			if parent.root {
				return reasoningJSONEnvelope
			}
		case "messages":
			return reasoningJSONMessage
		case "choices":
			return reasoningJSONChoice
		case "candidates":
			return reasoningJSONCandidate
		case "contents":
			return reasoningJSONContent
		case "content", "input", "output", "parts":
			return reasoningJSONBlock
		case "item":
			if strings.HasPrefix(event, "response.output_item.") {
				return reasoningJSONBlock
			}
		case "content_block":
			if event == "content_block_start" {
				return reasoningJSONBlock
			}
		}
	}
	switch parent.reasoning {
	case reasoningJSONChoice:
		if key == "message" || key == "delta" {
			return reasoningJSONMessage
		}
	case reasoningJSONMessage:
		if key == "content" || key == "reasoning" || key == "reasoning_details" {
			return reasoningJSONBlock
		}
	case reasoningJSONCandidate:
		if key == "content" {
			return reasoningJSONContent
		}
	case reasoningJSONContent:
		if key == "parts" {
			return reasoningJSONBlock
		}
	}
	return 0
}

/**
 * @brief 匹配编码成 JSON Unicode 转义的协议前缀
 * @param prefix 不含引号、斜杠或反斜杠的 ASCII 标记
 * @return 可混合匹配原字符与 Unicode 转义的正则
 */
func escapedJSONPrefix(prefix string) *regexp.Regexp {
	var pattern strings.Builder
	for _, c := range prefix {
		fmt.Fprintf(&pattern, `(?:%s|\\u(?i:%04x))`, regexp.QuoteMeta(string(c)), c)
	}
	return regexp.MustCompile(pattern.String())
}

/**
 * @brief 可逆占位符和重放签名的 JSON 转义形式
 */
var (
	escapedPlaceholderPrefix = escapedJSONPrefix("{{CCLOAD_")
	escapedSignaturePrefix   = escapedJSONPrefix(ReasoningSignaturePrefix)
)

/**
 * @brief 判断响应 JSON 是否可能含有完整或跨帧占位符前缀
 * @param body JSON 字节
 * @return 存在明文或 JSON 转义后的占位符标记时为 true
 */
func MayContainPlaceholder(body []byte) bool {
	return bytes.Contains(body, []byte("{{CCLOAD_")) ||
		bytes.Contains(body, []byte(`\u`)) && escapedPlaceholderPrefix.Match(body)
}

/**
 * @brief 遮蔽请求正文，并将带回放信息的思考块还原为上游签名时的文本
 * @param body 客户端协议 JSON
 * @return 可转发的正文，未发生替换时复用原切片
 * @return JSON 遍历、签名回放或脱敏失败时返回错误
 * @note nil 或停用的会话仅处理历史签名，无需进程内字典
 */
func (s *Session) MaskJSON(body []byte) ([]byte, error) {
	if s == nil || !s.Enabled() {
		if !bytes.Contains(body, []byte(ReasoningSignaturePrefix)) && (!bytes.Contains(body, []byte(`\u`)) || !escapedSignaturePrefix.Match(body)) {
			return body, nil
		}
		return (*Session)(nil).transformJSON(body, false)
	}
	defer s.addMaskTime(time.Now())
	return s.transformJSON(body, false)
}

// RestoreJSON restores string values with JSON-safe escaping, including the
// second level of escaping in stringified tool arguments and argument deltas.
func (s *Session) RestoreJSON(body []byte) ([]byte, error) {
	return s.RestoreJSONWithFields(body, nil)
}

// RestoreJSONWithFields supplies decoded values for fields whose output
// channels have already restored them. Keys are the opening quote's byte
// offset in this document; nested stringified JSON uses independent offsets.
func (s *Session) RestoreJSONWithFields(body []byte, replacements map[int]string) ([]byte, error) {
	if len(replacements) == 0 && !MayContainPlaceholder(body) {
		return body, nil
	}
	return s.transformJSONWithFields(body, true, replacements)
}

func (s *Session) transformJSON(body []byte, restore bool) ([]byte, error) {
	return s.transformJSONWithFields(body, restore, nil)
}

func (s *Session) transformJSONWithFields(body []byte, restore bool, replacements map[int]string) ([]byte, error) {
	if len(body) > maxTextBytes {
		return nil, errors.New("privacy JSON exceeds 64 MiB")
	}
	// encoding/json validates iteratively; gjson's validator recurses without a
	// depth limit.
	if !json.Valid(body) {
		return nil, errors.New("privacy requires a valid JSON document")
	}
	p := &jsonPatcher{src: string(body), replacements: replacements}
	out, changed, err := s.transformDocumentWithPatcher(restore, jsonContext{root: true}, p)
	if err != nil {
		return nil, err
	}
	if !changed {
		return body, nil
	}
	return []byte(out), nil
}

// jsonPatcher splices replacements into the source in document order.
type jsonPatcher struct {
	src            string
	out            strings.Builder
	last           int
	changed        bool
	decoded        *[]jsonTextMatch
	protected      *[][2]int
	rawMatches     []jsonTextMatch
	rawNext        int
	known          bool // collect session values instead of policy detections
	replacements   map[int]string
	reasoning      map[int]reasoningJSONPatch
	reasoningBytes int
}

func (p *jsonPatcher) replace(value gjson.Result, text string) error {
	start, end := value.Index, value.Index+len(value.Raw)
	if start < p.last || end > len(p.src) || p.src[start:end] != value.Raw {
		return errors.New("privacy could not locate a JSON string")
	}
	if !p.changed {
		p.out.Grow(len(p.src)) // replacements are about as long as the originals
	}
	p.out.WriteString(p.src[p.last:start])
	p.out.WriteString(text)
	p.last, p.changed = end, true
	return nil
}

/**
 * @brief 仅重写解码值发生变化的 JSON 字符串，保留原有转义
 * @param value 原字符串及其文档偏移
 * @param text 新的解码值
 * @return 字段无法按顺序定位时返回错误
 */
func (p *jsonPatcher) replaceString(value gjson.Result, text string) error {
	if JSONString(value) == text {
		return nil
	}
	return p.replace(value, jsonQuote(text))
}

/**
 * @brief 应用预先转换的字段或保留签名覆盖的字段，让兄弟字段继续正常遍历
 * @param value 当前 JSON 值
 * @return 若该值已有预先确定的处理方式则为 true
 * @return 字段无法按顺序定位时返回错误
 */
func (p *jsonPatcher) replaceAssigned(value gjson.Result) (bool, error) {
	if replacement, ok := p.replacements[value.Index]; ok {
		return true, p.replaceString(value, replacement)
	}
	if patch, ok := p.reasoning[value.Index]; ok {
		if patch.insertKey != "" {
			return true, p.replace(value, jsonQuote(patch.value)+","+jsonQuote(patch.insertKey)+":"+jsonQuote(patch.insertValue))
		}
		return true, p.replaceString(value, patch.value)
	}
	return false, nil
}

func (s *Session) transformDocument(text string, restore bool, ctx jsonContext) (string, bool, error) {
	p := &jsonPatcher{src: text}
	return s.transformDocumentWithPatcher(restore, ctx, p)
}

func (s *Session) transformDocumentWithPatcher(restore bool, ctx jsonContext, p *jsonPatcher) (string, bool, error) {
	if err := s.walk(gjson.Parse(p.src), restore, ctx, p, 0); err != nil {
		return "", false, err
	}
	if !p.changed {
		return p.src, false, nil
	}
	p.out.WriteString(p.src[p.last:])
	return p.out.String(), true, nil
}

type jsonMember struct {
	key      string
	keyValue gjson.Result
	value    gjson.Result
}

// last returns the member that encoding/json would decode for key.
func last(members []jsonMember, key string) gjson.Result {
	for _, member := range slices.Backward(members) {
		if member.key == key {
			return member.value
		}
	}
	return gjson.Result{}
}

func lastString(members []jsonMember, key string) string {
	if value := last(members, key); value.Type == gjson.String {
		return value.Str
	}
	return ""
}

func objectMembers(object gjson.Result) []jsonMember {
	var members []jsonMember
	object.ForEach(func(key, value gjson.Result) bool {
		members = append(members, jsonMember{key: key.Str, keyValue: key, value: value})
		return true
	})
	return members
}

func (s *Session) walk(value gjson.Result, restore bool, ctx jsonContext, p *jsonPatcher, depth int) error {
	if depth > 128 {
		return errors.New("privacy JSON nesting exceeds 128 levels")
	}
	switch {
	case value.IsObject():
		return s.walkObject(objectMembers(value), restore, ctx, p, depth)
	case value.IsArray():
		if ctx.root {
			// Responses stores its output array; Gemini HTTP can return an array of envelopes.
			ctx.root, ctx.reasoning = false, reasoningJSONRootItem
		}
		var err error
		value.ForEach(func(_, child gjson.Result) bool {
			err = s.walk(child, restore, ctx, p, depth+1)
			return err == nil
		})
		return err
	case value.Type == gjson.String:
		if assigned, err := p.replaceAssigned(value); assigned {
			return err
		}
		return s.walkString(value, restore, ctx, p)
	}
	return nil
}

/**
 * @brief 遍历协议对象，工具负载继承数据语义，调用信封仍保留协议字段。
 * @param members 当前对象的有序成员。
 * @param restore 是否还原而非遮蔽。
 * @param parent 当前对象从父节点继承的上下文。
 * @param p 当前文档的替换记录。
 * @param depth JSON 嵌套深度。
 * @return 遍历或替换失败时的错误，成功为 nil。
 */
func (s *Session) walkObject(members []jsonMember, restore bool, parent jsonContext, p *jsonPatcher, depth int) error {
	event, role := lastString(members, "type"), lastString(members, "role")
	if !parent.data && !parent.definition && (parent.root || parent.reasoning == reasoningJSONBlock || parent.reasoning == reasoningJSONRootItem) {
		if event == "" && parent.key == "reasoning_details" {
			event = "reasoning.text"
			if last(members, "summary").Type == gjson.String {
				event = "reasoning.summary"
			}
		}
		if sig := reasoningSignature(members, event); sig.Type == gjson.String && sig.Str != "" {
			if err := s.signedReasoning(members, event, sig, restore, p); err != nil {
				return err
			}
		}
	}
	for _, member := range members {
		key, child := member.key, member.value
		if assigned, err := p.replaceAssigned(child); assigned {
			if err != nil {
				return err
			}
			continue
		}
		if p.decoded != nil {
			s.normalizeRawMatches(member.keyValue, p)
		}
		if restore && parent.textKeys {
			if out := s.restore(key, false); out != key {
				if err := p.replace(member.keyValue, jsonQuote(out)); err != nil {
					return err
				}
			}
		}
		ctx := jsonContext{key: key, event: event, data: parent.data, definition: parent.definition,
			toolResult: parent.toolResult, textKeys: parent.textKeys, reasoning: reasoningPosition(parent, key, event)}
		if parent.root && (definitionKeys[key] || key == "text" && child.IsObject()) {
			ctx.definition = true
		}
		if !parent.data && (event == "additional_tools" || event == "tool_search_output") {
			ctx.definition = true
		}
		if !parent.data && (key == "output" && (event == "function_call_output" || event == "custom_tool_call_output") ||
			key == "content" && (role == "tool" || event == "tool_result") ||
			key == "response" && (parent.key == "functionResponse" || parent.key == "function_response")) {
			ctx.toolResult = true
		}
		if key == "input" && (event == "tool_use" || strings.HasSuffix(event, "_tool_use")) ||
			(key == "input" || key == "action" || key == "operation") && strings.HasSuffix(event, "_call") ||
			key == "args" && (parent.key == "functionCall" || parent.key == "function_call") ||
			key == "response" && (parent.key == "functionResponse" || parent.key == "function_response") ||
			key == "arguments" || key == "output" && event == "function_call_output" || key == "content" && role == "tool" {
			ctx.data = true
		}
		if !ctx.data && (protocolKeys[key] || identifierKey(key)) {
			continue
		}
		if !ctx.data && (key == "image_url" || key == "input_audio" || key == "audio" && child.IsObject() || key == "source" && child.IsObject() && isBinarySource(child)) {
			continue
		}
		if err := s.walk(child, restore, ctx, p, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// JSONString decodes a string value from a valid document as encoding/json
// does. gjson keeps invalid UTF-8 bytes and pairs an unpaired surrogate escape
// with the next \u escape, dropping that character, so matching or rewriting
// its result would miss text that upstream decoders still see.
func JSONString(value gjson.Result) string {
	text := value.Str
	if utf8.ValidString(text) && !surrogateEscape(value.Raw) {
		return text
	}
	_ = json.Unmarshal([]byte(value.Raw), &text) // the document was validated
	return text
}

// surrogateEscape reports a \uD800-\uDFFF escape. An escaped backslash before
// "ud8" only costs a redundant decode.
func surrogateEscape(raw string) bool {
	for {
		i := strings.Index(raw, `\u`)
		if i < 0 || i+3 >= len(raw) {
			return false
		}
		if c := raw[i+3] | 0x20; raw[i+2]|0x20 == 'd' && (c == '8' || c == '9' || c >= 'a' && c <= 'f') {
			return true
		}
		raw = raw[i+2:]
	}
}

func (s *Session) walkString(value gjson.Result, restore bool, ctx jsonContext, p *jsonPatcher) error {
	if s == nil {
		return nil // Only signed reasoning needs processing during replay.
	}
	item := JSONString(value)
	value.Str = item
	if strings.HasPrefix(item, "data:") || ctx.definition && ctx.key != "description" {
		return nil
	}
	if restore && !strings.Contains(item, "{{CCLOAD_") {
		return nil
	}
	if p.decoded != nil {
		return s.collectDecodedMatches(value, ctx.key, p)
	}
	if restore && ctx.toolResult {
		// Syntax placeholders must be expanded before parsing. A decoded
		// value may contain quotes or backslashes, so it must wait until the
		// restored document tells us its JSON string context.
		item = s.restoreSyntax(item)
	}
	arguments := ctx.key == "arguments" || ctx.key == "partial_json" || ctx.key == "delta" && (strings.Contains(ctx.event, "arguments") || ctx.event == "input_json_delta")
	var out string
	var err error
	switch {
	case arguments && json.Valid([]byte(item)):
		out, _, err = s.transformDocument(item, restore, jsonContext{data: true})
	case ctx.toolResult && json.Valid([]byte(item)):
		if restore {
			out, _, err = s.transformDocument(item, true, jsonContext{data: true, textKeys: true})
		} else {
			out, err = s.maskToolResult(item)
		}
	case restore:
		out = s.restore(item, arguments)
	case ctx.data && s.policy.namedSecret(ctx.key, item):
		out, err = s.apply(item, []match{{0, len(item), "SECRET", ""}})
	default:
		out, err = s.maskText(item)
	}
	if err != nil || out == value.Str {
		return err
	}
	return p.replace(value, jsonQuote(out))
}

// jsonMatch is safe to cache: it contains coordinates and rule metadata only.
type jsonMatch struct {
	match
	syntax bool
}

type jsonTextMatch struct {
	jsonMatch
	value string
}

func (s *Session) maskToolResult(text string) (string, error) {
	spans, err := s.policyToolMatches(text)
	if err != nil {
		return "", err
	}
	var candidates []jsonTextMatch
	for _, span := range spans {
		value := text[span.start:span.end]
		if !span.syntax {
			if err := json.Unmarshal([]byte(`"`+value+`"`), &value); err != nil {
				return "", err
			}
		}
		candidates = append(candidates, jsonTextMatch{span, value})
	}
	s.indexOnce.Do(s.buildIndex)
	s.mu.RLock()
	hasKnown := s.known.size != 0
	s.mu.RUnlock()
	if hasKnown {
		known, err := s.collectToolMatches(text, true)
		if err != nil {
			return "", err
		}
		candidates = append(candidates, known...)
	}
	selected := selectJSONMatches(candidates)
	if len(selected) > maxMatches {
		return "", errTooManyMatches
	}
	if len(selected) == 0 {
		return text, nil
	}
	// The dictionary distinguishes syntax from decoded values even when
	// both have identical bytes. Only the actual sensitive spans are held.
	tokens := make([]string, len(selected))
	for _, syntax := range []bool{false, true} {
		var originals strings.Builder
		var mappings []match
		for _, m := range selected {
			if m.syntax != syntax {
				continue
			}
			start := originals.Len()
			originals.WriteString(m.value)
			mappings = append(mappings, match{start, originals.Len(), m.kind, m.category})
		}
		if len(mappings) == 0 {
			continue
		}
		values, err := s.remember(originals.String(), mappings, syntax)
		if err != nil {
			return "", err
		}
		for i, m := range selected {
			if m.syntax == syntax {
				tokens[i] = values[m.value]
			}
		}
	}
	var out strings.Builder
	out.Grow(len(text))
	last := 0
	for i, m := range selected {
		out.WriteString(text[last:m.start])
		out.WriteString(tokens[i])
		last = m.end
	}
	out.WriteString(text[last:])
	return out.String(), nil
}

// policyToolMatches caches only policy-dependent coordinates. Session values
// are collected separately, so neither secrets nor one caller's dictionary
// can leak into the shared immutable policy's cache.
func (s *Session) policyToolMatches(text string) ([]jsonMatch, error) {
	var key cacheKey
	if len(text) >= cacheMinBytes {
		key = cacheKey{hash: sha256.Sum256([]byte(text)), json: true}
		if found, ok := s.policy.cache.get(key); ok {
			return found.json, nil
		}
	}
	found, err := s.collectToolMatches(text, false)
	if err != nil {
		return nil, err
	}
	spans := make([]jsonMatch, 0, len(found))
	for _, m := range found {
		spans = append(spans, m.jsonMatch)
	}
	if len(text) >= cacheMinBytes {
		s.policy.cache.put(key, cachedMatches{json: spans})
	}
	return spans, nil
}

func (s *Session) collectToolMatches(text string, known bool) ([]jsonTextMatch, error) {
	var found []match
	var err error
	if known {
		found = s.knownMatches(text)
		if len(found) == 0 && !strings.ContainsRune(text, '\\') && utf8.ValidString(text) {
			// Without JSON escapes or invalid UTF-8, decoding cannot reveal a
			// value that the scope's raw-text scan did not already see.
			return nil, nil
		}
	} else {
		found, err = s.policy.find(text)
	}
	if err != nil {
		return nil, err
	}
	found = append([]match(nil), found...)
	if len(found) > maxMatches {
		return nil, errTooManyMatches
	}
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].start != found[j].start {
			return found[i].start < found[j].start
		}
		return found[i].end > found[j].end
	})
	var rawMatches []jsonTextMatch
	for _, m := range found {
		if s.policy.allowed(text[m.start:m.end]) {
			continue
		}
		rawMatches = append(rawMatches, jsonTextMatch{jsonMatch{m, true}, text[m.start:m.end]})
	}
	var candidates []jsonTextMatch
	var protected [][2]int
	p := &jsonPatcher{src: text, decoded: &candidates, protected: &protected, rawMatches: rawMatches, known: known}
	// Reuse the argument traversal, but collect decoded string matches before
	// applying any replacements. Raw matches may replace numbers or cross JSON
	// syntax, so parsing the already-masked text would lose other decoded hits.
	if err := s.walk(gjson.Parse(text), false, jsonContext{data: true}, p, 0); err != nil {
		return nil, err
	}
	for _, m := range p.rawMatches {
		if m.value != "" {
			candidates = append(candidates, m)
		}
	}
	// Keep overlapping candidates until policy and scope detections have
	// been combined. A known value can displace the first policy hit, making
	// a later overlapping policy hit eligible again.
	for _, span := range placeholderPattern.FindAllStringIndex(text, -1) {
		protected = append(protected, [2]int{span[0], span[1]})
	}
	sort.Slice(protected, func(i, j int) bool { return protected[i][0] < protected[j][0] })
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].start != candidates[j].start {
			return candidates[i].start < candidates[j].start
		}
		return candidates[i].end > candidates[j].end
	})
	kept := candidates[:0]
	pi := 0
	for _, m := range candidates {
		if s.policy.allowed(m.value) {
			continue
		}
		for pi < len(protected) && protected[pi][1] <= m.start {
			pi++
		}
		if pi == len(protected) || protected[pi][0] >= m.end {
			kept = append(kept, m)
		}
	}
	return kept, nil
}

func selectJSONMatches(candidates []jsonTextMatch) []jsonTextMatch {
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].start != candidates[j].start {
			return candidates[i].start < candidates[j].start
		}
		return candidates[i].end > candidates[j].end
	})
	selected := candidates[:0]
	end := 0
	for _, m := range candidates {
		if m.start < end {
			continue
		}
		selected = append(selected, m)
		end = m.end
	}
	return selected
}

func (s *Session) collectDecodedMatches(value gjson.Result, key string, p *jsonPatcher) error {
	s.normalizeRawMatches(value, p)
	text := value.Str
	if p.known && !strings.ContainsRune(value.Raw, '\\') && utf8.ValidString(value.Raw) {
		return nil // already covered by the raw scope-value scan
	}
	var found []match
	var err error
	if p.known {
		found = s.knownMatches(text)
	} else {
		found, err = s.policy.find(text)
	}
	if err != nil {
		return err
	}
	found = append([]match(nil), found...)
	if !p.known && s.policy.namedSecret(key, text) {
		found = append(found, match{0, len(text), "SECRET", ""})
	}
	if len(found) > maxMatches {
		return errTooManyMatches
	}
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].start != found[j].start {
			return found[i].start < found[j].start
		}
		return found[i].end > found[j].end
	})
	rawSpans := jsonStringSpans(value, found)
	for i, m := range found {
		*p.decoded = append(*p.decoded, jsonTextMatch{jsonMatch{rawSpans[i], false}, text[m.start:m.end]})
	}
	if len(*p.decoded) > maxMatches {
		return errTooManyMatches
	}
	var placeholders []match
	for _, span := range placeholderPattern.FindAllStringIndex(text, -1) {
		placeholders = append(placeholders, match{start: span[0], end: span[1]})
	}
	for _, span := range jsonStringSpans(value, placeholders) {
		*p.protected = append(*p.protected, [2]int{span.start, span.end})
	}
	return nil
}

// Raw rules still see JSON syntax, keys and numeric values. When their hit is
// inside a string, store the decoded value and cover complete escape sequences
// so replacing part of a \\uXXXX or \\n cannot leave a broken JSON escape.
func (s *Session) normalizeRawMatches(value gjson.Result, p *jsonPatcher) {
	end := value.Index + len(value.Raw) - 1
	type boundary struct {
		offset, index int
		end           bool
	}
	var points []boundary
	var indices []int
	for p.rawNext < len(p.rawMatches) {
		m := &p.rawMatches[p.rawNext]
		if m.start >= end {
			break
		}
		index := p.rawNext
		p.rawNext++
		if m.start < value.Index+1 || m.end > end {
			continue
		}
		points = append(points, boundary{m.start - value.Index, len(indices), false}, boundary{m.end - value.Index, len(indices), true})
		indices = append(indices, index)
	}
	if len(points) == 0 {
		return
	}
	sort.Slice(points, func(i, j int) bool { return points[i].offset < points[j].offset })
	spans := make([][2]int, len(indices))
	raw, decoded, previousRaw, previousDecoded := 1, 0, 1, 0
	for _, point := range points {
		for raw < point.offset {
			previousRaw, previousDecoded = raw, decoded
			rawSize, size := jsonStringStep(value.Raw, raw)
			raw, decoded = raw+rawSize, decoded+size
		}
		m := &p.rawMatches[indices[point.index]]
		if point.end {
			m.end, spans[point.index][1] = value.Index+raw, decoded
		} else if raw > point.offset {
			m.start, spans[point.index][0] = value.Index+previousRaw, previousDecoded
		} else {
			m.start, spans[point.index][0] = value.Index+raw, decoded
		}
	}
	text := JSONString(value)
	for i, index := range indices {
		m := &p.rawMatches[index]
		m.value = text[spans[i][0]:spans[i][1]]
		m.syntax = false
		if s.policy.allowed(m.value) {
			m.value = ""
		}
	}
}

// jsonStringSpans translates overlapping decoded spans through JSON escapes in
// one forward pass. It stores only match boundaries, not every byte's offset.
func jsonStringSpans(value gjson.Result, found []match) []match {
	if len(found) == 0 {
		return nil
	}
	points := make([]int, 0, 2*len(found))
	for _, m := range found {
		points = append(points, m.start, m.end)
	}
	sort.Ints(points)
	points = slices.Compact(points)
	offsets := make([]int, len(points))
	raw, decoded := 1, 0 // skip the opening quote
	for i, point := range points {
		for decoded < point {
			rawSize, size := jsonStringStep(value.Raw, raw)
			raw, decoded = raw+rawSize, decoded+size
		}
		offsets[i] = value.Index + raw
	}
	out := make([]match, 0, len(found))
	for _, m := range found {
		m.start, m.end = offsets[sort.SearchInts(points, m.start)], offsets[sort.SearchInts(points, m.end)]
		out = append(out, m)
	}
	return out
}

func jsonStringStep(raw string, at int) (int, int) {
	if raw[at] != '\\' {
		if raw[at] >= utf8.RuneSelf {
			r, size := utf8.DecodeRuneInString(raw[at:])
			return size, utf8.RuneLen(r)
		}
		return 1, 1
	}
	if raw[at+1] != 'u' {
		return 2, 1
	}
	hex, _ := strconv.ParseUint(raw[at+2:at+6], 16, 16)
	r := rune(hex)
	if utf16.IsSurrogate(r) {
		if at+12 <= len(raw)-1 && raw[at+6:at+8] == `\u` {
			next, _ := strconv.ParseUint(raw[at+8:at+12], 16, 16)
			if joined := utf16.DecodeRune(r, rune(next)); joined != utf8.RuneError {
				return 12, utf8.RuneLen(joined)
			}
		}
		r = utf8.RuneError
	}
	return 6, utf8.RuneLen(r)
}

func isBinarySource(value gjson.Result) bool {
	members := objectMembers(value)
	typ := lastString(members, "type")
	mediaType := last(members, "media_type")
	return typ == "base64" || typ == "url" || mediaType.Exists() && mediaType.Type != gjson.Null
}

/**
 * @brief 定位供应商签名，Gemini 的普通文本 part 仍走常规脱敏
 * @param members 对象成员
 * @param event 对象的 type 字段
 * @return 签名字段，缺失时为零值
 */
func reasoningSignature(members []jsonMember, event string) gjson.Result {
	if event == "reasoning.encrypted" {
		return gjson.Result{}
	}
	if last(members, "thought").Type == gjson.True {
		if signature := last(members, "thoughtSignature"); signature.Str != "" {
			return signature
		}
		return last(members, "thought_signature")
	}
	if event == "thinking" || event == "reasoning" || strings.HasPrefix(event, "reasoning.") {
		return last(members, "signature")
	}
	return gjson.Result{}
}

/**
 * @brief 签名覆盖的可见正文及其在回放正文中的稳定位置
 */
type reasoningJSONField struct {
	path  string       ///< Responses 数组及下标；空字符串表示单字段正文
	value gjson.Result ///< 当前文档中的字符串及其字节位置
}

/**
 * @brief 签名回放需要改写或保护的字段
 */
type reasoningJSONPatch struct {
	value       string ///< 待写入的解码值
	insertKey   string ///< Gemini SDK 可省略的空正文字段；空字符串表示不插入
	insertValue string ///< 插入字段的解码值
}

/**
 * @brief 定位 Claude、Chat 和 Responses 中受同一签名覆盖的可见正文
 * @param members 签名对象成员
 * @param event 对象类型
 * @return 单字段正文的键名，供 Gemini 补写省略的正文
 * @return 与对象字段顺序无关的回放字段列表
 */
func signedReasoningFields(members []jsonMember, event string) (string, []reasoningJSONField) {
	key := "text"
	switch event {
	case "thinking":
		if last(members, "thinking").Type == gjson.String || last(members, "text").Type != gjson.String {
			key = "thinking"
		}
	case "reasoning.summary":
		key = "summary"
	}
	field := last(members, key)
	var fields []reasoningJSONField
	if event == "reasoning" {
		for _, section := range []string{"content", "summary"} {
			array := last(members, section)
			if !array.IsArray() {
				continue
			}
			for index, item := range array.Array() {
				if !item.IsObject() {
					continue
				}
				value := last(objectMembers(item), "text")
				if value.Type == gjson.String {
					fields = append(fields, reasoningJSONField{path: section + "." + strconv.Itoa(index), value: value})
				}
			}
		}
	}
	if len(fields) == 0 || field.Type == gjson.String {
		fields = append(fields, reasoningJSONField{value: field})
	}
	return key, fields
}

/**
 * @brief 为签名及可见思考正文准备替换，普通兄弟字段仍按正常顺序遍历
 * @param members 对象成员
 * @param event 对象类型
 * @param sig 原始签名字段
 * @param restore 输出给客户端时为 true，回放给供应商时为 false
 * @param p 当前文档的替换记录
 * @return 无法解封签名、记录替换或定位字段时返回错误
 */
func (s *Session) signedReasoning(members []jsonMember, event string, sig gjson.Result, restore bool, p *jsonPatcher) error {
	key, fields := signedReasoningFields(members, event)
	if p.reasoning == nil {
		p.reasoning = make(map[int]reasoningJSONPatch)
	}
	_, handled := p.replacements[sig.Index]
	visibleBytes := 0
	for _, field := range fields {
		if field.value.Type == gjson.String {
			visible := JSONString(field.value)
			p.reasoning[field.value.Index] = reasoningJSONPatch{value: visible}
			if text, precomputed := p.replacements[field.value.Index]; precomputed {
				handled = true
				visible = text
			}
			visibleBytes += len(visible)
		}
	}
	signature := JSONString(sig)
	if handled || !restore && !strings.HasPrefix(signature, ReasoningSignaturePrefix) {
		p.reasoningBytes += visibleBytes
		if p.reasoningBytes > maxTextBytes {
			return ErrInvalidReasoningSignature
		}
		return nil
	}
	if !restore {
		return p.replaySignedReasoning(signature, sig, key, fields)
	}
	var edits []ReasoningEdit
	remaining := maxTextBytes - p.reasoningBytes
	for _, field := range fields {
		if field.value.Type != gjson.String {
			continue
		}
		start := len(edits)
		visible, err := s.restoreReasoning(p.reasoning[field.value.Index].value, &edits, remaining, MaxReasoningEdits-start)
		if err != nil {
			return err
		}
		remaining -= len(visible)
		for i := start; i < len(edits); i++ {
			edits[i].Field = field.path
		}
		p.reasoning[field.value.Index] = reasoningJSONPatch{value: visible}
	}
	signature, err := WrapReasoningSignature(signature, edits)
	if err != nil {
		return err
	}
	p.reasoningBytes = maxTextBytes - remaining
	p.reasoning[sig.Index] = reasoningJSONPatch{value: signature}
	return nil
}

/**
 * @brief 仅在签名覆盖的字段中撤销客户端展示时的实际替换
 * @param signature 客户端提供的签名值
 * @param sig 签名在当前 JSON 中的位置
 * @param key 单字段正文的键名
 * @param fields 允许封套引用的正文位置
 * @return 封套损坏、引用缺失或容量超限时返回 ErrInvalidReasoningSignature
 */
func (p *jsonPatcher) replaySignedReasoning(signature string, sig gjson.Result, key string, fields []reasoningJSONField) error {
	original, edits, err := parseReasoningSignature(signature)
	if err != nil || len(edits) == 0 {
		return err
	}
	groups, err := reasoningEditsByField(edits)
	if err != nil {
		return err
	}
	signaturePatch := reasoningJSONPatch{value: original}
	remaining := maxTextBytes - p.reasoningBytes
	for _, field := range fields {
		visible := p.reasoning[field.value.Index].value
		if fieldEdits := groups[field.path]; len(fieldEdits) > 0 {
			if field.value.Type != gjson.String && field.value.Exists() {
				return ErrInvalidReasoningSignature
			}
			visible, err = applyReasoningEdits(visible, fieldEdits, remaining)
			if err != nil {
				return err
			}
			delete(groups, field.path)
			if field.value.Exists() {
				p.reasoning[field.value.Index] = reasoningJSONPatch{value: visible}
			} else {
				// Gemini SDKs may omit an empty text field when replaying its signed part.
				signaturePatch.insertKey, signaturePatch.insertValue = key, visible
			}
		}
		remaining -= len(visible)
	}
	if len(groups) != 0 || remaining < 0 {
		return ErrInvalidReasoningSignature
	}
	p.reasoningBytes = maxTextBytes - remaining
	p.reasoning[sig.Index] = signaturePatch
	return nil
}
