package app

import (
	"net/http"
	"testing"
	"time"
)

func TestHandleActiveRequests(t *testing.T) {
	t.Parallel()

	m := newActiveRequestManager()
	id := m.Register(time.Now(), "m1", "1.2.3.4", true)
	m.Update(id, 10, "ch", "openai", "sk-test", 7, 1.5) //nolint:gosec // 测试用假凭证
	m.AddBytes(id, 123)
	m.SetClientFirstByteTime(id, 50*time.Millisecond)

	s := &Server{activeRequests: m}

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/active_requests", nil))

	s.HandleActiveRequests(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d", w.Code, http.StatusOK)
	}

	var resp struct {
		Success bool            `json:"success"`
		Data    []ActiveRequest `json:"data"`
		Count   int             `json:"count"`
	}
	mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
	if !resp.Success || resp.Count != 1 || len(resp.Data) != 1 {
		t.Fatalf("unexpected resp: %+v", resp)
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
}

func TestActiveRequestManagerCount(t *testing.T) {
	t.Parallel()

	manager := newActiveRequestManager()
	if got := manager.Count(); got != 0 {
		t.Fatalf("Count on empty manager = %d, want 0", got)
	}

	first := manager.Register(time.Now(), "model-a", "127.0.0.1", true)
	second := manager.Register(time.Now(), "model-b", "127.0.0.1", false)
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
	id := m.Register(time.Now(), "m1", "1.2.3.4", true)
	m.Update(id, 10, "free-channel", "openai", "sk-test", 7, 0) //nolint:gosec // 测试用假凭证

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
