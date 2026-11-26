package claude

import (
	"encoding/json"
	"fmt"
	"strings"

	translatorcommon "ccLoad/internal/protocol/cliproxy/common"

	"github.com/MoeForge/moego/ojson"
)

func appendCodexWebSearchServerToolUse(output []byte, params *ConvertCodexResponseToClaudeParams, root, item ojson.Result) []byte {
	toolUseID := codexWebSearchToolUseID(params, root, item)
	if toolUseID == "" {
		return output
	}
	if params.WebSearchToolUseIDs == nil {
		params.WebSearchToolUseIDs = make(map[string]struct{})
	}
	query := codexWebSearchQuery(root, item)
	alreadyStarted := false
	if _, ok := params.WebSearchToolUseIDs[toolUseID]; ok {
		alreadyStarted = true
		if query == "" {
			return output
		}
	}

	if !alreadyStarted {
		output = append(output, stopCodexTextBlock(params)...)
		output = append(output, finalizeCodexThinkingBlock(params)...)
		template := []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"","name":"web_search","input":{}}}`)
		template, _ = ojson.SetBytes(template, "index", params.BlockIndex)
		template, _ = ojson.SetBytes(template, "content_block.id", toolUseID)
		output = translatorcommon.AppendSSEEventBytes(output, "content_block_start", template, 2)
	}

	if query != "" {
		partialJSON, _ := json.Marshal(map[string]string{"query": query})
		delta := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}`)
		delta, _ = ojson.SetBytes(delta, "index", params.BlockIndex)
		delta, _ = ojson.SetBytes(delta, "delta.partial_json", string(partialJSON))
		output = translatorcommon.AppendSSEEventBytes(output, "content_block_delta", delta, 2)
	}

	if !alreadyStarted {
		stop := []byte(`{"type":"content_block_stop","index":0}`)
		stop, _ = ojson.SetBytes(stop, "index", params.BlockIndex)
		output = translatorcommon.AppendSSEEventBytes(output, "content_block_stop", stop, 2)
		params.WebSearchToolUseIDs[toolUseID] = struct{}{}
		params.BlockIndex++
	}
	return output
}

func appendCodexWebSearchToolResult(output []byte, params *ConvertCodexResponseToClaudeParams, root, item ojson.Result) []byte {
	toolUseID := codexWebSearchToolUseID(params, root, item)
	if toolUseID == "" {
		return output
	}
	output = appendCodexWebSearchServerToolUse(output, params, root, item)
	if params.WebSearchToolResultIDs == nil {
		params.WebSearchToolResultIDs = make(map[string]struct{})
	}
	if _, ok := params.WebSearchToolResultIDs[toolUseID]; ok {
		return output
	}
	if codexWebSearchQuery(root, item) == "" && len(codexWebSearchResultContent(root, item)) == 0 && !item.Get("action").Exists() {
		return output
	}

	template := []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"web_search_tool_result","tool_use_id":"","content":[]}}`)
	template, _ = ojson.SetBytes(template, "index", params.BlockIndex)
	template, _ = ojson.SetBytes(template, "content_block.tool_use_id", toolUseID)
	if content := codexWebSearchResultContent(root, item); len(content) > 0 {
		template, _ = ojson.SetRawBytes(template, "content_block.content", content)
	}
	output = translatorcommon.AppendSSEEventBytes(output, "content_block_start", template, 2)

	stop := []byte(`{"type":"content_block_stop","index":0}`)
	stop, _ = ojson.SetBytes(stop, "index", params.BlockIndex)
	output = translatorcommon.AppendSSEEventBytes(output, "content_block_stop", stop, 2)
	params.WebSearchToolResultIDs[toolUseID] = struct{}{}
	params.BlockIndex++
	if toolUseID == params.LastWebSearchToolUseID {
		params.LastWebSearchToolUseID = ""
	}
	return output
}

func codexWebSearchToolUseID(params *ConvertCodexResponseToClaudeParams, root, item ojson.Result) string {
	for _, path := range []string{"id", "output_item_id", "call_id"} {
		if value := strings.TrimSpace(item.Get(path).String()); value != "" {
			return value
		}
		if value := strings.TrimSpace(root.Get(path).String()); value != "" {
			return value
		}
	}
	if params.LastWebSearchToolUseID != "" {
		return params.LastWebSearchToolUseID
	}
	for _, path := range []string{"item_id"} {
		if value := strings.TrimSpace(item.Get(path).String()); value != "" {
			return value
		}
		if value := strings.TrimSpace(root.Get(path).String()); value != "" {
			return value
		}
	}
	id := fmt.Sprintf("web_search_%d", params.BlockIndex)
	params.LastWebSearchToolUseID = id
	return id
}

func codexWebSearchQuery(root, item ojson.Result) string {
	for _, path := range []string{"action.query", "query", "input.query"} {
		if value := strings.TrimSpace(item.Get(path).String()); value != "" {
			return value
		}
		if value := strings.TrimSpace(root.Get(path).String()); value != "" {
			return value
		}
	}
	return ""
}

func codexWebSearchResultContent(root, item ojson.Result) []byte {
	var results ojson.Result
	for _, candidate := range []ojson.Result{
		item.Get("results"),
		root.Get("results"),
		item.Get("action.sources"),
		root.Get("action.sources"),
	} {
		if candidate.IsArray() {
			results = candidate
			break
		}
	}
	if !results.IsArray() {
		return nil
	}
	var resultBlocks [][]byte
	results.ForEach(func(_, result ojson.Result) bool {
		url := strings.TrimSpace(result.Get("url").String())
		if url == "" {
			return true
		}
		block := []byte(`{"type":"web_search_result","title":"","url":"","page_age":null}`)
		block, _ = ojson.SetBytes(block, "url", url)
		title := strings.TrimSpace(result.Get("title").String())
		if title == "" {
			title = url
		}
		block, _ = ojson.SetBytes(block, "title", title)
		resultBlocks = append(resultBlocks, block)
		return true
	})
	if len(resultBlocks) == 0 {
		return []byte(`[]`)
	}
	return translatorcommon.JoinRawArray(resultBlocks)
}

func appendCodexWebSearchNonStreamBlocks(contentBlocks [][]byte, item ojson.Result, seen map[string]struct{}) [][]byte {
	id := strings.TrimSpace(item.Get("id").String())
	if id == "" {
		return contentBlocks
	}
	if seen == nil {
		seen = make(map[string]struct{})
	}
	if _, ok := seen[id]; ok {
		return contentBlocks
	}
	emptyRoot := ojson.Result{}
	query := codexWebSearchQuery(emptyRoot, item)
	resultContent := codexWebSearchResultContent(emptyRoot, item)
	if query == "" && len(resultContent) == 0 {
		return contentBlocks
	}

	useBlock := []byte(`{"type":"server_tool_use","id":"","name":"web_search","input":{}}`)
	useBlock, _ = ojson.SetBytes(useBlock, "id", id)
	if query != "" {
		input, _ := json.Marshal(map[string]string{"query": query})
		useBlock, _ = ojson.SetRawBytes(useBlock, "input", input)
	}
	contentBlocks = append(contentBlocks, useBlock)

	resultBlock := []byte(`{"type":"web_search_tool_result","tool_use_id":"","content":[]}`)
	resultBlock, _ = ojson.SetBytes(resultBlock, "tool_use_id", id)
	if len(resultContent) > 0 {
		resultBlock, _ = ojson.SetRawBytes(resultBlock, "content", resultContent)
	}
	contentBlocks = append(contentBlocks, resultBlock)
	seen[id] = struct{}{}
	return contentBlocks
}
