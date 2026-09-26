package scheduler

import (
	"strings"
	"testing"
	"time"
)

func chicago(t *testing.T, s string) time.Time {
	t.Helper()
	loc, _ := time.LoadLocation("America/Chicago")
	v, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestParseRule(t *testing.T) {
	t.Setenv("FAMILIAR_TZ", "America/Chicago")
	for _, ok := range []string{"day 06:00", "Daily 6:05", "weekday 06:00", "weekend 09:30", "mon,wed,fri 17:00", "2h", "90m", "1d"} {
		if _, err := ParseRule(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "1m", "day", "day 25:00", "funday 06:00", "day 06", "every day 06:00", "-2h"} {
		if _, err := ParseRule(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestWallClockRulesHoldAcrossDST(t *testing.T) {
	t.Setenv("FAMILIAR_TZ", "America/Chicago")
	r, _ := ParseRule("day 06:00")
	// US DST ends 2026-11-01 02:00 CDT: 06:00 local both days, 25h apart.
	first := r.First(chicago(t, "2026-10-31 07:00"))
	if got := first.In(first.Location()).Format("2006-01-02 15:04 MST"); got != "2026-11-01 06:00 CST" {
		t.Fatalf("first after DST end = %s", got)
	}
	prev := chicago(t, "2026-10-31 06:00")
	if d := first.Sub(prev); d != 25*time.Hour {
		t.Fatalf("06:00 → 06:00 across fall-back = %s, want 25h", d)
	}
	wd, _ := ParseRule("weekday 06:00")
	// Friday 07:00 → Monday 06:00.
	if got := wd.First(chicago(t, "2026-10-02 07:00")).Format("Mon 15:04"); got != "Mon 06:00" {
		t.Fatalf("weekday after Friday = %s", got)
	}
}

func TestIntervalCatchUpFiresOnceAndKeepsPhase(t *testing.T) {
	r, _ := ParseRule("2h")
	due := time.Date(2026, 9, 26, 6, 0, 0, 0, time.UTC)
	// Delivered 7h late: next is the first phase slot after now, not a backlog.
	next := r.After(due, due.Add(7*time.Hour))
	if want := due.Add(8 * time.Hour); !next.Equal(want) {
		t.Fatalf("catch-up next = %s, want %s", next, want)
	}
	if n := r.After(due, due.Add(time.Minute)); !n.Equal(due.Add(2 * time.Hour)) {
		t.Fatalf("on-time next = %s", n)
	}
}

func TestRecurringSeriesEnqueuesNextOnDeliveryAndCancels(t *testing.T) {
	t.Setenv("FAMILIAR_TZ", "America/Chicago")
	s := openTest(t)
	clock := chicago(t, "2026-09-26 05:00")
	s.now = func() time.Time { return clock }
	e, _, err := s.Enqueue(Enqueue{ID: "brief", Target: "instance:a", Summary: "briefing", Rule: "day 06:00", Type: "fork"})
	if err != nil {
		t.Fatal(err)
	}
	if want := chicago(t, "2026-09-26 06:00").UnixMilli(); e.DueAt != want || e.Series != "brief" {
		t.Fatalf("first occurrence: %#v", e)
	}
	// The primary was offline for two days: one catch-up delivery, no storm.
	clock = chicago(t, "2026-09-28 09:00")
	got, err := s.Claim("instance:a", clock.UnixMilli())
	if err != nil || got == nil || got.ID != "brief" {
		t.Fatalf("claim: %#v %v", got, err)
	}
	if again, _ := s.Claim("instance:a", clock.UnixMilli()); again != nil {
		t.Fatalf("storm: second claim %#v", again)
	}
	list, _ := s.List("instance:a")
	var pending []Event
	for _, x := range list {
		if x.State == "pending" {
			pending = append(pending, x)
		}
	}
	if len(pending) != 1 || pending[0].DueAt != chicago(t, "2026-09-29 06:00").UnixMilli() || pending[0].Series != "brief" || pending[0].Rule != "day 06:00" {
		t.Fatalf("next occurrence: %#v", pending)
	}
	// Redelivery after a reconnect must not mint a second next occurrence.
	if err := s.Requeue("instance:a"); err != nil {
		t.Fatal(err)
	}
	if re, _ := s.Claim("instance:a", clock.UnixMilli()); re == nil || re.ID != "brief" {
		t.Fatalf("redelivery: %#v", re)
	}
	list, _ = s.List("instance:a")
	n := 0
	for _, x := range list {
		if x.State == "pending" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("redelivery duplicated the series: %d pending", n)
	}
	// Cancelling by the series id stops everything outstanding.
	if ok, err := s.Cancel("brief"); err != nil || !ok {
		t.Fatalf("cancel: %v %v", ok, err)
	}
	if list, _ = s.List("*"); len(list) != 0 {
		t.Fatalf("after cancel: %#v", list)
	}
}

func TestForkEventsPassDNDAndRequireInstance(t *testing.T) {
	s := openTest(t)
	if _, _, err := s.Enqueue(Enqueue{Target: "spawn:x.service", Summary: "f", Type: "fork"}); err == nil || !strings.Contains(err.Error(), "instance") {
		t.Fatalf("fork to spawn target: %v", err)
	}
	if _, err := s.DNDSet("instance:a", true, "user", nil); err != nil {
		t.Fatal(err)
	}
	_, _, _ = s.Enqueue(Enqueue{ID: "wake1", Target: "instance:a", Summary: "loud"})
	_, _, _ = s.Enqueue(Enqueue{ID: "fork1", Target: "instance:a", Summary: "background", Type: "fork"})
	got, err := s.Claim("instance:a", time.Now().UnixMilli()+1000)
	if err != nil || got == nil || got.ID != "fork1" {
		t.Fatalf("under DND want the fork, got %#v %v", got, err)
	}
	if more, _ := s.Claim("instance:a", time.Now().UnixMilli()+1000); more != nil {
		t.Fatalf("DND leaked a wake: %#v", more)
	}
}

func TestListStarSeesEveryTarget(t *testing.T) {
	s := openTest(t)
	_, _, _ = s.Enqueue(Enqueue{Target: "instance:a", Summary: "a"})
	_, _, _ = s.Enqueue(Enqueue{Target: "instance:b", Summary: "b"})
	if all, _ := s.List("*"); len(all) != 2 {
		t.Fatalf("list *: %d", len(all))
	}
}
