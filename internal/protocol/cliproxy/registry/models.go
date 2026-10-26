package registry

import (
	_ "embed"
	"encoding/json"
	"maps"
	"slices"
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

// NativeCapabilities describes static provider capabilities.
type NativeCapabilities struct {
	WebSearch *bool `json:"web_search,omitempty"`
}

// ModelInfo describes immutable model capabilities used during conversion.
type ModelInfo struct {
	NativeCapabilities  *NativeCapabilities `json:"native_capabilities,omitempty"`
	ID                  string              `json:"id"`
	Name                string              `json:"name,omitempty"`
	ContextLength       int                 `json:"context_length,omitempty"`
	InputModalities     []string            `json:"supportedInputModalities,omitempty"`
	MaxCompletionTokens int                 `json:"max_completion_tokens,omitempty"`
	// Gemini-family catalog entries record limits under the Gemini field names
	// instead of ContextLength/MaxCompletionTokens.
	InputTokenLimit  int              `json:"inputTokenLimit,omitempty"`
	OutputTokenLimit int              `json:"outputTokenLimit,omitempty"`
	Thinking         *ThinkingSupport `json:"thinking,omitempty"`
}

type modelCatalog struct {
	Claude      []*ModelInfo `json:"claude"`
	Gemini      []*ModelInfo `json:"gemini"`
	Vertex      []*ModelInfo `json:"vertex"`
	AIStudio    []*ModelInfo `json:"aistudio"`
	Antigravity []*ModelInfo `json:"antigravity"`
	CodexFree   []*ModelInfo `json:"codex-free"`
	CodexTeam   []*ModelInfo `json:"codex-team"`
	CodexPlus   []*ModelInfo `json:"codex-plus"`
	CodexPro    []*ModelInfo `json:"codex-pro"`
}

var staticModels modelCatalog

// allCatalogs is every top-level catalog in models.json, sorted by key, including
// providers modelCatalog leaves out of Lookup.
var allCatalogs [][]*ModelInfo

var catalogsByKey map[string][]*ModelInfo

func init() {
	if err := json.Unmarshal(modelsJSON, &staticModels); err != nil {
		panic("decode embedded CLIProxyAPI model catalog: " + err.Error())
	}
	var catalogs map[string][]*ModelInfo
	if err := json.Unmarshal(modelsJSON, &catalogs); err != nil {
		panic("decode embedded CLIProxyAPI model catalog: " + err.Error())
	}
	for _, key := range slices.Sorted(maps.Keys(catalogs)) {
		allCatalogs = append(allCatalogs, catalogs[key])
	}
	catalogsByKey = catalogs
}

// CatalogModelIDs returns the model IDs one top-level models.json catalog lists, in file order.
func CatalogModelIDs(catalog string) []string {
	models := catalogsByKey[catalog]
	ids := make([]string, 0, len(models))
	for _, model := range models {
		if model != nil && model.ID != "" {
			ids = append(ids, model.ID)
		}
	}
	return ids
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

// LookupThinkingLevels returns the longest level list any catalog in models.json
// gives the model; earlier catalog keys win ties. Unlike Lookup it also reads
// catalogs of providers ccLoad has no executor for.
func LookupThinkingLevels(modelID string) []string {
	modelID = strings.TrimSpace(modelID)
	var levels []string
	for _, models := range allCatalogs {
		for _, model := range models {
			if model != nil && model.Thinking != nil && (model.ID == modelID || model.Name == modelID) && len(model.Thinking.Levels) > len(levels) {
				levels = model.Thinking.Levels
			}
		}
	}
	if modelID == "" || len(levels) == 0 {
		return nil
	}
	return slices.Clone(levels)
}

func modelsForProvider(provider string) [][]*ModelInfo {
	switch provider {
	case "claude", "anthropic":
		return [][]*ModelInfo{staticModels.Claude}
	case "gemini":
		return [][]*ModelInfo{staticModels.Gemini, staticModels.Vertex, staticModels.AIStudio, staticModels.Antigravity}
	case "vertex", "aistudio":
		return [][]*ModelInfo{staticModels.Gemini, staticModels.Vertex, staticModels.AIStudio}
	case "antigravity":
		return [][]*ModelInfo{staticModels.Antigravity}
	case "codex", "openai":
		return [][]*ModelInfo{staticModels.CodexFree, staticModels.CodexTeam, staticModels.CodexPlus, staticModels.CodexPro}
	default:
		return [][]*ModelInfo{staticModels.Claude, staticModels.Gemini, staticModels.Vertex, staticModels.AIStudio, staticModels.Antigravity, staticModels.CodexFree, staticModels.CodexTeam, staticModels.CodexPlus, staticModels.CodexPro}
	}
}

func cloneModelInfo(model *ModelInfo) *ModelInfo {
	clone := *model
	clone.InputModalities = append([]string(nil), model.InputModalities...)
	if model.NativeCapabilities != nil {
		capabilities := *model.NativeCapabilities
		if capabilities.WebSearch != nil {
			enabled := *capabilities.WebSearch
			capabilities.WebSearch = &enabled
		}
		clone.NativeCapabilities = &capabilities
	}
	if model.Thinking != nil {
		thinking := *model.Thinking
		thinking.Levels = append([]string(nil), model.Thinking.Levels...)
		clone.Thinking = &thinking
	}
	return &clone
}
