package app

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// errorReader 模拟返回特定错误的 Reader
type errorReader struct {
	err error
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

type dataThenBlockReadCloser struct {
	closeOnce sync.Once
	data      []byte
	offset    int
	maxChunk  int
	closed    chan struct{}
}

func newDataThenBlockReadCloser(data []byte, maxChunk int) *dataThenBlockReadCloser {
	return &dataThenBlockReadCloser{
		data:     data,
		maxChunk: maxChunk,
		closed:   make(chan struct{}),
	}
}

func (r *dataThenBlockReadCloser) Read(p []byte) (int, error) {
	if r.offset < len(r.data) {
		n := len(r.data) - r.offset
		if r.maxChunk > 0 && n > r.maxChunk {
			n = r.maxChunk
		}
		if n > len(p) {
			n = len(p)
		}
		copy(p, r.data[r.offset:r.offset+n])
		r.offset += n
		return n, nil
	}
	<-r.closed
	return 0, errors.New("read closed")
}

func (r *dataThenBlockReadCloser) Close() error {
	r.closeOnce.Do(func() {
		close(r.closed)
	})
	return nil
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

func TestStreamAndParseResponse_CodexCompletesWithoutEOF(t *testing.T) {
	sse := []byte("event: response.created\ndata: {\"type\":\"response.created\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":4}}}\n\n")
	reader := newDataThenBlockReadCloser(sse, 7)
	defer func() { _ = reader.Close() }()
	recorder := newRecorder()

	type result struct {
		parser usageParser
		err    error
	}
	done := make(chan result, 1)
	go func() {
		parser, err := streamAndParseResponse(
			context.Background(),
			reader,
			recorder,
			"text/event-stream",
			"codex",
			true,
			nil,
		)
		done <- result{parser: parser, err: err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("streamAndParseResponse err=%v, want nil", got.err)
		}
		if got.parser == nil || !got.parser.IsStreamComplete() {
			t.Fatal("streamAndParseResponse should preserve response.completed semantics")
		}
		if recorder.Body.String() != string(sse) {
			t.Fatalf("forwarded body mismatch:\n got: %q\nwant: %q", recorder.Body.String(), string(sse))
		}
		if !recorder.Flushed {
			t.Fatal("response.completed must be flushed before the stream returns")
		}
		input, output, _, _ := got.parser.GetUsage()
		if input != 3 || output != 4 {
			t.Fatalf("usage=(%d,%d), want (3,4)", input, output)
		}
	case <-time.After(200 * time.Millisecond):
		_ = reader.Close()
		<-done
		t.Fatal("streamAndParseResponse kept waiting for EOF after response.completed")
	}
}

func TestStreamAndParseResponse_CodexPreservesPartialEventAtEOF(t *testing.T) {
	sse := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"tail\"}"
	recorder := newRecorder()

	_, err := streamAndParseResponse(
		context.Background(),
		io.NopCloser(strings.NewReader(sse)),
		recorder,
		"text/event-stream",
		"codex",
		true,
		nil,
	)
	if err != nil {
		t.Fatalf("streamAndParseResponse err=%v, want nil", err)
	}
	if recorder.Body.String() != sse {
		t.Fatalf("forwarded body mismatch:\n got: %q\nwant: %q", recorder.Body.String(), sse)
	}
}

func TestStreamAndParseResponse_CodexBareCRCompletesWithoutEOF(t *testing.T) {
	sse := []byte("event: response.completed\rdata: {\"type\":\"response.completed\"}\r\r")
	reader := newDataThenBlockReadCloser(sse, 5)
	defer func() { _ = reader.Close() }()

	type result struct {
		parser usageParser
		err    error
	}
	done := make(chan result, 1)
	recorder := newRecorder()
	go func() {
		parser, err := streamAndParseResponse(
			context.Background(), reader, recorder, "text/event-stream", "codex", true, nil,
		)
		done <- result{parser: parser, err: err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("streamAndParseResponse err=%v, want nil", got.err)
		}
		if got.parser == nil || !got.parser.IsStreamComplete() {
			t.Fatal("bare-CR response.completed must preserve stream-complete semantics")
		}
		if recorder.Body.String() != string(sse) {
			t.Fatalf("forwarded body mismatch:\n got: %q\nwant: %q", recorder.Body.String(), string(sse))
		}
	case <-time.After(200 * time.Millisecond):
		_ = reader.Close()
		<-done
		t.Fatal("streamAndParseResponse kept waiting for EOF after bare-CR response.completed")
	}
}

