package app

import (
	"math"
	"testing"
	"time"
)

func TestCostCache_CheckAndResetIfNewDay(t *testing.T) {
	c := NewCostCache()

	c.mu.Lock()
	c.costs[1] = 9.9
	tomorrow := c.dayStart.AddDate(0, 0, 1).Add(time.Hour)
	c.checkAndResetIfNewDay(tomorrow)
	if len(c.costs) != 0 {
		c.mu.Unlock()
		t.Fatalf("expected reset costs on new day, got len=%d", len(c.costs))
	}
	if !c.dayStart.Equal(todayStart(tomorrow)) {
		c.mu.Unlock()
		t.Fatalf("dayStart not updated: got=%v want=%v", c.dayStart, todayStart(tomorrow))
	}
	c.mu.Unlock()
}

func TestCostCache_Add_Get_GetAll_CrossDayBehavior(t *testing.T) {
	c := NewCostCache()

	// 伪造“跨天”：把 dayStart 回退到昨天，并填充一些旧数据。
	c.mu.Lock()
	c.dayStart = todayStart(time.Now().AddDate(0, 0, -1))
	c.costs = map[int64]float64{1: 9.9, 2: 1.1}
	c.mu.Unlock()

	if got := c.Get(1); got != 0 {
		t.Fatalf("Get() should return 0 after day boundary, got %v", got)
	}
	if got := c.GetAll(); len(got) != 0 {
		t.Fatalf("GetAll() should return empty after day boundary, got len=%d", len(got))
	}

	// Add() 会在写锁下重置并累加。
	c.Add(1, -1) // 不应影响
	c.Add(1, 1.25)

	if got := c.Get(1); math.Abs(got-1.25) > 1e-9 {
		t.Fatalf("Get() after Add = %v, want 1.25", got)
	}
	all := c.GetAll()
	if len(all) != 1 {
		t.Fatalf("GetAll() expected len=1, got len=%d", len(all))
	}
	if math.Abs(all[1]-1.25) > 1e-9 {
		t.Fatalf("GetAll()[1] = %v, want 1.25", all[1])
	}
}
