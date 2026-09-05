package model

import (
	"fmt"
	"strings"
	"time"
)

// DefaultScheduledCheckIntervalMinutes is the initial daily check interval.
const DefaultScheduledCheckIntervalMinutes = 300

// DefaultScheduledCheckStartTime starts the default schedule at local midnight.
const DefaultScheduledCheckStartTime = "00:00"

// ValidateScheduledCheckSchedule validates the persisted daily schedule contract.
func ValidateScheduledCheckSchedule(minutes int, start string) error {
	if minutes < 1 || minutes > 1440 {
		return fmt.Errorf("scheduled_check_interval_minutes must be an integer between 1 and 1440")
	}
	parsed, err := time.Parse("15:04", start)
	if err != nil || parsed.Format("15:04") != start {
		return fmt.Errorf("scheduled_check_start_time must use HH:MM (00:00–23:59)")
	}
	return nil
}

// NormalizeScheduledCheckSchedule supplies defaults for newly constructed configs.
func (c *Config) NormalizeScheduledCheckSchedule() error {
	if c.ScheduledCheckIntervalMinutes == 0 {
		c.ScheduledCheckIntervalMinutes = DefaultScheduledCheckIntervalMinutes
	}
	c.ScheduledCheckStartTime = strings.TrimSpace(c.ScheduledCheckStartTime)
	if c.ScheduledCheckStartTime == "" {
		c.ScheduledCheckStartTime = DefaultScheduledCheckStartTime
	}
	return ValidateScheduledCheckSchedule(c.ScheduledCheckIntervalMinutes, c.ScheduledCheckStartTime)
}

// ScheduledCheckDueAt uses wall-clock minutes, restarting the schedule each day.
func (c *Config) ScheduledCheckDueAt(now time.Time) bool {
	if c == nil || !c.Enabled || !c.ScheduledCheckEnabled || !c.IsAvailableAt(now) {
		return false
	}
	if ValidateScheduledCheckSchedule(c.ScheduledCheckIntervalMinutes, c.ScheduledCheckStartTime) != nil {
		return false
	}
	start, _ := time.Parse("15:04", c.ScheduledCheckStartTime)
	elapsed := now.Hour()*60 + now.Minute() - start.Hour()*60 - start.Minute()
	return elapsed >= 0 && elapsed%c.ScheduledCheckIntervalMinutes == 0
}
