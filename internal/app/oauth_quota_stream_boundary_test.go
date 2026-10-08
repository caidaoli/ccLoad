package app

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ccLoad/internal/model"
)

func TestOAuthQuotaLargeSSEOutput(t *testing.T) {
	image := strings.Repeat("YWJj", maxSSEEventSize/2)
	item, err := json.Marshal(map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "image_generation_call", "result": image}})
	if err != nil {
		t.Fatal(err)
	}
	stream := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"start\"}\n\n" + "data: " + string(item) + "\n\n" + "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_large\",\"output\":[],\"usage\":{\"input_tokens\":12,\"output_tokens\":3}}}\n\n"
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(&quotaChunkReader{Reader: strings.NewReader(stream)})}
	rec := httptest.NewRecorder()
	result, _, err := (&Server{}).handleResponse(&requestContext{ctx: context.Background(), startTime: time.Now(), isStreaming: true}, resp, rec, "codex", &model.Config{AuthType: model.AuthTypeCodexOAuth}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var found, completed bool
	for _, frame := range strings.Split(rec.Body.String(), "\n\n") {
		_, data := parseSSEEventChunk([]byte(frame))
		if len(data) == 0 {
			continue
		}
		var event struct {
			Type string `json:"type"`
			Item struct {
				Result string `json:"result"`
			} `json:"item"`
		}
		if err := json.Unmarshal(data, &event); err != nil {
			t.Fatal(err)
		}
		if event.Type == "response.output_item.done" {
			found = true
			if event.Item.Result != image {
				t.Fatal("large model output truncated")
			}
		}
		completed = completed || event.Type == "response.completed"
	}
	if !found || !completed || result.InputTokens != 12 || result.OutputTokens != 3 {
		t.Fatalf("incomplete output: found=%v completed=%v usage=%+v", found, completed, result)
	}
}

type quotaChunkReader struct{ *strings.Reader }

func (r *quotaChunkReader) Read(p []byte) (int, error) {
	if len(p) > 4096 {
		p = p[:4096]
	}
	return r.Reader.Read(p)
}

func TestOAuthQuotaFlushBeforeNextEventCompletes(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	first := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\nevent:"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := &quotaPausedReader{first: []byte(first), rest: strings.NewReader(" message_stop\ndata: {\"type\":\"message_stop\"}\n\n"), release: release}
		resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(body)}
		_, _, _ = (&Server{}).handleResponse(&requestContext{ctx: r.Context(), startTime: time.Now(), isStreaming: true}, resp, w, "anthropic", &model.Config{AuthType: model.AuthTypeAnthropicOAuth}, "", nil)
	}))
	defer server.Close()
	defer unblock()
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("complete event was not flushed before next event: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	reader := bufio.NewReader(resp.Body)
	var frame strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		frame.WriteString(line)
		if line == "\n" {
			break
		}
	}
	_, data := parseSSEEventChunk([]byte(frame.String()))
	var event struct {
		Delta struct {
			Text string `json:"text"`
		} `json:"delta"`
	}
	if err := json.Unmarshal(data, &event); err != nil || event.Delta.Text != "hello" {
		t.Fatalf("invalid first event: %s (%v)", data, err)
	}
	unblock()
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatal(err)
	}
}

type quotaPausedReader struct {
	first   []byte
	rest    *strings.Reader
	release <-chan struct{}
}

func (r *quotaPausedReader) Read(p []byte) (int, error) {
	if len(r.first) > 0 {
		n := copy(p, r.first)
		r.first = r.first[n:]
		return n, nil
	}
	<-r.release
	return r.rest.Read(p)
}
