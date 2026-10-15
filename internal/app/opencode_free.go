package app

import (
	_ "embed"
	"encoding/json"
	"errors"
	"sync"

	"ccLoad/internal/protocol"

	"github.com/tidwall/gjson"
)

// Official OpenCode tool descriptions and schemas; provenance and MIT notice
// are in opencode_tools.LICENSE. The shaping implementation below is local.
//
//go:embed opencode_tools.json
var openCodeFreeToolsJSON []byte

type openCodeFreeToolDefinition struct {
	Description json.RawMessage `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

var openCodeFreeTools = sync.OnceValues(func() (map[string]openCodeFreeToolDefinition, error) {
	var definitions map[string]openCodeFreeToolDefinition
	err := json.Unmarshal(openCodeFreeToolsJSON, &definitions)
	return definitions, err
})

// prepareOpenCodeFreeBody supplies the agent-shaped wire accepted by the free
// inference lane. Endpoint and free-model eligibility belong to the caller.
func prepareOpenCodeFreeBody(body []byte, upstream protocol.Protocol) ([]byte, error) {
	if upstream != protocol.OpenAI && upstream != protocol.Codex && upstream != protocol.Anthropic && upstream != protocol.Gemini {
		return body, nil
	}
	root, err := parseOrderedJSON(body)
	if err != nil || root.kind != '{' {
		return nil, errors.New("invalid OpenCode free inference request body")
	}
	definitions, err := openCodeFreeTools()
	if err != nil {
		return nil, err
	}
	_, tools := root.objectMember("tools")
	if tools == nil || gjson.Parse(tools.raw).Type == gjson.Null {
		tools = &orderedJSONNode{kind: '[', raw: "[]"}
		root.overridePath([]string{"tools"}, tools)
	} else if tools.kind != '[' {
		return nil, &requestValidationError{message: "OpenCode free inference tools must be an array"}
	}
	// Client definitions win over same-name official tools: the caller parses
	// tool-call arguments with its own schema. Only missing tools are appended.
	present := make(map[string]bool)
	// Gemini groups declarations inside tool objects; missing official tools
	// join the first declaration list instead of adding another tool object.
	var declarations *orderedJSONNode
	for _, tool := range tools.array {
		if tool.kind != '{' {
			return nil, &requestValidationError{message: "OpenCode free inference tools must contain objects"}
		}
		if upstream == protocol.Gemini {
			for _, key := range []string{"functionDeclarations", "function_declarations"} {
				_, list := tool.objectMember(key)
				if list == nil || list.kind != '[' {
					continue
				}
				if declarations == nil {
					declarations = list
				}
				for _, declaration := range list.array {
					if declaration.kind == '{' {
						present[openCodeBodyString(declaration, "name")] = true
					}
				}
			}
			continue
		}
		if upstream == protocol.Codex && openCodeBodyString(tool, "type") != "function" {
			continue
		}
		entry := tool
		if upstream == protocol.OpenAI {
			_, entry = tool.objectMember("function")
			if entry == nil || entry.kind != '{' {
				continue // Preserve opaque/custom tool kinds for their upstream.
			}
		}
		present[openCodeBodyString(entry, "name")] = true
	}
	for _, name := range []string{"shell", "edit", "glob", "grep", "read"} {
		if present[name] {
			continue
		}
		tool, buildErr := openCodeFreeTool(name, definitions[name], upstream)
		if buildErr != nil {
			return nil, buildErr
		}
		if upstream == protocol.Gemini {
			if declarations == nil {
				declarations = &orderedJSONNode{kind: '[', raw: "[]"}
				tools.array = append(tools.array, &orderedJSONNode{kind: '{', raw: "{}", dirty: true,
					object: []orderedJSONMember{{key: "functionDeclarations", value: declarations}}})
				tools.dirty = true
			}
			declarations.array = append(declarations.array, tool)
			declarations.dirty = true
			continue
		}
		tools.array = append(tools.array, tool)
		tools.dirty = true
	}
	// Gemini selects streaming by the URL action, not a body field.
	if upstream == protocol.Gemini {
		return root.render(), nil
	}
	if _, stream := root.objectMember("stream"); stream == nil || gjson.Parse(stream.raw).Type != gjson.True {
		root.overridePath([]string{"stream"}, &orderedJSONNode{raw: "true"})
	}
	if upstream == protocol.OpenAI {
		_, options := root.objectMember("stream_options")
		if options == nil || options.kind != '{' {
			options = &orderedJSONNode{kind: '{', raw: "{}"}
			root.overridePath([]string{"stream_options"}, options)
		}
		if _, usage := options.objectMember("include_usage"); usage == nil || gjson.Parse(usage.raw).Type != gjson.True {
			options.overridePath([]string{"include_usage"}, &orderedJSONNode{raw: "true"})
		}
	}
	return root.render(), nil
}

func openCodeFreeTool(name string, definition openCodeFreeToolDefinition, upstream protocol.Protocol) (*orderedJSONNode, error) {
	if len(definition.Description) == 0 || len(definition.Parameters) == 0 {
		return nil, errors.New("missing official OpenCode tool definition: " + name)
	}
	fields := `"name":` + jsonEscapedString(name) + `,"description":` + string(definition.Description)
	var raw string
	switch upstream {
	case protocol.OpenAI:
		raw = `{"type":"function","function":{` + fields + `,"parameters":` + string(definition.Parameters) + `}}`
	case protocol.Anthropic:
		raw = `{` + fields + `,"input_schema":` + string(definition.Parameters) + `}`
	case protocol.Gemini:
		// parametersJsonSchema takes the full JSON Schema; parameters is an
		// OpenAPI subset that rejects keywords such as additionalProperties.
		raw = `{` + fields + `,"parametersJsonSchema":` + string(definition.Parameters) + `}`
	default:
		raw = `{"type":"function",` + fields + `,"parameters":` + string(definition.Parameters) + `}`
	}
	return parseOrderedJSON([]byte(raw))
}
