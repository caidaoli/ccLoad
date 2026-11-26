package common

import (
	"errors"
	"strings"

	"github.com/MoeForge/moego/ojson"
)

// SetGeminiFunctionCallArguments writes an object to functionCall.args.
// Legacy non-object arguments are kept as text under the caller's fallback field.
func SetGeminiFunctionCallArguments(part []byte, rawArgs, fallbackField string) []byte {
	trimmed := strings.TrimSpace(rawArgs)
	switch {
	case trimmed == "" || trimmed == "null":
		part, _ = ojson.SetRawBytes(part, "functionCall.args", []byte("{}"))
	case strings.HasPrefix(trimmed, "{") && ojson.Valid(trimmed):
		part, _ = ojson.SetRawBytes(part, "functionCall.args", []byte(trimmed))
	default:
		part, _ = SetStringWithoutHTMLEscape(part, "functionCall.args."+fallbackField, rawArgs)
	}
	return part
}

// GeminiFunctionDeclarations returns the declared functions in request order.
func GeminiFunctionDeclarations(root ojson.Result) []ojson.Result {
	tools := root.Get("tools")
	if !tools.IsArray() {
		return nil
	}
	var declarations []ojson.Result
	for _, tool := range tools.Array() {
		for _, key := range []string{"functionDeclarations", "function_declarations"} {
			if functions := tool.Get(key); functions.IsArray() {
				declarations = append(declarations, functions.Array()...)
				break
			}
		}
	}
	return declarations
}

// GeminiFunctionCallingOptions resolves a nonempty ANY allowlist against original tool names.
func GeminiFunctionCallingOptions(root ojson.Result, declarations []ojson.Result) (string, []string, error) {
	var config ojson.Result
	var mode string
	for _, path := range [...]string{
		"toolConfig.functionCallingConfig",
		"toolConfig.function_calling_config",
		"tool_config.function_calling_config",
		"tool_config.functionCallingConfig",
	} {
		config = root.Get(path)
		mode = strings.ToUpper(strings.TrimSpace(config.Get("mode").Str))
		if mode != "" {
			break
		}
	}
	if mode != "ANY" {
		return mode, nil, nil
	}
	names := config.Get("allowedFunctionNames")
	if !names.IsArray() {
		snakeNames := config.Get("allowed_function_names")
		if !snakeNames.IsArray() && (names.Type != ojson.Null || snakeNames.Type != ojson.Null) {
			return "", nil, errors.New("allowedFunctionNames must be an array")
		}
		names = snakeNames
	}
	requested := names.Array()
	if len(requested) == 0 {
		return mode, nil, nil
	}
	declared := make(map[string]struct{}, len(declarations))
	for _, tool := range declarations {
		if name := tool.Get("name").Str; name != "" {
			declared[name] = struct{}{}
		}
	}
	var allowed []string
	for _, item := range requested {
		name := strings.TrimSpace(item.Str)
		if _, exists := declared[name]; exists {
			allowed = append(allowed, name)
			delete(declared, name)
		}
	}
	if len(allowed) == 0 {
		return "", nil, errors.New("tool allowlist does not match any declared function")
	}
	return mode, allowed, nil
}
