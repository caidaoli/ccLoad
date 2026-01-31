package app

import "testing"

func TestKeySelector_RemoveChannelCounter(t *testing.T) {
	t.Parallel()

	ks := NewKeySelector()
	_ = ks.getOrCreateCounter(123)

	ks.rrMutex.RLock()
	_, okBefore := ks.rrCounters[123]
	ks.rrMutex.RUnlock()
	if !okBefore {
		t.Fatal("expected counter to exist before removal")
	}

	ks.RemoveChannelCounter(123)

	ks.rrMutex.RLock()
	_, okAfter := ks.rrCounters[123]
	ks.rrMutex.RUnlock()
	if okAfter {
		t.Fatal("expected counter to be removed")
	}
}
