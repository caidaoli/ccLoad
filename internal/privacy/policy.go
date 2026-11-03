package privacy

import (
	"crypto/sha256"
	"errors"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

const (
	// Texts shorter than this are cheaper to scan than to hash and look up.
	cacheMinBytes = 256
	// Every turn resends a conversation's history, so the cache must hold the
	// active conversations of all users, or each request rescans them in full.
	// Two generations bound it to 2×cacheEntries texts and 2×cacheMatches
	// spans, about 35 MiB; it stores offsets and kinds, never plaintext.
	cacheEntries = 65536
	cacheMatches = 262144
)

var errTooManyMatches = errors.New("privacy text has too many sensitive spans")

// Policy is an immutable, compiled configuration. Publish a new Policy instead
// of changing one: its detection cache is valid only for its own rules.
type Policy struct {
	config Config
	rules  []detector
	words  []wordMatcher
	cache  matchCache
}

// NewPolicy validates and compiles config. The policy keeps a private copy.
func NewPolicy(config Config) (*Policy, error) {
	c := config.clone()
	if err := c.validate(); err != nil {
		return nil, err
	}
	p := &Policy{config: c}
	for _, r := range builtinRules {
		if p.covers(r.category) {
			p.rules = append(p.rules, r)
		}
	}
	for _, r := range c.Rules {
		re, _ := compileRule(r) // validate compiled it already
		p.rules = append(p.rules, detector{kind: r.Kind, category: "custom", re: re})
	}
	for i := range p.rules {
		p.rules[i].gate = newGate(p.rules[i].re)
	}
	for _, word := range c.Words {
		p.words = append(p.words, newWordMatcher(word, c.WordsAnyCase))
	}
	return p, nil
}

// Enabled reports whether the policy masks new sensitive values. Restoration
// of a scope's existing placeholders works either way.
func (p *Policy) Enabled() bool { return p != nil && p.config.Enabled }

func (p *Policy) covers(category string) bool {
	switch category {
	case "":
		return p.config.Secrets
	case "personal":
		return p.config.Personal
	case "word":
		return len(p.config.Words) != 0
	case "custom":
		return len(p.config.Rules) != 0
	default:
		return p.config.Personal && p.config.Kinds[category]
	}
}

func (p *Policy) allowed(value string) bool {
	for _, allowed := range p.config.Allow {
		if strings.EqualFold(value, allowed) {
			return true
		}
	}
	return false
}

// find returns rule and word matches. The slice may be shared with the cache:
// callers must copy it before reordering or appending.
func (p *Policy) find(text string) ([]match, error) {
	if len(text) < cacheMinBytes {
		return p.scan(text)
	}
	key := sha256.Sum256([]byte(text))
	if found, ok := p.cache.get(key); ok {
		return found, nil
	}
	found, err := p.scan(text)
	if err == nil {
		p.cache.put(key, found)
	}
	return found, err
}

func (p *Policy) scan(text string) ([]match, error) {
	// Case-insensitive literals are searched in lower; see asciiFold.
	lower, unusualFold := asciiLower(text), strings.ContainsAny(text, "\u017F\u212A")
	var found []match
	for _, r := range p.rules {
		found = append(found, r.find(text, lower, unusualFold)...)
		if len(found) > maxMatches {
			return nil, errTooManyMatches
		}
	}
	for _, w := range p.words {
		found = w.find(text, lower, unusualFold, p.config.WordsWhole, found)
		if len(found) > maxMatches {
			return nil, errTooManyMatches
		}
	}
	return found, nil
}

// wordMatcher finds a configured word with the same leftmost, non-overlapping
// semantics as a quoted regular expression. A case-insensitive word is searched
// in the text's ASCII-lowercased copy when asciiFold allows it, and with its
// (?i) expression otherwise.
type wordMatcher struct {
	text string // ASCII-lowercased when fold
	fold bool
	re   *regexp.Regexp // nil for case-sensitive literals
}

func newWordMatcher(word string, anyCase bool) wordMatcher {
	if !anyCase {
		return wordMatcher{text: word}
	}
	re := regexp.MustCompile("(?i)" + regexp.QuoteMeta(word))
	if !asciiFold(word) {
		return wordMatcher{re: re} // e.g. é/É
	}
	return wordMatcher{text: asciiLower(word), fold: true, re: re}
}

func (w wordMatcher) find(text, lower string, unusualFold, wholeOnly bool, out []match) []match {
	add := func(a, b int) bool {
		if !wholeOnly || whole(text, a, b) {
			out = append(out, match{a, b, "WORD", "word"})
		}
		return len(out) <= maxMatches
	}
	if w.re != nil && (!w.fold || unusualFold) {
		for _, span := range w.re.FindAllStringIndex(text, maxMatches+1) {
			if !add(span[0], span[1]) {
				break
			}
		}
		return out
	}
	haystack := text
	if w.fold {
		haystack = lower
	}
	for offset := 0; ; {
		i := strings.Index(haystack[offset:], w.text)
		if i < 0 {
			return out
		}
		a := offset + i
		offset = a + len(w.text)
		if !add(a, offset) {
			return out
		}
	}
}

// asciiFold reports whether every case variant of s's runes is ASCII, apart
// from U+017F (ſ) and U+212A (Kelvin sign), which fold to s and k. In a text
// without those two runes, s then matches case-insensitively exactly where the
// text's ASCII-lowercased copy contains asciiLower(s), at the same offsets.
func asciiFold(s string) bool {
	for _, r := range s {
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if r >= utf8.RuneSelf || f >= utf8.RuneSelf && f != '\u017F' && f != '\u212A' {
				return false
			}
		}
	}
	return true
}

