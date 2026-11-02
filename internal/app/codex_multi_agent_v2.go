package app

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	codexSpawnAgentDescriptionMarker = "Spawns an agent"
	codexSpawnAgentModelsHeading     = "Available model overrides (optional; inherited parent model is preferred):"
)

var codexCollaborationMessageTools = map[string]struct{}{
	"spawn_agent":   {},
	"send_message":  {},
	"followup_task": {},
}

// codexMultiAgentV2Enabled gates the Codex-originating cross-protocol
// portability rewrite. Only official Codex clients carry the multi-agent
// wire shapes this rewrite understands; arbitrary callers are never touched.
func codexMultiAgentV2Enabled(headers http.Header) bool {
	return isCodexMultiAgentClient(codexMultiAgentUserAgent(headers))
}

func codexMultiAgentUserAgent(headers http.Header) string {
	if headers == nil {
		return ""
	}
	if value := strings.TrimSpace(headers.Get("User-Agent")); value != "" {
		return value
	}
	for key, values := range headers {
		if !strings.EqualFold(key, "User-Agent") {
			continue
		}
		for _, value := range values {
			if value = strings.TrimSpace(value); value != "" {
				return value
			}
		}
	}
	return ""
}

func isCodexMultiAgentClient(userAgent string) bool {
	userAgent = strings.TrimSpace(userAgent)
	return strings.HasPrefix(userAgent, "Codex Desktop/") ||
		strings.HasPrefix(userAgent, "codex-tui/") ||
		userAgent == "codex_cli_rs" ||
		strings.HasPrefix(userAgent, "codex_cli_rs/")
}

// prepareCodexMultiAgentV2Tools performs the collaboration tool-definition
// cleanup applied before a Codex request is translated to another protocol.
// It intentionally keeps the collaboration namespace unchanged.
func prepareCodexMultiAgentV2Tools(headers http.Header, payload []byte, models []string) []byte {
	if !codexMultiAgentV2Enabled(headers) {
		return payload
	}
	updated := removeCodexCollaborationMessageEncryption(payload, codexCollaborationMessageToolPaths(payload))
	spawnPaths := codexSpawnAgentToolPaths(updated)
	if len(spawnPaths) == 0 {
		return updated
	}
	return rewriteCodexSpawnAgentTools(updated, spawnPaths, models)
}

// rewriteCodexMultiAgentV2Input makes agent_message items portable before a
// Codex request is translated to Anthropic/OpenAI/Gemini.
func rewriteCodexMultiAgentV2Input(headers http.Header, payload []byte) []byte {
	if !codexMultiAgentV2Enabled(headers) {
		return payload
	}
	input := gjson.GetBytes(payload, "input")
	if !input.IsArray() {
		return payload
	}
	updated := rewriteCodexAgentMessageContent(payload)
	for index, item := range input.Array() {
		if strings.TrimSpace(item.Get("type").String()) != "agent_message" {
			continue
		}
		itemPath := fmt.Sprintf("input.%d", index)
		var err error
		updated, err = sjson.SetBytes(updated, itemPath+".role", "user")
		if err != nil {
			return payload
		}
		updated, err = sjson.SetBytes(updated, itemPath+".type", "message")
		if err != nil {
			return payload
		}
	}
	return updated
}

func rewriteCodexAgentMessageContent(payload []byte) []byte {
	input := gjson.GetBytes(payload, "input")
	if !input.IsArray() {
		return payload
	}
	updated := payload
	for itemIndex, item := range input.Array() {
		if strings.TrimSpace(item.Get("type").String()) != "agent_message" || !item.Get("content").IsArray() {
			continue
		}
		for partIndex, part := range item.Get("content").Array() {
			if strings.TrimSpace(part.Get("type").String()) != "encrypted_content" || part.Get("encrypted_content").Type != gjson.String {
				continue
			}
			partPath := fmt.Sprintf("input.%d.content.%d", itemIndex, partIndex)
			var err error
			updated, err = sjson.SetBytes(updated, partPath+".type", "input_text")
			if err != nil {
				return payload
			}
			updated, err = sjson.SetBytes(updated, partPath+".text", part.Get("encrypted_content").String())
			if err != nil {
				return payload
			}
			updated, err = sjson.DeleteBytes(updated, partPath+".encrypted_content")
			if err != nil {
				return payload
			}
		}
	}
	return updated
}

// codexRelayMessageCarrierPrefix marks an agent_message "encrypted_content"
// part forged by a third-party relay: base64 plaintext instead of an
// OpenAI-encrypted token. A multi-agent session whose sub-agent turn went
// through such a relay replays it to the official upstream, which rejects it
// with invalid_encrypted_content.
const codexRelayMessageCarrierPrefix = "c2a-msg-v1:"

// codexBodyWithRelayMessagesDecoded turns relay-forged agent_message parts
// back into the input_text part Codex sends for unencrypted messages. Genuine
// encrypted parts stay untouched; the rewrite never moves input indexes.
func codexBodyWithRelayMessagesDecoded(body []byte, input gjson.Result) ([]byte, bool) {
	updated := body
	rewritten := false
	for itemIndex, item := range input.Array() {
		if item.Get("type").String() != "agent_message" || !item.Get("content").IsArray() {
			continue
		}
		for partIndex, part := range item.Get("content").Array() {
			text, ok := decodeCodexRelayMessageCarrier(part)
			if !ok {
				continue
			}
			next, err := sjson.SetBytes(updated, fmt.Sprintf("input.%d.content.%d", itemIndex, partIndex), struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}{Type: "input_text", Text: text})
			if err != nil {
				return body, false
			}
			updated = next
			rewritten = true
		}
	}
	return updated, rewritten
}

