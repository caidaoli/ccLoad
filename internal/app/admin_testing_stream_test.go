package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/testutil"
	"ccLoad/internal/util"

	"github.com/bytedance/sonic"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type asyncResponseRecorder struct {
	header  http.Header
	writes  chan []byte
	flushes chan struct{}
	body    bytes.Buffer
	code    int
	mu      sync.Mutex
}

func newAsyncResponseRecorder() *asyncResponseRecorder {
	return &asyncResponseRecorder{
		header:  make(http.Header),
		writes:  make(chan []byte, 16),
		flushes: make(chan struct{}, 16),
	}
}

func (w *asyncResponseRecorder) Header() http.Header {
	return w.header
}

func (w *asyncResponseRecorder) WriteHeader(statusCode int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.code != 0 {
		return
	}
	w.code = statusCode
}

func (w *asyncResponseRecorder) Write(p []byte) (int, error) {
	chunk := append([]byte(nil), p...)

	w.mu.Lock()
	if w.code == 0 {
		w.code = http.StatusOK
	}
	_, _ = w.body.Write(p)
	w.mu.Unlock()

	select {
	case w.writes <- chunk:
	default:
	}
	return len(p), nil
}

func (w *asyncResponseRecorder) Flush() {
	select {
	case w.flushes <- struct{}{}:
	default:
	}
}

func (w *asyncResponseRecorder) BodyString() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.String()
}

type sseBlockResult struct {
	block string
	err   error
}

func readSSEBlock(reader *bufio.Reader) (string, error) {
	var block strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return block.String(), err
		}
		block.WriteString(line)
		if strings.TrimRight(line, "\r\n") == "" {
			return block.String(), nil
		}
	}
}

func readSSEBlockAsync(reader *bufio.Reader) <-chan sseBlockResult {
	ch := make(chan sseBlockResult, 1)
	go func() {
		block, err := readSSEBlock(reader)
		ch <- sseBlockResult{block: block, err: err}
	}()
	return ch
}

func TestStreamChatNativeEmitsOnlyFrontendDeltaEvents(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	c, w := newTestContext(t, req)
	upstream := strings.NewReader(strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"content":[]}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"I'm ready"}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n"))

	streamChatNative(c, upstream)

	body := w.Body.String()
	if !strings.Contains(body, `"delta":"I'm ready"`) {
		t.Fatalf("expected frontend delta event, got:\n%s", body)
	}
	if strings.Contains(body, "event: content_block_delta") || strings.Contains(body, `"type":"content_block_delta"`) {
		t.Fatalf("raw upstream event leaked into frontend chat stream:\n%s", body)
	}
}

func TestChatFrontendChunksFromSSEEventEmitsThinkingDelta(t *testing.T) {
	tests := []struct {
		name     string
		rawEvent string
		want     string
	}{
		{
			name: "anthropic thinking_delta",
			rawEvent: strings.Join([]string{
				"event: content_block_delta",
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"step one"}}`,
				"",
			}, "\n"),
			want: `"thinking_delta":"step one"`,
		},
		{
			name: "openai reasoning_content",
			rawEvent: strings.Join([]string{
				`data: {"choices":[{"index":0,"delta":{"reasoning_content":"reasoning"}}]}`,
				"",
			}, "\n"),
			want: `"thinking_delta":"reasoning"`,
		},
		{
			name: "gemini thought part",
			rawEvent: strings.Join([]string{
				`data: {"candidates":[{"content":{"parts":[{"text":"gemini thought","thought":true}]}}]}`,
				"",
			}, "\n"),
			want: `"thinking_delta":"gemini thought"`,
		},
		{
			name: "think tag",
			rawEvent: strings.Join([]string{
				`data: {"choices":[{"index":0,"delta":{"content":"<think>tagged thought</think>"}}]}`,
				"",
			}, "\n"),
			want: `"thinking_delta":"tagged thought"`,
		},
		{
			name: "codex reasoning summary delta",
			rawEvent: strings.Join([]string{
				"event: response.reasoning_summary_text.delta",
				`data: {"type":"response.reasoning_summary_text.delta","delta":"codex thought"}`,
				"",
			}, "\n"),
			want: `"thinking_delta":"codex thought"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunks := chatFrontendChunksFromSSEEvent([]byte(tt.rawEvent))
			body := string(bytes.Join(chunks, nil))
			if !strings.Contains(body, tt.want) {
				t.Fatalf("expected %s in chunks, got:\n%s", tt.want, body)
			}
			if strings.Contains(body, `"delta":"`) {
				t.Fatalf("thinking chunk must not be emitted as answer delta:\n%s", body)
			}
		})
	}
}

