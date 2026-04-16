package builtin

import (
	"crypto/sha1"
	"encoding/hex"
	"strings"

	"ccLoad/internal/protocol"

	"github.com/bytedance/sonic"
)

const codexToolNameLimit = 64

type codexToolAliases struct {
	OriginalToShort map[string]string
	ShortToOriginal map[string]string
}

func buildCodexToolAliases(names []string) codexToolAliases {
	aliases := codexToolAliases{
		OriginalToShort: make(map[string]string),
		ShortToOriginal: make(map[string]string),
	}
	used := make(map[string]string)
	for _, rawName := range names {
		name := strings.TrimSpace(rawName)
		if name == "" {
			continue
		}
		if short, ok := aliases.OriginalToShort[name]; ok {
			aliases.ShortToOriginal[short] = name
			continue
		}
		short := codexShortToolName(name, used)
		used[short] = name
		aliases.OriginalToShort[name] = short
		aliases.ShortToOriginal[short] = name
	}
	return aliases
}

func codexShortToolName(name string, used map[string]string) string {
	if len(name) <= codexToolNameLimit {
		if existing := used[name]; existing == "" || existing == name {
			return name
		}
	}
	sum := sha1.Sum([]byte(name))
	hash := hex.EncodeToString(sum[:])
	for n := 12; n <= len(hash); n += 4 {
		suffix := hash[:n]
		prefixLen := codexToolNameLimit - len(suffix) - 1
		if prefixLen < 1 {
			prefixLen = 1
		}
		prefix := name
		if len(prefix) > prefixLen {
			prefix = prefix[:prefixLen]
		}
		alias := prefix + "_" + suffix
		if existing := used[alias]; existing == "" || existing == name {
			return alias
		}
	}
	return hash[:codexToolNameLimit]
}

func (a codexToolAliases) shorten(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if short := a.OriginalToShort[name]; short != "" {
		return short
	}
	return name
}

func (a codexToolAliases) restore(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if original := a.ShortToOriginal[name]; original != "" {
		return original
	}
	return name
}

func collectCodexAliasNames(conv conversation) []string {
	names := make([]string, 0)
	seen := make(map[string]struct{})
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}

	for _, tool := range conv.Tools {
		if tool.toolType() == "function" {
			add(tool.Name)
		}
	}
	if conv.ToolChoice.Mode == "named" && conv.ToolChoice.toolType() == "function" {
		add(conv.ToolChoice.Name)
	}
	for _, turn := range conv.Turns {
		for _, part := range turn.Parts {
			switch part.Kind {
			case partKindToolCall:
				if part.ToolCall != nil {
					add(part.ToolCall.Name)
				}
			case partKindToolResult:
				if part.ToolResult != nil {
					add(part.ToolResult.Name)
				}
			}
		}
	}
	return names
}

func codexToolAliasesFromConversations(original, translated conversation) codexToolAliases {
	aliases := buildCodexToolAliases(collectCodexAliasNames(original))
	translatedNames := collectCodexAliasNames(translated)
	if len(translatedNames) == 0 {
		return aliases
	}
	originalNames := collectCodexAliasNames(original)
	for i, originalName := range originalNames {
		if i >= len(translatedNames) {
			break
		}
		shortName := strings.TrimSpace(translatedNames[i])
		if originalName == "" || shortName == "" {
			continue
		}
		aliases.OriginalToShort[originalName] = shortName
		aliases.ShortToOriginal[shortName] = originalName
	}
	return aliases
}

func codexToolAliasesFromRequests(source protocol.Protocol, rawReq, translatedReq []byte) codexToolAliases {
	original, ok := normalizeConversationFromRequest(source, rawReq)
	if !ok {
		return codexToolAliases{}
	}
	translated, ok := normalizeConversationFromRequest(protocol.Codex, translatedReq)
	if !ok {
		return buildCodexToolAliases(collectCodexAliasNames(original))
	}
	return codexToolAliasesFromConversations(original, translated)
}

func normalizeConversationFromRequest(source protocol.Protocol, rawReq []byte) (conversation, bool) {
	if len(rawReq) == 0 {
		return conversation{}, false
	}
	switch source {
	case protocol.OpenAI:
		var req openAIChatRequest
		if err := sonic.Unmarshal(rawReq, &req); err != nil {
			return conversation{}, false
		}
		conv, err := normalizeOpenAIConversation(req)
		return conv, err == nil
	case protocol.Gemini:
		var req geminiRequestPayload
		if err := sonic.Unmarshal(rawReq, &req); err != nil {
			return conversation{}, false
		}
		conv, err := normalizeGeminiConversation(req)
		return conv, err == nil
	case protocol.Anthropic:
		var req anthropicMessagesRequest
		if err := sonic.Unmarshal(rawReq, &req); err != nil {
			return conversation{}, false
		}
		conv, err := normalizeAnthropicConversation(req)
		return conv, err == nil
	case protocol.Codex:
		var req codexRequest
		if err := sonic.Unmarshal(rawReq, &req); err != nil {
			return conversation{}, false
		}
		conv, err := normalizeCodexConversation(req)
		return conv, err == nil
	default:
		return conversation{}, false
	}
}
