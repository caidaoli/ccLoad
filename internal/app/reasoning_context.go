package app

import (
	"bytes"
	"encoding/json"
	"regexp"
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
	incomplete bool
}

// reasoningEntry is one rendered conversation step, independent of the client
// protocol that carried it.
type reasoningEntry struct {
	role        string
	text        string
	instruction bool // joins the bounded instruction digest kept on truncation
	task        bool // a real user request, not client-injected context
	delegated   bool // a parent agent's task or follow-up, for subagent threads
}

type reasoningWalk struct {
	withText   bool
	entries    []reasoningEntry
	hasUser    bool
	incomplete bool // malformed structure or history held upstream
	media      bool // image/file in the input after the last model output
	parent     string
}

// Fernet tokens: Codex encrypts agent messages and reasoning content this way,
// in JSON arguments as well as in code-mode sources. Signed thinking blocks and
// thought signatures never reach the projection.
var reasoningEncryptedPayload = regexp.MustCompile(`gAAAAA[0-9A-Za-z_-]{60,}=*`)

// The judge only sees text. Media and unknown parts become placeholders so one
// replayed screenshot or new client item type does not void every later turn;
// only media in the newest input, which the judge cannot see, does.
func (w *reasoningWalk) placeholder(text *strings.Builder, kind string, media bool) {
	w.media = w.media || media
	if w.withText {
		text.WriteString("[" + kind + "]\n")
	}
}

func (w *reasoningWalk) add(entry reasoningEntry) {
	if w.withText && entry.text != "" {
		if strings.Contains(entry.text, "gAAAAA") {
			entry.text = reasoningEncryptedPayload.ReplaceAllLiteralString(entry.text, "[encrypted]")
		}
		w.entries = append(w.entries, entry)
	}
}

// Only conversation fields enter the decision state. Tool definitions, credentials
// and client sampling parameters are not needed to estimate the next step.
// withText=false only checks completeness, for turns that reuse the last level.
func projectReasoningContext(body []byte, client protocol.Protocol, withText bool) reasoningProjection {
	p := reasoningProjection{}
	if !json.Valid(body) {
		p.incomplete = true
		return p
	}
	w := &reasoningWalk{withText: withText}
	// HTTP and WS continuations reach here with local history already merged;
	// a remaining previous_response_id means the history lives upstream.
	if gjson.GetBytes(body, "previous_response_id").String() != "" {
		w.incomplete = true
	}
	switch client {
	case protocol.OpenAI, protocol.Codex:
		w.responses(body, client)
	case protocol.Anthropic:
		w.anthropic(body)
	case protocol.Gemini:
		w.gemini(body)
	default:
		w.incomplete = true
	}
	p.incomplete = w.incomplete || w.media || !w.hasUser
	var lines, instructions, conversation []string
	var firstUser, lastUser, firstDelegated, lastDelegated string
	for _, entry := range w.entries {
		line := entry.role + ": " + entry.text
		lines = append(lines, line)
		if entry.instruction {
			instructions = append(instructions, line)
		} else {
			conversation = append(conversation, line)
		}
		if entry.task {
			if firstUser == "" {
				firstUser = entry.text
			}
			lastUser = entry.text
		}
		if entry.delegated {
			if firstDelegated == "" {
				firstDelegated = entry.text
			}
			lastDelegated = entry.text
		}
	}
	// Subagent threads carry only injected user context; their task comes
	// from the parent agent.
	if firstUser == "" {
		firstUser, lastUser = firstDelegated, lastDelegated
	}
	p.text = strings.Join(lines, "\n\n")
	if len(p.text) > reasoningContextMaxBytes {
		const marker = "\n[context truncated; instructions abridged above, conversation tail follows]\n"
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
		// The tail is conversation only: instructions already have their digest,
		// and full system prompts would otherwise crowd out the actual work.
		tail := strings.Join(conversation, "\n\n")
		if remaining := reasoningContextMaxBytes - len(first) - len(marker); len(tail) > remaining {
			tail = tail[len(tail)-remaining:]
		}
		for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
			tail = tail[1:]
		}
		p.text = first + marker + tail
	}
	return p
}

// OpenAI Chat messages and Responses input items.
func (w *reasoningWalk) responses(body []byte, client protocol.Protocol) {
	items := gjson.GetBytes(body, "messages")
	if client == protocol.Codex {
		items = gjson.GetBytes(body, "input")
	}
	if value := gjson.GetBytes(body, "instructions"); value.Type == gjson.String {
		w.add(reasoningEntry{role: "instructions", text: value.String(), instruction: true})
	}
	switch {
	case items.Type == gjson.String:
		w.hasUser = strings.TrimSpace(items.String()) != ""
		w.add(reasoningEntry{role: "user", text: items.String(), task: true})
	case items.IsArray():
		items.ForEach(func(_, item gjson.Result) bool {
			w.responsesItem(item, client)
			return true
		})
	default:
		w.incomplete = true
	}
}

