package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteResponseWithHeaders_PreservesContentType(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()
	hdr := http.Header{}
	hdr.Set("Content-Type", "text/plain; charset=utf-8")
	hdr.Set("Connection", "keep-alive") // hop-by-hop should be stripped

	writeResponseWithHeaders(w, http.StatusBadGateway, hdr, []byte("oops"))

	if got := w.Code; got != http.StatusBadGateway {
		t.Fatalf("expected status %d, got %d", http.StatusBadGateway, got)
	}
	if got := w.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("expected Content-Type preserved, got %q", got)
	}
	if got := w.Header().Get("Connection"); got != "" {
		t.Fatalf("expected hop-by-hop header stripped, got %q", got)
	}
	if got := w.Body.String(); got != "oops" {
		t.Fatalf("expected body preserved, got %q", got)
	}
}

func TestWriteResponseWithHeaders_DefaultsToJSONContentTypeWhenBodyLooksJSON(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()
	writeResponseWithHeaders(w, http.StatusBadGateway, nil, []byte(`{"error":"x"}`))

	if got := w.Code; got != http.StatusBadGateway {
		t.Fatalf("expected status %d, got %d", http.StatusBadGateway, got)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("expected Content-Type json, got %q", got)
	}
}

func TestBuildLogEntry_StreamDiagMsg(t *testing.T) {
	channelID := int64(1)

	t.Run("正常成功响应", func(t *testing.T) {
		res := &fwResult{
			Status:       200,
			InputTokens:  10,
			OutputTokens: 20,
		}
		entry := buildLogEntry(logEntryParams{
			RequestModel: "claude-3",
			ChannelID:    channelID,
			StatusCode:   200,
			Duration:     1.5,
			IsStreaming:  true,
			APIKeyUsed:   "sk-test",
			Result:       res,
		})
		if entry.Message != "ok" {
			t.Errorf("expected Message='ok', got %q", entry.Message)
		}
	})

	t.Run("流传输中断诊断", func(t *testing.T) {
		res := &fwResult{
			Status:        200,
			StreamDiagMsg: "[WARN] 流传输中断: 错误=unexpected EOF | 已读取=1024字节(分5次)",
		}
		entry := buildLogEntry(logEntryParams{
			RequestModel: "claude-3",
			ChannelID:    channelID,
			StatusCode:   200,
			Duration:     1.5,
			IsStreaming:  true,
			APIKeyUsed:   "sk-test",
			Result:       res,
		})
		if entry.Message != res.StreamDiagMsg {
			t.Errorf("expected Message=%q, got %q", res.StreamDiagMsg, entry.Message)
		}
	})

	t.Run("流响应不完整诊断", func(t *testing.T) {
		res := &fwResult{
			Status:        200,
			StreamDiagMsg: "[WARN] 流响应不完整: 正常EOF但无usage | 已读取=512字节(分3次)",
		}
		entry := buildLogEntry(logEntryParams{
			RequestModel: "claude-3",
			ChannelID:    channelID,
			StatusCode:   200,
			Duration:     1.5,
			IsStreaming:  true,
			APIKeyUsed:   "sk-test",
			Result:       res,
		})
		if entry.Message != res.StreamDiagMsg {
			t.Errorf("expected Message=%q, got %q", res.StreamDiagMsg, entry.Message)
		}
	})

	t.Run("errMsg优先于StreamDiagMsg", func(t *testing.T) {
		res := &fwResult{
			Status:        200,
			StreamDiagMsg: "[WARN] 流传输中断",
		}
		errMsg := "network error"
		entry := buildLogEntry(logEntryParams{
			RequestModel: "claude-3",
			ChannelID:    channelID,
			StatusCode:   200,
			Duration:     1.5,
			IsStreaming:  true,
			APIKeyUsed:   "sk-test",
			Result:       res,
			ErrMsg:       errMsg,
		})
		if entry.Message != errMsg {
			t.Errorf("expected Message=%q, got %q", errMsg, entry.Message)
		}
	})
}

func TestCopyRequestHeaders_StripsHopByHopAndAuth(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}

	src := http.Header{}
	src.Set("Connection", "Upgrade, X-Hop")
	src.Set("Upgrade", "websocket")
	src.Set("X-Hop", "1")
	src.Set("Keep-Alive", "timeout=5")
	src.Set("TE", "trailers")
	src.Set("Trailer", "X-Trailer")
	src.Set("Proxy-Authorization", "secret")
	src.Set("Authorization", "Bearer client-token")
	src.Set("X-API-Key", "client-token2")
	src.Set("x-goog-api-key", "client-goog")
	src.Set("Accept-Encoding", "br")
	src.Set("X-Pass", "ok")

	copyRequestHeaders(req, src)

	if got := req.Header.Get("X-Pass"); got != "ok" {
		t.Fatalf("expected X-Pass=ok, got %q", got)
	}
	if got := req.Header.Get("Accept"); got != "application/json" {
		t.Fatalf("expected default Accept=application/json, got %q", got)
	}

	for _, k := range []string{
		"Connection",
		"Upgrade",
		"X-Hop",
		"Keep-Alive",
		"TE",
		"Trailer",
		"Proxy-Authorization",
		"Authorization",
		"X-API-Key",
		"x-goog-api-key",
		"Accept-Encoding",
	} {
		if v := req.Header.Get(k); v != "" {
			t.Fatalf("expected header %q stripped, got %q", k, v)
		}
	}
}

func TestFilterAndWriteResponseHeaders_StripsHopByHop(t *testing.T) {
	w := httptest.NewRecorder()

	hdr := http.Header{}
	hdr.Set("Connection", "Upgrade, X-Hop")
	hdr.Set("Upgrade", "websocket")
	hdr.Set("X-Hop", "1")
	hdr.Set("Transfer-Encoding", "chunked")
	hdr.Set("Trailer", "X-Trailer")
	hdr.Set("Content-Length", "123")
	hdr.Set("Content-Encoding", "br")
	hdr.Set("X-Pass", "ok")

	filterAndWriteResponseHeaders(w, hdr)

	if got := w.Header().Get("X-Pass"); got != "ok" {
		t.Fatalf("expected X-Pass=ok, got %q", got)
	}
	if got := w.Header().Get("Content-Encoding"); got != "br" {
		t.Fatalf("expected Content-Encoding=br, got %q", got)
	}

	for _, k := range []string{
		"Connection",
		"Upgrade",
		"X-Hop",
		"Transfer-Encoding",
		"Trailer",
		"Content-Length",
	} {
		if v := w.Header().Get(k); v != "" {
			t.Fatalf("expected header %q stripped, got %q", k, v)
		}
	}
}
