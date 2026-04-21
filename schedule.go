// schedule.go — time-based recording schedule evaluator
//
// A ScheduleConfig holds one or more ScheduleRules.  Recording is active
// whenever ANY rule matches the current wall-clock time (OR logic).
//
// Each rule can express:
//   - A daily time window (start_time … stop_time, "HH:MM" 24-hour)
//   - Day-of-week filtering (weekdays list; empty = every day)
//   - An absolute date range (from_date … to_date, "YYYY-MM-DD"; empty = unbounded)
//   - Midnight-spanning windows (e.g. "22:00" → "02:00")
//
// The schedule is evaluated in the timezone specified by ScheduleConfig.Timezone
// (IANA name, e.g. "Europe/London").  If the timezone is empty or invalid,
// UTC is used.
package main

import (
	"fmt"
	"log"
	"time"
)

// ScheduleRule defines one time window within a schedule.
type ScheduleRule struct {
	// Weekdays is the list of days-of-week this rule applies to.
	// 0 = Sunday, 1 = Monday, … 6 = Saturday.
	// An empty slice means every day of the week.
	Weekdays []int `json:"weekdays,omitempty"`

	// StartTime and StopTime are "HH:MM" strings in 24-hour format.
	// StopTime may be earlier than StartTime to express a midnight-spanning
	// window (e.g. "22:00" → "02:00").
	StartTime string `json:"start_time"` // e.g. "06:30"
	StopTime  string `json:"stop_time"`  // e.g. "08:00"

	// FromDate and ToDate are optional absolute date bounds in "YYYY-MM-DD"
	// format.  An empty string means no bound in that direction.
	// The rule only fires on dates within [FromDate, ToDate] (inclusive).
	FromDate string `json:"from_date,omitempty"` // e.g. "2026-04-21"
	ToDate   string `json:"to_date,omitempty"`   // e.g. "2026-04-25"
}

// ScheduleConfig holds the full schedule for a channel.
type ScheduleConfig struct {
	Enabled  bool           `json:"enabled"`
	Timezone string         `json:"timezone,omitempty"` // IANA tz, e.g. "Europe/London"; "" = UTC
	Rules    []ScheduleRule `json:"rules,omitempty"`
}

// location returns the time.Location for this schedule, falling back to UTC.
func (sc *ScheduleConfig) location() *time.Location {
	if sc.Timezone == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(sc.Timezone)
	if err != nil {
		log.Printf("[schedule] unknown timezone %q, using UTC: %v", sc.Timezone, err)
		return time.UTC
	}
	return loc
}

// Active reports whether the schedule is currently active at time t.
// Returns false if the schedule is disabled or has no rules.
func (sc *ScheduleConfig) Active(t time.Time) bool {
	if !sc.Enabled || len(sc.Rules) == 0 {
		return false
	}
	loc := sc.location()
	local := t.In(loc)
	for _, r := range sc.Rules {
		if r.matches(local) {
			return true
		}
	}
	return false
}

// NextTransition returns the next time the schedule will change state
// (active→inactive or inactive→active) after t, and whether it will be
// active at that transition.  It searches up to 8 days ahead.
// Returns zero time if no transition is found within that window.
func (sc *ScheduleConfig) NextTransition(t time.Time) (next time.Time, willBeActive bool) {
	if !sc.Enabled || len(sc.Rules) == 0 {
		return time.Time{}, false
	}
	loc := sc.location()
	current := sc.Active(t)

	// Search in 1-minute steps for up to 8 days.
	probe := t.Truncate(time.Minute).Add(time.Minute)
	limit := t.Add(8 * 24 * time.Hour)
	for probe.Before(limit) {
		active := sc.Active(probe)
		if active != current {
			return probe.In(loc), active
		}
		probe = probe.Add(time.Minute)
	}
	return time.Time{}, false
}

// parseHHMM parses "HH:MM" into (hour, minute).
func parseHHMM(s string) (int, int, error) {
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
		return 0, 0, fmt.Errorf("invalid time %q: %w", s, err)
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("time %q out of range", s)
	}
	return h, m, nil
}

// parseDate parses "YYYY-MM-DD" into a time.Time at midnight in the given location.
func parseDate(s string, loc *time.Location) (time.Time, error) {
	t, err := time.ParseInLocation("2006-01-02", s, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid date %q: %w", s, err)
	}
	return t, nil
}

// matches reports whether this rule is active at local time t.
// t must already be in the schedule's local timezone.
func (r *ScheduleRule) matches(t time.Time) bool {
	// ── 1. Absolute date range check ──────────────────────────────────────────
	loc := t.Location()
	if r.FromDate != "" {
		from, err := parseDate(r.FromDate, loc)
		if err == nil {
			// Compare date only (midnight of from_date).
			dayStart := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
			if dayStart.Before(from) {
				return false
			}
		}
	}
	if r.ToDate != "" {
		to, err := parseDate(r.ToDate, loc)
		if err == nil {
			// Include the entire to_date day (up to 23:59:59).
			toEnd := to.Add(24*time.Hour - time.Second)
			if t.After(toEnd) {
				return false
			}
		}
	}

	// ── 2. Day-of-week check ──────────────────────────────────────────────────
	if len(r.Weekdays) > 0 {
		wd := int(t.Weekday()) // 0=Sun … 6=Sat
		found := false
		for _, d := range r.Weekdays {
			if d == wd {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	// ── 3. Time-of-day window check ───────────────────────────────────────────
	if r.StartTime == "" || r.StopTime == "" {
		// No time window specified — rule matches all day (if date/weekday passed).
		return true
	}

	startH, startM, err1 := parseHHMM(r.StartTime)
	stopH, stopM, err2 := parseHHMM(r.StopTime)
	if err1 != nil || err2 != nil {
		return false
	}

	// Convert current time to minutes-since-midnight for easy comparison.
	nowMins := t.Hour()*60 + t.Minute()
	startMins := startH*60 + startM
	stopMins := stopH*60 + stopM

	if startMins <= stopMins {
		// Normal window: e.g. 06:00 → 08:00
		return nowMins >= startMins && nowMins < stopMins
	}
	// Midnight-spanning window: e.g. 22:00 → 02:00
	// Active when: nowMins >= startMins OR nowMins < stopMins
	return nowMins >= startMins || nowMins < stopMins
}

// Validate checks the schedule for obvious errors and returns a human-readable
// error string, or "" if valid.
func (sc *ScheduleConfig) Validate() string {
	for i, r := range sc.Rules {
		if r.StartTime != "" {
			if _, _, err := parseHHMM(r.StartTime); err != nil {
				return fmt.Sprintf("rule %d: %v", i, err)
			}
		}
		if r.StopTime != "" {
			if _, _, err := parseHHMM(r.StopTime); err != nil {
				return fmt.Sprintf("rule %d: %v", i, err)
			}
		}
		if r.FromDate != "" {
			if _, err := parseDate(r.FromDate, time.UTC); err != nil {
				return fmt.Sprintf("rule %d: %v", i, err)
			}
		}
		if r.ToDate != "" {
			if _, err := parseDate(r.ToDate, time.UTC); err != nil {
				return fmt.Sprintf("rule %d: %v", i, err)
			}
		}
		for _, wd := range r.Weekdays {
			if wd < 0 || wd > 6 {
				return fmt.Sprintf("rule %d: weekday %d out of range (0–6)", i, wd)
			}
		}
	}
	if sc.Timezone != "" {
		if _, err := time.LoadLocation(sc.Timezone); err != nil {
			return fmt.Sprintf("unknown timezone %q", sc.Timezone)
		}
	}
	return ""
}
