package cursorauth

import (
	"encoding/json"
	"regexp"
	"strings"
)

var (
	thinkingEffortSuffix = regexp.MustCompile(`(?i)-thinking-(none|low|medium|high|xhigh|max)(-fast)?$`)
	thinkingBareSuffix   = regexp.MustCompile(`(?i)-thinking(-fast)?$`)
	familyEffortSuffix   = regexp.MustCompile(`(?i)^(.*?)-(none|low|medium|high|xhigh|max|extra-high)(-fast)?$`)
)

var legacyFamilyAliases = map[string]string{
	"gpt-4":                      "gpt-5.3-codex",
	"gpt-4o":                     "gpt-5.3-codex",
	"gpt-4-turbo":                "gpt-5.3-codex",
	"gpt-4o-mini":                "gpt-5.3-codex-low-fast",
	"gpt-3.5-turbo":              "gpt-5.3-codex-low-fast",
	"claude-3-opus":              "claude-opus-5",
	"claude-3-opus-20240229":     "claude-opus-5",
	"claude-3-sonnet":            "claude-sonnet-5",
	"claude-3-sonnet-20240229":   "claude-sonnet-5",
	"claude-3.5-sonnet":          "claude-sonnet-5",
	"claude-3.5-sonnet-20241022": "claude-sonnet-5",
	"claude-3.5-haiku":           "claude-sonnet-5",
	"claude-3.5-haiku-20241022":  "claude-sonnet-5",
	"claude-4-5-sonnet-20250601": "claude-sonnet-5",
	"claude-4.5-sonnet":          "claude-sonnet-5",
	"claude-4-sonnet":            "claude-sonnet-5",
	"claude-sonnet-4-5":          "claude-sonnet-5",
	"claude-sonnet-4-5-20250929": "claude-sonnet-5",
	"claude-haiku-4-5":           "claude-sonnet-5",
	"claude-haiku-4-5-20251001":  "claude-sonnet-5",
	"gemini-pro":                 "gemini-3.1-pro",
	"gemini-2.0-flash":           "gemini-3.6-flash-high",
	"gemini-2.5-pro":             "gemini-3.1-pro",
}

// ClientThinking is the caller's thinking / reasoning preference.
type ClientThinking struct {
	Enabled bool
	Effort  string
}

// PublicModelID strips Cursor's -thinking-* infix so channel catalogs stay
// stable while inference still maps thinking from the client request.
func PublicModelID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	id = thinkingEffortSuffix.ReplaceAllString(id, "-$1$2")
	id = thinkingBareSuffix.ReplaceAllString(id, "$1")
	return id
}

// ResolveModel maps a client model id plus thinking fields onto the Cursor
// model slug cursor-agent expects.
func ResolveModel(requested string, thinking ClientThinking) string {
	if strings.TrimSpace(requested) == "" {
		requested = "claude-sonnet-5"
	}
	if mapped, ok := legacyFamilyAliases[strings.ToLower(strings.TrimSpace(requested))]; ok {
		requested = mapped
	}
	parsed := parseFamilyEffort(requested)
	effort := parsed.effort
	if effort == "" {
		effort = thinking.Effort
	}
	if effort == "" {
		effort = "high"
	}
	useThinking := parsed.hadThinking || thinking.Enabled
	if !strings.HasPrefix(strings.ToLower(parsed.family), "claude-") {
		if parsed.effort != "" {
			return parsed.family + "-" + parsed.effort + parsed.fastSuffix
		}
		return parsed.family
	}
	return claudeCursorID(parsed.family, useThinking, effort, parsed.fastSuffix)
}

type parsedModel struct {
	family      string
	effort      string
	fastSuffix  string
	hadThinking bool
}

func parseFamilyEffort(id string) parsedModel {
	hadThinking := strings.Contains(strings.ToLower(id), "thinking")
	stripped := upgradeLegacySonnet(PublicModelID(id))
	match := familyEffortSuffix.FindStringSubmatch(stripped)
	if len(match) == 4 {
		effort := strings.ToLower(match[2])
		if effort == "extra-high" {
			effort = "xhigh"
		}
		return parsedModel{family: match[1], effort: effort, fastSuffix: match[3], hadThinking: hadThinking}
	}
	return parsedModel{family: stripped, hadThinking: hadThinking}
}

func upgradeLegacySonnet(id string) string {
	lower := strings.ToLower(id)
	switch {
	case lower == "claude-4.5-sonnet", lower == "claude-4-sonnet",
		strings.HasPrefix(lower, "claude-4.6-sonnet"),
		strings.HasPrefix(lower, "claude-sonnet-4-5"),
		strings.HasPrefix(lower, "claude-sonnet-4-6"),
		strings.Contains(lower, "3.5-sonnet"),
		strings.Contains(lower, "3-sonnet"),
		lower == "claude-4-5-sonnet-20250601",
		lower == "claude-haiku-4-5",
		lower == "claude-haiku-4-5-20251001",
		strings.Contains(lower, "3.5-haiku"),
		strings.Contains(lower, "3-5-haiku"):
		return "claude-sonnet-5"
	default:
		return id
	}
}

