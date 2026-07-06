package app

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

func TestHandleGetDebugLog_NotFoundIncludesRelevantSettings(t *testing.T) {
	srv := newInMemoryServer(t)

	if err := srv.store.UpdateSetting(t.Context(), "debug_log_enabled", "false"); err != nil {
		t.Fatalf("update debug_log_enabled: %v", err)
	}
	if err := srv.store.UpdateSetting(t.Context(), "debug_log_retention_minutes", "15"); err != nil {
		t.Fatalf("update debug_log_retention_minutes: %v", err)
	}

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/debug-logs/123", nil))
	c.Params = gin.Params{{Key: "log_id", Value: "123"}}

	srv.HandleGetDebugLog(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want %d", w.Code, http.StatusNotFound)
	}

	type unavailableData struct {
		Reason                   string               `json:"reason"`
		DebugLogEnabled          *model.SystemSetting `json:"debug_log_enabled"`
		DebugLogRetentionMinutes *model.SystemSetting `json:"debug_log_retention_minutes"`
	}

	resp := mustParseAPIResponse[unavailableData](t, w.Body.Bytes())
	if resp.Success {
		t.Fatalf("success=%v, want false", resp.Success)
	}
	if resp.Error != "debug log unavailable" {
		t.Fatalf("error=%q, want %q", resp.Error, "debug log unavailable")
	}
	if resp.Data.Reason != "debug_log_not_found" {
		t.Fatalf("reason=%q, want %q", resp.Data.Reason, "debug_log_not_found")
	}
	if resp.Data.DebugLogEnabled == nil {
		t.Fatal("debug_log_enabled should be returned")
	}
	if resp.Data.DebugLogEnabled.Key != "debug_log_enabled" || resp.Data.DebugLogEnabled.Value != "false" {
		t.Fatalf("debug_log_enabled=%+v, want key/value debug_log_enabled/false", resp.Data.DebugLogEnabled)
	}
	if resp.Data.DebugLogRetentionMinutes == nil {
		t.Fatal("debug_log_retention_minutes should be returned")
	}
	if resp.Data.DebugLogRetentionMinutes.Key != "debug_log_retention_minutes" || resp.Data.DebugLogRetentionMinutes.Value != "15" {
		t.Fatalf("debug_log_retention_minutes=%+v, want key/value debug_log_retention_minutes/15", resp.Data.DebugLogRetentionMinutes)
	}
}

func TestMergeResponseBody_ReplaysDocsSamples(t *testing.T) {
	t.Parallel()

	root := testRepoRoot(t)
	docsDir := filepath.Join(root, "docs")
	if _, err := os.Stat(docsDir); err != nil {
		if os.IsNotExist(err) {
			t.Skip("docs replay samples are not present")
		}
		t.Fatalf("stat docs dir: %v", err)
	}
	for _, name := range []string{"1.txt", "2.txt", "3.txt", "4.txt", "5.txt", "6.txt"} {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			raw, err := os.ReadFile(filepath.Join(docsDir, name))
			if err != nil {
				if os.IsNotExist(err) {
					t.Skipf("docs replay sample %s is not present", name)
				}
				t.Fatalf("read docs sample: %v", err)
			}
			parts := mergeResponseBody(string(raw))
			if strings.TrimSpace(parts.Content) == "" && strings.TrimSpace(parts.Tools) == "" {
				t.Fatalf("merged response should contain content or tools for %s", name)
			}
			if strings.Contains(parts.Content, "event:") || strings.Contains(parts.Content, `"type":"response.output_text.delta"`) {
				t.Fatalf("merged content leaked raw SSE framing for %s", name)
			}
		})
	}
}

