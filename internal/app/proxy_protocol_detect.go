package app

import (
	"encoding/json"
	"fmt"

	"ccLoad/internal/protocol"
	"ccLoad/internal/util"

	"github.com/bytedance/sonic"
	"github.com/gin-gonic/gin"
)

const (
	clientProtocolContextKey = "ccLoad.clientProtocol"
	clientPathContextKey     = "ccLoad.clientPath"
)

func captureClientRequestMetadata() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(clientProtocolContextKey, protocol.Protocol(util.DetectChannelTypeFromPath(c.Request.URL.Path)))
		c.Set(clientPathContextKey, c.Request.URL.Path)
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
		p = protocol.Protocol(util.DetectChannelTypeFromPath(path))
	}
	return p, path
}

func validateClientBodyMatchesProtocol(clientProtocol protocol.Protocol, body []byte) error {
	if clientProtocol == protocol.OpenAI || !looksLikeOpenAIChatCompletionsBody(body) {
		return nil
	}
	return fmt.Errorf("request body looks like OpenAI chat completions but path uses %s protocol", clientProtocol)
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
		case "system", "developer", "tool":
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
