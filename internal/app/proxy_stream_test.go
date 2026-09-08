package app

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"ccLoad/internal/protocol"
	"ccLoad/internal/protocol/builtin"
)

func readCodexMalformedSSEFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/codex_responses_malformed_sse.txt")
	if err != nil {
		t.Fatalf("read malformed SSE fixture: %v", err)
	}
	return data
}

func TestCodexSSEFramingReaderPreservesAndRepairs(t *testing.T) {
	valid := []byte("event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
	got, err := io.ReadAll(newCodexSSEFramingReader(bytes.NewReader(valid)))
	if err != nil || !bytes.Equal(got, valid) {
		t.Fatalf("valid=%q err=%v", got, err)
	}
	malformed := []byte("event: response.created\ndata: {\"type\":\"response.created\"}\nevent: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")
	got, err = io.ReadAll(newCodexSSEFramingReader(bytes.NewReader(malformed)))
	if err != nil || !bytes.Contains(got, []byte("}\n\nevent: response.completed")) {
		t.Fatalf("repaired=%q err=%v", got, err)
	}
}

func TestCodexSSEFramingReaderEOFDoesNotAddBoundary(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte("event: response.completed\ndata: {\"type\":\"response.completed\"}"),
		[]byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n"),
	} {
		got, err := io.ReadAll(newCodexSSEFramingReader(bytes.NewReader(raw)))
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatalf("got=%q err=%v", got, err)
		}
	}
}

func TestCodexSSEFramingReaderWireContracts(t *testing.T) {
	tests := []struct{ name, input, want string }{
		{"leading blanks", "\n\nevent: response.created\ndata: {\"type\":\"response.created\"}\n\n", "\n\nevent: response.created\ndata: {\"type\":\"response.created\"}\n\n"},
		{"BOM and leading heartbeat", "\xef\xbb\xbf \t: ping\nevent: response.created\ndata: {\"type\":\"response.created\"}\nevent: response.completed\ndata: {\"type\":\"response.completed\"}\n\n", "\xef\xbb\xbf \t: ping\nevent: response.created\ndata: {\"type\":\"response.created\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"},
		{"json field names", "event: response.created\ndata: {\"type\":\"response.created\",\"text\":\"event: data:\"}\nevent: response.completed\ndata: {\"type\":\"response.completed\"}\n\n", "event: response.created\ndata: {\"type\":\"response.created\",\"text\":\"event: data:\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"},
		{"adjacent response fields", "event: response.first\ndata: x\nevent: response.second\ndata: y\n\n", "event: response.first\ndata: x\n\nevent: response.second\ndata: y\n\n"},
		{"data before event", "data: {\"type\":\"response.created\"}\nevent: response.created\nevent: response.completed\n", "data: {\"type\":\"response.created\"}\nevent: response.created\nevent: response.completed\n"},
		{"multiline data", "event: response.created\ndata: {\"type\":\"response.created\"}\ndata: {}\nevent: response.completed\n", "event: response.created\ndata: {\"type\":\"response.created\"}\ndata: {}\nevent: response.completed\n"},
		{"crlf", "event: response.created\r\ndata: {\"type\":\"response.created\"}\r\nevent: response.completed\r\n", "event: response.created\r\ndata: {\"type\":\"response.created\"}\r\n\r\nevent: response.completed\r\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newCodexSSEFramingReader(&oneByteReader{data: []byte(tc.input)})
			var got bytes.Buffer
			p := make([]byte, 3)
			for {
				n, err := r.Read(p)
				got.Write(p[:n])
				if err != nil {
					if err != io.EOF {
						t.Fatal(err)
					}
					break
				}
			}
			if got.String() != tc.want {
				t.Fatalf("got %q want %q", got.String(), tc.want)
			}
		})
	}
}

func TestCodexSSEFramingReaderPreservesCRLFAfterLongLine(t *testing.T) {
	longData := `{"type":"response.created","padding":"` + strings.Repeat("x", 64) + `"}`
	input := "event: response.created\r\ndata: " + longData + "\r\nevent: response.completed\r\ndata: {\"type\":\"response.completed\"}\r\n\r\n"
	got, err := io.ReadAll(newCodexSSEFramingReader(strings.NewReader(input)))
	if err != nil {
		t.Fatal(err)
	}
	wantBoundary := "\r\n\r\nevent: response.completed"
	if !bytes.Contains(got, []byte(wantBoundary)) {
		t.Fatalf("long CRLF line lost CRLF repair boundary: got %q", got)
	}
	if bytes.Contains(got, []byte("\n\nevent: response.completed")) {
		t.Fatalf("long CRLF line used LF-only repair boundary: got %q", got)
	}
}

