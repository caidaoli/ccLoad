package app

import (
	"sync"
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

func TestKeySelector_ModelRowsSkipCoolingPositionsWithoutBias(t *testing.T) {
	t.Parallel()
	selector := NewKeySelector()
	for _, want := range []int{0, 2, 0, 2} {
		got, ok := selector.SelectModelRow(7, "auto", []bool{true, false, true})
		if !ok || got != want {
			t.Fatalf("selected (%d, %v), want %d", got, ok, want)
		}
	}
	for _, want := range []int{0, 1, 2} {
		got, ok := selector.SelectModelRow(7, "auto", []bool{true, true, true})
		if !ok || got != want {
			t.Fatalf("after recovery selected (%d, %v), want %d", got, ok, want)
		}
	}
	if _, ok := selector.SelectModelRow(7, "auto", []bool{false, false}); ok {
		t.Fatal("all unavailable rows must not advance the cursor")
	}
}

func TestKeySelector_ModelRowsConcurrentBalance(t *testing.T) {
	t.Parallel()
	selector := NewKeySelector()
	const requests = 600
	selected := make(chan int, requests)
	var workers sync.WaitGroup
	for range requests {
		workers.Add(1)
		go func() {
			defer workers.Done()
			index, ok := selector.SelectModelRow(8, "auto", []bool{true, true, true})
			if !ok {
				selected <- -1
				return
			}
			selected <- index
		}()
	}
	workers.Wait()
	close(selected)
	var counts [3]int
	for index := range selected {
		if index < 0 || index >= len(counts) {
			t.Fatalf("invalid selected row %d", index)
		}
		counts[index]++
	}
	for index, count := range counts {
		if count != requests/len(counts) {
			t.Fatalf("row %d selected %d times, want %d", index, count, requests/len(counts))
		}
	}
}