func TestStreamChatNativeParsesSplitThinkTags(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	c, w := newTestContext(t, req)
	upstream := strings.NewReader(strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"content":"<think>"}}]}`,
		"",
		`data: {"choices":[{"index":0,"delta":{"content":"split thought"}}]}`,
		"",
		`data: {"choices":[{"index":0,"delta":{"content":"</think>"}}]}`,
		"",
		`data: {"choices":[{"index":0,"delta":{"content":"answer"}}]}`,
		"",
		"",
	}, "\n"))

	streamChatNative(c, upstream)

	body := w.Body.String()
	if !strings.Contains(body, `"thinking_delta":"split thought"`) {
		t.Fatalf("expected split think tag content as thinking_delta, got:\n%s", body)
	}
	if !strings.Contains(body, `"delta":"answer"`) {
		t.Fatalf("expected answer delta after think tag, got:\n%s", body)
	}
	if strings.Contains(body, `<think>`) || strings.Contains(body, `</think>`) {
		t.Fatalf("think tags must not leak to frontend stream:\n%s", body)
	}
}

func TestHandleChannelChatWritesOnlyUpstreamEvents(t *testing.T) {
	upstreamHeaders := make(chan struct{})
	releaseBody := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() { close(releaseBody) })
	}
	defer release()

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		if err := sonic.Unmarshal(body, &payload); err != nil {
			t.Fatalf("unmarshal upstream request failed: %v; body=%s", err, body)
		}
		searchOptions, ok := payload["web_search_options"].(map[string]any)
		if !ok {
			t.Fatalf("OpenAI chat request missing web_search_options: %s", body)
		}
		if len(searchOptions) != 0 {
			t.Fatalf("OpenAI chat web_search_options = %#v, want empty object: %s", searchOptions, body)
		}
		if _, ok := payload["tools"]; ok {
			t.Fatalf("OpenAI chat search must use web_search_options, not tools: %s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		close(upstreamHeaders)
		<-releaseBody
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"late answer"}}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()

	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:         "chat-handler-stream-upstream-only",
		URLs:         model.ChannelURLs{{URL: upstream.URL}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "gpt-4o-mini"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	channelID := fmt.Sprintf("%d", created.ID)
	req := newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/chat", map[string]any{
		"model":           "gpt-4o-mini",
		"client_protocol": "openai",
		"stream":          true,
		"builtin_search":  true,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
	})
	w := newAsyncResponseRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	done := make(chan struct{})
	go func() {
		srv.HandleChannelChat(c)
		close(done)
	}()

	select {
	case <-upstreamHeaders:
	case <-time.After(time.Second):
		t.Fatal("upstream did not receive chat request")
	}

	select {
	case chunk := <-w.writes:
		t.Fatalf("chat handler wrote before upstream body: %q", string(chunk))
	case <-time.After(100 * time.Millisecond):
	}

	release()
	select {
	case chunk := <-w.writes:
		if got := string(chunk); !strings.Contains(got, `"delta":"late answer"`) {
			t.Fatalf("first client write after upstream body = %q, want upstream delta", got)
		}
	case <-time.After(time.Second):
		t.Fatal("chat handler did not forward upstream delta")
	}

	select {
	case <-w.flushes:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("chat handler forwarded upstream delta without flushing it")
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("chat handler did not finish after upstream body was released")
	}

	body := w.BodyString()
	if !strings.Contains(body, `"delta":"late answer"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("expected upstream answer, got:\n%s", body)
	}
}

func TestHandleChannelChatPersistsDetectionLogWithStreamStatusAndDebugData(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"logged answer"}}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	srv.configService.mu.Lock()
	srv.configService.cache["debug_log_enabled"] = &model.SystemSetting{Key: "debug_log_enabled", Value: "true"}
	srv.configService.mu.Unlock()

	ctx := context.Background()
	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:         "chat-log-stream-debug",
		URLs:         model.ChannelURLs{{URL: upstream.URL}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "gpt-4o-mini"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	started := time.Now()
	channelID := fmt.Sprintf("%d", created.ID)
	req := newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/chat", map[string]any{
		"model":           "gpt-4o-mini",
		"client_protocol": "openai",
		"stream":          true,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
	})
	c, w := newTestContext(t, req)
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelChat(c)

	if got := w.Body.String(); !strings.Contains(got, `"delta":"logged answer"`) {
		t.Fatalf("expected frontend answer, got:\n%s", got)
	}

	logs, err := srv.store.ListLogsRange(
		ctx,
		started.Add(-time.Second),
		time.Now().Add(time.Second),
		10,
		0,
		&model.LogFilter{LogSource: model.LogSourceDetection},
	)
	if err != nil {
		t.Fatalf("ListLogsRange failed: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("len(logs)=%d, want 1; logs=%+v", len(logs), logs)
	}
	entry := logs[0]
	if entry.LogSource != model.LogSourceManualChat {
		t.Fatalf("log_source=%q, want %q", entry.LogSource, model.LogSourceManualChat)
	}
	if entry.StatusCode != http.StatusOK {
		t.Fatalf("status_code=%d, want 200; entry=%+v", entry.StatusCode, entry)
	}
	if !entry.IsStreaming {
		t.Fatalf("is_streaming=false, want true; entry=%+v", entry)
	}
	if entry.Message != "ok" {
		t.Fatalf("message=%q, want ok", entry.Message)
	}

	debugLog, err := srv.store.GetDebugLogByLogID(ctx, entry.ID)
	if err != nil {
		t.Fatalf("GetDebugLogByLogID failed: %v", err)
	}
	if debugLog == nil {
		t.Fatal("debug log should be persisted for chat detection log")
	}
	if debugLog.RespStatus != http.StatusOK {
		t.Fatalf("debug resp status=%d, want 200", debugLog.RespStatus)
	}
	if !strings.Contains(string(debugLog.RespBody), "logged answer") {
		t.Fatalf("debug response body missing upstream stream: %q", string(debugLog.RespBody))
	}
}