func TestCodexSSEFramingReaderDoesNotGuessAcrossFields(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "comment after data",
			input: "event: response.created\ndata: {}\n: id\nevent: response.completed\ndata: {}\n\n",
		},
		{
			name:  "id after data",
			input: "event: response.created\ndata: {}\nid: 1\nevent: response.completed\ndata: {}\n\n",
		},
		{
			name:  "retry after data",
			input: "event: response.created\ndata: {}\nretry: 1000\nevent: response.completed\ndata: {}\n\n",
		},
		{
			name:  "multiple data lines",
			input: "event: response.created\ndata: {}\ndata: {}\nevent: response.completed\ndata: {}\n\n",
		},
		{
			name:  "non response event",
			input: "event: message\ndata: {}\nevent: response.completed\ndata: {}\n\n",
		},
		{
			name:  "lone carriage return",
			input: "event: response.created\rdata: {}\revent: response.completed\rdata: {}\r",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := io.ReadAll(newCodexSSEFramingReader(strings.NewReader(tc.input)))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.input {
				t.Fatalf("got %q want unchanged input %q", got, tc.input)
			}
		})
	}
}

func BenchmarkCodexSSEFramingReader(b *testing.B) {
	const event = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"
	input := []byte(strings.Repeat(event, 128))
	b.SetBytes(int64(len(input)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		reader := newCodexSSEFramingReader(bytes.NewReader(input))
		if _, err := io.Copy(io.Discard, reader); err != nil {
			b.Fatal(err)
		}
	}
}

type oneByteReader struct{ data []byte }

func (r *oneByteReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	p[0] = r.data[0]
	r.data = r.data[1:]
	return 1, nil
}

type countingReadCloser struct{ closes int }

func (*countingReadCloser) Read([]byte) (int, error) { return 0, io.EOF }
func (r *countingReadCloser) Close() error           { r.closes++; return nil }

func TestCodexSSEFramingReaderCloseIsIdempotent(t *testing.T) {
	src := &countingReadCloser{}
	r := newCodexSSEFramingReader(src)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if src.closes != 1 {
		t.Fatalf("close count=%d", src.closes)
	}
}

var errFramingSource = errors.New("framing source failed")

type dataThenErrorReader struct {
	data []byte
	done bool
}

func (r *dataThenErrorReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, errFramingSource
	}
	r.done = true
	n := copy(p, r.data)
	return n, errFramingSource
}

