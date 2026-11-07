package qoderauth

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5" // Qoder's COSY wire signature requires MD5.
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

const cosyVersion = "1.1.38"

// These are public protocol constants distributed with the Qoder client.
const cosyModulus = "c0f22307e5cd362e296bb04470f6de8fbf935ce24e8fcf511a0e2701329769c4a76e499bb938036a52af1eaf818cf79a2600620e3ce87e371d2ca6d85803606a1b3fa5e874643c9ed2db7e85673ef7227fca56e2e7c08f0927609bb896a9f24be1782099a66016a5bfdc3f1ff756bfc9e88d7b5dc5be30bf45a0223a00ebcecf"
const cosyAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"

func (s *Service) wireURL(c *Credential, path string) string {
	base := s.BaseURL
	if base == "" {
		base = c.GatewayURL()
	}
	return strings.TrimRight(base, "/") + path
}

// BuildChatRequest constructs Qoder's native agent request. The upstream always
// streams; the caller may aggregate its normalized chunks for a nonstream client.
func (s *Service) BuildChatRequest(ctx context.Context, c *Credential, body []byte, model Model) (*http.Request, error) {
	if c == nil || model.Key == "" {
		return nil, errors.New("qoder: credential and model key are required")
	}
	var input map[string]json.RawMessage
	if err := json.Unmarshal(body, &input); err != nil {
		return nil, fmt.Errorf("qoder chat request: %w", err)
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(input["messages"], &messages); err != nil || len(messages) == 0 {
		return nil, errors.New("qoder: messages must be a nonempty array")
	}
	messages, err := cosyMessages(messages)
	if err != nil {
		return nil, err
	}
	lastUser := ""
	for _, message := range messages {
		var role string
		_ = json.Unmarshal(message["role"], &role)
		if role == "user" {
			lastUser = cosyContentText(message["content"])
		}
	}
	config := map[string]json.RawMessage{}
	if len(model.Config) != 0 {
		if err := json.Unmarshal(model.Config, &config); err != nil {
			return nil, fmt.Errorf("qoder model config: %w", err)
		}
	}
	var reasoning bool
	_ = json.Unmarshal(config["is_reasoning"], &reasoning)
	model.SupportsReasoning = model.SupportsReasoning || reasoning
	slim := map[string]any{"key": model.Key}
	for _, key := range []string{"is_reasoning", "is_vl", "source", "format"} {
		if value, ok := config[key]; ok {
			slim[key] = value
		}
	}
	params := map[string]any{}
	for _, key := range []string{"temperature", "top_p", "stop", "frequency_penalty", "presence_penalty", "seed", "tool_choice", "parallel_tool_calls", "response_format"} {
		if value, ok := input[key]; ok {
			params[key] = value
		}
	}
	limit := 32768
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		var n int
		if json.Unmarshal(input[key], &n) == nil && n > 0 {
			limit = min(n, 32768)
		}
	}
	params["max_tokens"] = limit
	cosyThinking(input, model, params)
	tools := json.RawMessage(`[]`)
	if value := input["tools"]; len(value) != 0 && string(value) != "null" {
		var declarations []struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(value, &declarations); err != nil {
			return nil, errors.New("qoder: tools must be an array")
		}
		for _, tool := range declarations {
			if tool.Type != "function" {
				return nil, fmt.Errorf("qoder: unsupported tool type %q", tool.Type)
			}
		}
		tools = value
	}
	contextLength, err := cosyContextLength(config, messages, tools)
	if err != nil {
		return nil, err
	}
	extra := map[string]any{"context": []any{}, "modelConfig": map[string]any{"key": model.Key, "is_reasoning": model.SupportsReasoning}, "originalContent": lastUser}
	if contextLength > 0 {
		params["context_length"] = contextLength
		slim["max_input_tokens"] = contextLength
		extra["ideModelConfigOverride"] = map[string]int{"max_input_tokens": contextLength}
	}
	recordID, requestID := uuid.NewString(), uuid.NewString()
	sessionID := uuid.NewString()
	for _, key := range []string{"session_id", "user"} {
		var seed string
		if json.Unmarshal(input[key], &seed) == nil && seed != "" {
			sessionID = uuid.NewSHA1(uuid.NameSpaceURL, []byte(c.UserID+"\x00"+model.Key+"\x00"+seed)).String()
			break
		}
	}
	envelope := map[string]any{
		"request_id": requestID, "request_set_id": recordID, "chat_record_id": recordID,
		"session_id": sessionID, "stream": true, "chat_task": "FREE_INPUT",
		"is_reply": true, "is_retry": false, "source": 1, "version": "3",
		"session_type": "qodercli", "agent_id": "agent_common", "task_id": "common",
		"code_language": "", "chat_prompt": "", "image_urls": nil, "aliyun_user_type": "", "system": "",
		"messages": messages, "tools": tools, "parameters": params, "model_config": slim,
		"chat_context": map[string]any{
			"chatPrompt": "", "imageUrls": nil, "features": []any{}, "text": lastUser,
			"extra": extra,
		},
		"business": map[string]any{"product": "cli", "version": "1.0.0", "type": "agent", "stage": "start", "id": uuid.NewString(), "name": string([]rune(lastUser)[:min(len([]rune(lastUser)), 30)]), "begin_at": time.Now().UnixMilli()},
	}
	plain, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("qoder envelope: %w", err)
	}
	encoded := cosyEncode(plain)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.wireURL(c, "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"), bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	if err := cosySign(req, c, encoded); err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("X-Model-Key", model.Key)
	source := "system"
	_ = json.Unmarshal(config["source"], &source)
	req.Header.Set("X-Model-Source", source)
	return req, nil
}

