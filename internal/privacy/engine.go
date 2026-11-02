package privacy

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	mappingTTL    = 24 * time.Hour
	maxScopes     = 4096
	maxEntries    = 65536
	maxScopeItems = 4096
	maxHeldBytes  = 32 << 20
	maxTextBytes  = 64 << 20
	maxMatches    = 65536
)

var errCapacity = errors.New("privacy mapping capacity exceeded")
var placeholderPattern = regexp.MustCompile(`\{\{CCLOAD_[A-Z][A-Z0-9_]{0,23}_[a-f0-9]{16}\}\}`)

type storedValue struct {
	value, placeholder, kind, category string
}

type scopeValues struct {
	values  map[string]storedValue // original value → mapping
	expires time.Time
	bytes   int
}

// Engine owns process-local mappings. Scopes and values are never persisted.
// Idle scopes expire after 24 hours; capacity exhaustion fails closed rather
// than evicting values another in-flight response still needs to restore.
type Engine struct {
	mu      sync.Mutex
	key     [32]byte
	scopes  map[[32]byte]*scopeValues
	entries int
	bytes   int
}

// NewEngine creates an engine with a fresh cryptographic process key.
func NewEngine() *Engine {
	e := &Engine{scopes: make(map[[32]byte]*scopeValues)}
	_, _ = rand.Read(e.key[:]) // crypto/rand.Read never returns an error.
	return e
}

// HasMappings reports whether scope still has unexpired reversible values.
// It does not extend their lifetime.
func (e *Engine) HasMappings(scope string) bool {
	return e.hasMappings(sha256.Sum256([]byte(scope)))
}

func (e *Engine) hasMappings(scope [32]byte) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	held := e.scopes[scope]
	return held != nil && len(held.values) > 0 && held.expires.After(time.Now())
}

// Session keeps one configuration snapshot while sharing its scope's history.
type Session struct {
	engine *Engine
	scope  [32]byte
	config Config
	rules  []detector
	err    error

	signedKnownOnce sync.Once
	signedKnown     []storedValue
}

// Session snapshots and validates config. Invalid programmatic configurations
// surface as an error from MaskText or MaskJSON, never as plaintext fallback.
func (e *Engine) Session(scope string, config Config) *Session {
	s := &Session{engine: e, scope: sha256.Sum256([]byte(scope))}
	raw, err := json.Marshal(config)
	if err == nil {
		s.config, err = ParseConfig(string(raw))
	}
	s.err = err
	if err == nil {
		for _, r := range builtinRules {
			if s.covers(r.category) {
				s.rules = append(s.rules, r)
			}
		}
		for _, r := range s.config.Rules {
			re, _ := compileRule(r)
			s.rules = append(s.rules, detector{kind: r.Kind, category: "custom", re: re})
		}
		for _, word := range s.config.Words {
			pattern := regexp.QuoteMeta(word)
			if s.config.WordsAnyCase {
				pattern = "(?i)" + pattern
			}
			s.rules = append(s.rules, detector{kind: "WORD", category: "word", re: regexp.MustCompile(pattern), whole: s.config.WordsWhole})
		}
	}
	return s
}

// Enabled reports whether this request's snapshot masks new sensitive values.
// Restoration and signed-history remapping can still work when it is false.
func (s *Session) Enabled() bool { return s.config.Enabled }

func (s *Session) covers(category string) bool {
	switch category {
	case "":
		return s.config.Secrets
	case "personal":
		return s.config.Personal
	case "word":
		return len(s.config.Words) != 0
	case "custom":
		return len(s.config.Rules) != 0
	default:
		return s.config.Personal && s.config.Kinds[category]
	}
}

func (e *Engine) prune(now time.Time) {
	for scope, held := range e.scopes {
		if !held.expires.After(now) {
			e.entries -= len(held.values)
			e.bytes -= held.bytes
			delete(e.scopes, scope)
		}
	}
}

