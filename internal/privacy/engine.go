package privacy

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	mappingTTL = 24 * time.Hour
	// At capacity, values idle for longer than this may be evicted. Newer
	// values can still be needed by an in-flight response, so they are kept
	// and masking fails closed instead.
	evictionGrace = time.Hour
	pruneInterval = time.Minute
	maxEntries    = 65536
	maxScopeItems = 4096
	maxHeldBytes  = 32 << 20
	maxTextBytes  = 64 << 20
	maxMatches    = 65536
)

var errCapacity = errors.New("privacy mapping capacity exceeded")
var placeholderPattern = regexp.MustCompile(`\{\{CCLOAD_[A-Z][A-Z0-9_]{0,23}_[a-f0-9]{16}\}\}`)

// entry fields other than used are immutable once published.
type entry struct {
	value, placeholder, kind, category string
	syntax                             bool      // original JSON syntax, rather than a decoded string value
	used                               time.Time // guarded by Engine.mu
}

type valueKey struct {
	value  string
	syntax bool
}

// A scope is in Engine.scopes only while it holds values; drop removes it.
type scopeValues struct {
	key          [32]byte
	values       map[valueKey]*entry // value and its JSON representation → mapping
	placeholders map[string]*entry   // placeholder → mapping
	used         time.Time
}

// Engine owns process-local mappings. Scopes and values are never persisted.
// A value expires 24 hours after it was last masked or restored. At capacity,
// values idle for over an hour are evicted least recently used first.
type Engine struct {
	mu      sync.Mutex
	key     [32]byte
	scopes  map[[32]byte]*scopeValues
	entries int
	bytes   int
	pruned  time.Time
	now     func() time.Time
}

// NewEngine creates an engine with a fresh cryptographic process key.
func NewEngine() *Engine {
	e := &Engine{scopes: make(map[[32]byte]*scopeValues), now: time.Now}
	_, _ = rand.Read(e.key[:]) // crypto/rand.Read never returns an error.
	return e
}

// HasMappings reports whether scope still has unexpired reversible values.
// It does not extend their lifetime.
func (e *Engine) HasMappings(scope string) bool {
	key := sha256.Sum256([]byte(scope))
	e.mu.Lock()
	defer e.mu.Unlock()
	held := e.scopes[key]
	return held != nil && e.now().Sub(held.used) < mappingTTL
}

// Session binds one request's policy to its scope's shared dictionary.
type Session struct {
	engine *Engine
	scope  [32]byte
	policy *Policy

	indexOnce sync.Once
	mu        sync.RWMutex
	known     knownIndex

	observe    bool
	unrestored atomic.Uint64
}

// Session returns a request session. policy must not be nil; it stays fixed
// for the request's retries and protocol conversions.
func (e *Engine) Session(scope string, policy *Policy) *Session {
	return &Session{engine: e, scope: sha256.Sum256([]byte(scope)), policy: policy}
}

// Enabled reports whether this request's policy masks new sensitive values.
// Restoration works when it is false.
func (s *Session) Enabled() bool { return s.policy.Enabled() }

type match struct {
	start, end     int
	kind, category string
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
	if !s.Enabled() {
		return text, nil
	}
	if len(text) > maxTextBytes {
		return "", errors.New("privacy text exceeds 64 MiB")
	}
	detected, err := s.policy.find(text)
	if err != nil {
		return "", err
	}
	// A secret found on an earlier turn stays masked when later prose no
	// longer has the label that originally identified it.
	known := s.knownMatches(text)
	if len(detected)+len(known) > maxMatches {
		return "", errTooManyMatches
	}
	if len(detected)+len(known) == 0 {
		return text, nil
	}
	found := make([]match, 0, len(detected)+len(known))
	found = append(append(found, detected...), known...)
	return s.apply(text, found)
}

