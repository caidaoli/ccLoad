package common

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/MoeForge/moego/ojson"

	"ccLoad/internal/protocol/cliproxy/util"
)

// ResponsesMessagePartText returns the visible value of a Responses message part.
func ResponsesMessagePartText(part ojson.Result) ojson.Result {
	switch part.Get("type").String() {
	case "output_text":
		return part.Get("text")
	case "refusal":
		if refusal := part.Get("refusal"); refusal.String() != "" {
			return refusal
		}
		return part.Get("text")
	default:
		return ojson.Result{}
	}
}

// ResponsesMessageStream tracks which message parts have already reached the client.
type ResponsesMessageStream struct {
	byID            map[string]*responsesStreamedMessage
	byIndex         map[int64]*responsesStreamedMessage
	nonMessageTypes map[int64]string
	current         *responsesStreamedMessage
	unkeyedOutput   bool
	deferred        []*responsesStreamedMessage
}

type responsesStreamedMessage struct {
	id               string
	phase            string
	indexed          bool
	outputIndex      int64
	parts            map[int64]bool
	nextContentIndex int64
	pending          map[int64]*strings.Builder
	unindexedText    bool
	unindexedRefusal bool
}

// ResponsesFinishedPart carries visible text recovered at item completion.
type ResponsesFinishedPart struct {
	Text        string
	Annotations ojson.Result
}

func (s *ResponsesMessageStream) find(id string, index int64, indexed bool) *responsesStreamedMessage {
	if message := s.byID[id]; message != nil {
		return message
	}
	// An index matches across events when either side omits the ID. A different
	// explicit ID can reuse an index in the terminal output array.
	if message := s.byIndex[index]; indexed && message != nil && (id == "" || message.id == "") {
		return message
	}
	return nil
}

func (s *ResponsesMessageStream) message(id string, index ojson.Result) *responsesStreamedMessage {
	indexed := index.Type == ojson.Number
	outputIndex := index.Int()
	message := s.find(id, outputIndex, indexed)
	if message == nil && s.current != nil && ((id == "" && !indexed) || !s.current.indexed && s.current.id == "") {
		message = s.current
	}
	if message == nil {
		message = &responsesStreamedMessage{}
	}
	if id != "" && message.id == "" {
		if s.byID == nil {
			s.byID = make(map[string]*responsesStreamedMessage)
		}
		message.id = strings.Clone(id)
		s.byID[message.id] = message
	}
	if indexed && s.byIndex[outputIndex] != message {
		if s.byIndex == nil {
			s.byIndex = make(map[int64]*responsesStreamedMessage)
		}
		message.indexed = true
		message.outputIndex = outputIndex
		s.byIndex[outputIndex] = message
	}
	return message
}

func (s *ResponsesMessageStream) forgetCurrent() {
	if message := s.current; message != nil && message.id == "" && !message.indexed {
		s.unkeyedOutput = s.unkeyedOutput || message.nextContentIndex > 0 || len(message.parts) > 0 || message.unindexedText || message.unindexedRefusal
	}
	s.current = nil
}

// ObserveEvent tracks message identity and the content positions that can be
// passed over without waiting for visible output.
func (s *ResponsesMessageStream) ObserveEvent(eventType string, event ojson.Result) {
	switch eventType {
	case "response.content_part.added", "response.content_part.done":
		part := event.Get("part")
		switch part.Get("type").String() {
		case "output_text", "refusal":
			if eventType == "response.content_part.added" || ResponsesMessagePartText(part).String() != "" {
				return
			}
		case "":
			return
		}
		if index := event.Get("content_index"); index.Type == ojson.Number {
			message := s.message(event.Get("item_id").String(), event.Get("output_index"))
			if (message.indexed || message.id != "") && message.pending[index.Int()] == nil {
				message.markPart(index.Int())
			}
		}
		return
	case "response.output_item.added", "response.output_item.done":
	default:
		return
	}
	item := event.Get("item")
	index := event.Get("output_index")
	switch itemType := item.Get("type").String(); itemType {
	case "message":
		if eventType == "response.output_item.added" {
			s.forgetCurrent()
			id := cmp.Or(item.Get("id").String(), event.Get("item_id").String())
			s.current = s.message(id, index)
			s.current.phase = strings.Clone(item.Get("phase").String())
		}
	case "":
	default:
		if index.Type == ojson.Number {
			if s.nonMessageTypes == nil {
				s.nonMessageTypes = make(map[int64]string)
			}
			s.nonMessageTypes[index.Int()] = strings.Clone(itemType)
		}
	}
}

