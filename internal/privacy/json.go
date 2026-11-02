package privacy

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
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
	root, signed     bool
	known            []storedValue
}

// MaskJSON preserves protocol identifiers, names, schemas, signatures and
// binary/URL fields. Tool payload keys belong to the user: only their values
// are scanned, including JSON embedded in a tool's arguments string.
func (s *Session) MaskJSON(body []byte) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	// Keep this dictionary for every masking pass and retry in the session.
	// Unsigned discoveries on the first pass cannot change a later signed pass.
	known := s.signedDictionary()
	if !s.config.Enabled && (len(known) == 0 || !json.Valid(body)) {
		return body, nil
	}
	return s.transformJSON(body, false, jsonContext{root: true, known: known})
}

// RestoreJSON restores string values with JSON-safe escaping, including the
// second level of escaping in stringified tool arguments and argument deltas.
// Unchanged documents are returned byte-for-byte, including numeric literals.
func (s *Session) RestoreJSON(body []byte) ([]byte, error) {
	if !bytes.Contains(body, []byte("{{CCLOAD_")) {
		return body, nil
	}
	return s.transformJSON(body, true, jsonContext{root: true})
}

func (s *Session) transformJSON(body []byte, restore bool, context jsonContext) ([]byte, error) {
	if len(body) > maxTextBytes && (restore || s.config.Enabled) {
		return nil, errors.New("privacy JSON exceeds 64 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil || !json.Valid(body) {
		return nil, errors.New("privacy requires a valid JSON document")
	}
	changed := false
	transformed, err := s.transformValue(value, restore, context, &changed, 0)
	if err != nil {
		return nil, err
	}
	if !changed {
		return body, nil
	}
	out, err := json.Marshal(transformed)
	if err != nil {
		return nil, errors.New("privacy could not encode JSON")
	}
	return out, nil
}

func (s *Session) transformValue(value any, restore bool, context jsonContext, changed *bool, depth int) (any, error) {
	if depth > 128 && (restore || s.config.Enabled) {
		return nil, errors.New("privacy JSON nesting exceeds 128 levels")
	}
	switch item := value.(type) {
	case map[string]any:
		event, _ := item["type"].(string)
		role, _ := item["role"].(string)
		signed := context.signed || !context.data && signedObject(item)
		for key, child := range item {
			ctx := jsonContext{key: key, event: event, data: context.data, definition: context.definition, signed: signed, known: context.known}
			if context.root && (definitionKeys[key] || key == "text" && isObject(child)) {
				ctx.definition = true
			}
			if event == "additional_tools" || event == "tool_search_output" {
				ctx.definition = true
			}
			if key == "input" && (event == "tool_use" || event == "server_tool_use") ||
				key == "args" && (context.key == "functionCall" || context.key == "function_call") ||
				key == "response" && (context.key == "functionResponse" || context.key == "function_response") ||
				key == "arguments" || key == "output" && event == "function_call_output" || key == "content" && role == "tool" {
				ctx.data = true
			}
			if !ctx.data && (protocolKeys[key] || identifierKey(key)) {
				continue
			}
			if !ctx.data && (key == "image_url" || key == "input_audio" || key == "audio" && isObject(child) || key == "source" && isObject(child) && isBinarySource(child)) {
				continue
			}
			transformed, err := s.transformValue(child, restore, ctx, changed, depth+1)
			if err != nil {
				return nil, err
			}
			item[key] = transformed
		}
		return item, nil
	case []any:
		for i, child := range item {
			ctx := context
			ctx.root = false
			transformed, err := s.transformValue(child, restore, ctx, changed, depth+1)
			if err != nil {
				return nil, err
			}
			item[i] = transformed
		}
		return item, nil
	case string:
		if strings.HasPrefix(item, "data:") || context.definition && context.key != "description" {
			return item, nil
		}
		var out string
		var err error
		arguments := context.key == "arguments" || context.key == "partial_json" || context.key == "delta" && (strings.Contains(context.event, "arguments") || context.event == "input_json_delta")
		if !restore && context.signed {
			out = remaskKnown(item, context.known)
		} else if !restore && !s.config.Enabled {
			return item, nil
		} else if arguments && json.Valid([]byte(item)) {
			var nested []byte
			nested, err = s.transformJSON([]byte(item), restore, jsonContext{data: true, known: context.known})
			out = string(nested)
		} else if restore {
			out = s.restore(item, arguments)
		} else if context.data && s.config.Secrets && secretName(context.key) && len(item) >= 8 && realValue(item) && !s.allowed(item) {
			out, err = s.apply(item, []match{{0, len(item), "SECRET", ""}})
		} else {
			out, err = s.MaskText(item)
		}
		if err != nil {
			return nil, err
		}
		if out != item {
			*changed = true
		}
		return out, nil
	default:
		return value, nil
	}
}

func isObject(value any) bool { _, ok := value.(map[string]any); return ok }
func isBinarySource(value any) bool {
	item, _ := value.(map[string]any)
	typ, _ := item["type"].(string)
	return typ == "base64" || typ == "url" || item["media_type"] != nil
}

// A signature is meaningful only on protocol thinking blocks or Gemini parts,
// not on a user's arbitrary tool-argument object with a similarly named key.
func signedObject(item map[string]any) bool {
	nonempty := func(key string) bool { value, _ := item[key].(string); return value != "" }
	if nonempty("thoughtSignature") || nonempty("thought_signature") {
		return true
	}
	typ, _ := item["type"].(string)
	return nonempty("signature") && (typ == "thinking" || typ == "reasoning" || strings.HasPrefix(typ, "reasoning."))
}
