package privacy

import "errors"

// Hit is one distinct value that masking replaced.
type Hit struct {
	Kind        string `json:"kind"`
	Value       string `json:"value"`
	Placeholder string `json:"placeholder"`
	Source      string `json:"source"`
	RuleIndex   *int   `json:"rule_index,omitempty"` // zero-based word/custom rule index
}

// RuleWarning identifies a custom rule that may hide ordinary text.
type RuleWarning struct {
	RuleIndex int    `json:"rule_index"` // zero-based custom rule index
	Code      string `json:"code"`
}

// PreviewResult describes the actual replacements and advisory rule warnings.
type PreviewResult struct {
	Masked   string        `json:"masked"`
	Hits     []Hit         `json:"hits"`
	Warnings []RuleWarning `json:"warnings"`
}

// Preview masks text as if config were enabled. It uses a throwaway engine, so
// previews never add values to, or read values from, any live dictionary.
// Hits are distinct values in order of first appearance in the masked text.
func Preview(config Config, text string) (PreviewResult, error) {
	result := PreviewResult{Hits: []Hit{}, Warnings: []RuleWarning{}}
	if len(text) > maxTextBytes {
		return result, errors.New("privacy text exceeds 64 MiB")
	}
	config.Enabled = true
	policy, err := NewPolicy(config)
	if err != nil {
		return result, err
	}
	// Provenance is only needed for previews. Keep it out of the production
	// match cache and reversible dictionary.
	origins := make(map[match]Hit)
	found, err := policy.scan(text, func(matches []match, source string, index int) {
		var ruleIndex *int
		if index >= 0 {
			ruleIndex = &index
		}
		for _, m := range matches {
			if _, exists := origins[m]; !exists {
				origins[m] = Hit{Source: source, RuleIndex: ruleIndex}
			}
		}
	})
	if err != nil {
		return result, err
	}
	customIndex := 0
	for _, rule := range policy.rules {
		if rule.category == "custom" {
			if broadPreviewRule(rule) {
				result.Warnings = append(result.Warnings, RuleWarning{RuleIndex: customIndex, Code: "broad_pattern"})
			}
			customIndex++
		}
	}
	s := NewEngine().Session("", policy)
	selected := s.selectMatches(text, found)
	result.Masked, err = s.apply(text, selected)
	if err != nil {
		return result, err
	}
	stored := make(map[string]*entry)
	for _, item := range s.engine.snapshot(s.scope) {
		stored[item.value] = item
	}
	for _, m := range selected {
		if item, ok := stored[text[m.start:m.end]]; ok {
			hit := origins[m]
			hit.Kind, hit.Value, hit.Placeholder = item.kind, item.value, item.placeholder
			result.Hits = append(result.Hits, hit)
			delete(stored, item.value)
		}
	}
	return result, nil
}

// This is a warning heuristic, not a reason to reject a rule. Use the same
// capture-group semantics as masking: a broad context with a narrow capture
// must not be reported as replacing the entire sample.
func broadPreviewRule(rule detector) bool {
	covered := 0
	for _, sample := range []string{"A normal message with several words.", "这是一段普通的请求内容。", "alpha 123\nbeta 456"} {
		length := 0
		for _, m := range rule.find(sample, asciiLower(sample), false) {
			length += m.end - m.start
		}
		if length*10 >= len(sample)*8 {
			covered++
		}
	}
	return covered >= 2
}