func claudeCursorID(family string, thinking bool, effort, fastSuffix string) string {
	if effort == "" {
		effort = "high"
	}
	if thinking && claudeSupportsThinking(family) {
		if (family == "claude-fable-5" || family == "claude-sonnet-5") && effort == "xhigh" {
			return family + "-thinking-xhigh" + fastSuffix
		}
		return family + "-thinking-" + effort + fastSuffix
	}
	if family == "claude-opus-5" && (effort == "xhigh" || effort == "max") && !thinking {
		return "claude-opus-5-high" + fastSuffix
	}
	return family + "-" + effort + fastSuffix
}

func claudeSupportsThinking(family string) bool {
	return family == "claude-opus-5" || family == "claude-sonnet-5" || family == "claude-fable-5" ||
		strings.HasPrefix(family, "claude-opus-4-") || strings.HasPrefix(family, "claude-4.")
}

// ParseClientThinking reads Anthropic thinking / OpenAI reasoning_effort from a
// JSON request body. The default matches cursor2Oauth: thinking on, effort high.
func ParseClientThinking(body []byte) ClientThinking {
	defaults := ClientThinking{Enabled: true, Effort: "high"}
	if len(body) == 0 {
		return defaults
	}
	var request struct {
		Thinking             json.RawMessage `json:"thinking"`
		ReasoningEffort      string          `json:"reasoning_effort"`
		ReasoningEffortCamel string          `json:"reasoningEffort"`
	}
	if json.Unmarshal(body, &request) != nil {
		return defaults
	}
	if len(request.Thinking) > 0 && string(request.Thinking) != "null" {
		var thinking struct {
			Type         string `json:"type"`
			BudgetTokens int    `json:"budget_tokens"`
		}
		if json.Unmarshal(request.Thinking, &thinking) == nil {
			typ := strings.ToLower(strings.TrimSpace(thinking.Type))
			if typ == "disabled" || typ == "none" {
				return ClientThinking{Enabled: false, Effort: "high"}
			}
			return ClientThinking{Enabled: true, Effort: budgetToEffort(thinking.BudgetTokens)}
		}
	}
	effort := strings.ToLower(strings.TrimSpace(request.ReasoningEffort))
	if effort == "" {
		effort = strings.ToLower(strings.TrimSpace(request.ReasoningEffortCamel))
	}
	switch effort {
	case "low", "medium", "high", "xhigh", "max":
		return ClientThinking{Enabled: true, Effort: effort}
	default:
		return defaults
	}
}

func budgetToEffort(budget int) string {
	switch {
	case budget >= 32000:
		return "max"
	case budget >= 16000:
		return "xhigh"
	case budget >= 8000:
		return "high"
	case budget >= 4000:
		return "medium"
	case budget > 0:
		return "low"
	default:
		return "high"
	}
}

// ExtractPrompt builds the text cursor-agent receives. Client tools are not
// forwarded: cursor-agent --mode ask is text-only, matching cursor2Oauth.
func ExtractPrompt(body []byte) string {
	var request map[string]any
	if json.Unmarshal(body, &request) != nil {
		return ""
	}
	var parts []string
	if system := stringifyContent(request["system"]); system != "" {
		parts = append(parts, system)
	}
	messages, _ := request["messages"].([]any)
	if len(messages) == 0 {
		if input, _ := request["input"].([]any); len(input) > 0 {
			messages = input
		}
	}
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		if message == nil {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(asString(message["role"])))
		text := stringifyContent(message["content"])
		if text == "" {
			text = stringifyContent(message["text"])
		}
		if text == "" {
			continue
		}
		if role != "" {
			parts = append(parts, role+": "+text)
			continue
		}
		parts = append(parts, text)
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func stringifyContent(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(typed)
	case []any:
		chunks := make([]string, 0, len(typed))
		for _, item := range typed {
			switch block := item.(type) {
			case string:
				if text := strings.TrimSpace(block); text != "" {
					chunks = append(chunks, text)
				}
			case map[string]any:
				if text := strings.TrimSpace(asString(block["text"])); text != "" {
					chunks = append(chunks, text)
				}
			}
		}
		return strings.TrimSpace(strings.Join(chunks, "\n"))
	default:
		return strings.TrimSpace(asString(value))
	}
}

func asString(value any) string {
	text, _ := value.(string)
	return text
}

// RequestModelID reads the top-level model field from a JSON request body.
func RequestModelID(body []byte) string {
	var request struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &request) != nil {
		return ""
	}
	return strings.TrimSpace(request.Model)
}