func TestCodexSSEFramingReaderDeliversDataBeforeSourceError(t *testing.T) {
	raw := []byte("event: response.created\ndata: {\"type\":\"response.created\"}\n")
	r := newCodexSSEFramingReader(&dataThenErrorReader{data: raw})
	got, err := io.ReadAll(r)
	if !errors.Is(err, errFramingSource) {
		t.Fatalf("err=%v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("got=%q", got)
	}
}

func TestCodexSSEFramingReaderRejectsOversizedLine(t *testing.T) {
	r := newCodexSSEFramingReader(&repeatedByteReader{remaining: maxSSEEventBytes + 1})
	_, err := io.ReadAll(r)
	if err == nil || !strings.Contains(err.Error(), "SSE event exceeds") {
		t.Fatalf("err=%v", err)
	}
}

type zeroNilReader struct{}

func (*zeroNilReader) Read([]byte) (int, error) { return 0, nil }

func TestCodexSSEFramingReaderZeroByteReadIsEOF(t *testing.T) {
	got, err := io.ReadAll(newCodexSSEFramingReader(&zeroNilReader{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %q", got)
	}
}

// errorReader 模拟返回特定错误的 Reader
type errorReader struct {
	err error
}

type streamStatsResponseWriter struct {
	header        http.Header
	status        int
	body          bytes.Buffer
	writeCalls    int
	flushCalls    int
	writeErr      error
	writeBytes    int
	flushObserved chan struct{}
}

func (w *streamStatsResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *streamStatsResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *streamStatsResponseWriter) Write(p []byte) (int, error) {
	w.writeCalls++
	if w.writeErr != nil {
		n := w.writeBytes
		if n > len(p) {
			n = len(p)
		}
		return n, w.writeErr
	}
	return w.body.Write(p)
}

func (w *streamStatsResponseWriter) Flush() {
	w.flushCalls++
	if w.flushObserved != nil {
		select {
		case <-w.flushObserved:
		default:
			close(w.flushObserved)
		}
	}
}

type repeatedByteReader struct {
	remaining int
}

func (r *repeatedByteReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.remaining)
	for i := range n {
		p[i] = 'x'
	}
	r.remaining -= n
	return n, nil
}

func (r *errorReader) Read(_ []byte) (int, error) {
	return 0, r.err
}

type blockingReadCloser struct {
	closeOnce sync.Once
	readOnce  sync.Once
	entered   chan struct{}
	closed    chan struct{}
}

func newBlockingReadCloser() *blockingReadCloser {
	return &blockingReadCloser{
		entered: make(chan struct{}),
		closed:  make(chan struct{}),
	}
}

func (r *blockingReadCloser) Read(_ []byte) (int, error) {
	r.readOnce.Do(func() {
		close(r.entered)
	})
	<-r.closed
	return 0, errors.New("read closed")
}

func (r *blockingReadCloser) Close() error {
	r.closeOnce.Do(func() {
		close(r.closed)
	})
	return nil
}

func TestStreamResponseWriterTracksSuccessfulOutput(t *testing.T) {
	start := time.Now().Add(-10 * time.Millisecond)
	stats := &streamReadStats{}
	target := &streamStatsResponseWriter{}
	writer := newStreamResponseWriter(target, stats, start)

	if n, err := writer.Write([]byte("hello")); err != nil || n != 5 {
		t.Fatalf("Write() = (%d, %v), want (5, nil)", n, err)
	}
	writer.Flush()

	if got, want := stats.downstreamBytes, int64(5); got != want {
		t.Fatalf("downstreamBytes = %d, want %d", got, want)
	}
	if got, want := stats.downstreamWrites, 1; got != want {
		t.Fatalf("downstreamWrites = %d, want %d", got, want)
	}
	if got, want := stats.downstreamFlushes, 1; got != want {
		t.Fatalf("downstreamFlushes = %d, want %d", got, want)
	}
	if stats.lastWriteSec <= 0 || stats.lastFlushSec <= 0 {
		t.Fatalf("output timings = write %.6f flush %.6f, want positive values", stats.lastWriteSec, stats.lastFlushSec)
	}
	if target.body.String() != "hello" {
		t.Fatalf("forwarded body = %q, want %q", target.body.String(), "hello")
	}
}

func TestStreamResponseWriterTracksPartialWriteWithError(t *testing.T) {
	stats := &streamReadStats{}
	target := &streamStatsResponseWriter{writeErr: io.ErrClosedPipe, writeBytes: 2}
	writer := newStreamResponseWriter(target, stats, time.Now().Add(-10*time.Millisecond))

	n, err := writer.Write([]byte("hello"))
	if n != 2 || !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Write() = (%d, %v), want (2, io.ErrClosedPipe)", n, err)
	}
	if stats.downstreamBytes != 2 || stats.downstreamWrites != 1 || stats.lastWriteSec <= 0 {
		t.Fatalf("partial write stats = %#v, want n>0 to be recorded", *stats)
	}
}

func TestDeferredResponseWriterTracksCommittedOutput(t *testing.T) {
	start := time.Now().Add(-10 * time.Millisecond)
	stats := &streamReadStats{}
	target := &streamStatsResponseWriter{}
	writer := newDeferredResponseWriter(newStreamResponseWriter(target, stats, start))

	if _, err := writer.Write([]byte("buffered")); err != nil {
		t.Fatalf("buffered Write() error = %v", err)
	}
	if stats.downstreamBytes != 0 || stats.downstreamWrites != 0 {
		t.Fatalf("uncommitted output was counted: bytes=%d writes=%d", stats.downstreamBytes, stats.downstreamWrites)
	}
	if err := writer.Commit(); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if _, err := writer.Write([]byte("visible")); err != nil {
		t.Fatalf("committed Write() error = %v", err)
	}
	writer.Flush()

	if got, want := stats.downstreamBytes, int64(len("buffered")+len("visible")); got != want {
		t.Fatalf("downstreamBytes = %d, want %d", got, want)
	}
	if got, want := stats.downstreamWrites, 2; got != want {
		t.Fatalf("downstreamWrites = %d, want %d", got, want)
	}
	if got, want := stats.downstreamFlushes, 1; got != want {
		t.Fatalf("downstreamFlushes = %d, want %d", got, want)
	}
	if target.body.String() != "bufferedvisible" {
		t.Fatalf("forwarded body = %q, want %q", target.body.String(), "bufferedvisible")
	}
}

func TestHandleSuccessResponseTracksRawStreamAndClientCancel(t *testing.T) {
	const body = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n"
	reqCtx := &requestContext{
		ctx:         context.Background(),
		startTime:   time.Now().Add(-10 * time.Millisecond),
		isStreaming: true,
	}
	readStats := &streamReadStats{}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(io.MultiReader(
			strings.NewReader(body),
			&errorReader{err: context.Canceled},
		)),
	}
	attachFirstByteDetector(reqCtx, resp, readStats, nil)
	target := &streamStatsResponseWriter{}
	result, _, err := (&Server{}).handleSuccessResponse(
		reqCtx, resp, resp.Header.Clone(), target, "openai", readStats, nil,
	)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("handleSuccessResponse() error = %v, want wrapped context.Canceled", err)
	}
	if result == nil || result.StreamDiagMsg != "" {
		t.Fatalf("client cancellation should keep StreamDiagMsg empty, result=%#v", result)
	}
	if !strings.Contains(err.Error(), "流时序:") || !strings.Contains(err.Error(), "下游最后写入") {
		t.Fatalf("client cancellation error lacks timing diagnostics: %v", err)
	}
	if readStats.lastReadSec <= 0 || readStats.downstreamBytes != int64(len(body)) ||
		readStats.downstreamWrites == 0 || readStats.downstreamFlushes == 0 {
		t.Fatalf("stream stats = %#v, want read/write/flush observations", *readStats)
	}
	if target.body.String() != body {
		t.Fatalf("forwarded body = %q, want original SSE body", target.body.String())
	}
}

