package responses

import (
	"regexp"
	"strings"

	translatorcommon "ccLoad/internal/protocol/cliproxy/common"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Claude reports server-side web search as a pair of assistant content blocks: a
// server_tool_use block carrying the query and a web_search_tool_result block
// carrying the hits. OpenAI Responses models the same exchange as a single
// web_search_call item. The pair folds into one item on the way out and expands
// back on the way in so replay keeps both the search and its sources.
const (
	claudeWebSearchToolName = "web_search"

	// responsesWebSearchIDPrefix mirrors the fc_/ctc_ prefixes used for tool calls.
	responsesWebSearchIDPrefix = "ws_"

	// claudeServerToolIDPrefix is mandated by Anthropic.
	claudeServerToolIDPrefix = "srvtoolu_"
)

// Claude server tool ids are stricter than ordinary tool_use ids and forbid '-'.
var claudeServerToolIDSanitizer = regexp.MustCompile(`[^a-zA-Z0-9_]`)

func responsesWebSearchCallID(claudeToolUseID string) string {
	return responsesWebSearchIDPrefix + claudeToolUseID
}

// claudeWebSearchToolUseID recovers and normalizes a Claude server_tool_use id.
func claudeWebSearchToolUseID(responsesItemID string) string {
	body := strings.TrimPrefix(strings.TrimSpace(responsesItemID), responsesWebSearchIDPrefix)
	body = claudeServerToolIDSanitizer.ReplaceAllString(strings.TrimPrefix(body, claudeServerToolIDPrefix), "_")
	if body == "" {
		return ""
	}
	return claudeServerToolIDPrefix + body
}

func claudeWebSearchQuery(input string) string {
	if input == "" {
		return ""
	}
	return strings.TrimSpace(gjson.Get(input, "query").String())
}

// Entries remain verbatim because encrypted_content is required for replay.
func claudeWebSearchResultsToResponses(content gjson.Result) []byte {
	if content.IsObject() {
		return []byte(content.Raw)
	}
	if !content.IsArray() {
		return nil
	}
	var results [][]byte
	content.ForEach(func(_, entry gjson.Result) bool {
		if entry.Get("type").String() == "web_search_tool_result_error" || strings.TrimSpace(entry.Get("url").String()) != "" {
			results = append(results, []byte(entry.Raw))
		}
		return true
	})
	if len(results) == 0 {
		return []byte(`[]`)
	}
	return translatorcommon.JoinRawArray(results)
}

func buildResponsesWebSearchCallItem(claudeToolUseID, query string, results []byte) []byte {
	item := []byte(`{"id":"","type":"web_search_call","status":"completed","action":{"type":"search","query":""}}`)
	item, _ = sjson.SetBytes(item, "id", responsesWebSearchCallID(claudeToolUseID))
	item, _ = sjson.SetBytes(item, "action.query", query)
	if len(results) > 0 {
		item, _ = sjson.SetRawBytes(item, "results", results)
	}
	return item
}

// convertResponsesWebSearchCallToClaudeBlocksWithQuery rebuilds the server_tool_use
// block, using fallbackQuery when the item carries none, and the result block
// only when withResult is set.
func convertResponsesWebSearchCallToClaudeBlocksWithQuery(item gjson.Result, fallbackQuery string, withResult bool) [][]byte {
	toolUseID := claudeWebSearchToolUseID(strings.TrimSpace(item.Get("id").String()))
	if toolUseID == "" {
		return nil
	}

	use := []byte(`{"type":"server_tool_use","id":"","name":"","input":{}}`)
	use, _ = sjson.SetBytes(use, "id", toolUseID)
	use, _ = sjson.SetBytes(use, "name", claudeWebSearchToolName)
	query := responsesWebSearchCallQuery(item)
	if query == "" {
		query = fallbackQuery
	}
	if query != "" {
		use, _ = sjson.SetBytes(use, "input.query", query)
	}
	if !withResult {
		return [][]byte{use}
	}

	result := []byte(`{"type":"web_search_tool_result","tool_use_id":"","content":[]}`)
	result, _ = sjson.SetBytes(result, "tool_use_id", toolUseID)
	if content := responsesWebSearchResultsToClaude(item.Get("results")); len(content) > 0 {
		result, _ = sjson.SetRawBytes(result, "content", content)
	}
	return [][]byte{use, result}
}

// responsesWebSearchHistory indexes web_search_call items of one request so a
// search paused by pause_turn or deferred by tool_use replays faithfully:
//   - while the open search turn is the tail of the history, the search is sent
//     back as a server_tool_use without a result, unchanged, as Anthropic requires;
//   - when the continuation delivered the result under the same id within the
//     same assistant run (no user turn in between), the open item is skipped and
//     the continuation item emits the full pair, keeping the query recorded by
//     the open item.
//
// Every other web_search_call, including repeated ids across separate turns,
// keeps its previous one-pair-per-item replay.
type responsesWebSearchHistory struct {
	// boundaryAfter[i] reports whether a user turn follows index i.
	boundaryAfter []bool
	// skip marks paused items that a later continuation item replaces.
	skip map[int]bool
	// pausedQuery carries the paused item's query to its continuation item.
	pausedQuery map[int]string
}

func responsesWebSearchItemIsPaused(item gjson.Result) bool {
	return item.Get("type").String() == "web_search_call" &&
		strings.EqualFold(strings.TrimSpace(item.Get("status").String()), "in_progress") &&
		!item.Get("results").Exists()
}

// responsesItemIsTurnBoundary reports a user turn. Tool outputs do not end the
// assistant turn: Anthropic runs a deferred search right after them.
func responsesItemIsTurnBoundary(item gjson.Result) bool {
	typ := item.Get("type").String()
	role := item.Get("role").String()
	if typ == "" && role != "" {
		typ = "message"
	}
	return typ == "message" && role != "assistant"
}

func scanResponsesWebSearchHistory(items []gjson.Result) responsesWebSearchHistory {
	history := responsesWebSearchHistory{
		boundaryAfter: make([]bool, len(items)),
		skip:          map[int]bool{},
		pausedQuery:   map[int]string{},
	}
	boundary := false
	for index := len(items) - 1; index >= 0; index-- {
		history.boundaryAfter[index] = boundary
		if responsesItemIsTurnBoundary(items[index]) {
			boundary = true
		}
	}
	for index, item := range items {
		if !responsesWebSearchItemIsPaused(item) {
			continue
		}
		id := strings.TrimSpace(item.Get("id").String())
		if id == "" {
			continue
		}
		for next := index + 1; next < len(items); next++ {
			if responsesItemIsTurnBoundary(items[next]) {
				break
			}
			if items[next].Get("type").String() == "web_search_call" && strings.TrimSpace(items[next].Get("id").String()) == id {
				history.skip[index] = true
				query := responsesWebSearchCallQuery(item)
				if previous, ok := history.pausedQuery[index]; ok && query == "" {
					query = previous
				}
				history.pausedQuery[next] = query
				break
			}
		}
	}
	return history
}

func (h responsesWebSearchHistory) claudeBlocks(index int, item gjson.Result) [][]byte {
	if h.skip[index] {
		return nil
	}
	query := h.pausedQuery[index]
	tail := index >= len(h.boundaryAfter) || !h.boundaryAfter[index]
	if responsesWebSearchItemIsPaused(item) && tail {
		return convertResponsesWebSearchCallToClaudeBlocksWithQuery(item, query, false)
	}
	return convertResponsesWebSearchCallToClaudeBlocksWithQuery(item, query, true)
}

func responsesWebSearchCallQuery(item gjson.Result) string {
	if query := strings.TrimSpace(item.Get("action.query").String()); query != "" {
		return query
	}
	if query := strings.TrimSpace(item.Get("action.queries.0").String()); query != "" {
		return query
	}
	return strings.TrimSpace(item.Get("action.url").String())
}

func responsesWebSearchResultsToClaude(results gjson.Result) []byte {
	if results.IsObject() {
		return []byte(results.Raw)
	}
	if !results.IsArray() {
		return nil
	}
	var blocks [][]byte
	results.ForEach(func(_, entry gjson.Result) bool {
		if entry.Get("type").String() == "web_search_tool_result_error" {
			blocks = append(blocks, []byte(entry.Raw))
			return true
		}
		// Anthropic rejects replayed results without genuine encrypted_content.
		if strings.TrimSpace(entry.Get("encrypted_content").String()) == "" {
			return true
		}
		block := []byte(entry.Raw)
		block, _ = sjson.SetBytes(block, "type", "web_search_result")
		blocks = append(blocks, block)
		return true
	})
	if len(blocks) == 0 {
		return nil
	}
	return translatorcommon.JoinRawArray(blocks)
}

// attachClaudeCitations mirrors replay-safe Responses annotations back to Claude.
func attachClaudeCitations(textBlock []byte, annotations gjson.Result) []byte {
	if !annotations.IsArray() {
		return textBlock
	}
	var citations [][]byte
	annotations.ForEach(func(_, annotation gjson.Result) bool {
		if strings.TrimSpace(annotation.Get("encrypted_index").String()) != "" {
			citations = append(citations, []byte(annotation.Raw))
		}
		return true
	})
	if len(citations) == 0 {
		return textBlock
	}
	updated, err := sjson.SetRawBytes(textBlock, "citations", translatorcommon.JoinRawArray(citations))
	if err != nil {
		return textBlock
	}
	return updated
}