func TestHandleChannelChatLogsThinkingEffortFromUpstreamRequestBody(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"output_config":{"effort":"high"}`) {
			t.Fatalf("upstream request missing output_config.effort high: %s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"answer"}}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()

	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:     "chat-thinking-from-upstream-request",
		URLs:     model.ChannelURLs{{URL: upstream.URL}},
		Priority: 1,
		ModelEntries: []model.ModelEntry{
			{Model: "gpt-4o-mini"},
		},
		Enabled: true,
		CustomRequestRules: &model.CustomRequestRules{
			Body: []model.CustomBodyRule{
				{Action: model.RuleActionOverride, Path: "output_config.effort", Value: json.RawMessage(`"high"`)},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	started := time.Now()
	channelID := fmt.Sprintf("%d", created.ID)
	req := newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/chat", map[string]any{
		"model":           "gpt-4o-mini",
		"client_protocol": "openai",
		"stream":          true,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
	})
	c, _ := newTestContext(t, req)
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelChat(c)

	logs, err := srv.store.ListLogsRange(
		ctx,
		started.Add(-time.Second),
		time.Now().Add(time.Second),
		10,
		0,
		&model.LogFilter{LogSource: model.LogSourceDetection},
	)
	if err != nil {
		t.Fatalf("ListLogsRange failed: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("len(logs)=%d, want 1; logs=%+v", len(logs), logs)
	}
	if got := logs[0].ThinkingEffort; got != "high" {
		t.Fatalf("thinking_effort=%q, want high", got)
	}
}

func TestHandleChannelChatStreamsUpstreamDeltaThroughZstdMiddleware(t *testing.T) {
	releaseSecond := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() { close(releaseSecond) })
	}
	defer release()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"first"}}]}`+"\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-releaseSecond
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":" second"}}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()

	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:         "chat-handler-zstd-stream",
		URLs:         model.ChannelURLs{{URL: upstream.URL}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "gpt-4o-mini"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	router := gin.New()
	admin := router.Group("/admin", ZstdMiddleware())
	admin.POST("/channels/:id/chat", srv.HandleChannelChat)
	app := httptest.NewServer(router)
	defer app.Close()

	payload, err := sonic.Marshal(map[string]any{
		"model":           "gpt-4o-mini",
		"client_protocol": "openai",
		"stream":          true,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
	})
	if err != nil {
		t.Fatalf("marshal request failed: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, app.URL+"/admin/channels/"+fmt.Sprintf("%d", created.ID)+"/chat", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "zstd")

	resp, err := app.Client().Do(req)
	if err != nil {
		t.Fatalf("chat request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding=%q, want empty for chat SSE", got)
	}

	reader := bufio.NewReader(resp.Body)
	first := readSSEBlockAsync(reader)
	select {
	case result := <-first:
		if result.err != nil {
			t.Fatalf("read first SSE block failed: %v", result.err)
		}
		if !strings.Contains(result.block, `"delta":"first"`) {
			t.Fatalf("first SSE block = %q, want first upstream delta", result.block)
		}
	case <-time.After(time.Second):
		t.Fatal("first upstream delta was not forwarded before stream completion")
	}

	second := readSSEBlockAsync(reader)
	select {
	case result := <-second:
		t.Fatalf("received second SSE block before upstream released it: %#v", result)
	case <-time.After(100 * time.Millisecond):
	}

	release()
	select {
	case result := <-second:
		if result.err != nil {
			t.Fatalf("read second SSE block failed: %v", result.err)
		}
		if !strings.Contains(result.block, `"delta":" second"`) {
			t.Fatalf("second SSE block = %q, want second upstream delta", result.block)
		}
	case <-time.After(time.Second):
		t.Fatal("second upstream delta was not forwarded after release")
	}
}

func TestStreamChatWithURLHandlesJSONResponseAsFrontendSSE(t *testing.T) {
	var upstreamBody string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		upstreamBody = string(body)
		if got := r.Header.Get("Accept"); strings.Contains(upstreamBody, `"stream":true`) && got != "text/event-stream" {
			t.Errorf("stream chat request Accept=%q, want text/event-stream", got)
		} else if strings.Contains(upstreamBody, `"stream":false`) && got != "" {
			t.Errorf("non-stream chat request must not ask for SSE, Accept=%q", got)
		}

		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"plain answer"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	cfg := &model.Config{
		ID:           1,
		Name:         "openai-non-stream",
		URLs:         model.ChannelURLs{{URL: upstream.URL}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "gpt-4o-mini"}},
		Enabled:      true,
	}
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			testReq := &testutil.TestChannelRequest{
				Model:          "gpt-4o-mini",
				ClientProtocol: "openai",
				Stream:         stream,
				Messages: []testutil.ChatMessage{
					{Role: "user", Content: "hi"},
				},
			}

			c, w := newTestContext(t, httptest.NewRequest(http.MethodPost, "/admin/channels/1/chat", nil))
			attempt := srv.streamChatWithURLForProtocol(c, cfg, "sk-test", testReq, "openai", "openai", upstream.URL, testReq.Model)
			if !attempt.handled {
				t.Fatal("expected JSON chat response to be handled without URL fallback")
			}

			wantStreamField := fmt.Sprintf(`"stream":%t`, stream)
			if !strings.Contains(upstreamBody, wantStreamField) {
				t.Fatalf("expected upstream request %s, got:\n%s", wantStreamField, upstreamBody)
			}
			body := w.Body.String()
			if !strings.Contains(body, `"delta":"plain answer"`) || !strings.Contains(body, "data: [DONE]") {
				t.Fatalf("expected frontend delta and DONE events, got:\n%s", body)
			}
			if strings.Contains(body, `"error"`) {
				t.Fatalf("JSON success must not be emitted as error, got:\n%s", body)
			}
		})
	}
}

func TestHandleChannelChatWritesErrorWhenAllURLsFailBeforeResponse(t *testing.T) {
	srv := newInMemoryServer(t)
	ctx := context.Background()

	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:         "chat-network-error",
		URLs:         model.ChannelURLs{{URL: "http://missing-chat-upstream.invalid"}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "gpt-4o-mini"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	channelID := fmt.Sprintf("%d", created.ID)
	req := newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/chat", map[string]any{
		"model":           "gpt-4o-mini",
		"client_protocol": "openai",
		"stream":          true,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
	})
	c, w := newTestContext(t, req)
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelChat(c)

	body := w.Body.String()
	if !strings.Contains(body, `"error"`) {
		t.Fatalf("expected chat error event when upstream request fails, got:\n%s", body)
	}
	if !strings.Contains(body, "网络请求失败") {
		t.Fatalf("expected network failure message, got:\n%s", body)
	}
}

