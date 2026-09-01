package app

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	codexSpawnAgentDescriptionMarker = "Spawns an agent"
	codexSpawnAgentModelsHeading     = "Available model overrides (optional; inherited parent model is preferred):"
	codexCollaborationNamespace      = "collaboration"
	codexOptimizedCollaboration      = "collaboration-optimize"
	codexOptimizedNamePrefix         = codexOptimizedCollaboration + "__"
)

var codexCollaborationMessageTools = map[string]struct{}{
	"spawn_agent":   {},
	"send_message":  {},
	"followup_task": {},
}

type codexMultiAgentV2RequestContextKey struct{}

func withCodexMultiAgentV2RequestContext(ctx context.Context, reqCtx *proxyRequestContext) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, codexMultiAgentV2RequestContextKey{}, reqCtx)
}

func codexMultiAgentV2RequestContextFromContext(ctx context.Context) *proxyRequestContext {
	if ctx == nil {
		return nil
	}
	requestContext, _ := ctx.Value(codexMultiAgentV2RequestContextKey{}).(*proxyRequestContext)
	return requestContext
}

// codexMultiAgentV2Enabled is intentionally a code default. ccLoad has no
// CLIProxyAPI runtime config tree; the feature is enabled for official Codex
// clients and never rewrites arbitrary OpenAI-compatible callers.
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

// optimizeCodexMultiAgentV2Request prepares a Codex Responses request for a
// provider that does not understand the reserved collaboration namespace.
// The bool reports whether the namespace was renamed and therefore must be
// restored on the response path.
func optimizeCodexMultiAgentV2Request(headers http.Header, payload []byte, models []string) ([]byte, bool) {
	if !codexMultiAgentV2Enabled(headers) {
		return payload, false
	}

	updated := rewriteCodexAgentMessageContent(payload)
	updated = prepareCodexMultiAgentV2Tools(headers, updated, models)
	spawnPaths := codexSpawnAgentToolPaths(updated)
	if len(spawnPaths) == 0 || hasCodexOptimizedCollaborationConflict(updated) {
		return updated, false
	}
	return optimizeCodexCollaborationNamespace(updated, spawnPaths)
}

