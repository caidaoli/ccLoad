package app

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"ccLoad/internal/protocol"
	translatorcommon "ccLoad/internal/protocol/cliproxy/common"

	"github.com/tidwall/gjson"
)

type xaiResponsesToolIdentity struct{ kind, namespace, name string }

// The same plan follows a request through retries; already lowered function names
// must retain their original custom/search/namespace identity.
type xaiResponsesToolsPlan struct {
	names      map[xaiResponsesToolIdentity]string
	identities map[string]xaiResponsesToolIdentity
}

const xaiCustomToolSchema = `{"type":"object","properties":{"input":{"type":"string","description":"The exact raw input for this tool."}},"required":["input"],"additionalProperties":false}`
const xaiToolSearchSchema = `{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer"}},"required":["query"]}`

func prepareXAIResponsesToolsRequest(body []byte, plan *xaiResponsesToolsPlan) (result []byte, resultPlan *xaiResponsesToolsPlan, err error) {
	defer func() {
		if err != nil {
			err = &protocol.RequestTranslationError{From: protocol.Codex, To: protocol.Codex, Err: err}
		}
	}()
	if !isMutableJSONObject(body) {
		return body, plan, fmt.Errorf("xAI Responses request must be a JSON object")
	}
	if plan == nil {
		plan = &xaiResponsesToolsPlan{names: make(map[xaiResponsesToolIdentity]string), identities: make(map[string]xaiResponsesToolIdentity)}
	}
	root := gjson.ParseBytes(body)
	type declaration struct {
		id  xaiResponsesToolIdentity
		raw gjson.Result
	}
	var declarations []declaration
	var identities []xaiResponsesToolIdentity
	var declarationErr error
	var collect func(gjson.Result, string)
	collect = func(tools gjson.Result, namespace string) {
		for _, tool := range tools.Array() {
			kind := tool.Get("type").String()
			if kind == "namespace" {
				name := tool.Get("name").String()
				if namespace != "" {
					name = namespace + "." + name
				}
				collect(tool.Get("tools"), name)
				continue
			}
			if kind != "function" && kind != "custom" && kind != "tool_search" {
				declarations = append(declarations, declaration{raw: tool})
				continue
			}
			if kind == "tool_search" {
				if execution := tool.Get("execution"); execution.Exists() && execution.String() != "client" {
					declarationErr = fmt.Errorf("xAI tool_search execution must be client")
				}
				if parameters := tool.Get("parameters"); parameters.Exists() && !parameters.IsObject() {
					declarationErr = fmt.Errorf("tool_search.parameters must be a JSON object")
				}
			}
			id := xaiResponsesToolIdentity{kind, namespace, tool.Get("name").String()}
			if kind == "tool_search" {
				id.name = "tool_search"
			}
			if kind == "function" && namespace == "" {
				if original, ok := plan.identities[id.name]; ok {
					id = original
				}
			}
			identities = append(identities, id)
			declarations = append(declarations, declaration{id, tool})
		}
	}
	collect(root.Get("tools"), "")
	for _, item := range root.Get("input").Array() {
		switch item.Get("type").String() {
		case "additional_tools", "tool_search_output":
			collect(xaiDiscoveredTools(item), "")
		case "function_call", "custom_tool_call", "tool_search_call":
			id := xaiToolHistoryIdentity(item)
			if id.kind == "function" && id.namespace == "" {
				if _, lowered := plan.identities[id.name]; lowered {
					continue
				}
			}
			identities = append(identities, id)
		}
	}
	if declarationErr != nil {
		return body, plan, declarationErr
	}
	// Reserve ordinary names before assigning namespace/custom/search aliases.
	for _, id := range identities {
		if id.kind == "function" && id.namespace == "" {
			plan.register(id)
		}
	}
	for _, id := range identities {
		plan.register(id)
	}
	tools := make([]string, 0, len(declarations))
	seen := make(map[string]bool)
	for _, d := range declarations {
		raw := []byte(d.raw.Raw)
		if d.id.kind != "" {
			wireName := plan.names[d.id]
			if seen[wireName] {
				continue
			}
			seen[wireName] = true
			raw = translatorcommon.SetResponsesToolCallIdentity(raw, wireName, "", "")
			raw = setJSONValue(raw, "type", "function")
			raw = deleteJSONPath(raw, "defer_loading")
			// A replayed function already carries the lowered schema and description.
			if d.raw.Get("type").String() == "custom" {
				raw = setJSONRaw(raw, "parameters", xaiCustomToolSchema)
				if format := d.raw.Get("format"); format.Exists() {
					description := d.raw.Get("description").String() + "\nThe input must follow this format: " + format.Raw
					raw = setJSONValue(raw, "description", strings.TrimSpace(description))
				}
				raw = deleteJSONPath(raw, "format")
			} else if d.raw.Get("type").String() == "tool_search" {
				builder := newJSONObjectBuilder()
				builder.Set("type", `"function"`)
				builder.Set("name", jsonEscapedString(wireName))
				builder.Set("description", jsonEscapedString("Search and load tools or namespaces for the current task."))
				parameters := d.raw.Get("parameters")
				if parameters.IsObject() {
					builder.Set("parameters", parameters.Raw)
				} else {
					builder.Set("parameters", xaiToolSearchSchema)
				}
				raw = []byte(builder.String())
			}
		}
		tools = append(tools, string(raw))
	}
	if root.Get("tools").IsArray() || len(tools) > 0 {
		body = setJSONRaw(body, "tools", joinJSONRaw(tools))
	}
	if input := root.Get("input"); input.IsArray() {
		items := make([]string, 0, len(input.Array()))
		for _, item := range input.Array() {
			if item.Get("type").String() == "additional_tools" {
				continue
			}
			raw, err := plan.lowerHistory(item)
			if err != nil {
				return body, plan, err
			}
			items = append(items, string(raw))
		}
		body = setJSONRaw(body, "input", joinJSONRaw(items))
	}
	if choice := root.Get("tool_choice"); choice.IsObject() {
		body = setJSONRaw(body, "tool_choice", string(plan.lowerChoice([]byte(choice.Raw))))
	}
	return body, plan, nil
}

