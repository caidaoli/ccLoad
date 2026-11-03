package privacy

import "time"

// SetClock lets tests age mappings without sleeping.
func SetClock(e *Engine, now func() time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.now = now
}