func TestHandleSuccessResponseTracksTranslatedStream(t *testing.T) {
	const body = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n\n"
	start := time.Now().Add(-10 * time.Millisecond)
	reqCtx := &requestContext{
		ctx:         context.Background(),
		startTime:   start,
		isStreaming: true,
		transformPlan: protocol.TransformPlan{
			ClientProtocol:   protocol.Anthropic,
			UpstreamProtocol: protocol.OpenAI,
			OriginalModel:    "claude-3-5-sonnet",
			ActualModel:      "gpt-4o",
			NeedsTransform:   true,
		},
	}
	readStats := &streamReadStats{}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	attachFirstByteDetector(reqCtx, resp, readStats, nil)
	target := &streamStatsResponseWriter{}
	registry := protocol.NewRegistry()
	builtin.Register(registry)
	result, _, err := (&Server{protocolRegistry: registry}).handleSuccessResponse(
		reqCtx, resp, resp.Header.Clone(), target, "openai", readStats, nil,
	)
	if err != nil {
		t.Fatalf("handleSuccessResponse() error = %v", err)
	}
	if result == nil || !result.ResponseCommitted {
		t.Fatalf("translated stream result = %#v, want committed response", result)
	}
	if readStats.lastReadSec <= 0 || readStats.downstreamBytes == 0 ||
		readStats.downstreamWrites == 0 || readStats.downstreamFlushes == 0 {
		t.Fatalf("translated stream stats = %#v, want read/write/flush observations", *readStats)
	}
	if !strings.Contains(target.body.String(), "message_stop") {
		t.Fatalf("translated response lacks message_stop: %s", target.body.String())
	}
}

