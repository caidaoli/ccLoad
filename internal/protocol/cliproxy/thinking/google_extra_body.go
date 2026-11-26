package thinking

import (
	"strconv"
	"strings"

	"github.com/MoeForge/moego/ojson"
)

func extractThinkingFieldsFromConfig(cfg ojson.Result) (ojson.Result, ojson.Result) {
	if !cfg.Exists() || !cfg.IsObject() {
		return ojson.Result{}, ojson.Result{}
	}
	var level, budget ojson.Result
	for _, key := range []string{"thinking_level", "thinkingLevel"} {
		if value := cfg.Get(key); value.Exists() && value.Type == ojson.String {
			if normalized := strings.ToLower(strings.TrimSpace(value.String())); normalized != "" {
				level = ojson.Result{Type: ojson.String, Str: normalized}
				break
			}
		}
	}
	for _, key := range []string{"thinking_budget", "thinkingBudget"} {
		if value := cfg.Get(key); value.Exists() && value.Type == ojson.Number {
			budget = value
			break
		}
	}
	return level, budget
}

func extractThinkingFieldsFromGoogleObject(google ojson.Result) (ojson.Result, ojson.Result) {
	if !google.Exists() || !google.IsObject() {
		return ojson.Result{}, ojson.Result{}
	}
	for _, key := range []string{"thinking_config", "thinkingConfig"} {
		if level, budget := extractThinkingFieldsFromConfig(google.Get(key)); level.Exists() || budget.Exists() {
			return level, budget
		}
	}
	return ojson.Result{}, ojson.Result{}
}

func extractOpenAIGoogleThinkingFields(get func(string) ojson.Result) (ojson.Result, ojson.Result) {
	if extraBody := get("extra_body"); extraBody.Exists() && extraBody.IsObject() {
		if level, budget := extractThinkingFieldsFromGoogleObject(extraBody.Get("google")); level.Exists() || budget.Exists() {
			return level, budget
		}
		if nestedExtra := extraBody.Get("extra_body"); nestedExtra.Exists() && nestedExtra.IsObject() {
			if level, budget := extractThinkingFieldsFromGoogleObject(nestedExtra.Get("google")); level.Exists() || budget.Exists() {
				return level, budget
			}
		}
	}
	if level, budget := extractThinkingFieldsFromGoogleObject(get("google")); level.Exists() || budget.Exists() {
		return level, budget
	}
	return ojson.Result{}, ojson.Result{}
}

func extractOpenAIGenerationThinkingFields(get func(string) ojson.Result) (ojson.Result, ojson.Result) {
	if gen := get("generationConfig"); gen.Exists() && gen.IsObject() {
		for _, key := range []string{"thinkingConfig", "thinking_config"} {
			if level, budget := extractThinkingFieldsFromConfig(gen.Get(key)); level.Exists() || budget.Exists() {
				return level, budget
			}
		}
	}
	if gen := get("generation_config"); gen.Exists() && gen.IsObject() {
		for _, key := range []string{"thinking_config", "thinkingConfig"} {
			if level, budget := extractThinkingFieldsFromConfig(gen.Get(key)); level.Exists() || budget.Exists() {
				return level, budget
			}
		}
	}
	return ojson.Result{}, ojson.Result{}
}

func extractOpenAIGoogleThinkingFallbackFields(get func(string) ojson.Result) (ojson.Result, ojson.Result) {
	if level, budget := extractOpenAIGenerationThinkingFields(get); level.Exists() || budget.Exists() {
		return level, budget
	}
	return extractOpenAIGoogleThinkingFields(get)
}

// ApplyOpenAIGoogleThinkingConfigDoc applies thinking_level/thinkingLevel or
// thinking_budget/thinkingBudget from OpenAI Chat compatibility fields
// (generationConfig/generation_config before extra_body.google.thinking_config)
// onto doc at thinkingPath.
func ApplyOpenAIGoogleThinkingConfigDoc(doc *ojson.Doc, rawJSON []byte, thinkingPath string) bool {
	if doc == nil || len(rawJSON) == 0 || !ojson.ValidBytes(rawJSON) || thinkingPath == "" {
		return false
	}
	level, budget := extractOpenAIGoogleThinkingFallbackFields(func(path string) ojson.Result {
		return ojson.GetBytesNoCopy(rawJSON, path)
	})
	if level.Exists() {
		if level.Str == "auto" {
			_ = doc.Set(thinkingPath+".thinkingBudget", -1)
		} else {
			_ = doc.Set(thinkingPath+".thinkingLevel", level.Str)
		}
		return true
	}
	if budget.Exists() {
		_ = doc.Set(thinkingPath+".thinkingBudget", budget.Int())
		return true
	}
	return false
}