func xaiDiscoveredTools(item gjson.Result) gjson.Result {
	if tools := item.Get("tools"); tools.IsArray() {
		return tools
	}
	output := item.Get("output")
	if output.Type == gjson.String {
		output = gjson.Parse(output.String())
	}
	if output.IsArray() {
		return output
	}
	return output.Get("tools")
}

func xaiToolHistoryIdentity(item gjson.Result) xaiResponsesToolIdentity {
	kind := strings.TrimSuffix(item.Get("type").String(), "_call")
	if kind == "custom_tool" {
		kind = "custom"
	}
	name := item.Get("name").String()
	if kind == "tool_search" {
		name = "tool_search"
	}
	return xaiResponsesToolIdentity{kind, item.Get("namespace").String(), name}
}

func (p *xaiResponsesToolsPlan) register(id xaiResponsesToolIdentity) {
	if _, exists := p.names[id]; exists {
		return
	}
	name := id.name
	if id.namespace != "" {
		name = id.namespace + "__" + name
	}
	valid := name != "" && len(name) <= 64 && strings.IndexFunc(name, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-'
	}) < 0
	for attempt := 0; ; attempt++ {
		if _, taken := p.identities[name]; valid && !taken {
			break
		}
		digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d", id.kind, id.namespace, id.name, attempt)))
		name, valid = fmt.Sprintf("tool_%x", digest[:24]), true
	}
	p.names[id], p.identities[name] = name, id
}

func (p *xaiResponsesToolsPlan) lowerChoice(raw []byte) []byte {
	root := gjson.ParseBytes(raw)
	id := xaiResponsesToolIdentity{root.Get("type").String(), root.Get("namespace").String(), root.Get("name").String()}
	if id.kind == "tool_search" {
		id.name = "tool_search"
	}
	if name, ok := p.names[id]; ok {
		raw = translatorcommon.SetResponsesToolCallIdentity(raw, name, "", "")
		raw = setJSONValue(raw, "type", "function")
	}
	for i, tool := range root.Get("tools").Array() {
		raw = setJSONRaw(raw, "tools."+strconv.Itoa(i), string(p.lowerChoice([]byte(tool.Raw))))
	}
	return raw
}

