package privacy

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

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
}

// MaskJSON preserves protocol identifiers, names, schemas, signatures and
// binary/URL fields. Tool payload keys belong to the user: only their values
// are scanned, including JSON embedded in a tool's arguments string. Signed
// reasoning is replayed verbatim; see signedObject. Only changed string
// literals are rewritten, so everything else stays byte-for-byte.
func (s *Session) MaskJSON(body []byte) ([]byte, error) {
	if !s.Enabled() {
		return body, nil
	}
	return s.transformJSON(body, false)
}

// RestoreJSON restores string values with JSON-safe escaping, including the
// second level of escaping in stringified tool arguments and argument deltas.
// Reasoning content stays masked; see reasoningObject.
func (s *Session) RestoreJSON(body []byte) ([]byte, error) {
	if !bytes.Contains(body, []byte("{{CCLOAD_")) {
		return body, nil
	}
	return s.transformJSON(body, true)
}

func (s *Session) transformJSON(body []byte, restore bool) ([]byte, error) {
	if len(body) > maxTextBytes {
		return nil, errors.New("privacy JSON exceeds 64 MiB")
	}
	// encoding/json validates iteratively; gjson's validator recurses without a
	// depth limit.
	if !json.Valid(body) {
		return nil, errors.New("privacy requires a valid JSON document")
	}
	out, changed, err := s.transformDocument(string(body), restore, jsonContext{root: true})
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
	src     string
	out     strings.Builder
	last    int
	changed bool
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

func (s *Session) transformDocument(text string, restore bool, ctx jsonContext) (string, bool, error) {
	p := &jsonPatcher{src: text}
	if err := s.walk(gjson.Parse(text), restore, ctx, p, 0); err != nil {
		return "", false, err
	}
	if !p.changed {
		return text, false, nil
	}
	p.out.WriteString(text[p.last:])
	return p.out.String(), true, nil
}

type jsonMember struct {
	key   string
	value gjson.Result
}

// last returns the member that encoding/json would decode for key.
func last(members []jsonMember, key string) gjson.Result {
	for i := len(members) - 1; i >= 0; i-- {
		if members[i].key == key {
			return members[i].value
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
		members = append(members, jsonMember{key.Str, value})
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
		ctx.root = false
		var err error
		value.ForEach(func(_, child gjson.Result) bool {
			err = s.walk(child, restore, ctx, p, depth+1)
			return err == nil
		})
		return err
	case value.Type == gjson.String:
		return s.walkString(value, restore, ctx, p)
	}
	return nil
}

func (s *Session) walkObject(members []jsonMember, restore bool, parent jsonContext, p *jsonPatcher, depth int) error {
	event, role := lastString(members, "type"), lastString(members, "role")
	if !parent.data && (restore && reasoningObject(members, event) || !restore && signedObject(members, event)) {
		return nil
	}
	for _, member := range members {
		key, child := member.key, member.value
		ctx := jsonContext{key: key, event: event, data: parent.data, definition: parent.definition}
		if parent.root && (definitionKeys[key] || key == "text" && child.IsObject()) {
			ctx.definition = true
		}
		if event == "additional_tools" || event == "tool_search_output" {
			ctx.definition = true
		}
		if key == "input" && (event == "tool_use" || event == "server_tool_use") ||
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

func (s *Session) walkString(value gjson.Result, restore bool, ctx jsonContext, p *jsonPatcher) error {
	item := value.Str
	if strings.HasPrefix(item, "data:") || ctx.definition && ctx.key != "description" {
		return nil
	}
	if restore && !strings.Contains(item, "{{CCLOAD_") {
		return nil
	}
	arguments := ctx.key == "arguments" || ctx.key == "partial_json" || ctx.key == "delta" && (strings.Contains(ctx.event, "arguments") || ctx.event == "input_json_delta")
	var out string
	var err error
	switch {
	case arguments && json.Valid([]byte(item)):
		out, _, err = s.transformDocument(item, restore, jsonContext{data: true})
	case restore:
		out = s.restore(item, arguments)
	case ctx.data && s.policy.config.Secrets && secretName(ctx.key) && len(item) >= 8 && realValue(item) && !s.policy.allowed(item):
		out, err = s.apply(item, []match{{0, len(item), "SECRET", ""}})
	default:
		out, err = s.MaskText(item)
	}
	if err != nil || out == item {
		return err
	}
	return p.replace(value, jsonQuote(out))
}

func isBinarySource(value gjson.Result) bool {
	members := objectMembers(value)
	typ := lastString(members, "type")
	mediaType := last(members, "media_type")
	return typ == "base64" || typ == "url" || mediaType.Exists() && mediaType.Type != gjson.Null
}

// signedObject reports vendor-signed reasoning: Anthropic/OpenRouter thinking
// with a signature, or a Gemini thought part with a thought signature. Its
// content is what the vendor produced from already-masked input, so it is
// replayed verbatim; masking or restoring it would invalidate the signature.
func signedObject(members []jsonMember, event string) bool {
	if (lastString(members, "thoughtSignature") != "" || lastString(members, "thought_signature") != "") && last(members, "thought").Type == gjson.True {
		return true
	}
	return lastString(members, "signature") != "" && (event == "thinking" || event == "reasoning" || strings.HasPrefix(event, "reasoning."))
}

// reasoningObject is model reasoning the client may replay as signed content,
// including stream deltas that carry no signature themselves. It is returned
// with placeholders so a replay matches what the vendor signed.
func reasoningObject(members []jsonMember, event string) bool {
	return event == "thinking" || event == "thinking_delta" || strings.HasPrefix(event, "reasoning.") ||
		last(members, "thought").Type == gjson.True || signedObject(members, event)
}
