package app

import (
	"strings"
	"testing"
)

func TestActiveRequestManager_ListSnapshotAndSort(t *testing.T) {
	m := newActiveRequestManager()

	id1 := m.Register("m1", "1.1.1.1", false)
	id2 := m.Register("m2", "2.2.2.2", true)

	// 人为制造可预测的开始时间，避免依赖 time.Sleep()
	m.mu.Lock()
	m.requests[id1].StartTime = 100
	m.requests[id2].StartTime = 200
	m.mu.Unlock()

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

	id := m.Register("m", "1.1.1.1", false)
	rawKey := "sk-1234567890abcdef"
	m.Update(id, 1, "ch", rawKey)

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