func (w *reasoningWalk) responsesItem(item gjson.Result, client protocol.Protocol) {
	if reasoningMetadataItem(client, item) {
		return
	}
	role := item.Get("role").String()
	kind := item.Get("type").String()
	// Model output (messages, reasoning and every *_call) closes the previous input.
	if role == "assistant" || kind == "reasoning" || strings.HasSuffix(kind, "_call") {
		w.media = false
	}
	if role == "user" {
		w.hasUser = true
	}
	if role == "" {
		role = kind
	}
	var text, reminders strings.Builder
	switch kind {
	case "", "message", "reasoning", "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output", "agent_message":
	default:
		// Other Codex items (compaction, web_search_call, ...) only mark their place.
		w.placeholder(&text, kind, false)
	}
	content := item.Get("content")
	// Chat tool calls may explicitly carry null instead of omitting content.
	toolOnly := client == protocol.OpenAI && role == "assistant" && content.Type == gjson.Null &&
		item.Get("tool_calls").IsArray() && item.Get("tool_calls.#").Int() > 0
	if (role == "" && kind == "") || (content.Exists() && content.Type != gjson.String && !content.IsArray() && !toolOnly) {
		w.incomplete = true
	}
	if content.Type == gjson.String && w.withText {
		text.WriteString(content.String())
	}
	if content.IsArray() {
		content.ForEach(func(_, part gjson.Result) bool {
			switch partType := part.Get("type").String(); partType {
			case "text", "input_text", "output_text":
				if !w.withText {
					break
				}
				// Claude Code reminders keep their own parts on every wire.
				value := part.Get("text").String()
				if role == "user" && claudeCodeLocalCommand(value) {
					break
				}
				if role == "user" && claudeCodeInjectedContext(value) {
					reminders.WriteString(value + "\n")
				} else {
					text.WriteString(value + "\n")
				}
			case "encrypted_content":
				// Codex multi-agent messages carry their payload encrypted end to end.
				w.placeholder(&text, "encrypted", false)
			default:
				w.placeholder(&text, partType, codexNonTextTypes[partType] || openaiNonTextTypes[partType])
			}
			return true
		})
	}
	// Responses tool outputs can carry image/file parts directly or as JSON
	// encoded in a string; render their text so media payloads stay out.
	output := item.Get("output")
	if kind == "function_call_output" || kind == "custom_tool_call_output" {
		parts := output
		if parts.Type == gjson.String && gjson.Valid(parts.String()) {
			parts = gjson.Parse(parts.String())
		}
		if parts.IsArray() && parts.Get("0.type").Exists() {
			var rendered strings.Builder
			parts.ForEach(func(_, part gjson.Result) bool {
				switch partType := part.Get("type").String(); partType {
				case "text", "input_text", "output_text":
					rendered.WriteString(part.Get("text").String() + "\n")
				default:
					w.placeholder(&rendered, partType, codexNonTextTypes[partType] || openaiNonTextTypes[partType])
				}
				return true
			})
			output = gjson.Result{Type: gjson.String, Str: rendered.String()}
		}
	}
	if !w.withText {
		return
	}
	for _, path := range []string{"name", "call_id", "tool_call_id", "arguments", "input"} {
		if value := item.Get(path); value.Exists() {
			text.WriteString("\n" + path + ": " + value.String())
		}
	}
	if output.Exists() {
		text.WriteString("\noutput: " + output.String())
	}
	if calls := item.Get("tool_calls"); calls.Exists() {
		text.WriteString("\ntool_calls: " + calls.Raw)
	}
	w.add(reasoningEntry{role: role, text: reminders.String(), instruction: true})
	rendered := text.String()
	injected := client == protocol.Codex && role == "user" && codexInjectedUserContext(rendered)
	w.add(reasoningEntry{role: role, text: rendered,
		instruction: role == "system" || role == "developer" ||
			(injected && !strings.HasPrefix(strings.TrimSpace(rendered), "<environment_context>")),
		task: role == "user" && !injected, delegated: kind == "agent_message" && w.delegated(item)})
}

// Codex multi-agent v2 hands a subagent its task as a NEW_TASK agent message;
// later messages from the same author are follow-up instructions. Main threads
// only receive reports addressed to them.
func (w *reasoningWalk) delegated(item gjson.Result) bool {
	author := item.Get("author").String()
	if strings.HasPrefix(strings.TrimSpace(item.Get("content.0.text").String()), "Message Type: NEW_TASK") {
		w.parent = author
	}
	return author != "" && author == w.parent
}