func (s *Session) snapshot() []storedValue {
	e := s.engine
	e.mu.Lock()
	defer e.mu.Unlock()
	held := e.scopes[s.scope]
	if held == nil || !held.expires.After(time.Now()) {
		return nil
	}
	held.expires = time.Now().Add(mappingTTL)
	out := make([]storedValue, 0, len(held.values))
	for _, value := range held.values {
		out = append(out, value)
	}
	return out
}

type match struct {
	start, end     int
	kind, category string
}

func (s *Session) allowed(value string) bool {
	for _, allowed := range s.config.Allow {
		if strings.EqualFold(value, allowed) {
			return true
		}
	}
	return false
}

func wordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' }

func whole(s string, a, b int) bool {
	if a > 0 {
		r, _ := utf8.DecodeLastRuneInString(s[:a])
		if wordRune(r) {
			return false
		}
	}
	if b < len(s) {
		r, _ := utf8.DecodeRuneInString(s[b:])
		if wordRune(r) {
			return false
		}
	}
	return true
}

// MaskText replaces sensitive spans with stable, scope-specific placeholders.
func (s *Session) MaskText(text string) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	if !s.config.Enabled {
		return text, nil
	}
	if len(text) > maxTextBytes {
		return "", errors.New("privacy text exceeds 64 MiB")
	}
	var found []match
	for _, r := range s.rules {
		found = append(found, r.find(text)...)
		if len(found) > maxMatches {
			return "", errors.New("privacy text has too many sensitive spans")
		}
	}
	// A secret found on an earlier turn stays masked when later prose no
	// longer has the label that originally identified it.
	for _, value := range s.snapshot() {
		if !s.covers(value.category) || s.allowed(value.value) {
			continue
		}
		if value.category == "word" || value.category == "custom" {
			continue // only the current word/custom rules determine matches
		}
		for offset := 0; offset < len(text); {
			i := strings.Index(text[offset:], value.value)
			if i < 0 {
				break
			}
			a, b := offset+i, offset+i+len(value.value)
			if whole(text, a, b) && (value.kind != "PHONE" || phoneAt(text, a, b)) {
				found = append(found, match{a, b, value.kind, value.category})
				if len(found) > maxMatches {
					return "", errors.New("privacy text has too many sensitive spans")
				}
			}
			offset = b
		}
	}
	return s.apply(text, found)
}

func (s *Session) apply(text string, found []match) (string, error) {
	protected := placeholderPattern.FindAllStringIndex(text, -1)
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].start != found[j].start {
			return found[i].start < found[j].start
		}
		return found[i].end > found[j].end
	})
	selected := make([]match, 0, len(found))
	end := 0
	for _, m := range found {
		if m.start < end || m.end <= m.start || s.allowed(text[m.start:m.end]) {
			continue
		}
		skip := false
		for _, p := range protected {
			if m.start < p[1] && m.end > p[0] {
				skip = true
				break
			}
		}
		if !skip {
			selected = append(selected, m)
			end = m.end
		}
	}
	if len(selected) == 0 {
		return text, nil
	}
	values, err := s.remember(text, selected)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	last := 0
	for _, m := range selected {
		out.WriteString(text[last:m.start])
		out.WriteString(values[text[m.start:m.end]])
		last = m.end
	}
	out.WriteString(text[last:])
	return out.String(), nil
}