// ObserveDelta returns whether to emit this delta immediately. Deltas after an
// unresolved content index wait for the message snapshot to fill the gap.
func (s *ResponsesMessageStream) ObserveDelta(event ojson.Result, text string) bool {
	if text == "" {
		return true
	}
	message := s.message(event.Get("item_id").String(), event.Get("output_index"))
	s.current = message
	if part := event.Get("content_index"); part.Type == ojson.Number {
		index := part.Int()
		if (index > message.nextContentIndex && (message.indexed || message.id != "")) || message.pending[index] != nil {
			if message.pending == nil {
				message.pending = make(map[int64]*strings.Builder)
				s.deferred = append(s.deferred, message)
			}
			buffer := message.pending[index]
			if buffer == nil {
				buffer = &strings.Builder{}
				message.pending[index] = buffer
			}
			buffer.WriteString(text)
			return false
		}
		message.markPart(index)
	} else if event.Get("type").String() == "response.refusal.delta" {
		message.unindexedRefusal = true
	} else {
		message.unindexedText = true
	}
	return true
}

func (m *responsesStreamedMessage) markPart(index int64) {
	if index < m.nextContentIndex {
		return
	}
	if index == m.nextContentIndex {
		m.nextContentIndex++
		for m.parts[m.nextContentIndex] {
			delete(m.parts, m.nextContentIndex)
			m.nextContentIndex++
		}
		return
	}
	if m.parts == nil {
		m.parts = make(map[int64]bool)
	}
	m.parts[index] = true
}

func (m *responsesStreamedMessage) sawPart(index int64, part ojson.Result) bool {
	if m == nil {
		return false
	}
	if index < m.nextContentIndex || m.parts[index] {
		return true
	}
	if part.Get("type").String() == "refusal" {
		return m.unindexedRefusal
	}
	return m.unindexedText
}

// FinishMessage returns visible parts that still need to be emitted at item completion.
func (s *ResponsesMessageStream) FinishMessage(event ojson.Result) []ResponsesFinishedPart {
	item := event.Get("item")
	id := cmp.Or(item.Get("id").String(), event.Get("item_id").String())
	message := s.message(id, event.Get("output_index"))
	var missing []ResponsesFinishedPart
	for index, part := range item.Get("content").Array() {
		contentIndex := int64(index)
		buffer := message.pending[contentIndex]
		if buffer == nil && message.sawPart(contentIndex, part) {
			continue
		}
		text := ResponsesMessagePartText(part).String()
		if buffer != nil {
			if text == "" {
				text = buffer.String()
			}
			delete(message.pending, contentIndex)
		}
		if text != "" {
			missing = append(missing, ResponsesFinishedPart{Text: text, Annotations: part.Get("annotations")})
			message.markPart(contentIndex)
		}
	}
	// Some terminal snapshots omit content (or the whole message). Preserve
	// the received deltas for positions that the snapshot did not describe.
	if len(message.pending) > 0 {
		for _, index := range slices.Sorted(maps.Keys(message.pending)) {
			missing = append(missing, ResponsesFinishedPart{Text: message.pending[index].String()})
			message.markPart(index)
		}
		clear(message.pending)
	}
	if s.current == nil || s.current == message {
		s.current = message
		s.forgetCurrent()
	}
	return missing
}

// MissingMessageDoneEvents builds completion events for message parts found in
// the terminal response or deferred after a missing content index.
func (s *ResponsesMessageStream) MissingMessageDoneEvents(output ojson.Result) [][]byte {
	s.forgetCurrent()
	if s.unkeyedOutput && len(s.deferred) == 0 {
		return nil
	}
	var events [][]byte
	var matched map[*responsesStreamedMessage]bool
	var index int64
	for _, item := range output.Array() {
		itemType := item.Get("type").String()
		// Some providers omit previously streamed reasoning or tools from output.
		// Keep their slots when mapping terminal positions back to SSE indices.
		for s.nonMessageTypes[index] != "" && s.nonMessageTypes[index] != itemType {
			index++
		}
		outputIndex := index
		index++
		if itemType != "message" {
			continue
		}
		message := s.find(item.Get("id").String(), outputIndex, true)
		if message != nil && len(message.pending) > 0 {
			if matched == nil {
				matched = make(map[*responsesStreamedMessage]bool)
			}
			matched[message] = true
			events = append(events, responsesMessageDoneEvent(item.Raw, outputIndex, true))
			continue
		}
		if s.unkeyedOutput {
			// Unknown earlier output cannot safely be matched to a snapshot.
			continue
		}
		for partIndex, part := range item.Get("content").Array() {
			if !message.sawPart(int64(partIndex), part) && ResponsesMessagePartText(part).String() != "" {
				events = append(events, responsesMessageDoneEvent(item.Raw, outputIndex, true))
				break
			}
		}
	}
	for _, message := range s.deferred {
		if len(message.pending) == 0 || matched[message] {
			continue
		}
		item := `{"type":"message"}`
		if message.id != "" {
			item, _ = ojson.Set(item, "id", message.id)
		}
		if message.phase != "" {
			item, _ = ojson.Set(item, "phase", message.phase)
		}
		events = append(events, responsesMessageDoneEvent(item, message.outputIndex, message.indexed))
	}
	return events
}

