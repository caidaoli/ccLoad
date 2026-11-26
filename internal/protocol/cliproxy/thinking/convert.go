package thinking

import (
	"slices"
	"strings"

	"ccLoad/internal/protocol/cliproxy/registry"
)

// levelToBudgetMap defines the standard Level → Budget mapping.
// All keys are lowercase; lookups should use strings.ToLower.
var levelToBudgetMap = map[string]int{
	"none":    0,
	"auto":    -1,
	"minimal": 512,
	"low":     1024,
	"medium":  8192,
	"high":    24576,
	"xhigh":   32768,
	"max":     128000,
}

// ConvertLevelToBudget converts a thinking level to a budget value.
//
// This is a semantic conversion that maps discrete levels to numeric budgets.
// Level matching is case-insensitive.
//
// Level → Budget mapping:
//   - none    → 0
//   - auto    → -1
//   - minimal → 512
//   - low     → 1024
//   - medium  → 8192
//   - high    → 24576
//   - xhigh   → 32768
//   - max     → 128000
//
// Returns:
//   - budget: The converted budget value
//   - ok: true if level is valid, false otherwise
func ConvertLevelToBudget(level string) (int, bool) {
	budget, ok := levelToBudgetMap[strings.ToLower(level)]
	return budget, ok
}

// BudgetThreshold constants define the upper bounds for each thinking level.
// These are used by ConvertBudgetToLevel for range-based mapping.
const (
	// ThresholdMinimal is the upper bound for "minimal" level (1-512)
	ThresholdMinimal = 512
	// ThresholdLow is the upper bound for "low" level (513-1024)
	ThresholdLow = 1024
	// ThresholdMedium is the upper bound for "medium" level (1025-8192)
	ThresholdMedium = 8192
	// ThresholdHigh is the upper bound for "high" level (8193-24576)
	ThresholdHigh = 24576
	// ThresholdXHigh is the upper bound for "xhigh" level (24577-32768)
	ThresholdXHigh = 32768
)

// ConvertBudgetToLevel converts a budget value to its canonical thinking level.
//
// This is a semantic conversion that maps numeric budgets to discrete levels.
// Uses threshold-based mapping for range conversion.
//
// Budget → Level thresholds:
//   - -1        → auto
//   - 0         → none
//   - 1-512     → minimal
//   - 513-1024  → low
//   - 1025-8192 → medium
//   - 8193-24576 → high
//   - 24577-32768 → xhigh
//   - 32769+      → max
//
// Returns:
//   - level: The converted thinking level string
//   - ok: true if budget is valid, false for invalid negatives (< -1)
func ConvertBudgetToLevel(budget int) (string, bool) {
	switch {
	case budget < -1:
		// Invalid negative values
		return "", false
	case budget == -1:
		return string(LevelAuto), true
	case budget == 0:
		return string(LevelNone), true
	case budget <= ThresholdMinimal:
		return string(LevelMinimal), true
	case budget <= ThresholdLow:
		return string(LevelLow), true
	case budget <= ThresholdMedium:
		return string(LevelMedium), true
	case budget <= ThresholdHigh:
		return string(LevelHigh), true
	case budget <= ThresholdXHigh:
		return string(LevelXHigh), true
	default:
		return string(LevelMax), true
	}
}

// OrderedLevels returns the discrete thinking effort levels in ascending order.
func OrderedLevels() [6]string {
	return [6]string{string(LevelMinimal), string(LevelLow), string(LevelMedium), string(LevelHigh), string(LevelXHigh), string(LevelMax)}
}

// ClampEffortToLevels finds the nearest supported level, preferring the lower
// level on a tie. Special values, unknown levels, and empty catalogs pass through.
func ClampEffortToLevels(effort string, levels []string) string {
	if len(levels) == 0 || HasLevel(levels, effort) {
		return effort
	}
	order := OrderedLevels()
	at := slices.Index(order[:], strings.ToLower(strings.TrimSpace(effort)))
	if at < 0 {
		return effort
	}
	best, bestDistance := -1, len(order)
	for _, level := range levels {
		idx := slices.Index(order[:], strings.ToLower(strings.TrimSpace(level)))
		if idx < 0 {
			continue
		}
		distance := max(idx-at, at-idx)
		if distance < bestDistance || (distance == bestDistance && idx < best) {
			best, bestDistance = idx, distance
		}
	}
	if best >= 0 {
		return order[best]
	}
	return effort
}

// ConvertBudgetToModelLevel converts a budget to an effort supported by the
// target model. Without level metadata, retain the legacy xhigh ceiling.
func ConvertBudgetToModelLevel(budget int, modelName, provider string) (string, bool) {
	effort, ok := ConvertBudgetToLevel(budget)
	if !ok || budget <= 0 {
		return effort, ok
	}
	var levels []string
	if info := registry.LookupModelInfo(modelName, provider); info == nil {
		levels = registry.LookupThinkingLevels(modelName)
	} else if info.Thinking != nil {
		levels = info.Thinking.Levels
	}
	if len(levels) == 0 && effort == string(LevelMax) {
		return string(LevelXHigh), true
	}
	return ClampEffortToLevels(effort, levels), true
}

// HasLevel reports whether the given target level exists in the levels slice.
// Matching is case-insensitive with leading/trailing whitespace trimmed.
func HasLevel(levels []string, target string) bool {
	for _, level := range levels {
		if strings.EqualFold(strings.TrimSpace(level), target) {
			return true
		}
	}
	return false
}

// MapToClaudeEffort maps a generic thinking level string to a Claude adaptive
// thinking effort value (low/medium/high/max).
//
// supportsMax indicates whether the target model supports "max" effort.
// Returns the mapped effort and true if the level is valid, or ("", false) otherwise.
func MapToClaudeEffort(level string, supportsMax bool) (string, bool) {
	level = strings.ToLower(strings.TrimSpace(level))
	switch level {
	case "":
		return "", false
	case "minimal":
		return "low", true
	case "low", "medium", "high":
		return level, true
	case "xhigh", "max":
		if supportsMax {
			return "max", true
		}
		return "high", true
	case "auto":
		return "high", true
	default:
		return "", false
	}
}