func TestStreamAndParseResponse_CodexBareCRCommitsDeferredWriter(t *testing.T) {
	sse := []byte("event: response.completed\rdata: {\"type\":\"response.completed\"}\r\r")
	target := newRecorder()
	deferred := newDeferredResponseWriter(target)

	parser, err := streamAndParseResponse(
		context.Background(),
		io.NopCloser(bytes.NewReader(sse)),
		deferred,
		"text/event-stream",
		"codex",
		true,
		func(parser usageParser) error {
			if parser.HasStreamOutput() || parser.IsStreamComplete() {
				return deferred.Commit()
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("streamAndParseResponse err=%v, want nil", err)
	}
	if parser == nil || !parser.IsStreamComplete() {
		t.Fatal("bare-CR response.completed must preserve stream-complete semantics")
	}
	if !deferred.Committed() {
		t.Fatal("bare-CR response.completed must commit the production deferred writer")
	}
	if target.Body.String() != string(sse) {
		t.Fatalf("forwarded body mismatch:\n got: %q\nwant: %q", target.Body.String(), string(sse))
	}
}

func TestHandleSuccessResponse_CodexBareCRCommitsProductionDeferredWriter(t *testing.T) {
	sse := []byte("event: response.completed\rdata: {\"type\":\"response.completed\"}\r\r")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reqCtx := &requestContext{
		ctx:         ctx,
		cancel:      cancel,
		startTime:   time.Now(),
		isStreaming: true,
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewReader(sse)),
	}
	recorder := newRecorder()

	_, _, err := (&Server{}).handleSuccessResponse(
		reqCtx,
		resp,
		resp.Header.Clone(),
		recorder,
		"codex",
		&streamReadStats{totalBytes: int64(len(sse))},
		nil,
	)
	if err != nil {
		t.Fatalf("handleSuccessResponse err=%v, want nil", err)
	}
	if recorder.Body.String() != string(sse) {
		t.Fatalf("forwarded body mismatch:\n got: %q\nwant: %q", recorder.Body.String(), string(sse))
	}
}

func TestStreamAndParseResponse_CodexStopsAtTerminalBoundary(t *testing.T) {
	terminal := []byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")
	trailing := []byte("event: response.output_text.delta\ndata: {\"delta\":\"late\"}\n\n")
	reader := newDataThenBlockReadCloser(append(append([]byte(nil), terminal...), trailing...), len(terminal)+len(trailing))
	defer func() { _ = reader.Close() }()
	recorder := newRecorder()

	_, err := streamAndParseResponse(
		context.Background(), reader, recorder, "text/event-stream", "codex", true, nil,
	)
	if err != nil {
		t.Fatalf("streamAndParseResponse err=%v, want nil", err)
	}
	if recorder.Body.String() != string(terminal) {
		t.Fatalf("forwarded body mismatch:\n got: %q\nwant: %q", recorder.Body.String(), string(terminal))
	}
}

func TestStreamAndParseResponse_CodexShortTextPlainCompletesWithoutEOF(t *testing.T) {
	for _, tt := range []struct {
		name string
		sse  []byte
	}{
		{name: "LF", sse: []byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")},
		{name: "bare CR", sse: []byte("event: response.completed\rdata: {\"type\":\"response.completed\"}\r\r")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reader := newDataThenBlockReadCloser(tt.sse, 7)
			defer func() { _ = reader.Close() }()
			recorder := newRecorder()

			type result struct {
				parser usageParser
				err    error
			}
			done := make(chan result, 1)
			go func() {
				parser, err := streamAndParseResponse(
					context.Background(), reader, recorder, "text/plain", "codex", true, nil,
				)
				done <- result{parser: parser, err: err}
			}()

			select {
			case got := <-done:
				if got.err != nil {
					t.Fatalf("streamAndParseResponse err=%v, want nil", got.err)
				}
				if got.parser == nil || !got.parser.IsStreamComplete() {
					t.Fatal("short text/plain response.completed must preserve stream-complete semantics")
				}
				if recorder.Body.String() != string(tt.sse) {
					t.Fatalf("forwarded body mismatch:\n got: %q\nwant: %q", recorder.Body.String(), string(tt.sse))
				}
			case <-time.After(200 * time.Millisecond):
				_ = reader.Close()
				<-done
				t.Fatal("text/plain SSE probing waited for 2048 bytes or EOF")
			}
		})
	}
}

func TestStreamAndParseResponse_CodexPreservesSplitTerminalCRLF(t *testing.T) {
	sse := []byte("event: response.completed\r\ndata: {\"type\":\"response.completed\"}\r\n\r\n")
	reader := newDataThenBlockReadCloser(sse, 1)
	defer func() { _ = reader.Close() }()
	recorder := newRecorder()

	_, err := streamAndParseResponse(
		context.Background(), reader, recorder, "text/event-stream", "codex", true, nil,
	)
	if err != nil {
		t.Fatalf("streamAndParseResponse err=%v, want nil", err)
	}
	if recorder.Body.String() != string(sse) {
		t.Fatalf("forwarded body mismatch:\n got: %q\nwant: %q", recorder.Body.String(), string(sse))
	}
}

func TestResponsesCompletedDetector_LineEndings(t *testing.T) {
	for _, tt := range []struct {
		name      string
		separator string
	}{
		{name: "LF", separator: "\n"},
		{name: "CRLF", separator: "\r\n"},
		{name: "CR", separator: "\r"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			detector := newResponsesCompletedDetector()
			event := []byte("event: response.completed" + tt.separator + "data: {}" + tt.separator)
			for _, b := range event {
				if detector.Feed([]byte{b}) {
					t.Fatal("detector completed before the blank event delimiter")
				}
			}
			if !detector.Feed([]byte(tt.separator)) {
				t.Fatalf("detector did not recognize response.completed with separator %q", tt.separator)
			}
		})
	}
}