// TestStreamCopySSE_ContextCanceledDuringRead 测试在 Read 期间 context 被取消的场景
// 场景：客户端取消请求 → HTTP/2 流关闭 → Read 返回 "http2: response body closed"
// 期望：返回 context.Canceled 而非原始错误，让上层正确识别为客户端断开（499）
func TestStreamCopySSE_ContextCanceledDuringRead(t *testing.T) {
	tests := []struct {
		name        string
		readErr     error
		ctxCanceled bool
		wantErr     error
		reason      string
	}{
		{
			name:        "http2_closed_with_ctx_canceled",
			readErr:     errors.New("http2: response body closed"),
			ctxCanceled: true,
			wantErr:     context.Canceled,
			reason:      "context 已取消时，应返回 context.Canceled 而非 http2 错误",
		},
		{
			name:        "http2_closed_without_ctx_canceled",
			readErr:     errors.New("http2: response body closed"),
			ctxCanceled: false,
			wantErr:     errors.New("http2: response body closed"),
			reason:      "context 未取消时，应返回原始错误",
		},
		{
			name:        "stream_error_with_ctx_canceled",
			readErr:     errors.New("stream error: stream ID 7; INTERNAL_ERROR"),
			ctxCanceled: true,
			wantErr:     context.Canceled,
			reason:      "context 已取消时，stream error 也应转换为 context.Canceled",
		},
		{
			name:        "network_error_with_ctx_canceled",
			readErr:     errors.New("connection reset by peer"),
			ctxCanceled: true,
			wantErr:     context.Canceled,
			reason:      "context 已取消时，网络错误应转换为 context.Canceled",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			if tt.ctxCanceled {
				cancel() // 模拟客户端取消
			} else {
				defer cancel()
			}

			// 创建模拟 Reader 返回指定错误
			reader := &errorReader{err: tt.readErr}
			recorder := newRecorder()

			// 调用 streamCopySSE
			err := streamCopySSE(ctx, reader, recorder, nil)

			if tt.ctxCanceled {
				if !errors.Is(err, context.Canceled) {
					t.Errorf("%s: got err=%v, want context.Canceled", tt.reason, err)
				}
			} else {
				if err == nil || err.Error() != tt.readErr.Error() {
					t.Errorf("%s: got err=%v, want %v", tt.reason, err, tt.readErr)
				}
			}
		})
	}
}

// TestStreamCopy_ContextCanceledDuringRead 测试非 SSE 流复制在 Read 期间 context 被取消的场景
func TestStreamCopy_ContextCanceledDuringRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 模拟客户端取消

	reader := &errorReader{err: errors.New("http2: response body closed")}
	recorder := newRecorder()

	err := streamCopy(ctx, reader, recorder, nil)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("streamCopy should return context.Canceled when ctx is canceled, got: %v", err)
	}
}

func TestStreamCopy_ClosesReadCloserOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := newBlockingReadCloser()
	done := make(chan error, 1)

	go func() {
		done <- streamCopy(ctx, reader, newRecorder(), nil)
	}()

	select {
	case <-reader.entered:
	case <-time.After(200 * time.Millisecond):
		_ = reader.Close()
		t.Fatal("streamCopy did not enter Read")
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("streamCopy err=%v, want context.Canceled", err)
		}
	case <-time.After(200 * time.Millisecond):
		_ = reader.Close()
		t.Fatal("streamCopy did not unblock Read after context cancellation")
	}
}

func TestStreamCopy_ClosesWrappedUnderlyingCloserOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	underlying := newBlockingReadCloser()
	reader := bufio.NewReader(underlying)
	wrapped := readerWithCloser{Reader: reader, Closer: underlying}
	done := make(chan error, 1)

	go func() {
		done <- streamCopy(ctx, wrapped, newRecorder(), nil)
	}()

	select {
	case <-underlying.entered:
	case <-time.After(200 * time.Millisecond):
		_ = underlying.Close()
		t.Fatal("streamCopy did not enter wrapped Read")
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("streamCopy err=%v, want context.Canceled", err)
		}
	case <-time.After(200 * time.Millisecond):
		_ = underlying.Close()
		t.Fatal("streamCopy did not close wrapped underlying reader after context cancellation")
	}
}