func cosyThinking(input map[string]json.RawMessage, model Model, params map[string]any) {
	var raw any
	for _, key := range []string{"reasoning_effort", "reasoning", "thinking"} {
		if value, ok := input[key]; ok && string(value) != "null" {
			_ = json.Unmarshal(value, &raw)
			break
		}
	}
	if obj, ok := raw.(map[string]any); ok {
		if effort, exists := obj["effort"]; exists {
			raw = effort
		} else if enabled, exists := obj["type"]; exists {
			raw = enabled
		}
	}
	if raw == false || raw == "off" || raw == "none" || raw == "disabled" {
		// Qoder reasoning models cannot reliably disable thinking; omitting the
		// flag keeps their native default instead of corrupting the response.
		return
	}
	if !model.SupportsReasoning && raw == nil {
		return
	}
	params["enable_thinking"] = true
	if effort, ok := raw.(string); ok {
		allowed := model.Efforts
		if len(allowed) == 0 {
			allowed = []string{"low", "medium", "xhigh"}
		}
		if !slices.Contains(allowed, effort) {
			switch effort {
			case "minimal", "min":
				effort = "low"
			case "high", "max":
				effort = "xhigh"
			case "enabled":
				return
			}
		}
		if !slices.Contains(allowed, effort) {
			effort = allowed[0]
			for _, supported := range allowed {
				if supported == "medium" {
					effort = supported
				}
			}
		}
		params["reasoning_effort"] = effort
	}
}