func responsesMessageDoneEvent(item string, index int64, indexed bool) []byte {
	event := []byte(`data: {"type":"response.output_item.done",`)
	if indexed {
		event = append(event, `"output_index":`...)
		event = strconv.AppendInt(event, index, 10)
		event = append(event, ',')
	}
	event = append(event, `"item":`...)
	event = append(event, item...)
	return append(event, '}')
}

// SetResponsesToolCallIdentityDoc writes a resolved Responses tool name and namespace into doc.
func SetResponsesToolCallIdentityDoc(doc *ojson.Doc, name, namespace, itemPath string) {
	if doc == nil {
		return
	}
	namePath := "name"
	namespacePath := "namespace"
	if itemPath != "" {
		namePath = itemPath + ".name"
		namespacePath = itemPath + ".namespace"
	}
	_ = doc.Set(namePath, name)
	if namespace != "" {
		_ = doc.Set(namespacePath, namespace)
	} else {
		_ = doc.Delete(namespacePath)
	}
}

// SetResponsesToolCallIdentity writes a resolved Responses tool name and namespace.
func SetResponsesToolCallIdentity(item []byte, name, namespace, itemPath string) []byte {
	doc := ojson.NewDoc(item)
	SetResponsesToolCallIdentityDoc(doc, name, namespace, itemPath)
	return doc.Bytes()
}

// ExtractResponsesCallID extracts the tool call ID from a Responses API item,
// prioritizing dedicated tool call references over generic item IDs:
// call_id -> tool_call_id -> callId -> id (excluding fco_ output item IDs)
func ExtractResponsesCallID(node ojson.Result) string {
	if callID := strings.TrimSpace(node.Get("call_id").String()); callID != "" {
		return callID
	}
	if toolCallID := strings.TrimSpace(node.Get("tool_call_id").String()); toolCallID != "" {
		return toolCallID
	}
	if callId := strings.TrimSpace(node.Get("callId").String()); callId != "" {
		return callId
	}
	id := strings.TrimSpace(node.Get("id").String())
	if strings.HasPrefix(id, "fco_") {
		return ""
	}
	return id
}

