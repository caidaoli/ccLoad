package qoderauth

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

/**
 * @brief Tool wrappers delimit Qoder's JSON or XML tool-call text.
 */
const (
	qoderToolOpen  = "<tool_call>"
	qoderToolClose = "</tool_call>"
)

/**
 * @brief XML matchers implement the Qwen and Anthropic function-call forms.
 */
var (
	qoderInvokeOpen     = regexp.MustCompile(`^<(?:antml:)?invoke\s+name\s*=\s*"([^"<>]+)"\s*>`)
	qoderFunctionOpen   = regexp.MustCompile(`^<function=([^\s<>]+)\s*>`)
	qoderInvokeClose    = regexp.MustCompile(`</(?:antml:)?invoke\s*>`)
	qoderFunctionClose  = regexp.MustCompile(`</function\s*>`)
	qoderNextCall       = regexp.MustCompile(`<(?:antml:)?invoke\s+name\s*=|<function=`)
	qoderParameterOpen  = regexp.MustCompile(`<(?:antml:)?parameter(?:\s+name\s*=\s*"([^"]+)"|=([^\s>]+))\s*>`)
	qoderParameterClose = regexp.MustCompile(`</(?:antml:)?parameter\s*>`)
	qoderParameterLines = regexp.MustCompile(`^\r?\n|\r?\n$`)
	qoderCallJSON       = regexp.MustCompile(`"(?:name|arguments)"\s*:`)
)

/**
 * @brief qoderTextCall is a complete call extracted from assistant text.
 */
type qoderTextCall struct {
	name      string
	arguments string
}

/**
 * @brief qoderTextTools buffers potential calls while preserving quoted examples.
 */
type qoderTextTools struct {
	tools    map[string]map[string]any
	buffer   string
	after    bool
	fence    byte
	inline   bool
	quote    byte
	quoteRun int
}

/**
 * @brief newQoderTextTools indexes the request's declared function schemas.
 * @param tools Declared Chat function schemas.
 * @return A text parser restricted to declared function names.
 */
func newQoderTextTools(tools []map[string]any) qoderTextTools {
	declared := make(map[string]map[string]any, len(tools))
	for _, tool := range tools {
		function, _ := tool["function"].(map[string]any)
		name, _ := function["name"].(string)
		if name != "" {
			parameters, _ := function["parameters"].(map[string]any)
			properties, _ := parameters["properties"].(map[string]any)
			declared[name] = properties
		}
	}
	return qoderTextTools{tools: declared}
}

/**
 * @brief quoteEnd applies a completed backtick or tilde run to the quoting state.
 */
func (s *qoderTextTools) quoteEnd() {
	if s.quoteRun >= 3 && !s.inline {
		switch s.fence {
		case 0:
			s.fence = s.quote
		case s.quote:
			s.fence = 0
		}
	} else if s.quote == '`' && s.fence == 0 {
		s.inline = !s.inline
	}
	s.quote, s.quoteRun = 0, 0
}

/**
 * @brief text records quote delimiters in text which is passed to the client.
 * @param out Ordinary text output.
 * @param value Text already classified as ordinary prose.
 */
func (s *qoderTextTools) text(out *strings.Builder, value string) {
	out.WriteString(value)
	for i := range len(value) {
		char := value[i]
		if char == '`' || char == '~' {
			if char != s.quote {
				s.quoteEnd()
				s.quote = char
			}
			s.quoteRun++
		} else {
			s.quoteEnd()
		}
	}
}

/**
 * @brief feed emits ordinary text and only complete, declared tool calls.
 * @param value Next assistant text fragment.
 * @param final Whether a real terminal event has arrived.
 * @return Ordinary text, complete calls, and any malformed-call error.
 */
func (s *qoderTextTools) feed(value string, final bool) (string, []qoderTextCall, error) {
	s.buffer += value
	var out strings.Builder
	var calls []qoderTextCall
	for s.buffer != "" {
		if s.after {
			s.buffer = strings.TrimLeft(s.buffer, " \t\r\n")
			dropped, wait := false, false
			for _, closeTag := range []string{"</function_calls>", "</antml:function_calls>", "</invoke>", "</antml:invoke>", "</function>"} {
				if strings.HasPrefix(s.buffer, closeTag) {
					s.buffer = s.buffer[len(closeTag):]
					dropped = true
					break
				}
				wait = wait || strings.HasPrefix(closeTag, s.buffer)
			}
			if dropped {
				continue
			}
			if !final && wait {
				break
			}
			s.after = false
		}
		at := strings.IndexByte(s.buffer, '<')
		if at < 0 {
			s.text(&out, s.buffer)
			s.buffer = ""
			break
		}
		s.text(&out, s.buffer[:at])
		s.buffer = s.buffer[at:]
		s.quoteEnd()
		if s.fence != 0 || s.inline || len(s.tools) == 0 {
			s.text(&out, "<")
			s.buffer = s.buffer[1:]
			continue
		}
		call, consumed, wait, err := s.call(s.buffer, final)
		if err != nil {
			return "", nil, err
		}
		if wait {
			break
		}
		if call != nil {
			calls = append(calls, *call)
			s.after = !strings.HasPrefix(s.buffer, qoderToolOpen)
			s.buffer = s.buffer[consumed:]
		} else {
			s.text(&out, s.buffer[:consumed])
			s.buffer = s.buffer[consumed:]
		}
	}
	return out.String(), calls, nil
}

