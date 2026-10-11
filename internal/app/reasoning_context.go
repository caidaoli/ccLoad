package app

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"ccLoad/internal/protocol"

	"github.com/tidwall/gjson"
)

const reasoningContextMaxBytes = 24 * 1024

type reasoningProjection struct {
	text       string
	truncated  bool
	incomplete bool
}

func jsonPathExists(body []byte, path string) bool { return gjson.GetBytes(body, path).Exists() }

// Only conversation fields enter the decision state. Tool definitions, credentials
// and client sampling parameters are not needed to estimate the next step.
// withText=false only checks completeness, for turns that reuse the last level.
func projectReasoningContext(body []byte, client protocol.Protocol, withText bool) reasoningProjection {
	p := reasoningProjection{}
	if !json.Valid(body) {
		p.incomplete = true
		return p
	}
	if gjson.GetBytes(body, "previous_response_id").String() != "" {
		p.incomplete = true
	}
	if requestHasNonTextContent(client, body) {
		p.incomplete = true
	}
	items := gjson.GetBytes(body, "messages")
	if client == protocol.Codex {
		items = gjson.GetBytes(body, "input")
	}
	var lines []string
	var instructions []string
	var firstUser, lastUser string
	if value := gjson.GetBytes(body, "instructions"); value.Type == gjson.String && withText {
		line := "instructions: " + value.String()
		instructions = append(instructions, line)
		lines = append(lines, line)
	}
	hasUser := false
	if items.Type == gjson.String {
		hasUser = strings.TrimSpace(items.String()) != ""
		if withText {
			lines = append(lines, "user: "+items.String())
			firstUser, lastUser = items.String(), items.String()
		}
	} else if items.IsArray() {
		items.ForEach(func(_, item gjson.Result) bool {
			role := item.Get("role").String()
			kind := item.Get("type").String()
			if client == protocol.Codex {
				switch kind {
				case "", "message", "reasoning", "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output":
				default:
					p.incomplete = true
				}
			}
			if role == "user" {
				hasUser = true
			}
			if role == "" {
				role = kind
			}
			content := item.Get("content")
			// Responses tool outputs can carry image/file parts directly or as
			// JSON encoded in a string. Inspect them even when reusing a level;
			// ordinary JSON tool results remain text.
			if kind == "function_call_output" || kind == "custom_tool_call_output" {
				output := item.Get("output")
				if output.Type == gjson.String && gjson.Valid(output.String()) {
					output = gjson.Parse(output.String())
				}
				if output.IsArray() {
					output.ForEach(func(_, part gjson.Result) bool {
						switch part.Get("type").String() {
						case "input_image", "image_url", "input_file":
							p.incomplete = true
						}
						return !p.incomplete
					})
				}
			}
			// Chat tool calls may explicitly carry null instead of omitting content.
			toolOnly := client == protocol.OpenAI && role == "assistant" && content.Type == gjson.Null &&
				item.Get("tool_calls").IsArray() && item.Get("tool_calls.#").Int() > 0
			if (role == "" && kind == "") || (content.Exists() && content.Type != gjson.String && !content.IsArray() && !toolOnly) {
				p.incomplete = true
			}
			var text strings.Builder
			if content.Type == gjson.String && withText {
				text.WriteString(content.String())
			}
			if content.IsArray() {
				content.ForEach(func(_, part gjson.Result) bool {
					switch part.Get("type").String() {
					case "text", "input_text", "output_text":
						if withText {
							text.WriteString(part.Get("text").String())
							text.WriteByte('\n')
						}
					default:
						p.incomplete = true
					}
					return true
				})
			}
			if !withText {
				return true
			}
			for _, path := range []string{"name", "call_id", "tool_call_id", "arguments", "input", "output"} {
				if value := item.Get(path); value.Exists() {
					text.WriteString("\n" + path + ": " + value.String())
				}
			}
			if calls := item.Get("tool_calls"); calls.Exists() {
				text.WriteString("\ntool_calls: " + calls.Raw)
			}
			if text.Len() > 0 {
				line := role + ": " + text.String()
				lines = append(lines, line)
				if role == "system" || role == "developer" || (client == protocol.Codex && role == "user" &&
					codexInjectedUserContext(text.String()) && !strings.HasPrefix(strings.TrimSpace(text.String()), "<environment_context>")) {
					instructions = append(instructions, line)
				}
				if role == "user" {
					if firstUser == "" && (client != protocol.Codex || !codexInjectedUserContext(text.String())) {
						firstUser = text.String()
					}
					lastUser = text.String()
				}
			}
			return true
		})
	} else {
		p.incomplete = true
	}
	if !hasUser {
		p.incomplete = true
	}
	p.text = strings.Join(lines, "\n\n")
	if len(p.text) > reasoningContextMaxBytes {
		p.truncated = true
		const marker = "\n[conversation truncated; omitted middle content]\n"
		first := "original task: " + truncateUTF8(firstUser, 4*1024) + "\nlatest user request: " + truncateUTF8(lastUser, 4*1024)
		if len(instructions) > 0 {
			// Share a separate bounded budget across instruction sources so a
			// large system message cannot displace developer or Codex rules.
			limit := 4*1024/len(instructions) - 1
			for _, instruction := range instructions {
				if limit <= 0 {
					break
				}
				first += "\n" + truncateUTF8(instruction, limit)
			}
		}
		remaining := reasoningContextMaxBytes - len(first) - len(marker)
		tail := p.text[len(p.text)-remaining:]
		for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
			tail = tail[1:]
		}
		p.text = first + marker + tail
	}
	return p
}