// prepareCodexMultiAgentV2Tools performs the shared tool-definition cleanup
// used before either native Codex forwarding or cross-protocol translation.
// It intentionally keeps the collaboration namespace unchanged.
func prepareCodexMultiAgentV2Tools(headers http.Header, payload []byte, models []string) []byte {
	if !codexMultiAgentV2Enabled(headers) {
		return payload
	}
	updated := removeCodexCollaborationMessageEncryption(payload, codexCollaborationMessageToolPaths(payload))
	spawnPaths := codexSpawnAgentToolPaths(updated)
	if len(spawnPaths) == 0 || hasCodexOptimizedCollaborationConflict(updated) {
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

func hasCodexOptimizedCollaborationConflict(payload []byte) bool {
	return codexToolsHaveOptimizedConflict(gjson.GetBytes(payload, "tools")) || func() bool {
		input := gjson.GetBytes(payload, "input")
		if !input.IsArray() {
			return false
		}
		for _, item := range input.Array() {
			if strings.TrimSpace(item.Get("type").String()) == "additional_tools" && codexToolsHaveOptimizedConflict(item.Get("tools")) {
				return true
			}
		}
		return false
	}()
}

func codexToolsHaveOptimizedConflict(tools gjson.Result) bool {
	if !tools.IsArray() {
		return false
	}
	for _, tool := range tools.Array() {
		name := strings.TrimSpace(tool.Get("name").String())
		if name == codexOptimizedCollaboration || strings.HasPrefix(name, codexOptimizedNamePrefix) {
			return true
		}
		if strings.TrimSpace(tool.Get("type").String()) == "namespace" && codexToolsHaveOptimizedConflict(tool.Get("tools")) {
			return true
		}
	}
	return false
}

func optimizeCodexCollaborationNamespace(payload []byte, paths []string) ([]byte, bool) {
	updated := payload
	optimized := false
	for _, path := range paths {
		separator := strings.LastIndex(path, ".tools.")
		if separator < 0 {
			continue
		}
		namespacePath := path[:separator]
		namespace := gjson.GetBytes(updated, namespacePath)
		if strings.TrimSpace(namespace.Get("type").String()) != "namespace" || strings.TrimSpace(namespace.Get("name").String()) != codexCollaborationNamespace {
			continue
		}
		var err error
		updated, err = sjson.SetBytes(updated, namespacePath+".name", codexOptimizedCollaboration)
		if err != nil {
			return payload, false
		}
		optimized = true
	}
	return updated, optimized
}

// restoreCodexMultiAgentV2Response restores only tool identity carriers. It
// deliberately does not walk arguments/input/output because those fields can
// contain user data that happens to mention the reserved names.
func restoreCodexMultiAgentV2Response(payload []byte, optimized bool) []byte {
	if !optimized || len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload
	}
	type update struct {
		path  string
		value string
	}
	updates := make([]update, 0, 2)
	var walk func(gjson.Result, string, int)
	walk = func(value gjson.Result, prefix string, depth int) {
		if depth > jsonWalkMaxDepth {
			return
		}
		if value.IsArray() {
			for index, child := range value.Array() {
				walk(child, sjsonPathJoin(prefix, fmt.Sprintf("%d", index)), depth+1)
			}
			return
		}
		if !value.IsObject() {
			return
		}

		itemType := strings.TrimSpace(value.Get("type").String())
		isToolCall := itemType == "function_call" || itemType == "custom_tool_call"
		if namespace := value.Get("namespace"); isToolCall && namespace.Type == gjson.String && namespace.String() == codexOptimizedCollaboration {
			updates = append(updates, update{
				path:  sjsonObjectPathJoin(prefix, "namespace"),
				value: codexCollaborationNamespace,
			})
		}
		if name := value.Get("name"); name.Type == gjson.String {
			newName := ""
			switch {
			case itemType == "namespace" && name.String() == codexOptimizedCollaboration:
				newName = codexCollaborationNamespace
			case isToolCall && strings.HasPrefix(name.String(), codexOptimizedNamePrefix):
				newName = codexCollaborationNamespace + "__" + strings.TrimPrefix(name.String(), codexOptimizedNamePrefix)
			}
			if newName != "" {
				updates = append(updates, update{
					path:  sjsonObjectPathJoin(prefix, "name"),
					value: newName,
				})
			}
		}

		value.ForEach(func(key, child gjson.Result) bool {
			keyName := key.String()
			if keyName == "arguments" || keyName == "input" || (keyName == "output" && (itemType == "function_call_output" || itemType == "custom_tool_call_output")) {
				return true
			}
			walk(child, sjsonObjectPathJoin(prefix, keyName), depth+1)
			return true
		})
	}
	walk(gjson.ParseBytes(payload), "", 0)
	if len(updates) == 0 {
		return payload
	}
	updated := payload
	for _, item := range updates {
		var err error
		updated, err = sjson.SetBytes(updated, item.path, item.value)
		if err != nil {
			return payload
		}
	}
	return updated
}

// restoreCodexMultiAgentV2SSEEvent applies response restoration to the JSON
// data line while keeping SSE framing, event names, and blank-line delimiters.
func restoreCodexMultiAgentV2SSEEvent(raw []byte, optimized bool) []byte {
	if !optimized || len(raw) == 0 {
		return raw
	}
	lines := bytes.SplitAfter(raw, []byte("\n"))
	changed := false
	for index, line := range lines {
		trimmed := bytes.TrimRight(line, "\r\n")
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) || !gjson.ValidBytes(data) {
			continue
		}
		restored := restoreCodexMultiAgentV2Response(data, true)
		if bytes.Equal(restored, data) {
			continue
		}
		newline := line[len(trimmed):]
		lines[index] = append(append([]byte("data: "), restored...), newline...)
		changed = true
	}
	if !changed {
		return raw
	}
	return bytes.Join(lines, nil)
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

func (s *Server) updateCodexMultiAgentV2SessionState(
	session *responsesExecutionSession,
	reqCtx *proxyRequestContext,
) {
	if session == nil || reqCtx == nil {
		return
	}
	if !reqCtx.codexMultiAgentV2Conflict && !reqCtx.codexMultiAgentV2Optimized {
		return
	}
	session.setCodexMultiAgentV2State(reqCtx.codexMultiAgentV2Optimized)
}