// NormalizeResponsesToolCallOutputs scans a slice of Responses input items,
// pairs function_call_output / custom_tool_call_output with preceding pending tool calls,
// and assigns missing call_ids to tool outputs using a multi-pass matching strategy:
// 1. Exact explicit call ID match (reserving calls for outputs that explicitly reference them across the entire conversation).
// 2. Function name match for outputs that omit a call ID (skipping pending calls reserved by future explicit outputs).
// 3. FIFO queue fallback for remaining outputs that omit a call ID (skipping pending calls reserved by future explicit outputs).
// Outputs that already carry an explicit call ID that does not match any pending call
// are never rewritten or stolen.
func NormalizeResponsesToolCallOutputs(items []ojson.Result) []ojson.Result {
	if len(items) == 0 {
		return items
	}

	normalized := make([]ojson.Result, len(items))
	copy(normalized, items)

	explicitOutputCounts := make(map[string]int)
	for _, item := range items {
		typ := item.Get("type").String()
		if typ == "function_call_output" || typ == "custom_tool_call_output" {
			if id := ExtractResponsesCallID(item); id != "" {
				explicitOutputCounts[id]++
			}
		}
	}

	var pendingCallIDs []string
	pendingCallNames := make(map[string]string)

	i := 0
	for i < len(normalized) {
		item := normalized[i]
		itemType := item.Get("type").String()

		switch itemType {
		case "function_call", "custom_tool_call":
			callID := ExtractResponsesCallID(item)
			if callID != "" {
				pendingCallIDs = append(pendingCallIDs, callID)
				name := item.Get("name").String()
				pendingCallNames[callID] = name
			}
			i++

		case "function_call_output", "custom_tool_call_output":
			start := i
			for i < len(normalized) && (normalized[i].Get("type").String() == "function_call_output" || normalized[i].Get("type").String() == "custom_tool_call_output") {
				i++
			}
			outputs := normalized[start:i]

			if len(pendingCallIDs) > 0 {
				used := make([]bool, len(outputs))
				matchedForPending := make([]int, len(pendingCallIDs))
				for idx := range matchedForPending {
					matchedForPending[idx] = -1
				}

				// Pass 1: exact explicit call ID match
				for pendingIdx, pendingID := range pendingCallIDs {
					for outIdx, out := range outputs {
						if !used[outIdx] && ExtractResponsesCallID(out) == pendingID {
							used[outIdx] = true
							matchedForPending[pendingIdx] = outIdx
							explicitOutputCounts[pendingID]--
							break
						}
					}
				}

				// Pass 2: match by function name for outputs with no explicit call ID (skipping pending IDs reserved by future explicit outputs)
				for pendingIdx, pendingID := range pendingCallIDs {
					if matchedForPending[pendingIdx] >= 0 || explicitOutputCounts[pendingID] > 0 {
						continue
					}
					expectedName := pendingCallNames[pendingID]
					if expectedName != "" {
						for outIdx, out := range outputs {
							if !used[outIdx] && ExtractResponsesCallID(out) == "" && !isCodexDelegationToolOutput(out) {
								outName := strings.TrimSpace(out.Get("name").String())
								if outName != "" && outName == expectedName {
									used[outIdx] = true
									matchedForPending[pendingIdx] = outIdx
									break
								}
							}
						}
					}
				}

				// Pass 3: FIFO fallback for outputs with no explicit call ID (skipping pending IDs reserved by future explicit outputs)
				for pendingIdx := range pendingCallIDs {
					pendingID := pendingCallIDs[pendingIdx]
					if matchedForPending[pendingIdx] >= 0 || explicitOutputCounts[pendingID] > 0 {
						continue
					}
					for outIdx, out := range outputs {
						if !used[outIdx] && ExtractResponsesCallID(out) == "" && !isCodexDelegationToolOutput(out) {
							outName := strings.TrimSpace(out.Get("name").String())
							expectedName := pendingCallNames[pendingID]
							if outName == "" || expectedName == "" || outName == expectedName {
								used[outIdx] = true
								matchedForPending[pendingIdx] = outIdx
								break
							}
						}
					}
				}

				// Apply matched call_ids to outputs
				var remainingPending []string
				for pendingIdx, pendingID := range pendingCallIDs {
					outIdx := matchedForPending[pendingIdx]
					if outIdx < 0 {
						remainingPending = append(remainingPending, pendingID)
						continue
					}
					matchedOut := outputs[outIdx]
					if matchedOut.Get("call_id").String() != pendingID {
						raw := []byte(matchedOut.Raw)
						raw, _ = ojson.SetBytes(raw, "call_id", pendingID)
						normalized[start+outIdx] = util.ParseGJSONBytesNoCopy(raw)
					}
				}
				pendingCallIDs = remainingPending
			}

		default:
			i++
		}
	}

	return normalized
}

// SanitizeOrphanResponsesToolOutputsDoc rewrites standalone function_call_output
// and custom_tool_call_output items that carry no call_id into user message
// items. Codex Desktop Companion Page context is injected as function_call_output
// items without call_id, which strict Responses upstreams reject with HTTP 400
// ("responses function_call_output call_id is required").
func SanitizeOrphanResponsesToolOutputsDoc(doc *ojson.Doc) bool {
	if doc == nil {
		return false
	}
	return SanitizeOrphanResponsesToolOutputsInputDoc(doc.Get("input"), func() *ojson.Doc {
		return doc
	})
}