// Codex CLI sends AGENTS.md and environment context as leading user messages;
// they are not the original task.
func codexInjectedUserContext(text string) bool {
	text = strings.TrimSpace(text)
	for _, prefix := range []string{"# AGENTS.md instructions", "<user_instructions>", "<environment_context>"} {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

func truncateUTF8(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}

// Inspect the newly supplied input, not the full replay, to avoid treating a
// historical user message as fresh input on every tool continuation.
func reasoningTurnInput(body []byte, client protocol.Protocol) (user, failed bool) {
	items := gjson.GetBytes(body, "messages")
	if client == protocol.Codex {
		items = gjson.GetBytes(body, "input")
	}
	if items.Type == gjson.String {
		return true, false
	}
	array := items.Array()
	if len(array) == 0 {
		return false, false
	}
	last := array[len(array)-1]
	user = last.Get("role").String() == "user"
	for i := len(array) - 1; i >= 0; i-- {
		item := array[i]
		tool := item.Get("role").String() == "tool" || strings.HasSuffix(item.Get("type").String(), "_call_output")
		if !tool {
			break
		}
		failed = failed || reasoningToolOutputFailed(item)
	}
	return user, failed
}

// Structured status wins. Chat Completions tool results carry none, so only the
// first line counts as a status line; markers deep in file or log dumps do not.
func reasoningToolOutputFailed(item gjson.Result) bool {
	if item.Get("is_error").Bool() {
		return true
	}
	value := item.Get("content")
	if output := item.Get("output"); output.Exists() {
		value = output
	}
	text := value.String()
	if value.IsArray() {
		// Content parts: the status line is the first line of the joined text.
		var parts []string
		value.ForEach(func(_, part gjson.Result) bool {
			if part.Get("text").Exists() {
				parts = append(parts, part.Get("text").String())
			}
			return true
		})
		text = strings.Join(parts, "\n")
	}
	if code := gjson.Get(text, "metadata.exit_code"); gjson.Valid(text) && code.Exists() {
		return code.Type == gjson.Number && code.Int() != 0
	}
	if rest, ok := strings.CutPrefix(text, "Exit code: "); ok {
		code, _, _ := strings.Cut(rest, "\n")
		n, err := strconv.Atoi(strings.TrimSpace(code))
		return err == nil && n != 0
	}
	head, _, _ := strings.Cut(text, "\n")
	head = strings.ToLower(truncateUTF8(head, 256))
	for _, marker := range []string{"error:", "failed", "failure", "conflict", "new constraint", "new requirement", "约束变更", "新约束", "失败", "冲突"} {
		if strings.Contains(head, marker) {
			return true
		}
	}
	return false
}

// A non-stream HTTP continuation may read locally owned WS history for the
// decision, while its upstream request retains ordinary HTTP wire semantics.
func (s *Server) localReasoningTranscript(r *proxyRequestContext) []byte {
	if s.responsesExecutionSessions == nil || r.clientProtocol != protocol.Codex || gjson.GetBytes(r.body, "previous_response_id").String() == "" {
		return nil
	}
	identity := responsesExecutionSessionID(r.header)
	if identity == "" || r.tokenHash == "" {
		return nil
	}
	store := s.responsesExecutionSessions
	store.mu.Lock()
	session := store.sessions[responsesExecutionSessionKey(r.tokenHash, identity)]
	if session == nil {
		store.mu.Unlock()
		return nil
	}
	select {
	case session.turn <- struct{}{}:
	default:
		store.mu.Unlock()
		return nil
	}
	session.lastAccess = time.Now()
	store.mu.Unlock()
	defer session.releaseTurn()
	replay, _, local, err := session.transcript.normalizeHTTPRequests(r.body)
	if err != nil || !local {
		return nil
	}
	return bytes.Clone(replay)
}
