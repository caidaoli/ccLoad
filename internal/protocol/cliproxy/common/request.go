package common

import (
	"crypto/rand"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const tooluLetters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// GenerateClaudeToolCallID generates a random tool use ID prefixed with toolu_
// using rejection sampling to guarantee a uniform distribution across the 62 alphanumeric characters.
func GenerateClaudeToolCallID() string {
	const maxValidByte = 256 - (256 % len(tooluLetters)) // 248: exact multiple of 62
	var b strings.Builder
	b.Grow(len("toolu_") + 24)
	b.WriteString("toolu_")

	var buf [32]byte
	n := 0
	for n < 24 {
		_, _ = rand.Read(buf[:])
		for _, bVal := range buf {
			if int(bVal) < maxValidByte {
				b.WriteByte(tooluLetters[int(bVal)%len(tooluLetters)])
				n++
				if n == 24 {
					break
				}
			}
		}
	}
	return b.String()
}

// RequestModelName returns the model name from the original request, falling
// back to the translated request when the original request is unavailable.
func RequestModelName(originalRequestRawJSON, requestRawJSON []byte) string {
	for _, rawJSON := range [][]byte{originalRequestRawJSON, requestRawJSON} {
		if modelName := requestModelName(rawJSON); modelName != "" {
			return modelName
		}
	}
	return ""
}

func requestModelName(rawJSON []byte) string {
	if len(rawJSON) == 0 || !gjson.ValidBytes(rawJSON) {
		return ""
	}

	root := gjson.ParseBytes(rawJSON)
	for _, path := range []string{"model", "request.model"} {
		model := root.Get(path)
		if model.Type == gjson.String && strings.TrimSpace(model.String()) != "" {
			return model.String()
		}
	}
	return ""
}

// ApplyClaudeGenerationConstraints copies Claude limits, stop sequences and a valid
// structured-output schema to a Gemini generation config using its target schema field.
func ApplyClaudeGenerationConstraints(out, source []byte, configPath, schemaKey string) []byte {
	if value := gjson.GetBytes(source, "max_tokens"); value.Type == gjson.Number {
		out, _ = sjson.SetRawBytes(out, configPath+".maxOutputTokens", []byte(value.Raw))
	}

	if stops := gjson.GetBytes(source, "stop_sequences"); stops.IsArray() {
		var sequences []string
		stops.ForEach(func(_, stop gjson.Result) bool {
			if stop.Type == gjson.String && stop.Str != "" {
				sequences = append(sequences, stop.Str)
			}
			return true
		})
		if len(sequences) > 0 {
			out, _ = sjson.SetBytes(out, configPath+".stopSequences", sequences)
		}
	}

	if format := gjson.GetBytes(source, "output_config.format"); format.IsObject() &&
		strings.EqualFold(strings.TrimSpace(format.Get("type").String()), "json_schema") {
		if schema := format.Get("schema"); schema.IsObject() {
			out, _ = sjson.SetBytes(out, configPath+".responseMimeType", "application/json")
			out, _ = sjson.SetRawBytes(out, configPath+"."+schemaKey, []byte(schema.Raw))
		}
	}
	return out
}