// Anthropic Messages: top-level system plus content blocks. Tool results ride
// in user messages but are tool output, not user input.
func (w *reasoningWalk) anthropic(body []byte) {
	if system := gjson.GetBytes(body, "system"); system.Exists() {
		w.add(reasoningEntry{role: "system", text: w.anthropicText(system), instruction: true})
	}
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		w.incomplete = true
		return
	}
	messages.ForEach(func(_, message gjson.Result) bool {
		role := message.Get("role").String()
		switch role {
		case "user":
			w.hasUser = true
		case "assistant":
			w.media = false
		case "system":
		default:
			w.incomplete = true
		}
		content := message.Get("content")
		if content.Type == gjson.String {
			w.add(reasoningEntry{role: role, text: content.String(), instruction: role == "system", task: role == "user"})
			return true
		}
		if !content.IsArray() {
			w.incomplete = true
			return true
		}
		var text, injected, tools strings.Builder
		content.ForEach(func(_, block gjson.Result) bool {
			switch blockType := block.Get("type").String(); blockType {
			case "text":
				value := block.Get("text").String()
				if role == "user" && claudeCodeLocalCommand(value) {
					break
				}
				if role == "user" && claudeCodeInjectedContext(value) {
					injected.WriteString(value + "\n")
				} else {
					text.WriteString(value + "\n")
				}
			case "thinking", "redacted_thinking":
			case "tool_use":
				text.WriteString("tool_use: " + block.Get("name").String() + " " + block.Get("input").Raw + "\n")
			case "tool_result":
				if block.Get("is_error").Bool() {
					tools.WriteString("is_error: true\n")
				}
				tools.WriteString(w.anthropicText(block.Get("content")) + "\n")
			default:
				w.placeholder(&text, blockType, anthropicNonTextTypes[blockType])
			}
			return true
		})
		w.add(reasoningEntry{role: role, text: injected.String(), instruction: true})
		w.add(reasoningEntry{role: "tool", text: tools.String()})
		w.add(reasoningEntry{role: role, text: text.String(), instruction: role == "system", task: role == "user"})
		return true
	})
}

// Text of a string or an array of text blocks; media blocks appear in tool
// results. Claude Code's tool_reference entries only announce loaded tools.
func (w *reasoningWalk) anthropicText(value gjson.Result) string {
	if value.Type == gjson.String || !value.Exists() {
		return value.String()
	}
	if !value.IsArray() {
		w.incomplete = true
		return ""
	}
	var text strings.Builder
	value.ForEach(func(_, block gjson.Result) bool {
		switch blockType := block.Get("type").String(); blockType {
		case "text":
			text.WriteString(block.Get("text").String() + "\n")
		case "tool_reference":
		default:
			w.placeholder(&text, blockType, anthropicNonTextTypes[blockType])
		}
		return true
	})
	return text.String()
}

// Gemini generateContent: parts carry no type tag, so they are classified by
// field presence.
func (w *reasoningWalk) gemini(body []byte) {
	for _, path := range []string{"systemInstruction", "system_instruction"} {
		if system := gjson.GetBytes(body, path); system.Exists() {
			w.add(reasoningEntry{role: "system", text: w.geminiParts(system.Get("parts"), nil), instruction: true})
		}
	}
	contents := gjson.GetBytes(body, "contents")
	if !contents.IsArray() {
		w.incomplete = true
		return
	}
	contents.ForEach(func(_, content gjson.Result) bool {
		role := content.Get("role").String()
		switch role {
		case "", "user":
			role = "user"
			w.hasUser = true
		case "model":
			role = "assistant"
			w.media = false
		default:
			w.incomplete = true
		}
		var tools strings.Builder
		text := w.geminiParts(content.Get("parts"), &tools)
		w.add(reasoningEntry{role: "tool", text: tools.String()})
		w.add(reasoningEntry{role: role, text: text, task: role == "user"})
		return true
	})
}

func (w *reasoningWalk) geminiParts(parts gjson.Result, tools *strings.Builder) string {
	if !parts.IsArray() {
		w.incomplete = true
		return ""
	}
	var text strings.Builder
	parts.ForEach(func(_, part gjson.Result) bool {
		switch {
		case part.Get("thought").Bool():
		case part.Get("text").Exists():
			text.WriteString(part.Get("text").String() + "\n")
		case part.Get("functionCall").Exists() && tools != nil:
			call := part.Get("functionCall")
			text.WriteString("function_call: " + call.Get("name").String() + " " + call.Get("args").Raw + "\n")
		case part.Get("functionResponse").Exists() && tools != nil:
			response := part.Get("functionResponse")
			tools.WriteString(response.Get("name").String() + ": " + response.Get("response").Raw + "\n")
			if forEachJSONValue(response.Get("parts"), 0, geminiMediaPart) {
				w.placeholder(tools, "media", true)
			}
		case part.Get("thoughtSignature").Exists():
		default:
			kind := "part"
			part.ForEach(func(key, _ gjson.Result) bool {
				kind = key.String()
				return false
			})
			w.placeholder(&text, kind, geminiMediaPart(part))
		}
		return true
	})
	return text.String()
}

