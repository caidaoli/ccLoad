package registry

import (
	_ "embed"
	"encoding/json"
	"strings"
)

//go:embed models/models.json
var modelsJSON []byte

// ThinkingSupport describes a model's supported thinking budget or levels.
type ThinkingSupport struct {
	Min            int      `json:"min,omitempty"`
	Max            int      `json:"max,omitempty"`
	ZeroAllowed    bool     `json:"zero_allowed,omitempty"`
	DynamicAllowed bool     `json:"dynamic_allowed,omitempty"`
	Levels         []string `json:"levels,omitempty"`
}

// ModelInfo describes immutable model capabilities used during conversion.
type ModelInfo struct {
	ID       string           `json:"id"`
	Name     string           `json:"name,omitempty"`
	Thinking *ThinkingSupport `json:"thinking,omitempty"`
}

type modelCatalog struct {
	Claude    []*ModelInfo `json:"claude"`
	Gemini    []*ModelInfo `json:"gemini"`
	Vertex    []*ModelInfo `json:"vertex"`
	AIStudio  []*ModelInfo `json:"aistudio"`
	CodexFree []*ModelInfo `json:"codex-free"`
	CodexTeam []*ModelInfo `json:"codex-team"`
	CodexPlus []*ModelInfo `json:"codex-plus"`
	CodexPro  []*ModelInfo `json:"codex-pro"`
}

var staticModels modelCatalog

func init() {
	if err := json.Unmarshal(modelsJSON, &staticModels); err != nil {
		panic("decode embedded CLIProxyAPI model catalog: " + err.Error())
	}
}

// LookupModelInfo returns immutable model capability data from the embedded catalog.
// Dynamic registration and network refresh deliberately do not belong to the translator core.
func LookupModelInfo(modelID string, provider ...string) *ModelInfo {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil
	}
	p := ""
	if len(provider) > 0 {
		p = strings.ToLower(strings.TrimSpace(provider[0]))
	}
	for _, models := range modelsForProvider(p) {
		for _, model := range models {
			if model != nil && (model.ID == modelID || model.Name == modelID) {
				return cloneModelInfo(model)
			}
		}
	}
	return nil
}

func modelsForProvider(provider string) [][]*ModelInfo {
	switch provider {
	case "claude", "anthropic":
		return [][]*ModelInfo{staticModels.Claude}
	case "gemini", "vertex", "aistudio":
		return [][]*ModelInfo{staticModels.Gemini, staticModels.Vertex, staticModels.AIStudio}
	case "codex", "openai":
		return [][]*ModelInfo{staticModels.CodexFree, staticModels.CodexTeam, staticModels.CodexPlus, staticModels.CodexPro}
	default:
		return [][]*ModelInfo{staticModels.Claude, staticModels.Gemini, staticModels.Vertex, staticModels.AIStudio, staticModels.CodexFree, staticModels.CodexTeam, staticModels.CodexPlus, staticModels.CodexPro}
	}
}

func cloneModelInfo(model *ModelInfo) *ModelInfo {
	clone := *model
	if model.Thinking != nil {
		thinking := *model.Thinking
		thinking.Levels = append([]string(nil), model.Thinking.Levels...)
		clone.Thinking = &thinking
	}
	return &clone
}