func TestHandleChannelChatPersistsLogOnHTTPError(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"type":"permission_error","message":"forbidden"}}`)
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()

	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:         "chat-http-error-log",
		URLs:         model.ChannelURLs{{URL: upstream.URL}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "gpt-4o-mini"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	started := time.Now()
	channelID := fmt.Sprintf("%d", created.ID)
	req := newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/chat", map[string]any{
		"model":           "gpt-4o-mini",
		"client_protocol": "openai",
		"stream":          true,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
	})
	c, _ := newTestContext(t, req)
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelChat(c)

	logs, err := srv.store.ListLogsRange(
		ctx,
		started.Add(-time.Second),
		time.Now().Add(time.Second),
		10,
		0,
		&model.LogFilter{LogSource: model.LogSourceDetection},
	)
	if err != nil {
		t.Fatalf("ListLogsRange failed: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("len(logs)=%d, want 1", len(logs))
	}
	entry := logs[0]
	if entry.StatusCode != http.StatusForbidden {
		t.Fatalf("status_code=%d, want 403", entry.StatusCode)
	}
	if entry.LogSource != model.LogSourceManualChat {
		t.Fatalf("log_source=%q, want %q", entry.LogSource, model.LogSourceManualChat)
	}
}

func TestHandleChannelChatDoesNotFallbackAfterModelScopedHTTPError(t *testing.T) {
	failCalls := 0
	failUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":{"type":"server_error","message":"bad gateway"}}`)
	}))
	defer failUpstream.Close()

	okCalls := 0
	okUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okCalls++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"fallback answer"}}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer okUpstream.Close()

	srv := newInMemoryServer(t)
	ctx := context.Background()

	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:         "chat-http-fallback",
		URLs:         channelURLsForTest(failUpstream.URL, okUpstream.URL),
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "gpt-4o-mini"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}
	srv.urlSelector.RecordLatency(created.ID, failUpstream.URL, 10*time.Millisecond)
	srv.urlSelector.RecordLatency(created.ID, okUpstream.URL, 100*time.Millisecond)
	srv.urlSelector.CooldownURL(created.ID, okUpstream.URL)

	channelID := fmt.Sprintf("%d", created.ID)
	req := newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/chat", map[string]any{
		"model":           "gpt-4o-mini",
		"client_protocol": "openai",
		"stream":          true,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
	})
	c, w := newTestContext(t, req)
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelChat(c)

	if failCalls != 1 || okCalls != 0 {
		t.Fatalf("expected only the model-scoped failure call, failCalls=%d okCalls=%d", failCalls, okCalls)
	}
	body := w.Body.String()
	if strings.Contains(body, `"delta":"fallback answer"`) {
		t.Fatalf("model-scoped failure must not retry another URL, got:\n%s", body)
	}
	if !strings.Contains(body, `"error"`) {
		t.Fatalf("expected SSE error event, got:\n%s", body)
	}
}

func TestHandleChannelChatAutoFallsBackToChannelProtocolOnMissingEndpoint(t *testing.T) {
	var paths []string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"endpoint not found"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","status":"completed","model":"test-model","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"fallback answer"}]}],"usage":{"input_tokens":1,"output_tokens":2}}`)
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	ctx := context.Background()
	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name: "chat-auto", URLs: model.ChannelURLs{{URL: upstream.URL}}, ProtocolTransformMode: model.ProtocolTransformModeAuto,
		ModelEntries: []model.ModelEntry{{Model: "test-model"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch: %v", err)
	}

	channelID := fmt.Sprintf("%d", created.ID)
	req := newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/chat", map[string]any{
		"model": "test-model", "client_protocol": "anthropic", "stream": false,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	c, w := newTestContext(t, req)
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelChat(c)

	if !slices.Equal(paths, []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"}) {
		t.Fatalf("paths=%v, want Anthropic, OpenAI, then Codex", paths)
	}
	if !strings.Contains(w.Body.String(), `"delta":"fallback answer"`) || !strings.Contains(w.Body.String(), "data: [DONE]") {
		t.Fatalf("unexpected frontend SSE: %s", w.Body.String())
	}
}

func TestHandleChannelChatDisablesServerWriteTimeoutForDelayedStreamBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		time.Sleep(80 * time.Millisecond)
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"after wait"}}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()

	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:                  "chat-write-timeout",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		Priority:              1,
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-4o-mini"}},
		Enabled:               true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	router := gin.New()
	router.POST("/admin/channels/:id/chat", srv.HandleChannelChat)
	app := httptest.NewUnstartedServer(router)
	app.Config.WriteTimeout = 30 * time.Millisecond
	app.Start()
	defer app.Close()

	payload, err := sonic.Marshal(map[string]any{
		"model":           "gpt-4o-mini",
		"client_protocol": "openai",
		"stream":          true,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
	})
	if err != nil {
		t.Fatalf("marshal request failed: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, app.URL+"/admin/channels/"+fmt.Sprintf("%d", created.ID)+"/chat", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := app.Client()
	client.Timeout = 2 * time.Second
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("chat request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read chat response failed: %v", err)
	}
	if !strings.Contains(string(body), `"delta":"after wait"`) {
		t.Fatalf("expected delayed stream body to survive WriteTimeout, got:\n%s", string(body))
	}
}

func TestStreamChatWithURLKeepsFirstContentTimeoutUntilValidSSEEvent(t *testing.T) {
	releaseBody := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() { close(releaseBody) })
	}
	defer release()

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-releaseBody
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"too late"}}]}`+"\n\n")
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.firstByteTimeout = 25 * time.Millisecond
	cfg := &model.Config{
		ID:           77,
		Name:         "chat-first-content-timeout",
		URLs:         model.ChannelURLs{{URL: upstream.URL}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "gpt-4o-mini"}},
		Enabled:      true,
	}
	testReq := &testutil.TestChannelRequest{
		Model:          "gpt-4o-mini",
		ClientProtocol: "openai",
		Stream:         true,
		Messages: []testutil.ChatMessage{
			{Role: "user", Content: "hi"},
		},
	}

	reqCtx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	c, w := newTestContext(t, httptest.NewRequest(http.MethodPost, "/admin/channels/77/chat", nil).WithContext(reqCtx))

	done := make(chan chatURLAttemptResult, 1)
	go func() {
		done <- srv.streamChatWithURLForProtocol(c, cfg, "sk-test", testReq, "openai", "openai", upstream.URL, testReq.Model)
	}()

	select {
	case attempt := <-done:
		if !attempt.handled {
			t.Fatal("expected timeout to be handled as final chat error")
		}
	case <-time.After(120 * time.Millisecond):
		t.Fatal("streamChatWithURL did not stop at first content timeout")
	}

	body := w.Body.String()
	if !strings.Contains(body, `"error"`) {
		t.Fatalf("expected first content timeout error event, got:\n%s", body)
	}
}

