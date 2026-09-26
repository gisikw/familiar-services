package scheduler

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // the service may run where the host zoneinfo is absent
)

// Rule is a legible recurrence: either an interval ("90m", "2h", "1d") or a
// wall-clock time on a set of weekdays ("day 06:00", "weekday 06:00",
// "weekend 09:30", "mon,wed,fri 17:00"). Wall-clock rules are evaluated in
// the scheduler's zone (FAMILIAR_TZ, default America/Chicago), so they stay at
// 06:00 local across DST changes.
type Rule struct {
	Every  time.Duration // interval rules
	Days   [7]bool       // wall-clock rules, indexed by time.Weekday
	Hour   int
	Minute int
	loc    *time.Location
}

// MinInterval keeps an interval rule from becoming a delivery storm.
const MinInterval = 5 * time.Minute

var dayNames = map[string]time.Weekday{"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday, "thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday}

// Zone is the location wall-clock rules are evaluated in.
func Zone() *time.Location {
	name := os.Getenv("FAMILIAR_TZ")
	if name == "" {
		name = "America/Chicago"
	}
	if loc, err := time.LoadLocation(name); err == nil {
		return loc
	}
	return time.UTC
}

func ParseRule(s string) (Rule, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return Rule{}, errors.New("empty rule")
	}
	fields := strings.Fields(s)
	if len(fields) == 1 {
		d, err := parseInterval(fields[0])
		if err != nil {
			return Rule{}, fmt.Errorf("rule %q: want an interval like 90m/2h/1d or DAYS HH:MM", s)
		}
		if d < MinInterval {
			return Rule{}, fmt.Errorf("interval must be at least %s", MinInterval)
		}
		return Rule{Every: d}, nil
	}
	if len(fields) != 2 {
		return Rule{}, fmt.Errorf("rule %q: want DAYS HH:MM (e.g. 'day 06:00', 'weekday 06:00', 'mon,thu 17:00')", s)
	}
	r := Rule{loc: Zone()}
	switch fields[0] {
	case "day", "daily", "everyday":
		for i := range r.Days {
			r.Days[i] = true
		}
	case "weekday", "weekdays":
		for d := time.Monday; d <= time.Friday; d++ {
			r.Days[d] = true
		}
	case "weekend", "weekends":
		r.Days[time.Saturday], r.Days[time.Sunday] = true, true
	default:
		for _, name := range strings.Split(fields[0], ",") {
			d, ok := dayNames[strings.TrimSpace(name)]
			if !ok {
				return Rule{}, fmt.Errorf("rule %q: unknown day %q", s, name)
			}
			r.Days[d] = true
		}
	}
	hm := strings.Split(fields[1], ":")
	if len(hm) != 2 {
		return Rule{}, fmt.Errorf("rule %q: time must be HH:MM", s)
	}
	h, e1 := strconv.Atoi(hm[0])
	m, e2 := strconv.Atoi(hm[1])
	if e1 != nil || e2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return Rule{}, fmt.Errorf("rule %q: time must be HH:MM (24h)", s)
	}
	r.Hour, r.Minute = h, m
	return r, nil
}

func parseInterval(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil || n <= 0 {
			return 0, errors.New("bad days")
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, errors.New("bad interval")
	}
	return d, nil
}

// First is the first occurrence strictly after now.
func (r Rule) First(now time.Time) time.Time {
	if r.Every > 0 {
		return now.Add(r.Every)
	}
	return r.wallAfter(now)
}

// After is the next occurrence after an occurrence due at `due`, delivered at
// `now`. It never returns a time at or before now: a late delivery (service
// down, DND, primary offline) fires once on catch-up, then resumes the
// cadence, rather than replaying every missed slot.
func (r Rule) After(due, now time.Time) time.Time {
	if r.Every > 0 {
		next := due.Add(r.Every)
		if !next.After(now) {
			// Keep the phase: skip whole missed intervals.
			missed := now.Sub(due)/r.Every + 1
			next = due.Add(missed * r.Every)
			for !next.After(now) {
				next = next.Add(r.Every)
			}
		}
		return next
	}
	from := due
	if now.After(from) {
		from = now
	}
	return r.wallAfter(from)
}

func (r Rule) wallAfter(t time.Time) time.Time {
	loc := r.loc
	if loc == nil {
		loc = Zone()
	}
	local := t.In(loc)
	for i := 0; i <= 8; i++ {
		day := local.AddDate(0, 0, i)
		c := time.Date(day.Year(), day.Month(), day.Day(), r.Hour, r.Minute, 0, 0, loc)
		if c.After(t) && r.Days[c.Weekday()] {
			return c
		}
	}
	return t.Add(24 * time.Hour) // unreachable for a rule with at least one day
}