func (p *xaiResponsesToolsPlan) lowerHistory(item gjson.Result) ([]byte, error) {
	raw := []byte(item.Raw)
	typ := item.Get("type").String()
	switch typ {
	case "function_call", "custom_tool_call", "tool_search_call":
		id := xaiToolHistoryIdentity(item)
		if name, ok := p.names[id]; ok {
			raw = translatorcommon.SetResponsesToolCallIdentity(raw, name, "", "")
		}
		switch typ {
		case "custom_tool_call":
			if item.Get("input").Type != gjson.String {
				return nil, fmt.Errorf("custom_tool_call.input must be a string")
			}
			raw = setJSONValue(raw, "arguments", `{"input":`+item.Get("input").Raw+`}`)
			raw = deleteJSONPath(raw, "input")
		case "tool_search_call":
			if execution := item.Get("execution"); execution.Exists() && execution.String() != "client" {
				return nil, fmt.Errorf("xAI tool_search_call execution must be client")
			}
			arguments, err := xaiToolSearchArguments(item.Get("arguments"))
			if err != nil {
				return nil, err
			}
			raw = setJSONValue(raw, "arguments", arguments)
			raw = deleteJSONPath(raw, "execution")
		}
		raw = setJSONValue(raw, "type", "function_call")
		if typ != "function_call" {
			raw = xaiRetypeToolItemID(raw, "id", "fc_")
		}
	case "custom_tool_call_output", "tool_search_output":
		if typ == "tool_search_output" && strings.TrimSpace(item.Get("call_id").String()) == "" {
			return nil, fmt.Errorf("tool_search_output.call_id must be a non-empty string")
		}
		raw = setJSONValue(raw, "type", "function_call_output")
		output := item.Get("output")
		if typ == "tool_search_output" && !output.Exists() {
			output = item.Get("tools")
			if !output.Exists() {
				return nil, fmt.Errorf("tool_search_output requires output or tools")
			}
		}
		if output.Type == gjson.Null {
			raw = setJSONValue(raw, "output", "")
		} else if output.Type != gjson.String && (typ == "tool_search_output" || !output.IsArray()) {
			raw = setJSONValue(raw, "output", output.Raw)
		}
		for _, field := range []string{"id", "tools", "execution", "status"} {
			raw = deleteJSONPath(raw, field)
		}
	}
	return raw, nil
}

func xaiToolSearchArguments(value gjson.Result) (string, error) {
	if value.Type == gjson.String {
		value = gjson.Parse(value.String())
	}
	if !value.IsObject() || !gjson.Valid(value.Raw) {
		return "", fmt.Errorf("tool_search_call.arguments must be a JSON object")
	}
	return value.Raw, nil
}

func xaiRetypeToolItemID(body []byte, path, prefix string) []byte {
	id := gjson.GetBytes(body, path).String()
	for _, old := range []string{"fc_", "ctc_", "tsc_"} {
		if strings.HasPrefix(id, old) {
			return setJSONValue(body, path, prefix+strings.TrimPrefix(id, old))
		}
	}
	return body
}

type xaiResponsesToolCall struct {
	identity           xaiResponsesToolIdentity
	arguments, emitted string
}

type xaiResponsesToolsRestorer struct {
	plan     *xaiResponsesToolsPlan
	calls    map[string]*xaiResponsesToolCall
	byOutput map[int64]*xaiResponsesToolCall
	nextSeq  int64
	seenSeq  bool
}

func prepareXAIResponsesToolsResponse(resp *http.Response, plan *xaiResponsesToolsPlan, streaming bool) {
	if plan == nil || len(plan.identities) == 0 || resp == nil || resp.Body == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return
	}
	restorer := &xaiResponsesToolsRestorer{plan: plan, calls: make(map[string]*xaiResponsesToolCall), byOutput: make(map[int64]*xaiResponsesToolCall)}
	body := &xaiResponsesToolsReader{ReadCloser: resp.Body, restorer: restorer}
	if responseIsSSE(resp, streaming) {
		body.ReadCloser = resp.Body
		body.scanner = bufio.NewScanner(wrapCodexSSEBody(resp.Body))
		body.scanner.Buffer(make([]byte, SSEBufferSize), maxSSEEventSize)
		body.scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
			if end := firstSSEEventEnd(data); end >= 0 {
				return end, data[:end], nil
			}
			if atEOF && len(data) > 0 {
				return len(data), data, nil
			}
			return 0, nil, nil
		})
	}
	resp.Body = body
	resp.ContentLength = -1
	resp.Header.Del("Content-Length")
}