func TestStreamChatWithURLDoesNotTreatDoneEventAsFirstContent(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	cfg := &model.Config{
		ID:           78,
		Name:         "chat-done-is-not-content",
		URLs:         model.ChannelURLs{{URL: upstream.URL}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "gpt-4o-mini"}},
		Enabled:      true,
	}
	testReq := &testutil.TestChannelRequest{
		Model:          "gpt-4o-mini",
		ClientProtocol: "openai",
		Stream:         true,
		Messages: []testutil.ChatMessage{
			{Role: "user", Content: "hi"},
		},
	}

	c, w := newTestContext(t, httptest.NewRequest(http.MethodPost, "/admin/channels/78/chat", nil))
	attempt := srv.streamChatWithURLForProtocol(c, cfg, "sk-test", testReq, "openai", "openai", upstream.URL, testReq.Model)
	if !attempt.handled {
		t.Fatal("expected stream attempt to be handled")
	}
	body := w.Body.String()
	if !strings.Contains(body, `"duration_ms"`) {
		t.Fatalf("expected summary duration, got:\n%s", body)
	}
	if strings.Contains(body, `"first_byte_ms"`) {
		t.Fatalf("DONE control event must not produce first_byte_ms, got:\n%s", body)
	}
}

func TestHandleChannelChatDoesNotWriteSyntheticOneMillisecondURLLatency(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"ok"}}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer upstream.Close()

	unusedUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unused upstream should not be called")
	}))
	defer unusedUpstream.Close()

	srv := newInMemoryServer(t)
	ctx := context.Background()

	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:                  "chat-selector-latency",
		URLs:                  channelURLsForTest(upstream.URL, unusedUpstream.URL),
		Priority:              1,
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-4o-mini"}},
		Enabled:               true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}
	srv.urlSelector.RecordLatency(created.ID, upstream.URL, 80*time.Millisecond)
	srv.urlSelector.RecordLatency(created.ID, unusedUpstream.URL, 800*time.Millisecond)
	srv.urlSelector.CooldownURL(created.ID, unusedUpstream.URL)

	channelID := fmt.Sprintf("%d", created.ID)
	req := newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/chat", map[string]any{
		"model":           "gpt-4o-mini",
		"client_protocol": "openai",
		"stream":          true,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
	})
	c, w := newTestContext(t, req)
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelChat(c)

	if !strings.Contains(w.Body.String(), `"delta":"ok"`) {
		t.Fatalf("expected successful chat response, got:\n%s", w.Body.String())
	}
	stats := srv.urlSelector.GetURLStats(created.ID, []string{upstream.URL})
	if len(stats) != 1 {
		t.Fatalf("expected one URL stat, got %d", len(stats))
	}
	if stats[0].LatencyMs < 70 {
		t.Fatalf("chat handler must not overwrite URL latency with synthetic 1ms, got %.3fms", stats[0].LatencyMs)
	}
}

func TestChatRequestErrorResultClassifiesLimitAndNetworkFailures(t *testing.T) {
	start := time.Now()
	req := &testutil.TestChannelRequest{Stream: true}
	timeout := &channelTestTimeout{}

	tests := []struct {
		name        string
		err         error
		wantStatus  int
		wantMessage string
		wantKey     string
	}{
		{
			name:        "network",
			err:         errors.New("dial tcp refused"),
			wantStatus:  0,
			wantMessage: "网络请求失败",
		},
		{
			name:        "rpm",
			err:         ErrChannelRPMExceeded,
			wantStatus:  http.StatusTooManyRequests,
			wantMessage: "渠道已达到RPM限制",
			wantKey:     "rpm_limited",
		},
		{
			name:        "concurrency",
			err:         &channelConcurrencyExceededError{active: 1, limit: 1},
			wantStatus:  http.StatusTooManyRequests,
			wantMessage: "渠道已达到并发限制",
			wantKey:     "concurrency_limited",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := chatRequestErrorResult(start, req, timeout, tt.err)
			if tt.wantStatus > 0 {
				if statusCode, _ := getResultInt(result["status_code"]); statusCode != tt.wantStatus {
					t.Fatalf("status_code=%d, want %d, result=%+v", statusCode, tt.wantStatus, result)
				}
			} else if _, ok := result["status_code"]; ok {
				t.Fatalf("network error should not invent status_code, result=%+v", result)
			}
			if errMsg, _ := result["error"].(string); !strings.Contains(errMsg, tt.wantMessage) {
				t.Fatalf("error=%q, want containing %q", errMsg, tt.wantMessage)
			}
			if tt.wantKey != "" {
				if got, _ := result[tt.wantKey].(bool); !got {
					t.Fatalf("expected %s=true, result=%+v", tt.wantKey, result)
				}
			}
		})
	}

	timeout.firstStreamContentTimedOut.Store(true)
	result := chatRequestErrorResult(start, req, timeout, context.Canceled)
	if statusCode, _ := getResultInt(result["status_code"]); statusCode != util.StatusFirstByteTimeout {
		t.Fatalf("status_code=%d, want %d, result=%+v", statusCode, util.StatusFirstByteTimeout, result)
	}

	timeout.firstStreamContentTimedOut.Store(false)
	timeout.streamTimedOut.Store(true)
	result = chatRequestErrorResult(start, req, timeout, context.Canceled)
	if statusCode, _ := getResultInt(result["status_code"]); statusCode != util.StatusStreamIncomplete {
		t.Fatalf("status_code=%d, want %d, result=%+v", statusCode, util.StatusStreamIncomplete, result)
	}
}