/**
 * @brief call distinguishes a tool wrapper, a bare XML call, and ordinary markup.
 * @param value Text beginning with a possible call opener.
 * @param final Whether a real terminal event has arrived.
 * @return A call, consumed byte count, wait flag, and parse error.
 */
func (s *qoderTextTools) call(value string, final bool) (*qoderTextCall, int, bool, error) {
	if strings.HasPrefix(value, qoderToolOpen) {
		end := strings.Index(value[len(qoderToolOpen):], qoderToolClose)
		if end < 0 && !final {
			return nil, 0, true, nil
		}
		bodyEnd, consumed := len(value), len(value)
		if end >= 0 {
			bodyEnd = len(qoderToolOpen) + end
			consumed = bodyEnd + len(qoderToolClose)
		}
		body := strings.TrimSpace(value[len(qoderToolOpen):bodyEnd])
		var decoded struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		decodeErr := json.Unmarshal([]byte(body), &decoded)
		decoded.Name = strings.TrimSpace(decoded.Name)
		if decoded.Name != "" {
			if _, declared := s.tools[decoded.Name]; !declared {
				return nil, consumed, false, nil
			}
		}
		if decodeErr == nil && decoded.Name != "" && decoded.Arguments != nil {
			args, _ := json.Marshal(decoded.Arguments)
			return &qoderTextCall{name: decoded.Name, arguments: string(args)}, consumed, false, nil
		}
		call, _, _, err := s.xmlCall(body, true)
		if err != nil {
			return nil, 0, false, err
		}
		if call != nil {
			return call, consumed, false, nil
		}
		if (strings.HasPrefix(body, "{") && qoderCallJSON.MatchString(body)) || body == "{" {
			return nil, 0, false, errors.New("text tool call is incomplete or invalid")
		}
		return nil, consumed, false, nil
	}
	return s.xmlCall(value, final)
}

/**
 * @brief xmlCall reads Qwen and Anthropic call tags using declared parameter types.
 * @param value Text beginning with possible XML call markup.
 * @param final Whether a real terminal event has arrived.
 * @return A declared call, consumed bytes, wait flag, and parse error.
 */
func (s *qoderTextTools) xmlCall(value string, final bool) (*qoderTextCall, int, bool, error) {
	prefix := 0
	for _, wrapper := range []string{"<function_calls>", "<antml:function_calls>"} {
		if strings.HasPrefix(value, wrapper) {
			rest := strings.TrimLeft(value[len(wrapper):], " \t\r\n")
			prefix = len(value) - len(rest)
			value = rest
			break
		}
	}
	match := qoderInvokeOpen.FindStringSubmatch(value)
	closePattern := qoderInvokeClose
	if match == nil {
		match = qoderFunctionOpen.FindStringSubmatch(value)
		closePattern = qoderFunctionClose
	}
	if match == nil {
		if !final {
			for _, mark := range []string{qoderToolOpen, "<function_calls>", "<antml:function_calls>", "<invoke ", "<antml:invoke ", "<function="} {
				if strings.HasPrefix(mark, value) || ((strings.HasPrefix(value, "<invoke ") || strings.HasPrefix(value, "<antml:invoke ") || strings.HasPrefix(value, "<function=")) && !strings.Contains(value, ">")) {
					return nil, 0, true, nil
				}
			}
		}
		return nil, max(prefix, 1), false, nil
	}
	name := strings.TrimSpace(match[1])
	properties, declared := s.tools[name]
	if !declared {
		return nil, prefix + len(match[0]), false, nil
	}
	body := value[len(match[0]):]
	closeAt := closePattern.FindStringIndex(body)
	nextAt := qoderNextCall.FindStringIndex(body)
	stop := len(body)
	whole := closeAt != nil
	if whole {
		stop = closeAt[0]
	}
	if nextAt != nil && (!whole || nextAt[0] < stop) {
		stop, whole = nextAt[0], false
	}
	if !whole && nextAt == nil && !final {
		return nil, 0, true, nil
	}
	parameters := qoderParameterOpen.FindAllStringSubmatchIndex(body[:stop], -1)
	if !whole && len(parameters) == 0 {
		return nil, prefix + len(match[0]), false, nil
	}
	arguments := map[string]any{}
	for i, parameter := range parameters {
		end := stop
		if i+1 < len(parameters) {
			end = parameters[i+1][0]
		}
		text := body[parameter[1]:end]
		closeParameter := qoderParameterClose.FindStringIndex(text)
		if closeParameter == nil {
			return nil, 0, false, errors.New("text tool call has an incomplete parameter")
		}
		keyAt := parameter[2:4]
		if keyAt[0] < 0 {
			keyAt = parameter[4:6]
		}
		key := body[keyAt[0]:keyAt[1]]
		text = qoderParameterLines.ReplaceAllString(text[:closeParameter[0]], "")
		prop, _ := properties[key].(map[string]any)
		var parsed any
		if prop["type"] == "string" || json.Unmarshal([]byte(text), &parsed) != nil {
			parsed = text
		}
		arguments[key] = parsed
	}
	args, _ := json.Marshal(arguments)
	consumed := prefix + len(match[0]) + stop
	if whole {
		consumed += closeAt[1] - closeAt[0]
	}
	return &qoderTextCall{name: name, arguments: string(args)}, consumed, false, nil
}