// Codex CLI interleaves tool definitions and its own configuration changes with
// the conversation; they carry no turn content.
func reasoningMetadataItem(client protocol.Protocol, item gjson.Result) bool {
	if client != protocol.Codex {
		return false
	}
	switch item.Get("type").String() {
	case "additional_tools", "configuration_update":
		return true
	}
	return false
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

// Claude Code wraps CLAUDE.md, environment and hook context in system reminders
// placed in user messages.
func claudeCodeInjectedContext(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), "<system-reminder>")
}

// Claude Code records local slash commands (/exit, /model, ...) and their output
// in user messages behind a caveat telling the model not to respond to them.
// A command with arguments stays: the arguments are the user's request.
func claudeCodeLocalCommand(text string) bool {
	text = strings.TrimSpace(text)
	for _, prefix := range []string{"<local-command-caveat>", "<local-command-stdout>", "<local-command-stderr>"} {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return (strings.HasPrefix(text, "<command-name>") || strings.HasPrefix(text, "<command-message>")) &&
		strings.Contains(text, "<command-args></command-args>")
}

func claudeCodeNonInput(text string) bool {
	return claudeCodeInjectedContext(text) || claudeCodeLocalCommand(text)
}

// A user message made only of Claude Code reminder or local command parts
// carries no input.
func claudeCodeReminderOnly(item gjson.Result) bool {
	content := item.Get("content")
	if item.Get("role").String() != "user" || !content.IsArray() {
		return false
	}
	reminder := false
	for _, part := range content.Array() {
		if !claudeCodeNonInput(part.Get("text").String()) {
			return false
		}
		reminder = true
	}
	return reminder
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
	switch client {
	case protocol.Anthropic:
		return anthropicTurnInput(body)
	case protocol.Gemini:
		return geminiTurnInput(body)
	}
	items := gjson.GetBytes(body, "messages")
	if client == protocol.Codex {
		items = gjson.GetBytes(body, "input")
	}
	if items.Type == gjson.String {
		return true, false
	}
	var array []gjson.Result
	items.ForEach(func(_, item gjson.Result) bool {
		if !reasoningMetadataItem(client, item) {
			array = append(array, item)
		}
		return true
	})
	// Reminder-only messages trail both user input and tool results.
	for len(array) > 0 && claudeCodeReminderOnly(array[len(array)-1]) {
		array = array[:len(array)-1]
	}
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

// The latest non-system message decides. Claude Code merges input typed during
// a tool run into the tool result message, so real text counts as user input
// there too; only its injected reminders and local commands do not.
func anthropicTurnInput(body []byte) (user, failed bool) {
	messages := gjson.GetBytes(body, "messages")
	if messages.Type == gjson.String {
		return true, false
	}
	array := messages.Array()
	for i := len(array) - 1; i >= 0; i-- {
		message := array[i]
		switch message.Get("role").String() {
		case "system":
			continue
		case "user":
		default:
			return false, false
		}
		content := message.Get("content")
		if content.Type == gjson.String {
			return true, false
		}
		hasText, hasTool := false, false
		content.ForEach(func(_, block gjson.Result) bool {
			switch block.Get("type").String() {
			case "tool_result":
				hasTool = true
				failed = failed || reasoningToolOutputFailed(block)
			case "text":
				hasText = hasText || !claudeCodeNonInput(block.Get("text").String())
			}
			return true
		})
		if !hasText && !hasTool {
			continue
		}
		return hasText, failed
	}
	return false, false
}

func geminiTurnInput(body []byte) (user, failed bool) {
	contents := gjson.GetBytes(body, "contents").Array()
	if len(contents) == 0 {
		return false, false
	}
	last := contents[len(contents)-1]
	if role := last.Get("role").String(); role != "" && role != "user" {
		return false, false
	}
	last.Get("parts").ForEach(func(_, part gjson.Result) bool {
		if response := part.Get("functionResponse.response"); part.Get("functionResponse").Exists() {
			failed = failed || response.Get("error").Exists() || reasoningToolOutputFailed(response)
		} else if part.Get("text").Exists() && !part.Get("thought").Bool() {
			user = true
		}
		return true
	})
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
	// "error[" covers coded compiler diagnostics such as rustc's error[E0382]:.
	for _, marker := range []string{"error:", "error[", "failed", "failure", "conflict", "new constraint", "new requirement", "约束变更", "新约束", "失败", "冲突"} {
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