func cosyContextLength(config map[string]json.RawMessage, messages []map[string]json.RawMessage, tools json.RawMessage) (int, error) {
	raw := bytes.TrimSpace(config["context_config"])
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return 0, nil
	}
	type tier struct {
		Tokens       int  `json:"token_count"`
		TokensCamel  int  `json:"tokenCount"`
		Default      bool `json:"is_default"`
		DefaultCamel bool `json:"isDefault"`
	}
	var tiers []tier
	if raw[0] == '{' {
		var entries map[string]tier
		if err := json.Unmarshal(raw, &entries); err != nil {
			return 0, fmt.Errorf("qoder context config: %w", err)
		}
		for _, entry := range entries {
			tiers = append(tiers, entry)
		}
	} else if err := json.Unmarshal(raw, &tiers); err != nil {
		return 0, fmt.Errorf("qoder context config: %w", err)
	}
	minimum, maximum, defaultTokens := 0, 0, 0
	for i := range tiers {
		entry := &tiers[i]
		if entry.Tokens == 0 {
			entry.Tokens = entry.TokensCamel
		}
		if entry.Tokens <= 0 {
			continue
		}
		if minimum == 0 || entry.Tokens < minimum {
			minimum = entry.Tokens
		}
		maximum = max(maximum, entry.Tokens)
		if entry.Default || entry.DefaultCamel {
			defaultTokens = entry.Tokens
		}
	}
	if maximum == 0 {
		return 0, nil
	}
	var current int
	if raw := config["max_input_tokens"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &current); err != nil {
			return 0, fmt.Errorf("qoder current context length: %w", err)
		}
	}
	if current <= 0 {
		current = defaultTokens
		if current == 0 {
			current = minimum
		}
	}
	payload, err := json.Marshal(map[string]any{"messages": messages, "tools": tools})
	if err != nil {
		return 0, fmt.Errorf("qoder context input: %w", err)
	}
	var characters, cjk int
	for _, ch := range string(payload) {
		characters++
		if ch >= 0x1100 && ch <= 0x11ff || ch >= 0x2e80 && ch <= 0x9fff ||
			ch >= 0xac00 && ch <= 0xd7af || ch >= 0xf900 && ch <= 0xfaff || ch >= 0xff00 && ch <= 0xffef {
			cjk++
		}
	}
	// Keep room for tokenizer differences and serialized tool declarations.
	needed := ((cjk+(characters-cjk+3)/4)*115 + 99) / 100
	if needed <= current {
		return 0, nil
	}
	selected := maximum
	for _, entry := range tiers {
		if entry.Tokens > current && entry.Tokens >= needed {
			selected = min(selected, entry.Tokens)
		}
	}
	if selected <= current {
		return 0, nil
	}
	return selected, nil
}

func cosyContentText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var texts []string
	for _, part := range parts {
		if part.Text != "" {
			texts = append(texts, part.Text)
		}
	}
	return strings.Join(texts, "\n")
}

func cosyMessages(messages []map[string]json.RawMessage) ([]map[string]json.RawMessage, error) {
	systems := make([]map[string]json.RawMessage, 0)
	conversation := make([]map[string]json.RawMessage, 0, len(messages))
	for _, message := range messages {
		var role string
		_ = json.Unmarshal(message["role"], &role)
		switch role {
		case "system", "developer", "user", "assistant", "tool":
		default:
			return nil, fmt.Errorf("qoder: unsupported message role %q", role)
		}
		var text string
		raw := message["content"]
		if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
			if role != "assistant" {
				return nil, fmt.Errorf("qoder: %s message requires content", role)
			}
		} else if json.Unmarshal(raw, &text) != nil {
			var parts []struct {
				Type     string          `json:"type"`
				Text     string          `json:"text"`
				ImageURL json.RawMessage `json:"image_url"`
			}
			if err := json.Unmarshal(raw, &parts); err != nil {
				return nil, fmt.Errorf("qoder: invalid %s message content", role)
			}
			var texts []string
			images := false
			for _, part := range parts {
				switch part.Type {
				case "text":
					texts = append(texts, part.Text)
				case "image_url":
					if role != "user" || len(part.ImageURL) == 0 {
						return nil, fmt.Errorf("qoder: image content is only supported in user messages")
					}
					images = true
				default:
					return nil, fmt.Errorf("qoder: unsupported content type %q", part.Type)
				}
			}
			text = strings.Join(texts, "\n")
			if images {
				conversation = append(conversation, message)
				continue
			}
		}
		if role == "assistant" {
			var calls []struct {
				Type     string `json:"type"`
				Function struct {
					Arguments json.RawMessage `json:"arguments"`
				} `json:"function"`
			}
			if rawCalls := message["tool_calls"]; len(rawCalls) != 0 && !bytes.Equal(rawCalls, []byte("null")) {
				if json.Unmarshal(rawCalls, &calls) != nil {
					return nil, errors.New("qoder: tool_calls must be an array")
				}
				for _, call := range calls {
					var arguments string
					if call.Type != "function" || json.Unmarshal(call.Function.Arguments, &arguments) != nil {
						return nil, errors.New("qoder: tool_calls require function calls with JSON string arguments")
					}
				}
			}
			if text == "" && len(calls) != 0 {
				text = " "
			}
		}
		message["content"], _ = json.Marshal(text)
		if role == "system" || role == "developer" {
			message["role"] = json.RawMessage(`"system"`)
			systems = append(systems, message)
		} else {
			conversation = append(conversation, message)
		}
	}
	return append(systems, conversation...), nil
}