func TestResponsesCompletedDetector_UsesMostRecentLineEndingStyle(t *testing.T) {
	detector := newResponsesCompletedDetector()
	if detector.Feed([]byte("event: response.completed\r\ndata: {}\n")) {
		t.Fatal("detector completed before the blank event delimiter")
	}
	if !detector.Feed([]byte{'\r'}) {
		t.Fatal("bare-CR terminal delimiter after an LF line must complete immediately")
	}
}

func TestResponsesCompletedDetector_DoesNotBufferUnboundedLines(t *testing.T) {
	detector := newResponsesCompletedDetector()
	if detector.Feed(bytes.Repeat([]byte{'x'}, 1<<20)) {
		t.Fatal("unterminated non-event line must not complete the stream")
	}
	if len(detector.line) > maxTrackedSSELineBytes {
		t.Fatalf("tracked line grew to %d bytes, limit=%d", len(detector.line), maxTrackedSSELineBytes)
	}
}

func TestResponsesCompletedDetector_RequiresExactEventType(t *testing.T) {
	for _, eventLine := range []string{
		"event: response.output_item.done",
		"event: response.failed",
		"event: response.completed.extra",
		"event:	response.completed",
		"event:  response.completed",
		"event: response.completed ",
	} {
		detector := newResponsesCompletedDetector()
		if detector.Feed([]byte(eventLine + "\n\n")) {
			t.Fatalf("event line %q must not complete the stream", eventLine)
		}
	}
}

func TestResponsesCompletedDetector_AllowsOversizedDataAfterTerminalEvent(t *testing.T) {
	detector := newResponsesCompletedDetector()
	payload := append([]byte("event: response.completed\ndata: "), bytes.Repeat([]byte{'x'}, 1<<20)...)
	payload = append(payload, '\n', '\n')
	if !detector.Feed(payload) {
		t.Fatal("oversized data line must not hide a preceding response.completed event type")
	}
	if len(detector.line) > maxTrackedSSELineBytes {
		t.Fatalf("tracked line grew to %d bytes, limit=%d", len(detector.line), maxTrackedSSELineBytes)
	}
}