func TestStreamTransformSSEEventsUntil_ReassemblesLongLinesAndMultipleEvents(t *testing.T) {
	longValue := strings.Repeat("x", SSEBufferSize*2)
	input := "data: " + longValue + "\n\ndata: second\n\n"
	var events [][]byte
	recorder := newRecorder()

	err := streamTransformSSEEventsUntil(
		context.Background(),
		strings.NewReader(input),
		recorder,
		func(rawEvent []byte) error {
			events = append(events, bytes.Clone(rawEvent))
			return nil
		},
		func(rawEvent []byte) ([][]byte, error) {
			return [][]byte{bytes.ToUpper(rawEvent)}, nil
		},
		nil,
	)
	if err != nil {
		t.Fatalf("streamTransformSSEEventsUntil() error = %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("reassembled event count=%d, want 2", len(events))
	}
	if string(events[0]) != "data: "+longValue+"\n\n" || string(events[1]) != "data: second\n\n" {
		t.Fatalf("reassembled events mismatch: first=%d bytes second=%q", len(events[0]), events[1])
	}
	if got, want := recorder.Body.String(), strings.ToUpper(input); got != want {
		t.Fatalf("translated output length=%d, want %d", len(got), len(want))
	}
}

func TestCodexFramingReaderFeedsSSETransformDistinctEvents(t *testing.T) {
	input := readCodexMalformedSSEFixture(t)
	var events [][]byte
	recorder := newRecorder()
	err := streamTransformSSEEventsUntil(
		context.Background(),
		newCodexSSEFramingReader(bytes.NewReader(input)),
		recorder,
		func(rawEvent []byte) error {
			events = append(events, bytes.Clone(rawEvent))
			return nil
		},
		func(rawEvent []byte) ([][]byte, error) { return [][]byte{rawEvent}, nil },
		nil,
	)
	if err != nil {
		t.Fatalf("streamTransformSSEEventsUntil() error = %v", err)
	}
	if len(events) != 6 {
		t.Fatalf("event count=%d, want 6", len(events))
	}
	for i, event := range events {
		if !bytes.HasSuffix(event, []byte("\n\n")) {
			t.Fatalf("event %d is not independently framed: %q", i, event)
		}
		if !bytes.Contains(event, []byte("event: response.")) || !bytes.Contains(event, []byte("data: {")) {
			t.Fatalf("event %d missing Codex event/data fields: %q", i, event)
		}
	}
	if got := strings.Count(recorder.Body.String(), "\n\n"); got != 6 {
		t.Fatalf("output frame count=%d, want 6; body=%q", got, recorder.Body.String())
	}
}

func TestStreamTransformSSEEventsUntil_DoesNotCommitEOFBlock(t *testing.T) {
	input := []byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n")
	var events [][]byte
	err := streamTransformSSEEventsUntil(
		context.Background(), bytes.NewReader(input), newRecorder(),
		func(rawEvent []byte) error {
			events = append(events, bytes.Clone(rawEvent))
			return nil
		},
		func(rawEvent []byte) ([][]byte, error) { return [][]byte{rawEvent}, nil },
		nil,
	)
	if err != nil {
		t.Fatalf("streamTransformSSEEventsUntil() error = %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("EOF-terminated SSE block committed as %q", events)
	}
}

func TestStreamTransformSSEEventsUntil_PreservesValidSSEBytes(t *testing.T) {
	input := []byte("event: response.created\ndata: {\"type\":\"response.created\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")
	recorder := newRecorder()
	err := streamTransformSSEEventsUntil(
		context.Background(), bytes.NewReader(input), recorder, nil,
		func(rawEvent []byte) ([][]byte, error) { return [][]byte{rawEvent}, nil },
		nil,
	)
	if err != nil {
		t.Fatalf("streamTransformSSEEventsUntil() error = %v", err)
	}
	if got := recorder.Body.Bytes(); !bytes.Equal(got, input) {
		t.Fatalf("valid SSE bytes changed: got %q, want %q", got, input)
	}
}

func TestStreamTransformSSEEventsUntil_RejectsOversizedEvent(t *testing.T) {
	reader := io.MultiReader(
		&repeatedByteReader{remaining: maxSSEEventBytes},
		strings.NewReader("\n\n"),
	)
	err := streamTransformSSEEventsUntil(
		context.Background(), reader, newRecorder(), nil,
		func([]byte) ([][]byte, error) { return nil, nil }, nil,
	)
	if err == nil || !strings.Contains(err.Error(), "SSE event exceeds") {
		t.Fatalf("oversized event error = %v", err)
	}
}

func TestStreamTransformSSEEventsUntil_ClosesReaderOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := newBlockingReadCloser()
	done := make(chan error, 1)
	go func() {
		done <- streamTransformSSEEventsUntil(
			ctx, reader, newRecorder(), nil,
			func([]byte) ([][]byte, error) { return nil, nil }, nil,
		)
	}()

	select {
	case <-reader.entered:
	case <-time.After(200 * time.Millisecond):
		_ = reader.Close()
		t.Fatal("stream transform did not enter Read")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stream transform err=%v, want context.Canceled", err)
		}
	case <-time.After(200 * time.Millisecond):
		_ = reader.Close()
		t.Fatal("stream transform did not unblock Read after context cancellation")
	}
}