func TestHandleChannelChatRespectsNonStreamFlag(t *testing.T) {
	var upstreamBody string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"handler answer"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()

	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:                  "chat-handler-non-stream",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		Priority:              1,
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-4o-mini"}},
		Enabled:               true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	channelID := fmt.Sprintf("%d", created.ID)
	req := newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/chat", map[string]any{
		"model":           "gpt-4o-mini",
		"client_protocol": "openai",
		"stream":          false,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
	})
	c, w := newTestContext(t, req)
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelChat(c)

	if !strings.Contains(upstreamBody, `"stream":false`) {
		t.Fatalf("handler must preserve stream=false, upstream body:\n%s", upstreamBody)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"delta":"handler answer"`) {
		t.Fatalf("expected frontend delta event, got:\n%s", body)
	}
}

func TestHandleChannelChatPreservesCodexMessages(t *testing.T) {
	var upstreamBody []byte
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		upstreamBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"real answer"}

event: response.completed
data: {"type":"response.completed"}

`)
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()

	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:         "chat-handler-codex",
		URLs:         model.ChannelURLs{{URL: upstream.URL}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "gpt-5.5"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	channelID := fmt.Sprintf("%d", created.ID)
	req := newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/chat", map[string]any{
		"model":           "gpt-5.5",
		"client_protocol": "codex",
		"stream":          true,
		"messages": []map[string]string{
			{"role": "user", "content": "macbook m5有几款"},
			{"role": "assistant", "content": "Test received. How can I help?"},
			{"role": "user", "content": "联网搜索一下"},
		},
	})
	c, w := newTestContext(t, req)
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelChat(c)

	var payload map[string]any
	if err := sonic.Unmarshal(upstreamBody, &payload); err != nil {
		t.Fatalf("unmarshal upstream body failed: %v; body=%s", err, upstreamBody)
	}
	input, ok := payload["input"].([]any)
	if !ok || len(input) != 3 {
		t.Fatalf("codex chat input length = %d, want 3; body=%s", len(input), upstreamBody)
	}
	bodyText := string(upstreamBody)
	for _, want := range []string{
		`"text":"macbook m5有几款"`,
		`"type":"output_text"`,
		`"text":"Test received. How can I help?"`,
		`"text":"联网搜索一下"`,
	} {
		if !strings.Contains(bodyText, want) {
			t.Fatalf("codex upstream body missing %s:\n%s", want, bodyText)
		}
	}
	if strings.Contains(bodyText, `"text":"test"`) {
		t.Fatalf("codex chat must not fall back to default test prompt:\n%s", bodyText)
	}
	if got := w.Body.String(); !strings.Contains(got, `"delta":"real answer"`) || !strings.Contains(got, "data: [DONE]") {
		t.Fatalf("expected frontend SSE answer and DONE, got:\n%s", got)
	}
}

func TestHandleChannelChat_CodexOAuthWithoutAPIKeyOrSSEContentType(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer at-admin-test" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "account-admin-test" {
			t.Errorf("ChatGPT-Account-ID = %q", got)
		}
		if r.Header.Get("X-Api-Key") != "" || r.Header.Get("Originator") != "codex-tui" ||
			(r.Header.Get("Session_id") == "" && r.Header.Get("Session-Id") == "") {
			t.Errorf("incomplete Codex OAuth stream headers: %v", r.Header)
		}
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"status\":\"in_progress\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"response\":{\"status\":\"in_progress\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"oauth answer\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	}))

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	created := createCodexOAuthChannelForAdminTest(t, srv, upstream.URL+"/backend-api/codex/responses")
	channelID := fmt.Sprintf("%d", created.ID)
	req := newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/chat", map[string]any{
		"model":           "gpt-5.6-sol",
		"client_protocol": "codex",
		"stream":          true,
	})
	c, w := newTestContext(t, req)
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelChat(c)

	if got := w.Body.String(); !strings.Contains(got, `"delta":"oauth answer"`) || !strings.Contains(got, "data: [DONE]") || strings.Contains(got, `"error"`) {
		t.Fatalf("Codex OAuth chat failed: %s", got)
	}
}

