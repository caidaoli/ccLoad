package app

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestHandleActiveRequests(t *testing.T) {
	t.Parallel()

	m := newActiveRequestManager()
	id := m.BeginAttempt(0, activeRequestAttempt{
		StartTime: time.Now(), Model: "m1", ClientIP: "1.2.3.4", Streaming: true,
		ChannelID: 10, ChannelName: "ch", APIKey: "sk-test",
		ClientProtocol: "openai", TokenID: 7, BaseURL: "https://upstream.example.com", CostMultiplier: 1.5,
	})
	m.SetUpstreamWebsocket(id, true)
	m.AddBytes(id, 123)
	m.SetClientFirstByteTime(id, 50*time.Millisecond)

	s := &Server{activeRequests: m}

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/active_requests", nil))

	s.HandleActiveRequests(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d", w.Code, http.StatusOK)
	}

	var resp struct {
		Success                   bool            `json:"success"`
		Data                      []ActiveRequest `json:"data"`
		Count                     int             `json:"count"`
		ActiveRequestTitleEnabled bool            `json:"active_request_title_enabled"`
	}
	mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
	if !resp.Success || resp.Count != 1 || len(resp.Data) != 1 {
		t.Fatalf("unexpected resp: %+v", resp)
	}
	if resp.ActiveRequestTitleEnabled {
		t.Fatal("active_request_title_enabled=true, want false by default")
	}
	if resp.Data[0].BytesReceived != 123 {
		t.Fatalf("bytes_received=%d, want 123", resp.Data[0].BytesReceived)
	}
	if resp.Data[0].ClientFirstByteTime <= 0 {
		t.Fatalf("client_first_byte_time=%v, want >0", resp.Data[0].ClientFirstByteTime)
	}
	if resp.Data[0].CostMultiplier != 1.5 {
		t.Fatalf("cost_multiplier=%v, want 1.5", resp.Data[0].CostMultiplier)
	}
	if resp.Data[0].ClientProtocol != "openai" {
		t.Fatalf("client_protocol=%q, want openai", resp.Data[0].ClientProtocol)
	}
	if !resp.Data[0].UpstreamWebsocket {
		t.Fatal("upstream_websocket=false, want true")
	}
	if resp.Data[0].UpstreamStatus != activeRequestStatusReceiving {
		t.Fatalf("upstream_status=%q, want %q", resp.Data[0].UpstreamStatus, activeRequestStatusReceiving)
	}
}

func TestHandleActiveRequestsExposesTitleSetting(t *testing.T) {
	t.Parallel()

	s := &Server{
		activeRequests:            newActiveRequestManager(),
		activeRequestTitleEnabled: true,
	}
	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/active-requests", nil))

	s.HandleActiveRequests(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d", w.Code, http.StatusOK)
	}
	var resp struct {
		ActiveRequestTitleEnabled bool `json:"active_request_title_enabled"`
	}
	mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
	if !resp.ActiveRequestTitleEnabled {
		t.Fatal("active_request_title_enabled=false, want true")
	}
}

func TestHandleAbortActiveRequest(t *testing.T) {
	t.Parallel()

	m := newActiveRequestManager()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	id := m.BeginAttempt(0, activeRequestAttempt{
		StartTime: time.Now(), Model: "m1", ClientIP: "1.2.3.4",
		ChannelID: 10, ChannelName: "ch", APIKey: "sk-test",
		BaseURL: "https://upstream.example.com", CostMultiplier: 1, Abort: cancel,
	})
	notAbortable := beginTestActiveRequest(m, time.Now(), "m2", "1.2.3.4", false)

	s := &Server{activeRequests: m}

	abort := func(param string) int {
		c, w := newTestContext(t, newRequest(http.MethodPost, "/admin/active-requests/"+param+"/abort", nil))
		c.Params = gin.Params{{Key: "request_id", Value: param}}
		s.HandleAbortActiveRequest(c)
		return w.Code
	}

	if code := abort("abc"); code != http.StatusBadRequest {
		t.Fatalf("abort with non-numeric id: status=%d, want %d", code, http.StatusBadRequest)
	}
	if code := abort("0"); code != http.StatusBadRequest {
		t.Fatalf("abort with id=0: status=%d, want %d", code, http.StatusBadRequest)
	}
	if code := abort(strconv.FormatInt(id+9999, 10)); code != http.StatusNotFound {
		t.Fatalf("abort with unknown id: status=%d, want %d", code, http.StatusNotFound)
	}
	if code := abort(strconv.FormatInt(notAbortable, 10)); code != http.StatusNotFound {
		t.Fatalf("abort without a registered handle: status=%d, want %d", code, http.StatusNotFound)
	}

	if code := abort(strconv.FormatInt(id, 10)); code != http.StatusOK {
		t.Fatalf("abort: status=%d, want %d", code, http.StatusOK)
	}
	<-ctx.Done()
	if !errors.Is(context.Cause(ctx), errOperatorAbort) {
		t.Fatalf("cancel cause=%v, want errOperatorAbort", context.Cause(ctx))
	}
}

func TestHandleAbortActiveRequestWithoutManager(t *testing.T) {
	t.Parallel()

	s := &Server{}
	c, w := newTestContext(t, newRequest(http.MethodPost, "/admin/active-requests/1/abort", nil))
	c.Params = gin.Params{{Key: "request_id", Value: "1"}}

	s.HandleAbortActiveRequest(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestActiveRequestManagerCount(t *testing.T) {
	t.Parallel()

	manager := newActiveRequestManager()
	if got := manager.Count(); got != 0 {
		t.Fatalf("Count on empty manager = %d, want 0", got)
	}

	first := beginTestActiveRequest(manager, time.Now(), "model-a", "127.0.0.1", true)
	second := beginTestActiveRequest(manager, time.Now(), "model-b", "127.0.0.1", false)
	if got := manager.Count(); got != 2 {
		t.Fatalf("Count after register = %d, want 2", got)
	}

	manager.Remove(first)
	if got := manager.Count(); got != 1 {
		t.Fatalf("Count after one remove = %d, want 1", got)
	}

	manager.Remove(second)
	if got := manager.Count(); got != 0 {
		t.Fatalf("Count after all removed = %d, want 0", got)
	}
}

func TestHandleActiveRequests_PreservesZeroCostMultiplier(t *testing.T) {
	t.Parallel()

	m := newActiveRequestManager()
	m.BeginAttempt(0, activeRequestAttempt{
		StartTime: time.Now(), Model: "m1", ClientIP: "1.2.3.4", Streaming: true,
		ChannelID: 10, ChannelName: "free-channel", APIKey: "sk-test",
		TokenID: 7, BaseURL: "https://upstream.example.com", CostMultiplier: 0,
	})

	s := &Server{activeRequests: m}

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/active_requests", nil))

	s.HandleActiveRequests(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d", w.Code, http.StatusOK)
	}

	var resp struct {
		Success bool             `json:"success"`
		Data    []map[string]any `json:"data"`
		Count   int              `json:"count"`
	}
	mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
	if !resp.Success || resp.Count != 1 || len(resp.Data) != 1 {
		t.Fatalf("unexpected resp: %+v", resp)
	}

	value, ok := resp.Data[0]["cost_multiplier"]
	if !ok {
		t.Fatalf("cost_multiplier missing in response: %+v", resp.Data[0])
	}
	if got, ok := value.(float64); !ok || got != 0 {
		t.Fatalf("cost_multiplier=%v, want 0", value)
	}
}
