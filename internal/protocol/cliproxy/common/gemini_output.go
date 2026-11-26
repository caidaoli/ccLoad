package common

import (
	"fmt"
	"strings"

	"ccLoad/internal/protocol/cliproxy/util"

	"github.com/MoeForge/moego/ojson"
)

// GeminiJSONObjectInstruction supplies the JSON context required by JSON object mode.
const GeminiJSONObjectInstruction = "Return your response as a valid JSON object."

// GeminiResponseFormat returns JSON constraints in the text.format shape or
// a plain-text instruction for Gemini enum output.
func GeminiResponseFormat(root ojson.Result, requireObjectRoot bool) ([]byte, string, error) {
	var mime string
	var schema ojson.Result
	var schemaKey string
	for _, generationKey := range [...]string{"generationConfig", "generation_config"} {
		config := root.Get(generationKey)
		if mime == "" {
			mime = strings.TrimSpace(config.Get("responseMimeType").Str)
			if mime == "" {
				mime = strings.TrimSpace(config.Get("response_mime_type").Str)
			}
		}
		if schema.Exists() {
			continue
		}
		for _, key := range [...]string{"responseJsonSchema", "response_json_schema", "responseSchema", "response_schema"} {
			value := config.Get(key)
			if value.IsObject() || ((key == "responseJsonSchema" || key == "response_json_schema") && (value.Type == ojson.True || value.Type == ojson.False)) {
				schema, schemaKey = value, key
				break
			}
			if value.Type != ojson.Null && schemaKey == "" {
				schemaKey = key
			}
		}
	}

	jsonMode := strings.EqualFold(mime, "application/json")
	if !schema.Exists() {
		if schemaKey != "" {
			return nil, "", fmt.Errorf("invalid Gemini %s", schemaKey)
		}
		if jsonMode {
			return []byte(`{"type":"json_object"}`), "", nil
		}
		return nil, "", nil
	}
	if strings.EqualFold(mime, "text/x.enum") {
		instruction, err := geminiEnumOutputInstruction(schema, schemaKey)
		return nil, instruction, err
	}
	if mime != "" && !jsonMode {
		return nil, "", fmt.Errorf("response schema requires application/json, got %q", mime)
	}
	if requireObjectRoot && !schema.IsObject() {
		return nil, "", fmt.Errorf("%s uses a boolean schema that the target output format cannot represent", schemaKey)
	}
	if schema.Type == ojson.False {
		return nil, "", fmt.Errorf("%s is false and matches no JSON value", schemaKey)
	}
	raw := schema.Raw
	if schemaKey == "responseSchema" || schemaKey == "response_schema" {
		if requireObjectRoot && schema.Get("nullable").Type == ojson.True {
			return nil, "", fmt.Errorf("%s permits null at the root, which the target output format cannot represent", schemaKey)
		}
		raw = util.NormalizeGeminiToolSchema(raw)
		schema = ojson.Parse(raw)
		raw = normalizeGeminiProtoOutputNullable(schema)
	}
	if schemaType := schema.Get("type"); requireObjectRoot && schemaType.Exists() && schemaType.Str != "object" {
		return nil, "", fmt.Errorf("%s uses a root type that the target output format cannot represent", schemaKey)
	}
	format, err := ojson.SetRawBytes([]byte(`{"type":"json_schema","name":"gemini_response","strict":false}`), "schema", []byte(raw))
	return format, "", err
}

func geminiEnumOutputInstruction(schema ojson.Result, schemaKey string) (string, error) {
	protobuf := schemaKey == "responseSchema" || schemaKey == "response_schema"
	if protobuf {
		schema = ojson.Parse(util.NormalizeGeminiToolSchema(schema.Raw))
	}
	schemaType := schema.Get("type")
	enum := schema.Get("enum")
	values := enum.Array()
	if !enum.IsArray() || len(values) == 0 || (schemaType.Str != "string" && (protobuf || schemaType.Exists())) {
		return "", fmt.Errorf("text/x.enum requires a Gemini response schema with a non-empty string enum")
	}
	for _, value := range values {
		if value.Type != ojson.String {
			return "", fmt.Errorf("text/x.enum requires string enum values")
		}
	}
	return "Return exactly one of these values as plain text, without adding quotation marks or explanations: " + enum.Raw, nil
}

func normalizeGeminiProtoOutputNullable(schema ojson.Result) string {
	var updated strings.Builder
	cursor := 0
	rewrite := func(node ojson.Result) {
		if !node.IsObject() {
			return
		}
		if value := normalizeGeminiProtoOutputNullable(node); value != node.Raw {
			offset := node.Index - schema.Index
			updated.WriteString(schema.Raw[cursor:offset])
			updated.WriteString(value)
			cursor = offset + len(node.Raw)
		}
	}
	schema.ForEach(func(key, value ojson.Result) bool {
		switch key.Str {
		case "properties", "$defs", "definitions":
			if value.IsObject() {
				value.ForEach(func(_, node ojson.Result) bool {
					rewrite(node)
					return true
				})
			}
		case "items", "additionalProperties":
			rewrite(value)
		case "anyOf", "oneOf", "allOf":
			if value.IsArray() {
				value.ForEach(func(_, node ojson.Result) bool {
					rewrite(node)
					return true
				})
			}
		}
		return true
	})
	raw := schema.Raw
	if cursor != 0 {
		updated.WriteString(raw[cursor:])
		raw = updated.String()
	}
	switch schema.Get("nullable").Type {
	case ojson.True:
		raw, _ = ojson.Delete(raw, "nullable")
		// An outer union preserves every constraint on the non-null branch, including enum.
		return `{"anyOf":[` + raw + `,{"type":"null"}]}`
	case ojson.False:
		raw, _ = ojson.Delete(raw, "nullable")
	}
	return raw
}