func (s *Session) remember(text string, selected []match) (map[string]string, error) {
	e := s.engine
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	e.prune(now)
	held := e.scopes[s.scope]
	if held == nil {
		if len(e.scopes) >= maxScopes {
			return nil, errCapacity
		}
		held = &scopeValues{values: make(map[string]storedValue)}
	}
	added := make(map[string]storedValue)
	out := make(map[string]string)
	bytesAdded := 0
	for _, m := range selected {
		value := text[m.start:m.end]
		if prior, ok := held.values[value]; ok {
			out[value] = prior.placeholder
			continue
		}
		if _, ok := added[value]; ok {
			continue
		}
		h := hmac.New(sha256.New, e.key[:])
		_, _ = h.Write(s.scope[:])
		_, _ = h.Write([]byte(m.kind + "\x00" + value))
		placeholder := "{{CCLOAD_" + m.kind + "_" + hex.EncodeToString(h.Sum(nil)[:8]) + "}}"
		value = strings.Clone(value)
		added[value] = storedValue{value, placeholder, m.kind, m.category}
		out[value] = placeholder
		bytesAdded += len(value) + len(placeholder)
	}
	if len(held.values)+len(added) > maxScopeItems || e.entries+len(added) > maxEntries || e.bytes+bytesAdded > maxHeldBytes {
		return nil, errCapacity
	}
	for value, mapping := range added {
		held.values[value] = mapping
	}
	held.bytes += bytesAdded
	held.expires = now.Add(mappingTTL)
	e.scopes[s.scope] = held
	e.entries += len(added)
	e.bytes += bytesAdded
	return out, nil
}

// RestoreText restores only placeholders owned by this authenticated scope.
func (s *Session) RestoreText(text string) string {
	return s.restore(text, false)
}

func (s *Session) restore(text string, escaped bool) string {
	if !strings.Contains(text, "{{CCLOAD_") {
		return text
	}
	values := make(map[string]string)
	for _, v := range s.snapshot() {
		value := v.value
		if escaped {
			b, _ := json.Marshal(value)
			value = string(b[1 : len(b)-1])
		}
		values[v.placeholder] = value
	}
	return placeholderPattern.ReplaceAllStringFunc(text, func(placeholder string) string {
		if v, ok := values[placeholder]; ok {
			return v
		}
		return placeholder
	})
}

// signedDictionary is fixed for the request session. Final wire preparation
// can mask the same body again after its unsigned fields discovered new values;
// those discoveries must never change the vendor's signed content on a retry.
func (s *Session) signedDictionary() []storedValue {
	s.signedKnownOnce.Do(func() {
		s.signedKnown = s.snapshot()
		sort.Slice(s.signedKnown, func(i, j int) bool {
			return len(s.signedKnown[i].value) > len(s.signedKnown[j].value)
		})
	})
	return s.signedKnown
}

// remaskKnown reconstructs what the vendor signed without discovering any new
// values or changing the dictionary. Longest values win; existing placeholders
// are protected from custom words that happen to match part of their spelling.
func remaskKnown(text string, known []storedValue) string {
	if len(known) == 0 {
		return text
	}
	pairs := make([]string, 0, len(known)*2)
	for _, entry := range known {
		pairs = append(pairs, entry.value, entry.placeholder)
	}
	replacer := strings.NewReplacer(pairs...)
	var out strings.Builder
	last := 0
	for _, span := range placeholderPattern.FindAllStringIndex(text, -1) {
		out.WriteString(replacer.Replace(text[last:span[0]]))
		out.WriteString(text[span[0]:span[1]])
		last = span[1]
	}
	out.WriteString(replacer.Replace(text[last:]))
	return out.String()
}

// Hit is one distinct value that masking replaced.
type Hit struct {
	Kind        string `json:"kind"`
	Value       string `json:"value"`
	Placeholder string `json:"placeholder"`
}

// Preview masks text as if config were enabled. It uses a throwaway engine, so
// previews never add values to, or read values from, any live dictionary.
// Hits are distinct values in order of first appearance in the masked text.
func Preview(config Config, text string) (string, []Hit, error) {
	config.Enabled = true
	s := NewEngine().Session("", config)
	masked, err := s.MaskText(text)
	if err != nil {
		return "", nil, err
	}
	stored := make(map[string]storedValue)
	for _, v := range s.snapshot() {
		stored[v.placeholder] = v
	}
	hits := make([]Hit, 0, len(stored))
	for _, placeholder := range placeholderPattern.FindAllString(masked, -1) {
		if v, ok := stored[placeholder]; ok {
			hits = append(hits, Hit{Kind: v.kind, Value: v.value, Placeholder: placeholder})
			delete(stored, placeholder)
		}
	}
	return masked, hits, nil
}