func TestHandleChannelChat_CodexOAuthTransformsOpenAIClientProtocol(t *testing.T) {
	var upstreamBody []byte
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/responses" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer at-admin-test" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "account-admin-test" {
			t.Errorf("ChatGPT-Account-ID = %q", got)
		}
		upstreamBody, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"status\":\"in_progress\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"openai to codex answer\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	}))

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	created := createCodexOAuthChannelForAdminTest(t, srv, upstream.URL+"/backend-api/codex/responses")
	updated := created.Clone()
	updated.ProtocolTransformMode = model.ProtocolTransformModeLocal
	created, err := srv.store.UpdateConfig(context.Background(), created.ID, updated)
	if err != nil {
		t.Fatalf("enable local protocol transform: %v", err)
	}
	channelID := fmt.Sprintf("%d", created.ID)
	req := newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/chat", map[string]any{
		"model":           "gpt-5.6-sol",
		"client_protocol": "openai",
		"stream":          true,
		"thinking_effort": "none",
		"builtin_search":  true,
		"messages": []map[string]string{
			{"role": "user", "content": "which header carries the API key?"},
		},
	})
	c, response := newTestContext(t, req)
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelChat(c)

	if len(upstreamBody) == 0 {
		t.Fatalf("OpenAI client request did not reach Codex upstream: %s", response.Body.String())
	}
	var payload map[string]any
	if err := sonic.Unmarshal(upstreamBody, &payload); err != nil {
		t.Fatalf("decode Codex upstream body: %v; body=%s", err, upstreamBody)
	}
	if _, exists := payload["messages"]; exists {
		t.Fatalf("OpenAI messages leaked to Codex upstream: %s", upstreamBody)
	}
	if input, ok := payload["input"].([]any); !ok || len(input) != 1 {
		t.Fatalf("Codex input = %#v; body=%s", payload["input"], upstreamBody)
	}
	if stream, ok := payload["stream"].(bool); !ok || !stream {
		t.Fatalf("Codex stream = %#v; body=%s", payload["stream"], upstreamBody)
	}
	if store, ok := payload["store"].(bool); !ok || store {
		t.Fatalf("Codex store = %#v; body=%s", payload["store"], upstreamBody)
	}
	if reasoning, exists := payload["reasoning"]; exists {
		t.Fatalf("Codex reasoning = %#v; want request thinking_effort=none to remove template reasoning; body=%s", reasoning, upstreamBody)
	}
	textConfig, _ := payload["text"].(map[string]any)
	if got, _ := textConfig["verbosity"].(string); got != "low" {
		t.Fatalf("Codex text.verbosity = %q, want low; body=%s", got, upstreamBody)
	}
	if got, _ := payload["prompt_cache_key"].(string); got == "" {
		t.Fatalf("Codex prompt_cache_key is missing; body=%s", upstreamBody)
	}
	if got, _ := payload["tool_choice"].(string); got != "auto" {
		t.Fatalf("Codex tool_choice = %q, want auto; body=%s", got, upstreamBody)
	}
	clientMetadata, _ := payload["client_metadata"].(map[string]any)
	if got, _ := clientMetadata["x-codex-installation-id"].(string); got == "" {
		t.Fatalf("Codex installation id is missing; body=%s", upstreamBody)
	}
	tools, _ := payload["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("Codex tools = %#v; want web_search; body=%s", tools, upstreamBody)
	}
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "web_search" {
		t.Fatalf("Codex tool = %#v; want web_search; body=%s", tool, upstreamBody)
	}
	got := response.Body.String()
	if !strings.Contains(got, `"delta":"openai to codex answer"`) || !strings.Contains(got, "data: [DONE]") || strings.Contains(got, `"error"`) {
		t.Fatalf("OpenAI to Codex chat response failed: %s", got)
	}
}

func TestHandleChannelChat_CodexOAuthKeepsConversationCacheIdentity(t *testing.T) {
	type capturedRequest struct {
		body      []byte
		sessionID string
		threadID  string
	}
	var captured []capturedRequest
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		captured = append(captured, capturedRequest{
			body:      body,
			sessionID: r.Header.Get("Session-Id"),
			threadID:  r.Header.Get("Thread-Id"),
		})
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"status\":\"in_progress\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"answer\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	}))

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	created := createCodexOAuthChannelForAdminTest(t, srv, upstream.URL+"/backend-api/codex/responses")
	updated := created.Clone()
	updated.ProtocolTransformMode = model.ProtocolTransformModeLocal
	created, err := srv.store.UpdateConfig(context.Background(), created.ID, updated)
	if err != nil {
		t.Fatalf("enable local protocol transform: %v", err)
	}
	channelID := fmt.Sprintf("%d", created.ID)

	send := func(sessionID string, messages []map[string]string) {
		t.Helper()
		req := newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/chat", map[string]any{
			"model":           "gpt-5.6-sol",
			"client_protocol": "openai",
			"stream":          true,
			"session_id":      sessionID,
			"messages":        messages,
		})
		c, response := newTestContext(t, req)
		c.Params = gin.Params{{Key: "id", Value: channelID}}
		srv.HandleChannelChat(c)
		if got := response.Body.String(); !strings.Contains(got, `"delta":"answer"`) || strings.Contains(got, `"error"`) {
			t.Fatalf("chat response failed: %s", got)
		}
	}

	send("browser-conversation", []map[string]string{
		{"role": "user", "content": "first question"},
	})
	send("browser-conversation", []map[string]string{
		{"role": "user", "content": "first question"},
		{"role": "assistant", "content": "first answer"},
		{"role": "user", "content": "second question"},
	})
	send("different-conversation", []map[string]string{
		{"role": "user", "content": "first question"},
	})

	if len(captured) != 3 {
		t.Fatalf("captured requests = %d, want 3", len(captured))
	}
	firstKey := gjson.GetBytes(captured[0].body, "prompt_cache_key").String()
	secondKey := gjson.GetBytes(captured[1].body, "prompt_cache_key").String()
	thirdKey := gjson.GetBytes(captured[2].body, "prompt_cache_key").String()
	if firstKey == "" || firstKey != secondKey {
		t.Fatalf("same conversation prompt_cache_key = %q, %q", firstKey, secondKey)
	}
	if thirdKey == "" || thirdKey == firstKey {
		t.Fatalf("different conversation prompt_cache_key = %q, want different from %q", thirdKey, firstKey)
	}
	if captured[0].sessionID == "" || captured[0].sessionID != captured[1].sessionID {
		t.Fatalf("same conversation headers changed: first=%+v second=%+v", captured[0], captured[1])
	}
	if captured[2].sessionID == captured[0].sessionID {
		t.Fatalf("different conversation reused headers: first=%+v third=%+v", captured[0], captured[2])
	}
	if captured[0].threadID != "" || captured[1].threadID != "" || captured[2].threadID != "" {
		t.Fatalf("Thread-Id is not part of the Codex upstream HTTP contract: %+v", captured)
	}
	firstInstallationID := gjson.GetBytes(captured[0].body, "client_metadata.x-codex-installation-id").String()
	secondInstallationID := gjson.GetBytes(captured[1].body, "client_metadata.x-codex-installation-id").String()
	thirdInstallationID := gjson.GetBytes(captured[2].body, "client_metadata.x-codex-installation-id").String()
	if firstInstallationID == "" || firstInstallationID != secondInstallationID {
		t.Fatalf("same conversation installation id = %q, %q", firstInstallationID, secondInstallationID)
	}
	if thirdInstallationID == "" || thirdInstallationID == firstInstallationID {
		t.Fatalf("different conversation installation id = %q, want different from %q", thirdInstallationID, firstInstallationID)
	}
	firstInput := gjson.GetBytes(captured[0].body, "input.0").Raw
	secondInput := gjson.GetBytes(captured[1].body, "input.0").Raw
	if firstInput == "" || firstInput != secondInput {
		t.Fatalf("existing message JSON prefix changed:\nfirst:  %s\nsecond: %s", firstInput, secondInput)
	}
}

