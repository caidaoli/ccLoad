// Claude CCH signing follows CLIProxyAPI's MIT-licensed Claude Code 2.1.220
// wire implementation, pinned by this repository at commit 34d59e06.
package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	xxHash64 "github.com/pierrec/xxHash/xxHash64"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	anthropicCCHSeed   uint64 = 0x4D659218E32A3268
	anthropicCCHLength        = 5
	anthropicCCHZero          = "00000"
)

type anthropicCCHNormalizationEdit struct {
	start int
	end   int
}

type anthropicCCHJSONMember struct {
	start       int
	end         int
	commaBefore int
	commaAfter  int
	excluded    bool
}

type anthropicCCHJSONScanner struct {
	body  []byte
	pos   int
	edits []anthropicCCHNormalizationEdit
}

func finalizeAnthropicCCH(body []byte) ([]byte, error) {
	billing := gjson.GetBytes(body, "system.0.text")
	if billing.Type != gjson.String || !strings.HasPrefix(billing.String(), "x-anthropic-billing-header:") {
		return body, nil
	}
	if _, ok := anthropicCCHDigitsOffset(body); !ok {
		billingText := billing.String()
		entrypoint := strings.Index(billingText, "cc_entrypoint=")
		if entrypoint < 0 {
			return body, nil
		}
		entrypointEnd := strings.IndexByte(billingText[entrypoint:], ';')
		if entrypointEnd < 0 {
			return body, nil
		}
		insertAt := entrypoint + entrypointEnd + 1
		billingText = billingText[:insertAt] + " cch=00000;" + billingText[insertAt:]
		var err error
		body, err = sjson.SetBytes(body, "system.0.text", billingText)
		if err != nil {
			return nil, fmt.Errorf("insert Claude CCH placeholder: %w", err)
		}
	}

	offset, ok := anthropicCCHDigitsOffset(body)
	if !ok {
		return body, nil
	}
	unsigned := bytes.Clone(body)
	copy(unsigned[offset:offset+anthropicCCHLength], anthropicCCHZero)
	normalized, err := normalizeAnthropicCCHInput(unsigned)
	if err != nil {
		return nil, fmt.Errorf("normalize Claude CCH input: %w", err)
	}
	hasher := xxHash64.New(anthropicCCHSeed)
	if _, err = hasher.Write(normalized); err != nil {
		return nil, fmt.Errorf("hash Claude CCH input: %w", err)
	}
	copy(unsigned[offset:offset+anthropicCCHLength], fmt.Sprintf("%05x", hasher.Sum64()&0xFFFFF))
	return unsigned, nil
}

func anthropicCCHDigitsOffset(body []byte) (int, bool) {
	billing := gjson.GetBytes(body, "system.0.text")
	if billing.Type != gjson.String || !strings.HasPrefix(billing.String(), "x-anthropic-billing-header:") {
		return 0, false
	}
	raw := []byte(billing.Raw)
	for searchFrom := 0; searchFrom < len(raw); {
		relative := bytes.Index(raw[searchFrom:], []byte("cch="))
		if relative < 0 {
			return 0, false
		}
		prefix := searchFrom + relative
		digits := prefix + len("cch=")
		end := digits + anthropicCCHLength
		if end < len(raw) && raw[end] == ';' && isLowerHex(raw[digits:end]) {
			return billing.Index + digits, true
		}
		searchFrom = prefix + len("cch=")
	}
	return 0, false
}

