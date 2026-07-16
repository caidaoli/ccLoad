package app

import (
	"encoding/json"
	"fmt"
	"strings"

	"ccLoad/internal/protocol"

	"github.com/bytedance/sonic"
	"github.com/gin-gonic/gin"
)

const (
	clientProtocolContextKey = "ccLoad.clientProtocol"
	clientPathContextKey     = "ccLoad.clientPath"
)

func captureClientRequestMetadata() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(clientProtocolContextKey, detectClientProtocolFromPath(c.Request.URL.Path))
		c.Set(clientPathContextKey, c.Request.URL.Path)
		c.Next()
	}
}

func captureDashboardProxyMetadata() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := strings.TrimPrefix(c.Request.URL.Path, "/dashboard")
		if path == "" {
			path = "/"
		}
		c.Request.URL.Path = path
		c.Set(clientProtocolContextKey, detectClientProtocolFromPath(path))
		c.Set(clientPathContextKey, path)
		c.Next()
	}
}

func clientRequestMetadata(c *gin.Context) (protocol.Protocol, string) {
	clientProtocol, _ := c.Get(clientProtocolContextKey)
	clientPath, _ := c.Get(clientPathContextKey)

	p, _ := clientProtocol.(protocol.Protocol)
	path, _ := clientPath.(string)
	if path == "" {
		path = c.Request.URL.Path
	}
	if p == "" {
		p = detectClientProtocolFromPath(path)
	}
	return p, path
}

func detectClientProtocolFromPath(path string) protocol.Protocol {
	switch protocol.DetectRequestFamily(path) {
	case protocol.RequestFamilyMessages:
		return protocol.Anthropic
	case protocol.RequestFamilyResponses, protocol.RequestFamilyAlphaSearch:
		return protocol.Codex
	case protocol.RequestFamilyChatCompletions,
		protocol.RequestFamilyCompletions,
		protocol.RequestFamilyEmbeddings,
		protocol.RequestFamilyImages:
		return protocol.OpenAI
	case protocol.RequestFamilyGenerateContent:
		return protocol.Gemini
	default:
		return ""
	}
}

func validateClientBodyMatchesProtocol(clientProtocol protocol.Protocol, body []byte) error {
	if clientProtocol == protocol.OpenAI || !looksLikeOpenAIChatCompletionsBody(body) {
		return nil
	}
	return fmt.Errorf("request body looks like OpenAI chat completions but path uses %s protocol", clientProtocol)
}

func sanitizeCodexAlphaSearchBody(body []byte) []byte {
	var payload map[string]json.RawMessage
	if err := sonic.Unmarshal(body, &payload); err != nil || payload == nil {
		return body
	}

	removed := false
	for _, field := range []string{"prompt_cache_key", "prompt_cache_retention"} {
		if _, exists := payload[field]; exists {
			delete(payload, field)
			removed = true
		}
	}
	if !removed {
		return body
	}

	sanitized, err := sonic.Marshal(payload)
	if err != nil {
		return body
	}
	return sanitized
}

func looksLikeOpenAIChatCompletionsBody(body []byte) bool {
	var root map[string]json.RawMessage
	if err := sonic.Unmarshal(body, &root); err != nil {
		return false
	}
	if _, ok := root["messages"]; !ok {
		return false
	}

	for _, key := range []string{
		"response_format",
		"stream_options",
		"prompt_cache_key",
		"parallel_tool_calls",
		"max_completion_tokens",
		"reasoning_effort",
		"frequency_penalty",
		"presence_penalty",
		"seed",
	} {
		if _, ok := root[key]; ok {
			return true
		}
	}

	if isOpenAITools(root["tools"]) || isOpenAIToolChoice(root["tool_choice"]) {
		return true
	}
	return hasOpenAIMessageOnlyFields(root["messages"])
}

func isOpenAITools(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var tools []map[string]json.RawMessage
	if err := sonic.Unmarshal(raw, &tools); err != nil {
		return false
	}
	for _, tool := range tools {
		if hasRawKey(tool, "function") || rawStringValue(tool["type"]) == "function" {
			return true
		}
	}
	return false
}

func isOpenAIToolChoice(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var choice string
	if err := sonic.Unmarshal(raw, &choice); err == nil {
		return choice == "none" || choice == "auto" || choice == "required"
	}
	var obj map[string]json.RawMessage
	if err := sonic.Unmarshal(raw, &obj); err != nil {
		return false
	}
	return rawStringValue(obj["type"]) == "function" || hasRawKey(obj, "function")
}

func hasOpenAIMessageOnlyFields(raw json.RawMessage) bool {
	var messages []map[string]json.RawMessage
	if err := sonic.Unmarshal(raw, &messages); err != nil {
		return false
	}
	for _, message := range messages {
		switch rawStringValue(message["role"]) {
		case "developer", "tool":
			return true
		}
		for _, key := range []string{"tool_calls", "tool_call_id", "reasoning_content"} {
			if hasRawKey(message, key) {
				return true
			}
		}
	}
	return false
}

func hasRawKey(m map[string]json.RawMessage, key string) bool {
	_, ok := m[key]
	return ok
}

func rawStringValue(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value string
	_ = sonic.Unmarshal(raw, &value)
	return value
}
