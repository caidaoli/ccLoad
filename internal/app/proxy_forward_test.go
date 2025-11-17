package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHandleSuccessResponse_ExtractsUsageFromJSON(t *testing.T) {
	body := `{"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":5,"cache_creation_input_tokens":7}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}

	reqCtx := &requestContext{
		ctx:         context.Background(),
		startTime:   time.Now(),
		isStreaming: false,
	}

	rec := httptest.NewRecorder()
	s := &Server{}

	res, _, err := s.handleSuccessResponse(reqCtx, resp, 0, resp.Header.Clone(), rec)
	if err != nil {
		t.Fatalf("handleSuccessResponse returned error: %v", err)
	}

	if res.InputTokens != 10 || res.OutputTokens != 20 || res.CacheReadInputTokens != 5 || res.CacheCreationInputTokens != 7 {
		t.Fatalf("unexpected usage extracted: %+v", res)
	}

	if rec.Body.String() != body {
		t.Fatalf("unexpected response body forwarded: %q", rec.Body.String())
	}
}

func TestHandleSuccessResponse_ExtractsUsageFromTextPlainSSE(t *testing.T) {
	body := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":4,\"cache_read_input_tokens\":1,\"cache_creation_input_tokens\":2}}}\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
	}

	reqCtx := &requestContext{
		ctx:         context.Background(),
		startTime:   time.Now(),
		isStreaming: true,
	}

	rec := httptest.NewRecorder()
	s := &Server{}

	res, _, err := s.handleSuccessResponse(reqCtx, resp, 0, resp.Header.Clone(), rec)
	if err != nil {
		t.Fatalf("handleSuccessResponse returned error: %v", err)
	}

	if res.InputTokens != 3 || res.OutputTokens != 4 || res.CacheReadInputTokens != 1 || res.CacheCreationInputTokens != 2 {
		t.Fatalf("unexpected usage extracted: %+v", res)
	}

	if rec.Body.String() != body {
		t.Fatalf("unexpected response body forwarded: %q", rec.Body.String())
	}
}