func isLowerHex(value []byte) bool {
	if len(value) != anthropicCCHLength {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

// normalizeAnthropicCCHInput reproduces Claude Code 2.1.220's byte-level hash view.
// Re-marshalling is forbidden here because JSON member order and whitespace are signed.
func normalizeAnthropicCCHInput(body []byte) ([]byte, error) {
	if !json.Valid(body) {
		return nil, fmt.Errorf("invalid JSON body")
	}
	scanner := anthropicCCHJSONScanner{body: body, edits: make([]anthropicCCHNormalizationEdit, 0)}
	if err := scanner.parseValue(true); err != nil {
		return nil, err
	}
	scanner.skipWhitespace()
	if scanner.pos != len(body) {
		return nil, fmt.Errorf("unexpected JSON data at byte %d", scanner.pos)
	}
	sort.Slice(scanner.edits, func(i, j int) bool { return scanner.edits[i].start < scanner.edits[j].start })
	normalized := make([]byte, 0, len(body))
	last := 0
	for _, edit := range scanner.edits {
		if edit.start < last || edit.end > len(body) {
			return nil, fmt.Errorf("overlapping CCH normalization edit at byte %d", edit.start)
		}
		normalized = append(normalized, body[last:edit.start]...)
		last = edit.end
	}
	normalized = append(normalized, body[last:]...)
	return normalized, nil
}

func (scanner *anthropicCCHJSONScanner) parseValue(collect bool) error {
	scanner.skipWhitespace()
	if scanner.pos >= len(scanner.body) {
		return fmt.Errorf("missing JSON value at byte %d", scanner.pos)
	}
	switch scanner.body[scanner.pos] {
	case '{':
		return scanner.parseObject(collect)
	case '[':
		return scanner.parseArray(collect)
	case '"':
		_, _, err := scanner.parseString()
		return err
	default:
		start := scanner.pos
		for scanner.pos < len(scanner.body) {
			switch scanner.body[scanner.pos] {
			case ',', '}', ']', ' ', '\t', '\r', '\n':
				if scanner.pos == start {
					return fmt.Errorf("missing JSON value at byte %d", start)
				}
				return nil
			default:
				scanner.pos++
			}
		}
		if scanner.pos == start {
			return fmt.Errorf("missing JSON value at byte %d", start)
		}
		return nil
	}
}

func (scanner *anthropicCCHJSONScanner) parseObject(collect bool) error {
	scanner.pos++
	scanner.skipWhitespace()
	if scanner.consume('}') {
		return nil
	}
	members := make([]anthropicCCHJSONMember, 0)
	commaBefore := -1
	for {
		scanner.skipWhitespace()
		memberStart := scanner.pos
		keyStart, keyEnd, err := scanner.parseString()
		if err != nil {
			return err
		}
		scanner.skipWhitespace()
		if !scanner.consume(':') {
			return fmt.Errorf("missing object colon at byte %d", scanner.pos)
		}
		scanner.skipWhitespace()
		key := scanner.body[keyStart:keyEnd]
		excluded := collect && isAnthropicCCHExcludedKey(key)
		if collect && bytes.Equal(key, []byte(`"model"`)) && scanner.pos < len(scanner.body) && scanner.body[scanner.pos] == '"' {
			valueStart, valueEnd, errString := scanner.parseString()
			if errString != nil {
				return errString
			}
			scanner.addEdit(valueStart+1, valueEnd-1)
		} else if err = scanner.parseValue(collect && !excluded); err != nil {
			return err
		}
		memberEnd := scanner.pos
		scanner.skipWhitespace()
		commaAfter := -1
		if scanner.consume(',') {
			commaAfter = scanner.pos - 1
		}
		members = append(members, anthropicCCHJSONMember{
			start: memberStart, end: memberEnd, commaBefore: commaBefore, commaAfter: commaAfter, excluded: excluded,
		})
		if commaAfter >= 0 {
			commaBefore = commaAfter
			continue
		}
		if !scanner.consume('}') {
			return fmt.Errorf("missing object end at byte %d", scanner.pos)
		}
		break
	}
	if collect {
		scanner.addExcludedMemberEdits(members)
	}
	return nil
}

func (scanner *anthropicCCHJSONScanner) parseArray(collect bool) error {
	scanner.pos++
	scanner.skipWhitespace()
	if scanner.consume(']') {
		return nil
	}
	for {
		if err := scanner.parseValue(collect); err != nil {
			return err
		}
		scanner.skipWhitespace()
		if scanner.consume(',') {
			continue
		}
		if !scanner.consume(']') {
			return fmt.Errorf("missing array end at byte %d", scanner.pos)
		}
		return nil
	}
}

func (scanner *anthropicCCHJSONScanner) parseString() (start, end int, err error) {
	if scanner.pos >= len(scanner.body) || scanner.body[scanner.pos] != '"' {
		return 0, 0, fmt.Errorf("missing JSON string at byte %d", scanner.pos)
	}
	start = scanner.pos
	scanner.pos++
	for scanner.pos < len(scanner.body) {
		switch scanner.body[scanner.pos] {
		case '\\':
			scanner.pos += 2
		case '"':
			scanner.pos++
			return start, scanner.pos, nil
		default:
			scanner.pos++
		}
	}
	return 0, 0, fmt.Errorf("unterminated JSON string at byte %d", start)
}

func (scanner *anthropicCCHJSONScanner) addExcludedMemberEdits(members []anthropicCCHJSONMember) {
	for start := 0; start < len(members); {
		if !members[start].excluded {
			start++
			continue
		}
		end := start
		for end+1 < len(members) && members[end+1].excluded {
			end++
		}
		switch {
		case end+1 < len(members):
			scanner.addEdit(members[start].start, members[end].commaAfter+1)
		case start > 0 && end > start:
			scanner.addEdit(members[start].start, members[end].end)
		case start > 0:
			scanner.addEdit(members[start].commaBefore, members[end].end)
		default:
			scanner.addEdit(members[start].start, members[end].end)
		}
		start = end + 1
	}
}

func (scanner *anthropicCCHJSONScanner) addEdit(start, end int) {
	if start < end {
		scanner.edits = append(scanner.edits, anthropicCCHNormalizationEdit{start: start, end: end})
	}
}

func (scanner *anthropicCCHJSONScanner) skipWhitespace() {
	for scanner.pos < len(scanner.body) {
		switch scanner.body[scanner.pos] {
		case ' ', '\t', '\r', '\n':
			scanner.pos++
		default:
			return
		}
	}
}

func (scanner *anthropicCCHJSONScanner) consume(character byte) bool {
	if scanner.pos >= len(scanner.body) || scanner.body[scanner.pos] != character {
		return false
	}
	scanner.pos++
	return true
}

func isAnthropicCCHExcludedKey(key []byte) bool {
	switch string(key) {
	case `"max_tokens"`, `"fallbacks"`, `"fallback_credit_token"`:
		return true
	default:
		return false
	}
}
