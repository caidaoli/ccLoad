package app

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestActiveRequestManager_ListSnapshotAndSort(t *testing.T) {
	m := newActiveRequestManager()

	id1 := m.Register(time.UnixMilli(100), "m1", "1.1.1.1", false)
	id2 := m.Register(time.UnixMilli(200), "m2", "2.2.2.2", true)

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

func TestActiveRequestManager_UpdateMasksKey(t *testing.T) {
	m := newActiveRequestManager()

	id := m.Register(time.UnixMilli(100), "m", "1.1.1.1", false)
	rawKey := "sk-1234567890abcdef"
	m.Update(id, 1, "ch", "anthropic", rawKey, 0)

	got := m.List()
	if len(got) != 1 {
		t.Fatalf("expected 1 request, got %d", len(got))
	}
	if got[0].APIKeyUsed == rawKey {
		t.Fatalf("expected masked key, got raw")
	}
	if got[0].APIKeyUsed != "****" && !strings.Contains(got[0].APIKeyUsed, "...") {
		t.Fatalf("expected masked key format, got %q", got[0].APIKeyUsed)
	}
}

func TestActiveRequestManager_BytesAndFirstByteTime(t *testing.T) {
	m := newActiveRequestManager()

	id := m.Register(time.UnixMilli(100), "m", "1.1.1.1", true)

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