func (r *xaiResponsesToolsRestorer) restoreItem(raw []byte, partial bool) ([]byte, error) {
	root := gjson.ParseBytes(raw)
	if root.Get("type").String() != "function_call" {
		return raw, nil
	}
	id, ok := r.plan.identities[root.Get("name").String()]
	if !ok {
		return raw, nil
	}
	raw = translatorcommon.SetResponsesToolCallIdentity(raw, id.name, id.namespace, "")
	switch id.kind {
	case "custom":
		input := xaiCustomInput(root.Get("arguments").String())
		if partial {
			input = xaiCustomInputPrefix(root.Get("arguments").String())
		}
		raw = setJSONValue(raw, "type", "custom_tool_call")
		raw = xaiRetypeToolItemID(raw, "id", "ctc_")
		raw = setJSONValue(raw, "input", input)
		raw = deleteJSONPath(raw, "arguments")
	case "tool_search":
		arguments := root.Get("arguments")
		if partial && arguments.String() == "" {
			arguments = gjson.Parse(`{}`)
		}
		object, err := xaiToolSearchArguments(arguments)
		if err != nil {
			return nil, err
		}
		raw = setJSONValue(raw, "type", "tool_search_call")
		raw = xaiRetypeToolItemID(raw, "id", "tsc_")
		raw = setJSONValue(raw, "execution", "client")
		raw = setJSONRaw(raw, "arguments", object)
		raw = deleteJSONPath(deleteJSONPath(raw, "name"), "namespace")
	}
	return raw, nil
}

func xaiCustomInput(arguments string) string {
	if value := gjson.Get(arguments, "input"); gjson.Valid(arguments) && value.Type == gjson.String {
		return value.String()
	}
	return arguments
}

func (r *xaiResponsesToolsRestorer) rewrite(raw []byte) ([][]byte, error) {
	root := gjson.ParseBytes(raw)
	typ := root.Get("type").String()
	call := r.calls[root.Get("item_id").String()]
	if call == nil && root.Get("output_index").Exists() {
		call = r.byOutput[root.Get("output_index").Int()]
	}
	if item := root.Get("item"); item.Get("type").String() == "function_call" {
		if id, ok := r.plan.identities[item.Get("name").String()]; ok {
			if call == nil {
				call = &xaiResponsesToolCall{identity: id}
			}
			if arguments := item.Get("arguments").String(); arguments != "" {
				call.arguments = arguments
			} else if call.arguments != "" {
				item = gjson.ParseBytes(setJSONValue([]byte(item.Raw), "arguments", call.arguments))
			}
			if itemID := item.Get("id").String(); itemID != "" {
				r.calls[itemID] = call
			}
			if root.Get("output_index").Exists() {
				r.byOutput[root.Get("output_index").Int()] = call
			}
		}
		itemRaw, err := r.restoreItem([]byte(item.Raw), typ == "response.output_item.added")
		if err != nil {
			return nil, err
		}
		raw = setJSONRaw(raw, "item", string(itemRaw))
	}
	var out [][]byte
	if strings.HasPrefix(typ, "response.function_call_arguments.") && call != nil {
		if call.identity.kind == "tool_search" {
			if strings.HasSuffix(typ, ".delta") {
				call.arguments += root.Get("delta").String()
			} else if arguments := root.Get("arguments").String(); arguments != "" {
				call.arguments = arguments
			}
			return nil, nil // tool_search exposes its arguments on the output item.
		}
		raw = translatorcommon.SetResponsesToolCallIdentity(raw, call.identity.name, call.identity.namespace, "")
		if call.identity.kind == "custom" {
			raw = xaiRetypeToolItemID(raw, "item_id", "ctc_")
			if strings.HasSuffix(typ, ".delta") {
				call.arguments += root.Get("delta").String()
				input := xaiCustomInputPrefix(call.arguments)
				if len(input) <= len(call.emitted) {
					return nil, nil
				}
				raw = setJSONValue(raw, "type", "response.custom_tool_call_input.delta")
				raw = setJSONValue(raw, "delta", strings.TrimPrefix(input, call.emitted))
				call.emitted = input
			} else {
				if arguments := root.Get("arguments").String(); arguments != "" {
					call.arguments = arguments
				}
				input := xaiCustomInput(call.arguments)
				if !strings.HasPrefix(input, call.emitted) {
					return nil, fmt.Errorf("xAI custom tool input changed after emitted deltas")
				}
				if remainder := strings.TrimPrefix(input, call.emitted); remainder != "" {
					delta := setJSONValue(raw, "type", "response.custom_tool_call_input.delta")
					delta = setJSONValue(deleteJSONPath(delta, "arguments"), "delta", remainder)
					out = append(out, delta)
				}
				call.emitted = input
				raw = setJSONValue(raw, "type", "response.custom_tool_call_input.done")
				raw = setJSONValue(deleteJSONPath(raw, "arguments"), "input", input)
			}
		}
	}
	for _, prefix := range []string{"response.output", "output"} {
		for i, item := range root.Get(prefix).Array() {
			itemRaw, err := r.restoreItem([]byte(item.Raw), false)
			if err != nil {
				return nil, err
			}
			raw = setJSONRaw(raw, prefix+"."+strconv.Itoa(i), string(itemRaw))
		}
	}
	out = append(out, raw)
	if strings.HasPrefix(typ, "response.") {
		for i := range out {
			if seq := root.Get("sequence_number"); seq.Exists() {
				if !r.seenSeq {
					r.nextSeq, r.seenSeq = seq.Int(), true
				}
				out[i] = setJSONValue(out[i], "sequence_number", r.nextSeq)
				r.nextSeq++
			}
		}
	}
	return out, nil
}

