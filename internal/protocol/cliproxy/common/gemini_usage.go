package common

import "github.com/MoeForge/moego/ojson"

// GeminiUsageSnapshot accumulates numeric counts from cumulative Gemini usage frames.
type GeminiUsageSnapshot struct {
	Present    bool
	Prompt     int64
	Candidates int64
	Thoughts   int64
	Total      int64
	Cached     int64
}

// Merge retains counts absent from a later snapshot and accepts explicit zeroes.
func (u *GeminiUsageSnapshot) Merge(metadata ojson.Result) bool {
	valid := false
	for _, field := range [...]struct {
		name  string
		count *int64
	}{
		{"promptTokenCount", &u.Prompt},
		{"candidatesTokenCount", &u.Candidates},
		{"thoughtsTokenCount", &u.Thoughts},
		{"totalTokenCount", &u.Total},
		{"cachedContentTokenCount", &u.Cached},
	} {
		if value := metadata.Get(field.name); value.Type == ojson.Number {
			*field.count = value.Int()
			valid = true
		}
	}
	u.Present = u.Present || valid
	return valid
}

// ClaudeInputTokens excludes the cache reads reported separately to Claude.
func (u GeminiUsageSnapshot) ClaudeInputTokens() int64 {
	return max(0, u.Prompt-u.Cached)
}

// ClaudeOutputTokens includes hidden thought and falls back to total minus prompt.
func (u GeminiUsageSnapshot) ClaudeOutputTokens() int64 {
	if output := u.Candidates + u.Thoughts; output != 0 {
		return output
	}
	return max(0, u.Total-u.Prompt)
}