func (s *Session) selectMatches(text string, found []match) []match {
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].start != found[j].start {
			return found[i].start < found[j].start
		}
		return found[i].end > found[j].end
	})
	protected := placeholderPattern.FindAllStringIndex(text, -1)
	selected := make([]match, 0, len(found))
	end, p := 0, 0
	for _, m := range found {
		for p < len(protected) && protected[p][1] <= m.start {
			p++
		}
		if m.start < end || m.end <= m.start || p < len(protected) && protected[p][0] < m.end || s.policy.allowed(text[m.start:m.end]) {
			continue
		}
		selected = append(selected, m)
		end = m.end
	}
	return selected
}

func (s *Session) apply(text string, found []match) (string, error) {
	selected := s.selectMatches(text, found)
	if len(selected) == 0 {
		return text, nil
	}
	values, err := s.remember(text, selected, false)
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

func (s *Session) remember(text string, selected []match, syntax bool) (map[string]string, error) {
	e := s.engine
	e.mu.Lock()
	now := e.now()
	e.prune(now)
	held := e.scopes[s.scope]
	if held == nil {
		held = &scopeValues{key: s.scope, values: make(map[valueKey]*entry), placeholders: make(map[string]*entry)}
	}
	out := make(map[string]string, len(selected))
	items := make([]*entry, 0, len(selected))
	var added []*entry
	size := 0
	for _, m := range selected {
		value := text[m.start:m.end]
		if _, ok := out[value]; ok {
			continue
		}
		item := held.values[valueKey{value, syntax}]
		if item == nil {
			h := hmac.New(sha256.New, e.key[:])
			_, _ = h.Write(s.scope[:])
			mode := byte(0)
			if syntax {
				mode = 1
			}
			_, _ = h.Write([]byte{mode})
			_, _ = h.Write([]byte(m.kind + "\x00" + value))
			item = &entry{
				value:       strings.Clone(value),
				placeholder: "{{CCLOAD_" + m.kind + "_" + hex.EncodeToString(h.Sum(nil)[:8]) + "}}",
				kind:        m.kind,
				category:    m.category,
				syntax:      syntax,
			}
			added = append(added, item)
			size += len(item.value) + len(item.placeholder)
		}
		item.used = now
		out[item.value] = item.placeholder
		items = append(items, item)
	}
	if len(added) > 0 {
		if err := e.makeRoom(held, len(added), size, now); err != nil {
			e.mu.Unlock()
			return nil, err
		}
		for _, item := range added {
			held.values[valueKey{item.value, item.syntax}] = item
			held.placeholders[item.placeholder] = item
		}
		e.entries += len(added)
		e.bytes += size
		e.scopes[s.scope] = held
	}
	held.used = now
	e.mu.Unlock()
	s.learn(items)
	return out, nil
}

func (e *Engine) prune(now time.Time) {
	if now.Sub(e.pruned) < pruneInterval {
		return
	}
	e.pruned = now
	for _, held := range e.scopes {
		for _, item := range held.values {
			if now.Sub(item.used) >= mappingTTL {
				e.drop(held, item)
			}
		}
	}
}

func (e *Engine) drop(held *scopeValues, item *entry) {
	delete(held.values, valueKey{item.value, item.syntax})
	delete(held.placeholders, item.placeholder)
	e.entries--
	e.bytes -= len(item.value) + len(item.placeholder)
	if len(held.values) == 0 {
		delete(e.scopes, held.key)
	}
}

type heldEntry struct {
	held *scopeValues
	item *entry
}

// makeRoom evicts only values idle beyond evictionGrace, least recently used
// first. The scope count needs no limit of its own: every scope holds values.
func (e *Engine) makeRoom(held *scopeValues, items, size int, now time.Time) error {
	if size > maxHeldBytes {
		return errCapacity
	}
	idle := func(scopes ...*scopeValues) []heldEntry {
		var out []heldEntry
		for _, scope := range scopes {
			for _, item := range scope.values {
				if now.Sub(item.used) > evictionGrace {
					out = append(out, heldEntry{scope, item})
				}
			}
		}
		slices.SortFunc(out, func(a, b heldEntry) int { return a.item.used.Compare(b.item.used) })
		return out
	}
	if over := len(held.values) + items - maxScopeItems; over > 0 {
		victims := idle(held)
		if len(victims) < over {
			return errCapacity
		}
		for _, victim := range victims[:over] {
			e.drop(held, victim.item)
		}
	}
	if e.entries+items > maxEntries || e.bytes+size > maxHeldBytes {
		all := make([]*scopeValues, 0, len(e.scopes))
		for _, scope := range e.scopes {
			all = append(all, scope)
		}
		// Free some headroom too, so a full engine does not sort every value
		// under the lock for each new one.
		for _, victim := range idle(all...) {
			if e.entries+items <= maxEntries-maxEntries/16 && e.bytes+size <= maxHeldBytes-maxHeldBytes/16 {
				break
			}
			e.drop(victim.held, victim.item)
		}
	}
	if e.entries+items > maxEntries || e.bytes+size > maxHeldBytes {
		return errCapacity
	}
	return nil
}

func (e *Engine) snapshot(scope [32]byte) []*entry {
	e.mu.Lock()
	defer e.mu.Unlock()
	held := e.scopes[scope]
	if held == nil {
		return nil
	}
	now := e.now()
	out := make([]*entry, 0, len(held.values))
	for _, item := range held.values {
		if now.Sub(item.used) < mappingTTL {
			out = append(out, item)
		}
	}
	return out
}

// knownIndex finds a scope's earlier values in one pass over the text. Every
// match must be a whole word, so only positions after a non-word rune are tried.
// Values share a bucket by their first two bytes and a group by length, so
// each position costs one lookup per distinct length, longest first. Scanning
// continues inside a hit: when another detection displaces it, an overlapping
// later value must still be a candidate for selectMatches.
type knownIndex struct {
	pairs  map[uint16][]knownGroup
	single [256]*entry
	size   int
}

type knownGroup struct {
	length int
	values map[string]*entry
}

func (k *knownIndex) add(item *entry) {
	if len(item.value) == 1 {
		if k.single[item.value[0]] == nil {
			k.single[item.value[0]] = item
			k.size++
		}
		return
	}
	if k.pairs == nil {
		k.pairs = make(map[uint16][]knownGroup)
	}
	key := uint16(item.value[0])<<8 | uint16(item.value[1])
	groups := k.pairs[key]
	i, found := slices.BinarySearchFunc(groups, len(item.value), func(g knownGroup, length int) int { return length - g.length })
	if !found {
		groups = slices.Insert(groups, i, knownGroup{length: len(item.value), values: make(map[string]*entry)})
		k.pairs[key] = groups
	}
	if groups[i].values[item.value] == nil {
		groups[i].values[item.value] = item
		k.size++
	}
}

func (k *knownIndex) find(text string) []match {
	if k.size == 0 {
		return nil
	}
	var out []match
	afterWord := false
	for i := 0; i < len(text); {
		if !afterWord {
			if item := k.at(text, i); item != nil {
				out = append(out, match{i, i + len(item.value), item.kind, item.category})
				if len(out) > maxMatches {
					return out
				}
			}
		}
		r, size := rune(text[i]), 1
		if r >= utf8.RuneSelf {
			r, size = utf8.DecodeRuneInString(text[i:])
		}
		afterWord = wordRune(r)
		i += size
	}
	return out
}

func (k *knownIndex) at(text string, i int) *entry {
	accept := func(item *entry) bool {
		end := i + len(item.value)
		return whole(text, i, end) && (item.kind != "PHONE" || phoneAt(text, i, end))
	}
	if i+1 < len(text) {
		for _, g := range k.pairs[uint16(text[i])<<8|uint16(text[i+1])] {
			if end := i + g.length; end <= len(text) {
				if item := g.values[text[i:end]]; item != nil && accept(item) {
					return item
				}
			}
		}
	}
	if item := k.single[text[i]]; item != nil && accept(item) {
		return item
	}
	return nil
}

func (s *Session) knownMatches(text string) []match {
	s.indexOnce.Do(s.buildIndex)
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.known.find(text)
}

func (s *Session) buildIndex() {
	for _, item := range s.engine.snapshot(s.scope) {
		s.index(item)
	}
}

func (s *Session) learn(items []*entry) {
	s.indexOnce.Do(s.buildIndex)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range items {
		s.index(item)
	}
}

// Only the current word and custom rules decide those matches; values from
// disabled categories or now on the allow list are not masked again either.
func (s *Session) index(item *entry) {
	if item.syntax || item.category == "word" || item.category == "custom" || !s.policy.covers(item.category) || s.policy.allowed(item.value) {
		return
	}
	s.known.add(item)
}

// ObserveRestoration returns an independent output observer sharing this scope's
// dictionary. Internal history restoration on the original session is not counted.
func (s *Session) ObserveRestoration() *Session {
	return &Session{engine: s.engine, scope: s.scope, policy: s.policy, observe: true}
}

// UnrestoredTokens counts observed occurrences with the {{CCLOAD_ marker that
// could not be restored, including damaged and truncated placeholders.
func (s *Session) UnrestoredTokens() uint64 { return s.unrestored.Load() }

// RestoreText restores only placeholders owned by this authenticated scope.
func (s *Session) RestoreText(text string) string {
	return s.restore(text, false)
}

func (s *Session) restore(text string, escaped bool) string {
	return s.restoreMode(text, escaped, false)
}

// Rebuild tool JSON syntax before restoring its decoded string values. A raw
// fragment and an identical decoded value have different placeholder identities.
func (s *Session) restoreSyntax(text string) string {
	return s.restoreMode(text, false, true)
}

func (s *Session) restoreMode(text string, escaped, syntaxOnly bool) string {
	if !strings.Contains(text, "{{CCLOAD_") {
		return text
	}
	spans := placeholderPattern.FindAllStringIndex(text, -1)
	values := s.engine.resolve(s.scope, text, spans, syntaxOnly)
	if s.observe && !syntaxOnly {
		missing := strings.Count(text, "{{CCLOAD_")
		for _, span := range spans {
			if _, ok := values[text[span[0]:span[1]]]; ok {
				missing--
			}
		}
		s.unrestored.Add(uint64(missing))
	}
	if len(values) == 0 {
		return text
	}
	var out strings.Builder
	last := 0
	for _, span := range spans {
		value, ok := values[text[span[0]:span[1]]]
		if !ok {
			continue
		}
		if escaped {
			value = jsonQuote(value)
			value = value[1 : len(value)-1]
		}
		out.WriteString(text[last:span[0]])
		out.WriteString(value)
		last = span[1]
	}
	out.WriteString(text[last:])
	return out.String()
}

func (e *Engine) resolve(scope [32]byte, text string, spans [][]int, syntaxOnly bool) map[string]string {
	if len(spans) == 0 {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	held := e.scopes[scope]
	if held == nil {
		return nil
	}
	now := e.now()
	out := make(map[string]string)
	for _, span := range spans {
		placeholder := text[span[0]:span[1]]
		if item := held.placeholders[placeholder]; item != nil && (!syntaxOnly || item.syntax) && now.Sub(item.used) < mappingTTL {
			item.used = now
			out[placeholder] = item.value
		}
	}
	if len(out) > 0 {
		held.used = now
	}
	return out
}

// jsonQuote encodes s as a JSON string without HTML escaping.
func jsonQuote(s string) string {
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(s) // strings always encode
	return strings.TrimSuffix(b.String(), "\n")
}
