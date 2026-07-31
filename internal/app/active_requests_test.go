package app

import (
	"math"
	"strings"
	"testing"
	"time"
)

func beginTestActiveRequest(m *activeRequestManager, start time.Time, model, clientIP string, streaming bool) int64 {
	return m.BeginAttempt(0, activeRequestAttempt{
		StartTime: start, Model: model, ClientIP: clientIP, Streaming: streaming,
		ChannelID: 1, ChannelName: "test-channel", APIKey: "sk-test", BaseURL: "https://upstream.example.com", CostMultiplier: 1,
	})
}

func TestActiveRequestManager_ListSnapshotAndSort(t *testing.T) {
	m := newActiveRequestManager()

	id1 := beginTestActiveRequest(m, time.UnixMilli(100), "m1", "1.1.1.1", false)
	id2 := beginTestActiveRequest(m, time.UnixMilli(200), "m2", "2.2.2.2", true)

	got := m.List()
	if len(got) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(got))
	}
	if got[0].ID != id2 || got[1].ID != id1 {
		t.Fatalf("expected order [%d,%d], got [%d,%d]", id2, id1, got[0].ID, got[1].ID)
	}

	// List() 必须返回快照：改返回值不应影响内部状态
	got[0].Model = "hacked"
	got2 := m.List()
	if got2[0].Model != "m2" {
		t.Fatalf("expected snapshot copy, got model=%q", got2[0].Model)
	}
}

func TestActiveRequestManager_BeginAttemptMasksKey(t *testing.T) {
	m := newActiveRequestManager()

	id := beginTestActiveRequest(m, time.UnixMilli(100), "m", "1.1.1.1", false)
	m.SetUpstreamWebsocket(id, true)
	rawKey := "sk-1234567890abcdef"
	m.BeginAttempt(id, activeRequestAttempt{
		StartTime: time.UnixMilli(200), Model: "m", ClientIP: "1.1.1.1",
		ChannelID: 1, ChannelName: "ch", APIKey: rawKey, BaseURL: "https://upstream.example.com", CostMultiplier: 1,
	})

	got := m.List()
	if len(got) != 1 {
		t.Fatalf("expected 1 request, got %d", len(got))
	}
	if got[0].APIKeyUsed == rawKey {
		t.Fatalf("expected masked key, got raw")
	}
	if got[0].APIKeyUsed != "****" && !strings.Contains(got[0].APIKeyUsed, ".") {
		t.Fatalf("expected masked key format, got %q", got[0].APIKeyUsed)
	}
	if got[0].UpstreamWebsocket {
		t.Fatal("channel/key update must reset upstream websocket state")
	}
}

func TestActiveRequestManager_UpstreamStatusTransitions(t *testing.T) {
	m := newActiveRequestManager()

	id := beginTestActiveRequest(m, time.UnixMilli(100), "m", "1.1.1.1", true)
	if got := m.List()[0].UpstreamStatus; got != activeRequestStatusRequesting {
		t.Fatalf("status after register=%q, want %q", got, activeRequestStatusRequesting)
	}

	m.AddBytes(id, 10)
	if got := m.List()[0].UpstreamStatus; got != activeRequestStatusReceiving {
		t.Fatalf("status after receiving bytes=%q, want %q", got, activeRequestStatusReceiving)
	}

	m.Retry(id)
	if got := m.List()[0].UpstreamStatus; got != activeRequestStatusRetrying {
		t.Fatalf("status before retry=%q, want %q", got, activeRequestStatusRetrying)
	}

	m.BeginAttempt(id, activeRequestAttempt{
		StartTime: time.UnixMilli(200), Model: "m", ClientIP: "1.1.1.1", Streaming: true,
		ChannelID: 1, ChannelName: "ch", APIKey: "sk-test", BaseURL: "https://upstream.example.com", CostMultiplier: 1,
	})
	if got := m.List()[0].UpstreamStatus; got != activeRequestStatusRetrying {
		t.Fatalf("status during retry=%q, want %q", got, activeRequestStatusRetrying)
	}
}

func TestActiveRequestManager_BytesAndFirstByteTime(t *testing.T) {
	m := newActiveRequestManager()

	id := beginTestActiveRequest(m, time.UnixMilli(100), "m", "1.1.1.1", true)

	m.AddBytes(id, 10)
	m.AddBytes(id, 0) // no-op

	m.SetClientFirstByteTime(id, -1*time.Second)        // must not poison the value
	m.SetClientFirstByteTime(id, 750*time.Millisecond)  // first set wins
	m.SetClientFirstByteTime(id, 1250*time.Millisecond) // ignored

	got := m.List()
	if len(got) != 1 {
		t.Fatalf("expected 1 request, got %d", len(got))
	}
	if got[0].BytesReceived != 10 {
		t.Fatalf("expected bytes_received=10, got %d", got[0].BytesReceived)
	}
	if math.Abs(got[0].ClientFirstByteTime-0.75) > 1e-6 {
		t.Fatalf("expected client_first_byte_time≈0.75, got %f", got[0].ClientFirstByteTime)
	}
}
