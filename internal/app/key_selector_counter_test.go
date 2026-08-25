package app

import (
	"testing"

	"ccLoad/internal/model"
)

func TestKeySelector_RemoveChannelCounter(t *testing.T) {
	t.Parallel()

	ks := NewKeySelector()
	firstScope := newRRCounterScope(123, []*model.APIKey{{KeyIndex: 0}, {KeyIndex: 1}})
	secondScope := newRRCounterScope(123, []*model.APIKey{{KeyIndex: 2}, {KeyIndex: 3}})
	otherScope := newRRCounterScope(456, []*model.APIKey{{KeyIndex: 0}, {KeyIndex: 1}})
	_ = ks.getOrCreateCounter(firstScope)
	_ = ks.getOrCreateCounter(secondScope)
	_ = ks.getOrCreateCounter(otherScope)

	ks.rrMutex.RLock()
	_, firstExists := ks.rrCounters[firstScope]
	_, secondExists := ks.rrCounters[secondScope]
	ks.rrMutex.RUnlock()
	if !firstExists || !secondExists {
		t.Fatal("expected both channel counters to exist before removal")
	}

	ks.RemoveChannelCounter(123)

	ks.rrMutex.RLock()
	_, firstExists = ks.rrCounters[firstScope]
	_, secondExists = ks.rrCounters[secondScope]
	_, otherExists := ks.rrCounters[otherScope]
	ks.rrMutex.RUnlock()
	if firstExists || secondExists {
		t.Fatal("expected every counter for the removed channel to be deleted")
	}
	if !otherExists {
		t.Fatal("expected counters for other channels to remain")
	}
}
