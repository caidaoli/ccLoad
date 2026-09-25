package app

import (
	"net/http"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"

	"github.com/tidwall/gjson"
)

// anyrouterClaudeCodeFallbackToolsJSON 是 anyrouter 当前 Claude Code 路由所需的
// 最小真实工具集合。它们不是 Anthropic Messages 的协议必填字段，只在 anyrouter
// 对原生 Claude Code 请求且调用方未提供工具时作为兼容兜底注入。
const anyrouterClaudeCodeFallbackToolsJSON = `[
  {
    "name": "Edit",
    "description": "Performs exact string replacement in a file.\n\n- You must Read the file in this conversation before editing, or the call will fail.\n- \u0060old_string\u0060 must match the file exactly, including indentation, and be unique — the edit fails otherwise. Strip the Read line prefix (line number + tab) before matching.\n- \u0060replace_all: true\u0060 replaces every occurrence instead.",
    "input_schema": {
      "$schema": "https://json-schema.org/draft/2020-12/schema",
      "type": "object",
      "properties": {
        "file_path": {
          "description": "The absolute path to the file to modify",
          "type": "string"
        },
        "old_string": {
          "description": "The text to replace",
          "type": "string"
        },
        "new_string": {
          "description": "The text to replace it with (must be different from old_string)",
          "type": "string"
        },
        "replace_all": {
          "description": "Replace all occurrences of old_string (default false)",
          "default": false,
          "type": "boolean"
        }
      },
      "required": [
        "file_path",
        "old_string",
        "new_string"
      ],
      "additionalProperties": false
    }
  },
  {
    "name": "Read",
    "description": "Reads a file from the local filesystem.\n\n- \u0060file_path\u0060 must be an absolute path.\n- Reads up to 2000 lines by default.\n- You can optionally specify a line offset and limit (especially handy for long files), but it's recommended to read the whole file by not providing these parameters\n- Results are returned using cat -n format, with line numbers starting at 1\n- Reads images (PNG, JPG, …) and presents them visually. Reads PDFs via the \u0060pages\u0060 parameter (e.g. \"1-5\", max 20 pages/request; required for PDFs over 10 pages). Reads Jupyter notebooks (.ipynb) as cells with outputs.\n- Reading a directory, a missing file, or an empty file returns an error or system reminder rather than content.\n- Do NOT re-read a file you just edited to verify — Edit/Write would have errored if the change failed, and the harness tracks file state for you.",
    "input_schema": {
      "$schema": "https://json-schema.org/draft/2020-12/schema",
      "type": "object",
      "properties": {
        "file_path": {
          "description": "The absolute path to the file to read",
          "type": "string"
        },
        "offset": {
          "description": "The line number to start reading from. Only provide if the file is too large to read at once",
          "type": "integer",
          "minimum": 0,
          "maximum": 9007199254740991
        },
        "limit": {
          "description": "The number of lines to read. Only provide if the file is too large to read at once.",
          "type": "integer",
          "exclusiveMinimum": 0,
          "maximum": 9007199254740991
        },
        "pages": {
          "description": "Page range for PDF files (e.g., \"1-5\", \"3\", \"10-20\"). Only applicable to PDF files. Maximum 20 pages per request.",
          "type": "string"
        }
      },
      "required": [
        "file_path"
      ],
      "additionalProperties": false
    }
  },
  {
    "name": "Write",
    "description": "Writes a file to the local filesystem, overwriting if one exists.\n\nWhen to use: creating a new file, or fully replacing one you've already Read. Overwriting an existing file you haven't Read will fail. For partial changes, use Edit instead.",
    "input_schema": {
      "$schema": "https://json-schema.org/draft/2020-12/schema",
      "type": "object",
      "properties": {
        "file_path": {
          "description": "The absolute path to the file to write (must be absolute, not relative)",
          "type": "string"
        },
        "content": {
          "description": "The content to write to the local filesystem",
          "type": "string"
        }
      },
      "required": [
        "file_path",
        "content"
      ],
      "additionalProperties": false
    }
  }
]`

// injectAnyrouterClaudeCodeFallbackTools 为 anyrouter 的原生 Claude Code 请求补齐
// 上游路由识别所需的最小工具集合。已有非空 tools 或非目标请求保持原字节不变。
func injectAnyrouterClaudeCodeFallbackTools(
	cfg *model.Config,
	upstreamProtocol protocol.Protocol,
	requestPath string,
	headers http.Header,
	callerBody []byte,
	body []byte,
) []byte {
	if cfg == nil || upstreamProtocol != protocol.Anthropic ||
		!isAnthropicClaudeCodeMessagesRequest(cfg, upstreamProtocol, requestPath) ||
		!isAnyrouterChannel(cfg) || !isNativeAnthropicClaudeCodeRequest(callerBody, headers) ||
		!isAnthropicJSONObject(body) {
		return body
	}

	tools := gjson.GetBytes(body, "tools")
	if tools.IsArray() {
		if jsonMemberCount(tools) > 0 {
			return body
		}
		return setJSONRaw(body, "tools", anyrouterClaudeCodeFallbackToolsJSON)
	}
	if tools.Exists() {
		// 保留调用方的非法/未知 tools 形态，由既有校验链处理，不覆盖用户输入。
		return body
	}
	return setJSONRaw(body, "tools", anyrouterClaudeCodeFallbackToolsJSON)
}