func decodeCodexRelayMessageCarrier(part gjson.Result) (string, bool) {
	value := part.Get("encrypted_content")
	if part.Get("type").String() != "encrypted_content" || value.Type != gjson.String {
		return "", false
	}
	encoded, ok := strings.CutPrefix(value.String(), codexRelayMessageCarrierPrefix)
	if !ok {
		return "", false
	}
	decoded, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(encoded, "="))
	if err != nil || !utf8.Valid(decoded) {
		return "", false
	}
	return string(decoded), true
}

func codexSpawnAgentToolPaths(payload []byte) []string {
	return codexToolPathsByNames(payload, map[string]struct{}{"spawn_agent": {}})
}

func codexCollaborationMessageToolPaths(payload []byte) []string {
	return codexToolPathsByNames(payload, codexCollaborationMessageTools)
}

func codexToolPathsByNames(payload []byte, names map[string]struct{}) []string {
	paths := make([]string, 0, len(names))
	collectCodexToolPaths(gjson.GetBytes(payload, "tools"), "tools", &paths, names)
	input := gjson.GetBytes(payload, "input")
	if input.IsArray() {
		for index, item := range input.Array() {
			if strings.TrimSpace(item.Get("type").String()) == "additional_tools" {
				collectCodexToolPaths(item.Get("tools"), fmt.Sprintf("input.%d.tools", index), &paths, names)
			}
		}
	}
	return paths
}

func collectCodexToolPaths(tools gjson.Result, path string, paths *[]string, names map[string]struct{}) {
	if !tools.IsArray() {
		return
	}
	for index, tool := range tools.Array() {
		toolPath := fmt.Sprintf("%s.%d", path, index)
		typeName := strings.TrimSpace(tool.Get("type").String())
		if typeName == "function" {
			if _, ok := names[strings.TrimSpace(tool.Get("name").String())]; ok {
				*paths = append(*paths, toolPath)
			}
		}
		if typeName == "namespace" {
			collectCodexToolPaths(tool.Get("tools"), toolPath+".tools", paths, names)
		}
	}
}

func removeCodexCollaborationMessageEncryption(payload []byte, paths []string) []byte {
	updated := payload
	for _, path := range paths {
		var err error
		updated, err = sjson.DeleteBytes(updated, path+".parameters.properties.message.encrypted")
		if err != nil {
			return payload
		}
	}
	return updated
}

func rewriteCodexSpawnAgentTools(payload []byte, paths []string, models []string) []byte {
	modelList := formatCodexSpawnAgentModels(models)
	if modelList == "" {
		return removeCodexCollaborationMessageEncryption(payload, paths)
	}
	updated := payload
	for _, path := range paths {
		description := gjson.GetBytes(updated, path+".description")
		if description.Type != gjson.String {
			continue
		}
		rewritten := replaceCodexSpawnAgentModels(description.String(), modelList)
		var err error
		updated, err = sjson.SetBytes(updated, path+".description", rewritten)
		if err != nil {
			return payload
		}
	}
	return removeCodexCollaborationMessageEncryption(updated, paths)
}

func formatCodexSpawnAgentModels(models []string) string {
	seen := make(map[string]struct{}, len(models))
	cleaned := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.Join(strings.Fields(model), " ")
		if model == "" {
			continue
		}
		if _, ok := seen[model]; ok {
			continue
		}
		seen[model] = struct{}{}
		cleaned = append(cleaned, model)
	}
	sort.Strings(cleaned)
	var builder strings.Builder
	for _, model := range cleaned {
		builder.WriteString("- `")
		builder.WriteString(strings.ReplaceAll(model, "`", "'"))
		builder.WriteString("`: ")
		builder.WriteString(model)
		builder.WriteString(".\n")
	}
	return strings.TrimSuffix(builder.String(), "\n")
}

func replaceCodexSpawnAgentModels(description, modelList string) string {
	if modelList == "" {
		return description
	}
	cleaned, indent := removeCodexSpawnAgentModelSections(description)
	section := indent + codexSpawnAgentModelsHeading + "\n" + modelList + "\n"
	if markerIndex := strings.Index(cleaned, codexSpawnAgentDescriptionMarker); markerIndex >= 0 {
		lineStart := strings.LastIndex(cleaned[:markerIndex], "\n") + 1
		return cleaned[:lineStart] + section + cleaned[lineStart:]
	}
	separator := ""
	if cleaned != "" && !strings.HasSuffix(cleaned, "\n") {
		separator = "\n\n"
	}
	return cleaned + separator + strings.TrimSuffix(section, "\n")
}

func removeCodexSpawnAgentModelSections(description string) (string, string) {
	lines := strings.SplitAfter(description, "\n")
	var cleaned strings.Builder
	indent := ""
	for index := 0; index < len(lines); {
		line := lines[index]
		if strings.TrimSpace(line) != codexSpawnAgentModelsHeading {
			cleaned.WriteString(line)
			index++
			continue
		}
		if indent == "" {
			headingIndex := strings.Index(line, codexSpawnAgentModelsHeading)
			if headingIndex > 0 {
				indent = line[:headingIndex]
			}
		}
		index++
		for index < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[index]), "- ") {
			index++
		}
	}
	return cleaned.String(), indent
}

// codexMultiAgentV2Models returns the currently visible model names. Model
// lookup is advisory; inability to read it must never break a proxy request.
func (s *Server) codexMultiAgentV2Models(ctx context.Context) []string {
	if s == nil || (s.channelCache == nil && s.store == nil) {
		return nil
	}
	models, err := s.getAllEnabledModels(ctx)
	if err != nil {
		return nil
	}
	return models
}
