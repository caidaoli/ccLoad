package app

import (
	_ "embed"
	"strings"
)

// The embedded prompts mirror the model instructions shipped by the official
// Codex client. Keep them as data: the request hot path only selects a string.
var (
	//go:embed codex_prompts/gpt-5-codex.md
	codexInstructionsGPT5Codex string

	//go:embed codex_prompts/gpt-5.1.md
	codexInstructionsGPT51 string

	//go:embed codex_prompts/gpt-5.2.md
	codexInstructionsGPT52 string

	//go:embed codex_prompts/gpt-5.4.md
	codexInstructionsGPT54 string

	//go:embed codex_prompts/gpt-5.4-mini.md
	codexInstructionsGPT54Mini string

	//go:embed codex_prompts/gpt-5.5.md
	codexInstructionsGPT55 string

	//go:embed codex_prompts/gpt-5.6.md
	codexInstructionsGPT56 string
)

func codexBaseInstructionsForModel(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if slash := strings.LastIndexByte(model, '/'); slash >= 0 {
		model = model[slash+1:]
	}

	instructions := codexInstructionsGPT56
	switch {
	case model == "codex-auto-review":
		instructions = codexInstructionsGPT54
	case strings.Contains(model, "codex"):
		instructions = codexInstructionsGPT5Codex
	case strings.HasPrefix(model, "gpt-5.6"):
		instructions = codexInstructionsGPT56
	case strings.HasPrefix(model, "gpt-5.5"):
		instructions = codexInstructionsGPT55
	case strings.HasPrefix(model, "gpt-5.4-mini"), strings.HasPrefix(model, "gpt-5.4-nano"):
		instructions = codexInstructionsGPT54Mini
	case strings.HasPrefix(model, "gpt-5.4"):
		instructions = codexInstructionsGPT54
	case strings.HasPrefix(model, "gpt-5.2"):
		instructions = codexInstructionsGPT52
	case strings.HasPrefix(model, "gpt-5.1"):
		instructions = codexInstructionsGPT51
	}

	// The official model catalog leaves the default personality empty. Resolve
	// the template here so no internal placeholder reaches the upstream API.
	return strings.TrimSpace(strings.ReplaceAll(instructions, "{{ personality }}", ""))
}