func TestHandleMergeDebugResponse_AcceptsGzipBody(t *testing.T) {
	t.Parallel()

	srv := newInMemoryServer(t)
	payload, err := json.Marshal(map[string]any{
		"resp_body": "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write(payload); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	req := newJSONRequestBytes(http.MethodPost, "/admin/debug-logs/merged-response", compressed.Bytes())
	req.Header.Set("Content-Encoding", "gzip")
	c, w := newTestContext(t, req)

	srv.HandleMergeDebugResponse(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	resp := mustParseAPIResponse[mergedResponseParts](t, w.Body.Bytes())
	if !resp.Success {
		t.Fatalf("success=%v, want true", resp.Success)
	}
	if resp.Data.Content != "hello" {
		t.Fatalf("content=%q, want hello", resp.Data.Content)
	}
}

func TestMergeResponseBody_FormatsConcatenatedCommandToolsAsBash(t *testing.T) {
	t.Parallel()

	raw := strings.Join([]string{
		`data: {"type":"response.function_call_arguments.delta","delta":"{\"cmd\":\"echo one\"}\n\n{\"cmd\":\"echo two\"}"}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	parts := mergeResponseBody(raw)
	if !strings.Contains(parts.Tools, "```bash\necho one\n```") {
		t.Fatalf("tools should render first command as bash, got:\n%s", parts.Tools)
	}
	if !strings.Contains(parts.Tools, "```bash\necho two\n```") {
		t.Fatalf("tools should render second command as bash, got:\n%s", parts.Tools)
	}
	if strings.Contains(parts.Tools, "```swift") || strings.Contains(parts.Tools, `"cmd"`) {
		t.Fatalf("tools leaked raw command JSON or wrong language, got:\n%s", parts.Tools)
	}
}

func TestMergeResponseBody_SeparatesCodexMessageItems(t *testing.T) {
	t.Parallel()

	raw := strings.Join([]string{
		`data: {"type":"response.output_text.delta","item_id":"msg_1","delta":"first"}`,
		``,
		`data: {"type":"response.output_text.delta","item_id":"msg_1","delta":" message"}`,
		``,
		`data: {"type":"response.output_text.done","item_id":"msg_1","text":"first message"}`,
		``,
		`data: {"type":"response.output_text.delta","item_id":"msg_2","delta":"second message"}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	parts := mergeResponseBody(raw)
	if parts.Content != "first message\n\n---\n\nsecond message" {
		t.Fatalf("content=%q", parts.Content)
	}
}

func TestMergeResponseBody_FormatsApplyPatchToolInputAsDiff(t *testing.T) {
	t.Parallel()

	raw := strings.Join([]string{
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"custom_tool_call","name":"apply_patch","input":""}}`,
		``,
		`data: {"type":"response.custom_tool_call_input.delta","output_index":1,"delta":"*** Begin Patch\n*** Add File: demo.go\n+package main\n*** End Patch"}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	parts := mergeResponseBody(raw)
	want := "### apply_patch\n\n```diff\n*** Begin Patch\n*** Add File: demo.go\n+package main\n*** End Patch\n```"
	if parts.Tools != want {
		t.Fatalf("tools=%q, want %q", parts.Tools, want)
	}
}

func TestMergeResponseBody_DeduplicatesCodexToolCallLifecycle(t *testing.T) {
	t.Parallel()

	arguments := `{"cmd":"git status --short"}`
	raw := strings.Join([]string{
		`data: {"type":"response.output_item.added","output_index":2,"item":{"id":"fc_1","type":"function_call","status":"in_progress","arguments":"","call_id":"call_1","name":"exec_command"}}`,
		``,
		`data: {"type":"response.function_call_arguments.delta","output_index":2,"item_id":"fc_1","delta":"` + strings.ReplaceAll(arguments, `"`, `\"`) + `"}`,
		``,
		`data: {"type":"response.function_call_arguments.done","output_index":2,"item_id":"fc_1","arguments":"` + strings.ReplaceAll(arguments, `"`, `\"`) + `"}`,
		``,
		`data: {"type":"response.output_item.done","output_index":2,"item":{"id":"fc_1","type":"function_call","status":"completed","arguments":"` + strings.ReplaceAll(arguments, `"`, `\"`) + `","call_id":"call_1","name":"exec_command"}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	parts := mergeResponseBody(raw)
	wantBlock := "```bash\ngit status --short\n```"
	if strings.Count(parts.Tools, wantBlock) != 1 {
		t.Fatalf("tool call should render once, got:\n%s", parts.Tools)
	}
	if strings.Count(parts.Tools, "### exec_command") != 1 {
		t.Fatalf("tool heading should render once, got:\n%s", parts.Tools)
	}
}

func TestMergeResponseBody_DeduplicatesDocs6ToolCalls(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join(testRepoRoot(t), "docs", "6.txt"))
	if err != nil {
		if os.IsNotExist(err) {
			t.Skip("docs/6.txt replay sample is not present")
		}
		t.Fatalf("read docs sample: %v", err)
	}

	parts := mergeResponseBody(string(raw))
	for _, cmd := range []string{
		"test -d .codegraph && printf yes || printf no",
		"git status --short",
		"git diff --name-only --diff-filter=U",
	} {
		block := "```bash\n" + cmd + "\n```"
		if strings.Count(parts.Tools, block) != 1 {
			t.Fatalf("command %q should render once, got tools:\n%s", cmd, parts.Tools)
		}
	}
}

func testRepoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repo root not found")
		}
		dir = parent
	}
}