func cosyEncode(plain []byte) []byte {
	src := base64.StdEncoding.EncodeToString(plain)
	n := len(src) / 3
	src = src[len(src)-n:] + src[n:len(src)-n] + src[:n]
	const standard = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	return []byte(strings.Map(func(r rune) rune {
		if r == '=' {
			return '$'
		}
		return rune(cosyAlphabet[strings.IndexRune(standard, r)])
	}, src))
}

func cosySign(req *http.Request, c *Credential, body []byte) error {
	if c == nil || c.UserID == "" || c.AccessToken == "" || c.MachineID == "" {
		return errors.New("qoder: user ID, access token and machine ID are required for COSY")
	}
	randomKey := make([]byte, 8)
	if _, err := rand.Read(randomKey); err != nil {
		return err
	}
	key := []byte(hex.EncodeToString(randomKey))
	identity, err := json.Marshal(map[string]string{"uid": c.UserID, "security_oauth_token": c.AccessToken, "name": c.Name, "aid": "", "email": c.Email})
	if err != nil {
		return err
	}
	padding := aes.BlockSize - len(identity)%aes.BlockSize
	identity = append(identity, bytes.Repeat([]byte{byte(padding)}, padding)...)
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	cipher.NewCBCEncrypter(block, key).CryptBlocks(identity, identity)
	modulus, ok := new(big.Int).SetString(cosyModulus, 16)
	if !ok {
		return errors.New("qoder: invalid COSY public key")
	}
	//nolint:staticcheck // COSY requires PKCS#1 v1.5; OAEP changes the external wire contract.
	wrapped, err := rsa.EncryptPKCS1v15(rand.Reader, &rsa.PublicKey{N: modulus, E: 65537}, key)
	if err != nil {
		return err
	}
	requestID := uuid.NewString()
	payloadJSON, err := json.Marshal(map[string]string{"version": "v1", "requestId": requestID, "info": base64.StdEncoding.EncodeToString(identity), "cosyVersion": cosyVersion, "ideVersion": ""})
	if err != nil {
		return err
	}
	payload, wrappedKey := base64.StdEncoding.EncodeToString(payloadJSON), base64.StdEncoding.EncodeToString(wrapped)
	date := strconv.FormatInt(time.Now().Unix(), 10)
	path := strings.TrimPrefix(req.URL.EscapedPath(), "/algo")
	digest := md5.Sum([]byte(payload + "\n" + wrappedKey + "\n" + date + "\n" + string(body) + "\n" + path))
	bodyHash := md5.Sum(body)
	arch := runtime.GOARCH
	switch arch {
	case "arm64":
		arch = "aarch64"
	case "amd64":
		arch = "x86_64"
	}
	for name, value := range map[string]string{
		"User-Agent":    "Qoder",
		"Authorization": "Bearer COSY." + payload + "." + hex.EncodeToString(digest[:]),
		"Cosy-Key":      wrappedKey, "Cosy-User": c.UserID, "Cosy-Date": date, "Cosy-Version": cosyVersion,
		"Cosy-Machineid": c.MachineID, "Cosy-Machinetoken": c.MachineID, "Cosy-Machinetype": "5",
		"Cosy-Machineos": arch + "_" + runtime.GOOS, "Cosy-Clienttype": "5", "Cosy-Clientip": "127.0.0.1",
		"Cosy-Bodyhash": hex.EncodeToString(bodyHash[:]), "Cosy-Bodylength": strconv.Itoa(len(body)), "Cosy-Sigpath": path,
		"Cosy-Data-Policy": "disagree", "Cosy-Organization-Id": "", "Cosy-Organization-Tags": "", "Login-Version": "v2", "X-Request-Id": requestID,
	} {
		req.Header.Set(name, value)
	}
	return nil
}

// cosyModelID derives the public model name: display name without whitespace, lowercased.
func cosyModelID(display string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, display)
}