// SanitizeOrphanResponsesToolOutputsInputDoc pairs tool outputs missing call_id
// with preceding unmatched tool calls when possible, and rewrites remaining
// standalone function_call_output and custom_tool_call_output items without
// call_id into user message items in-place on the document returned by ensureDoc.
func SanitizeOrphanResponsesToolOutputsInputDoc(inputResult ojson.Result, ensureDoc func() *ojson.Doc) bool {
	if !inputResult.IsArray() || ensureDoc == nil {
		return false
	}
	inputItems := inputResult.Array()
	if len(inputItems) == 0 {
		return false
	}

	normalizedItems := NormalizeResponsesToolCallOutputs(inputItems)

	changed := false
	pendingMissingCallIDOutputType := ""
	for i := 0; i < len(inputItems); i++ {
		item := normalizedItems[i]
		if !item.IsObject() {
			pendingMissingCallIDOutputType = ""
			continue
		}
		itemType := strings.TrimSpace(item.Get("type").String())
		if itemType == "function_call" || itemType == "custom_tool_call" {
			if ExtractResponsesCallID(item) == "" {
				pendingMissingCallIDOutputType = itemType + "_output"
			} else {
				pendingMissingCallIDOutputType = ""
			}
			continue
		}
		if itemType != "function_call_output" && itemType != "custom_tool_call_output" {
			pendingMissingCallIDOutputType = ""
			continue
		}
		if isCodexDelegationToolOutput(item) {
			pendingMissingCallIDOutputType = ""
			continue
		}
		if resolvedCallID := responsesToolOutputExplicitCallID(item); resolvedCallID != "" {
			pendingMissingCallIDOutputType = ""
			origCallID := strings.TrimSpace(inputItems[i].Get("call_id").String())
			if origCallID != resolvedCallID {
				itemPath := fmt.Sprintf("input.%d.call_id", i)
				if errSet := ensureDoc().Set(itemPath, resolvedCallID); errSet == nil {
					changed = true
				}
			}
			continue
		}
		if pendingMissingCallIDOutputType == itemType &&
			strings.TrimSpace(item.Get("name").String()) == "" &&
			strings.TrimSpace(item.Get("id").String()) == "" {
			pendingMissingCallIDOutputType = ""
			continue
		}
		pendingMissingCallIDOutputType = ""

		itemPath := fmt.Sprintf("input.%d", i)
		if setOrphanResponsesToolOutputMessageDoc(ensureDoc(), itemPath, item) {
			changed = true
		}
	}
	return changed
}

func responsesToolOutputExplicitCallID(item ojson.Result) string {
	for _, key := range []string{"call_id", "tool_call_id", "callId"} {
		val := item.Get(key)
		if val.Type == ojson.String {
			if s := strings.TrimSpace(val.String()); s != "" {
				return s
			}
		}
	}
	return ""
}

func isCodexDelegationToolOutput(item ojson.Result) bool {
	if strings.TrimSpace(item.Get("type").String()) != "function_call_output" {
		return false
	}
	if strings.TrimSpace(item.Get("namespace").String()) != "codex_app" {
		return false
	}
	switch strings.TrimSpace(item.Get("name").String()) {
	case "create_thread", "send_message_to_thread":
		return true
	default:
		return false
	}
}

func setOrphanResponsesToolOutputMessageDoc(targetDoc *ojson.Doc, itemPath string, item ojson.Result) bool {
	if targetDoc == nil {
		return false
	}
	fullText, extraParts := buildOrphanResponsesToolOutputContent(item)
	if errSet := targetDoc.SetRawBytes(itemPath, []byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}`)); errSet != nil {
		return false
	}
	if errSet := targetDoc.Set(itemPath+".content.0.text", fullText); errSet != nil {
		return false
	}
	for idx, partRaw := range extraParts {
		partPath := fmt.Sprintf("%s.content.%d", itemPath, idx+1)
		if errSet := targetDoc.SetRawBytes(partPath, partRaw); errSet != nil {
			return false
		}
	}
	return true
}

func buildOrphanResponsesToolOutputContent(item ojson.Result) (string, [][]byte) {
	name := strings.TrimSpace(item.Get("name").String())
	if name == "" {
		name = "tool"
	}
	outputText, extraParts := extractOrphanResponsesToolOutputParts(item.Get("output"))
	if outputText == "" {
		outputText = "(empty)"
	}
	return "Tool output from " + name + ":\n" + outputText, extraParts
}

func extractOrphanResponsesToolOutputParts(output ojson.Result) (string, [][]byte) {
	if !output.Exists() || output.Type == ojson.Null {
		return "", nil
	}
	if output.Type == ojson.String {
		return strings.TrimSpace(output.String()), nil
	}
	if output.IsArray() {
		items := output.Array()
		parts := make([]string, 0, len(items))
		var extraParts [][]byte
		for i := 0; i < len(items); i++ {
			part := items[i]
			if part.Type == ojson.String {
				if s := strings.TrimSpace(part.String()); s != "" {
					parts = append(parts, s)
				}
				continue
			}
			if !part.IsObject() {
				continue
			}
			matchedText := false
			for _, key := range []string{"text", "input_text", "output_text"} {
				val := part.Get(key)
				if val.Type == ojson.String {
					matchedText = true
					if s := strings.TrimSpace(val.String()); s != "" {
						parts = append(parts, s)
					}
					break
				}
			}
			if !matchedText {
				if raw := strings.TrimSpace(part.Raw); raw != "" {
					extraParts = append(extraParts, []byte(raw))
				}
			}
		}
		if len(parts) > 0 || len(extraParts) > 0 {
			return strings.Join(parts, "\n"), extraParts
		}
	}
	return strings.TrimSpace(output.Raw), nil
}
