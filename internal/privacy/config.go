// Package privacy masks sensitive text before it leaves the gateway and restores
// the original values from bounded, process-local, scope-isolated memory.
package privacy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Rule matches its first participating capture group, or the whole match when
// there are no groups. Prefix and Regex are mutually exclusive.
type Rule struct {
	Kind   string `json:"kind"`
	Prefix string `json:"prefix,omitempty"`
	Regex  string `json:"regex,omitempty"`
}

// Config is a request's immutable masking policy. Personal must be enabled for
// any of Kinds to take effect. Words and Rules are independent of Secrets and Personal.
type Config struct {
	Enabled      bool            `json:"enabled"`
	Secrets      bool            `json:"secrets"`
	Personal     bool            `json:"personal"`
	Kinds        map[string]bool `json:"kinds"`
	Words        []string        `json:"words"`
	WordsAnyCase bool            `json:"words_any_case"`
	WordsWhole   bool            `json:"words_whole"`
	Rules        []Rule          `json:"rules"`
	Allow        []string        `json:"allow"`
}

// DefaultConfig leaves masking disabled until the user opts in.
func DefaultConfig() Config {
	return Config{Secrets: true, Kinds: map[string]bool{
		"ssn": true, "passport": true, "iban": true, "birthday": true,
		"mac": true, "serial": true, "home": false, "userhost": false,
		"ip": false, "bucket": false,
	}, Words: []string{}, Rules: []Rule{}, Allow: []string{}}
}

var kindPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,23}$`)

// ParseConfig merges omitted fields with defaults and rejects malformed or
// excessive configuration. Error messages never echo configuration values.
func ParseConfig(raw string) (Config, error) {
	c := DefaultConfig()
	if strings.TrimSpace(raw) == "" {
		return c, nil
	}
	if len(raw) > 2<<20 {
		return Config{}, errors.New("privacy configuration exceeds 2 MiB")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil || fields == nil {
		return Config{}, errors.New("privacy configuration must be a JSON object")
	}
	for k, value := range fields {
		switch k {
		case "enabled", "secrets", "personal", "kinds", "words", "words_any_case", "words_whole", "rules", "allow":
		default:
			return Config{}, errors.New("unknown privacy configuration field")
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return Config{}, errors.New("privacy configuration fields cannot be null")
		}
		if k == "rules" {
			var rules []map[string]json.RawMessage
			if err := json.Unmarshal(value, &rules); err != nil {
				return Config{}, errors.New("privacy rules must be an array of objects")
			}
			for _, rule := range rules {
				for field, raw := range rule {
					var text string
					if field != "kind" && field != "prefix" && field != "regex" || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &text) != nil {
						return Config{}, errors.New("invalid privacy rule field or type")
					}
				}
			}
		}
		if k == "kinds" {
			var kinds map[string]json.RawMessage
			if err := json.Unmarshal(value, &kinds); err != nil {
				return Config{}, errors.New("privacy kinds must be an object of booleans")
			}
			for kind, v := range kinds {
				if _, ok := c.Kinds[kind]; !ok || string(v) != "true" && string(v) != "false" {
					return Config{}, errors.New("privacy kinds contain an unknown category or non-boolean value")
				}
			}
		}
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return Config{}, errors.New("invalid privacy configuration field or type")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return Config{}, errors.New("privacy configuration must contain one JSON object")
	}
	if len(c.Words) > 256 || len(c.Allow) > 256 || len(c.Rules) > 32 {
		return Config{}, errors.New("privacy configuration has too many words, allow entries, or rules")
	}
	for _, list := range []struct {
		values []string
		limit  int
	}{{c.Words, 512}, {c.Allow, 512}} {
		for _, v := range list.values {
			if strings.TrimSpace(v) == "" || strings.ContainsAny(v, "\r\n") || utf8.RuneCountInString(v) > list.limit {
				return Config{}, errors.New("privacy word or allow entry is empty, multiline, or too long")
			}
		}
	}
	for i := range c.Rules {
		r := &c.Rules[i]
		r.Kind = strings.TrimSpace(r.Kind)
		if r.Kind == "" {
			r.Kind = "CUSTOM"
		}
		if !kindPattern.MatchString(r.Kind) || (r.Prefix == "") == (r.Regex == "") ||
			r.Prefix != "" && (utf8.RuneCountInString(r.Prefix) < 3 || utf8.RuneCountInString(r.Prefix) > 64) || utf8.RuneCountInString(r.Regex) > 300 {
			return Config{}, errors.New("invalid privacy rule kind, prefix, or pattern")
		}
		re, err := compileRule(*r)
		if err != nil || re.MatchString("") {
			return Config{}, errors.New("privacy rule has an invalid or empty-matching regular expression")
		}
	}
	return c, nil
}

func compileRule(r Rule) (*regexp.Regexp, error) {
	if r.Prefix != "" {
		return regexp.Compile(regexp.QuoteMeta(r.Prefix) + `[A-Za-z0-9_-]{8,}`)
	}
	return regexp.Compile(r.Regex)
}