func asciiLower(s string) string {
	i := strings.IndexFunc(s, func(r rune) bool { return r >= 'A' && r <= 'Z' })
	if i < 0 {
		return s
	}
	b := []byte(s)
	for ; i < len(b); i++ {
		if c := b[i]; c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// gate is a necessary condition derived from a rule's syntax: every match
// contains one of lits. Go's regexp scans every byte unless a pattern starts
// with a single literal, so the gate skips texts, or for line-bound rules the
// lines, that cannot match.
type gate struct {
	lits  []literal // nil: search the whole text
	lines bool
}

type literal struct {
	text string // ASCII-lowercased when fold
	fold bool
}

func newGate(re *regexp.Regexp) gate {
	tree, err := syntax.Parse(re.String(), syntax.Perl) // as regexp.Compile does
	if err != nil {
		return gate{}
	}
	lits, ok := required(tree)
	if !ok {
		return gate{}
	}
	// A literal containing another is redundant: finding the shorter suffices.
	slices.SortFunc(lits, func(a, b literal) int { return len(a.text) - len(b.text) })
	var minimal []literal
	for _, l := range lits {
		if !slices.ContainsFunc(minimal, func(m literal) bool { return m.fold == l.fold && strings.Contains(l.text, m.text) }) {
			minimal = append(minimal, l)
		}
	}
	return gate{lits: minimal, lines: lineBound(tree)}
}

// required returns literals of which every match of re contains one.
func required(re *syntax.Regexp) ([]literal, bool) {
	switch re.Op {
	case syntax.OpLiteral:
		text, fold := string(re.Rune), re.Flags&syntax.FoldCase != 0
		if fold {
			if !asciiFold(text) {
				return nil, false
			}
			text = asciiLower(text)
		}
		return []literal{{text, fold}}, true
	case syntax.OpCapture, syntax.OpPlus:
		return required(re.Sub[0])
	case syntax.OpRepeat:
		if re.Min > 0 {
			return required(re.Sub[0])
		}
	case syntax.OpConcat:
		var best []literal
		for _, sub := range re.Sub {
			if lits, ok := required(sub); ok && (best == nil || stronger(lits, best)) {
				best = lits
			}
		}
		return best, best != nil
	case syntax.OpAlternate:
		var all []literal
		for _, sub := range re.Sub {
			lits, ok := required(sub)
			if !ok {
				return nil, false
			}
			all = append(all, lits...)
		}
		return all, true
	}
	return nil, false
}

// stronger prefers the set whose shortest literal is longer, then the smaller set.
func stronger(a, b []literal) bool {
	shortest := func(lits []literal) int {
		n := len(lits[0].text)
		for _, l := range lits[1:] {
			n = min(n, len(l.text))
		}
		return n
	}
	if sa, sb := shortest(a), shortest(b); sa != sb {
		return sa > sb
	}
	return len(a) < len(b)
}

// lineBound reports whether no match contains a newline and no anchor depends
// on where the text starts or ends. Each line can then be searched alone: its
// neighbors are newlines, which word boundaries and (?m) anchors treat like
// the ends of a text.
func lineBound(re *syntax.Regexp) bool {
	switch re.Op {
	case syntax.OpAnyChar, syntax.OpBeginText, syntax.OpEndText:
		return false
	case syntax.OpLiteral:
		if slices.Contains(re.Rune, '\n') {
			return false
		}
	case syntax.OpCharClass:
		for i := 0; i < len(re.Rune); i += 2 {
			if re.Rune[i] <= '\n' && '\n' <= re.Rune[i+1] {
				return false
			}
		}
	}
	for _, sub := range re.Sub {
		if !lineBound(sub) {
			return false
		}
	}
	return true
}

// windows returns the ranges of text that may hold a match, in order.
func (g gate) windows(text, lower string, unusualFold bool) [][2]int {
	all := [][2]int{{0, len(text)}}
	if g.lits == nil {
		return all
	}
	var lines [][2]int
	for _, l := range g.lits {
		haystack := text
		if l.fold {
			if unusualFold {
				return all
			}
			haystack = lower
		}
		for from := 0; from < len(text); {
			i := strings.Index(haystack[from:], l.text)
			if i < 0 {
				break
			}
			if !g.lines {
				return all
			}
			i += from
			start, end := strings.LastIndexByte(text[:i], '\n')+1, len(text)
			if j := strings.IndexByte(text[i:], '\n'); j >= 0 {
				end = i + j
			}
			lines = append(lines, [2]int{start, end})
			from = end
		}
	}
	slices.SortFunc(lines, func(a, b [2]int) int { return a[0] - b[0] })
	merged := lines[:0]
	for _, line := range lines {
		// Neighboring lines are searched together across their newline.
		if n := len(merged); n > 0 && line[0] <= merged[n-1][1]+1 {
			merged[n-1][1] = max(merged[n-1][1], line[1])
			continue
		}
		merged = append(merged, line)
	}
	return merged
}

// matchCache lets conversation history repeated on every turn skip detection.
// Rotating generations keep recently used texts without per-entry bookkeeping.
type matchCache struct {
	mu                sync.Mutex
	current, previous map[[32]byte][]match
	size              int
}

func (c *matchCache) get(key [32]byte) ([]match, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if found, ok := c.current[key]; ok {
		return found, true
	}
	found, ok := c.previous[key]
	if ok {
		c.addLocked(key, found)
	}
	return found, ok
}

func (c *matchCache) put(key [32]byte, found []match) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.addLocked(key, found)
}

func (c *matchCache) addLocked(key [32]byte, found []match) {
	if c.current == nil || len(c.current) >= cacheEntries || c.size+len(found) > cacheMatches {
		c.previous, c.current, c.size = c.current, make(map[[32]byte][]match), 0
	}
	c.current[key] = found
	c.size += len(found)
}