func TestTestChannelAPI_StreamIncludesUsageAndCost(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		time.Sleep(20 * time.Millisecond)

		// 模拟Claude风格SSE：usage在message_start/message_delta给出，内容在content_block_delta给出
		_, _ = io.WriteString(w, "event: message_start\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":0,\"cache_read_input_tokens\":5,\"cache_creation\":{\"ephemeral_5m_input_tokens\":3,\"ephemeral_1h_input_tokens\":2}}}}\n\n")

		_, _ = io.WriteString(w, "event: content_block_delta\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hi\"}}\n\n")

		_, _ = io.WriteString(w, "event: message_delta\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":10,\"output_tokens\":20,\"cache_read_input_tokens\":5,\"cache_creation_input_tokens\":5,\"cache_creation\":{\"ephemeral_5m_input_tokens\":3,\"ephemeral_1h_input_tokens\":2}}}\n\n")
		time.Sleep(20 * time.Millisecond)

		_, _ = io.WriteString(w, "event: message_stop\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()

	cfg := &model.Config{
		ID:           1,
		Name:         "test-channel",
		URLs:         model.ChannelURLs{{URL: upstream.URL}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "claude-3-haiku", RedirectModel: ""}},
		Enabled:      true,
	}

	req := &testutil.TestChannelRequest{
		Model:          "claude-3-haiku",
		ClientProtocol: "anthropic",
		Stream:         true,
		Content:        "hi",
	}

	result := srv.testChannelAPI(context.Background(), cfg, "sk-test", req)

	if success, _ := result["success"].(bool); !success {
		t.Fatalf("expected success, got: %#v", result)
	}

	if result["response_text"] != "hi" {
		t.Fatalf("expected response_text=hi, got: %#v", result["response_text"])
	}

	apiResp, ok := result["api_response"].(map[string]any)
	if !ok || apiResp == nil {
		t.Fatalf("expected api_response, got: %#v", result["api_response"])
	}

	usage, ok := apiResp["usage"].(map[string]any)
	if !ok || usage == nil {
		t.Fatalf("expected api_response.usage, got: %#v", apiResp["usage"])
	}

	if usage["input_tokens"] == nil || usage["output_tokens"] == nil {
		t.Fatalf("expected usage tokens, got: %#v", usage)
	}

	cost, ok := result["cost_usd"].(float64)
	if !ok {
		t.Fatalf("expected cost_usd(float64), got: %#v", result["cost_usd"])
	}
	if cost <= 0 {
		t.Fatalf("expected cost_usd > 0, got: %v", cost)
	}

	firstByteDurationMs, ok := result["first_byte_duration_ms"].(int64)
	if !ok || firstByteDurationMs <= 0 {
		t.Fatalf("expected first_byte_duration_ms(int64)>0, got: %#v", result["first_byte_duration_ms"])
	}

	totalDurationMs, ok := result["duration_ms"].(int64)
	if !ok || totalDurationMs <= 0 {
		t.Fatalf("expected duration_ms(int64)>0, got: %#v", result["duration_ms"])
	}
	if totalDurationMs < firstByteDurationMs {
		t.Fatalf("expected duration_ms>=first_byte_duration_ms, got %d < %d", totalDurationMs, firstByteDurationMs)
	}
}

func TestTestChannelAPI_GeminiStreamIncludesTTFBAndText(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Gemini 流式端点: /v1beta/models/{model}:streamGenerateContent
		if r.URL.Path != "/v1beta/models/gemini-2.5-flash-lite:streamGenerateContent" {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		time.Sleep(20 * time.Millisecond)

		// Gemini SSE: candidates[0].content.parts[0].text, usage在usageMetadata中
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hello\"}],\"role\":\"model\"}}],\"modelVersion\":\"gemini-2.5-flash-lite\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\" world\"}],\"role\":\"model\"},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":10,\"candidatesTokenCount\":20,\"totalTokenCount\":30},\"modelVersion\":\"gemini-2.5-flash-lite\"}\n\n")
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()

	cfg := &model.Config{
		ID:           1,
		Name:         "gemini-channel",
		URLs:         model.ChannelURLs{{URL: upstream.URL}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "gemini-2.5-flash-lite"}},
		Enabled:      true,
	}

	req := &testutil.TestChannelRequest{
		Model:          "gemini-2.5-flash-lite",
		ClientProtocol: "gemini",
		Stream:         true,
		Content:        "hi",
	}

	result := srv.testChannelAPI(context.Background(), cfg, "test-key", req)

	if success, _ := result["success"].(bool); !success {
		t.Fatalf("expected success, got: %#v", result)
	}

	// 验证文本提取
	if result["response_text"] != "Hello world" {
		t.Fatalf("expected response_text='Hello world', got: %#v", result["response_text"])
	}

	// 验证 TTFB
	firstByteDurationMs, ok := result["first_byte_duration_ms"].(int64)
	if !ok || firstByteDurationMs <= 0 {
		t.Fatalf("expected first_byte_duration_ms(int64)>0, got: %#v", result["first_byte_duration_ms"])
	}

	// 验证总耗时
	totalDurationMs, ok := result["duration_ms"].(int64)
	if !ok || totalDurationMs <= 0 {
		t.Fatalf("expected duration_ms(int64)>0, got: %#v", result["duration_ms"])
	}
}