// Decode only the complete prefix of the JSON input string. Artificially closing
// it lets encoding/json handle escapes; incomplete escapes and surrogate pairs
// stay buffered until the next argument delta.
func xaiCustomInputPrefix(arguments string) string {
	key := strings.Index(arguments, `"input"`)
	if key < 0 {
		return ""
	}
	value := strings.TrimSpace(arguments[key+len(`"input"`):])
	if !strings.HasPrefix(value, ":") {
		return ""
	}
	value = strings.TrimSpace(value[1:])
	if !strings.HasPrefix(value, `"`) {
		return ""
	}
	end := 1
	for end < len(value) {
		if value[end] == '"' {
			break
		}
		if value[end] == '\\' {
			if end+1 >= len(value) {
				break
			}
			if value[end+1] == 'u' {
				if end+6 > len(value) {
					break
				}
				code, err := strconv.ParseUint(value[end+2:end+6], 16, 16)
				if err != nil {
					break
				}
				if code >= 0xd800 && code <= 0xdbff {
					if end+12 > len(value) || value[end+6:end+8] != `\u` {
						break
					}
					end += 6
				}
				end += 6
			} else {
				end += 2
			}
		} else {
			_, size := utf8.DecodeRuneInString(value[end:])
			end += size
		}
	}
	var decoded string
	if err := json.Unmarshal([]byte(value[:end]+`"`), &decoded); err != nil {
		return ""
	}
	return decoded
}

type xaiResponsesToolsReader struct {
	io.ReadCloser
	restorer *xaiResponsesToolsRestorer
	scanner  *bufio.Scanner
	pending  []byte
	jsonRead bool
}

func (r *xaiResponsesToolsReader) Read(dst []byte) (int, error) {
	if len(dst) == 0 {
		return 0, nil
	}
	for len(r.pending) == 0 {
		if r.scanner == nil {
			if r.jsonRead {
				return 0, io.EOF
			}
			r.jsonRead = true
			raw, err := io.ReadAll(r.ReadCloser)
			if err != nil {
				return 0, err
			}
			payloads, err := r.restorer.rewrite(raw)
			if err != nil {
				return 0, err
			}
			r.pending = bytes.Join(payloads, nil)
			continue
		}
		if !r.scanner.Scan() {
			if err := r.scanner.Err(); err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		frame := bytes.Clone(r.scanner.Bytes())
		_, data := parseSSEEventChunk(frame)
		if firstSSEEventEnd(frame) < 0 || !gjson.ValidBytes(data) {
			r.pending = frame
			continue
		}
		payloads, err := r.restorer.rewrite(data)
		if err != nil {
			return 0, err
		}
		for _, payload := range payloads {
			for _, line := range bytes.SplitAfter(frame, []byte{'\n'}) {
				if bytes.HasPrefix(line, []byte("data:")) || len(bytes.TrimSpace(line)) == 0 {
					continue
				}
				if bytes.HasPrefix(line, []byte("event:")) {
					r.pending = append(r.pending, "event: "+gjson.GetBytes(payload, "type").String()+"\n"...)
				} else {
					r.pending = append(r.pending, line...)
				}
			}
			r.pending = append(r.pending, "data: "...)
			r.pending = append(r.pending, payload...)
			r.pending = append(r.pending, '\n', '\n')
		}
	}
	n := copy(dst, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}
