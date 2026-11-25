package thinking

import (
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ParseOpenAIGoogleThinking reads the first OpenAI Chat Google thinking config
// containing a level or integer budget. Native generation settings take
// precedence over provider extensions. A nonempty level wins within a config.
func ParseOpenAIGoogleThinking(body []byte) (level string, budget int, found bool) {
	var generation, snakeGeneration, extraBody, rootGoogle gjson.Result
	gjson.ParseBytes(body).ForEach(func(key, value gjson.Result) bool {
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
	for _, config := range [...]gjson.Result{
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

func parseGoogleThinkingConfig(config gjson.Result) (string, int, bool) {
	for _, key := range [...]string{"thinking_level", "thinkingLevel"} {
		if value := config.Get(key); value.Type == gjson.String {
			if level := strings.ToLower(strings.TrimSpace(value.Str)); level != "" {
				return level, 0, true
			}
		}
	}
	for _, key := range [...]string{"thinking_budget", "thinkingBudget"} {
		if value := config.Get(key); value.Type == gjson.Number {
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
	if effort := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "reasoning_effort").Str)); effort != "" {
		return setGoogleThinking(out, generationPath, effort, 0)
	}

	generation := gjson.GetBytes(out, generationPath)
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
	generation := gjson.GetBytes(out, generationPath)
	config := generation.Get("thinkingConfig")
	if !config.IsObject() {
		config = generation.Get("thinking_config")
	}
	settings := []byte("{}")
	if config.IsObject() {
		settings = []byte(config.Raw)
	}
	for _, key := range [...]string{"thinkingLevel", "thinking_level", "thinkingBudget", "thinking_budget"} {
		settings, _ = sjson.DeleteBytes(settings, key)
	}
	switch level {
	case string(LevelAuto):
		level, budget = "", -1
	case string(LevelNone):
		level, budget = "", 0
	}
	if level != "" {
		settings, _ = sjson.SetBytes(settings, "thinkingLevel", level)
	} else {
		settings, _ = sjson.SetBytes(settings, "thinkingBudget", budget)
	}
	out, _ = sjson.DeleteBytes(out, generationPath+".thinking_config")
	out, _ = sjson.SetRawBytes(out, generationPath+".thinkingConfig", settings)
	return out
}