// ParseOpenAIGoogleThinking reads the first OpenAI Chat Google thinking config
// containing a level or integer budget. Native generation settings take
// precedence over provider extensions. A nonempty level wins within a config.
func ParseOpenAIGoogleThinking(body []byte) (level string, budget int, found bool) {
	var generation, snakeGeneration, extraBody, rootGoogle ojson.Result
	ojson.ParseBytes(body).ForEach(func(key, value ojson.Result) bool {
		switch key.Str {
		case "generationConfig":
			generation = value
		case "generation_config":
			snakeGeneration = value
		case "extra_body":
			extraBody = value
		case "google":
			rootGoogle = value
		}
		return true
	})
	extraGoogle := extraBody.Get("google")
	nestedGoogle := extraBody.Get("extra_body.google")
	for _, config := range [...]ojson.Result{
		generation.Get("thinkingConfig"), generation.Get("thinking_config"),
		snakeGeneration.Get("thinking_config"), snakeGeneration.Get("thinkingConfig"),
		extraGoogle.Get("thinking_config"), extraGoogle.Get("thinkingConfig"),
		nestedGoogle.Get("thinking_config"), nestedGoogle.Get("thinkingConfig"),
		rootGoogle.Get("thinking_config"), rootGoogle.Get("thinkingConfig"),
	} {
		if level, budget, found := parseGoogleThinkingConfig(config); found {
			return level, budget, true
		}
	}
	return "", 0, false
}

func parseGoogleThinkingConfig(config ojson.Result) (string, int, bool) {
	for _, key := range [...]string{"thinking_level", "thinkingLevel"} {
		if value := config.Get(key); value.Type == ojson.String {
			if level := strings.ToLower(strings.TrimSpace(value.Str)); level != "" {
				return level, 0, true
			}
		}
	}
	for _, key := range [...]string{"thinking_budget", "thinkingBudget"} {
		if value := config.Get(key); value.Type == ojson.Number {
			if budget, err := strconv.Atoi(value.Raw); err == nil && budget >= -1 {
				return "", budget, true
			}
		}
	}
	return "", 0, false
}

// ApplyOpenAIGoogleThinking writes explicit reasoning_effort first, otherwise
// preserves native thinking settings or fills them from compatible Chat fields.
func ApplyOpenAIGoogleThinking(out, body []byte, generationPath string) []byte {
	if effort := strings.ToLower(strings.TrimSpace(ojson.GetBytes(body, "reasoning_effort").Str)); effort != "" {
		return setGoogleThinking(out, generationPath, effort, 0)
	}

	generation := ojson.GetBytes(out, generationPath)
	for _, key := range [...]string{"thinkingConfig", "thinking_config"} {
		if _, _, found := parseGoogleThinkingConfig(generation.Get(key)); found {
			return out
		}
	}
	level, budget, found := ParseOpenAIGoogleThinking(body)
	if !found {
		return out
	}
	return setGoogleThinking(out, generationPath, level, budget)
}

func setGoogleThinking(out []byte, generationPath, level string, budget int) []byte {
	generation := ojson.GetBytes(out, generationPath)
	config := generation.Get("thinkingConfig")
	if !config.IsObject() {
		config = generation.Get("thinking_config")
	}
	settings := []byte("{}")
	if config.IsObject() {
		settings = []byte(config.Raw)
	}
	for _, key := range [...]string{"thinkingLevel", "thinking_level", "thinkingBudget", "thinking_budget"} {
		settings, _ = ojson.DeleteBytes(settings, key)
	}
	switch level {
	case string(LevelAuto):
		level, budget = "", -1
	case string(LevelNone):
		level, budget = "", 0
	}
	if level != "" {
		settings, _ = ojson.SetBytes(settings, "thinkingLevel", level)
	} else {
		settings, _ = ojson.SetBytes(settings, "thinkingBudget", budget)
	}
	out, _ = ojson.DeleteBytes(out, generationPath+".thinking_config")
	out, _ = ojson.SetRawBytes(out, generationPath+".thinkingConfig", settings)
	return out
}
