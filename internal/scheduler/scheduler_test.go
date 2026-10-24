package scheduler

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, e := Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func TestDedupeAndLifecycle(t *testing.T) {
	s := openTest(t)
	p := 1
	e, created, err := s.Enqueue(Enqueue{ID: "stable", Target: "instance:a", Priority: &p, Summary: "first"})
	if err != nil || !created {
		t.Fatal(err)
	}
	again, created, err := s.Enqueue(Enqueue{ID: "stable", Target: "instance:b", Summary: "second"})
	if err != nil || created || again.Summary != "first" {
		t.Fatalf("dedupe: %#v %v", again, err)
	}
	got, err := s.Claim("instance:a", time.Now().UnixMilli())
	if err != nil || got == nil || got.ID != e.ID || got.State != "delivered" {
		t.Fatalf("claim: %#v %v", got, err)
	}
	if ok, err := s.Ack(e.ID, "instance:b"); err != nil || ok {
		t.Fatalf("cross-target ack: %v %v", ok, err)
	}
	if ok, err := s.Ack(e.ID, "instance:a"); err != nil || !ok {
		t.Fatalf("ack: %v %v", ok, err)
	}
}
func TestMergeValidation(t *testing.T) {
	s := openTest(t)
	valid := `{"summary":"I finished","forkSessionId":"fork","forkSessionFile":"/tmp/fork.jsonl","branchEntryId":"branch","firstEntryId":"first","lastEntryId":"last","mergedAt":"2026-01-01T00:00:00Z","turnCount":2,"forkedFurther":false}`
	if _, _, err := s.Enqueue(Enqueue{Target: "instance:parent", Type: "merge", Summary: "I finished", Body: valid}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Enqueue(Enqueue{Target: "instance:parent", Type: "merge", Summary: "bad", Body: `{}`}); err == nil {
		t.Fatal("incomplete merge accepted")
	}
}

func TestDNDHoldsThenReleases(t *testing.T) {
	s := openTest(t)
	_, _, _ = s.Enqueue(Enqueue{ID: "held", Target: "instance:a", Summary: "wait"})
	dur := int64(time.Hour / time.Millisecond)
	if _, err := s.DNDSet("instance:a", true, "familiar", &dur); err != nil {
		t.Fatal(err)
	}
	if e, err := s.Claim("instance:a", time.Now().UnixMilli()); err != nil || e != nil {
		t.Fatalf("delivered under DND: %#v %v", e, err)
	}
	_, _, _ = s.Enqueue(Enqueue{ID: "soft", Target: "instance:a", Summary: "quiet", Urgency: "soft"})
	if e, err := s.Claim("instance:a", time.Now().UnixMilli()); err != nil || e == nil || e.ID != "soft" || e.Urgency != "soft" {
		t.Fatalf("soft event did not pass DND: %#v %v", e, err)
	}
	if _, err := s.DNDSet("instance:a", false, "familiar", nil); err != nil {
		t.Fatal(err)
	}
	if e, err := s.Claim("instance:a", time.Now().UnixMilli()); err != nil || e == nil || e.ID != "held" {
		t.Fatalf("not released: %#v %v", e, err)
	}
}
func TestUrgencyValidationAndDefault(t *testing.T) {
	s := openTest(t)
	e, _, err := s.Enqueue(Enqueue{Target: "instance:a", Summary: "default"})
	if err != nil || e.Urgency != "wake" {
		t.Fatalf("default: %#v %v", e, err)
	}
	if _, _, err = s.Enqueue(Enqueue{Target: "instance:a", Summary: "bad", Urgency: "now"}); err == nil {
		t.Fatal("invalid urgency accepted")
	}
}

func TestRoutingAndReconnectRequeue(t *testing.T) {
	s := openTest(t)
	_, _, _ = s.Enqueue(Enqueue{ID: "a-only", Target: "instance:a", Summary: "a"})
	if e, _ := s.Claim("instance:b", time.Now().UnixMilli()); e != nil {
		t.Fatalf("wrong target: %#v", e)
	}
	e, _ := s.Claim("instance:a", time.Now().UnixMilli())
	if e == nil {
		t.Fatal("not delivered")
	}
	if next, _ := s.Claim("instance:a", time.Now().UnixMilli()); next != nil {
		t.Fatal("redelivered without reconnect")
	}
	if err := s.Requeue("instance:a"); err != nil {
		t.Fatal(err)
	}
	if next, _ := s.Claim("instance:a", time.Now().UnixMilli()); next == nil || next.ID != "a-only" {
		t.Fatalf("not redelivered: %#v", next)
	}
}
func TestFamiliarDNDCapped(t *testing.T) {
	s := openTest(t)
	day := int64(24 * time.Hour / time.Millisecond)
	d, err := s.DNDSet("instance:a", true, "familiar", &day)
	if err != nil {
		t.Fatal(err)
	}
	if d.ExpiresAt-d.SetAt != int64(2*time.Hour/time.Millisecond) {
		t.Fatalf("not capped: %#v", d)
	}
}
func TestMigrateLegacyFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "worklist", "items"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "wakes", "pending"), 0700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "worklist", "items", "live.json"), []byte(`{"id":"live","ts":10,"priority":2,"type":"question","summary":"hello","body":"detail","source":"golem"}`), 0600)
	os.WriteFile(filepath.Join(root, "wakes", "pending", "wake-1.json"), []byte(`{"version":1,"id":"wake-1","mode":"always","reason":"check it","scheduledAt":10,"fireAt":20}`), 0600)
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	n, err := s.Migrate(root, "session-1")
	if err != nil || n != 2 {
		t.Fatalf("migration: %d %v", n, err)
	}
	events, err := s.List("instance:session-1")
	if err != nil || len(events) != 2 {
		t.Fatalf("events: %#v %v", events, err)
	}
	n, err = s.Migrate(root, "session-1")
	if err != nil || n != 0 {
		t.Fatalf("rerun: %d %v", n, err)
	}
}

func mergeBody(fork string) string {
	return `{"summary":"home","forkSessionId":"` + fork + `","forkSessionFile":"/tmp/f.jsonl","branchEntryId":"b","firstEntryId":"f","lastEntryId":"l","mergedAt":"2026-01-01T00:00:00Z","turnCount":1,"forkedFurther":false}`
}

func TestMergedForkForwarding(t *testing.T) {
	s := openTest(t)
	now := time.Now().UnixMilli()
	// Waiting for the fork before it merges: one pending, one delivered to a Pi about to exit.
	s.Enqueue(Enqueue{ID: "pending", Target: "instance:fork", Summary: "agent settled"})
	s.Enqueue(Enqueue{ID: "inflight", Target: "instance:fork", Summary: "agent blocked"})
	if got, _ := s.Claim("instance:fork", now); got == nil {
		t.Fatal("setup claim")
	}
	if _, _, err := s.Enqueue(Enqueue{ID: "m1", Target: "instance:parent", Type: "merge", Summary: "home", Body: mergeBody("fork")}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"pending", "inflight"} {
		e, _ := s.Get(id)
		if e.Target != "instance:parent" || e.State != "pending" || e.Summary[:17] != "(for merged fork " {
			t.Fatalf("%s not forwarded: %#v", id, e)
		}
	}
	// Arriving after the merge.
	late, _, err := s.Enqueue(Enqueue{Target: "fork", Summary: "late settle"})
	if err != nil || late.Target != "instance:parent" || late.Summary != "(for merged fork fork) late settle" {
		t.Fatalf("late event: %#v %v", late, err)
	}
	// A fork of the merged fork comes home to the grandparent, and forwards through it.
	if _, _, err := s.Enqueue(Enqueue{ID: "m2", Target: "instance:fork", Type: "merge", Summary: "home", Body: mergeBody("grandchild")}); err != nil {
		t.Fatal(err)
	}
	if m2, _ := s.Get("m2"); m2.Target != "instance:parent" {
		t.Fatalf("grandchild merge not forwarded: %s", m2.Target)
	}
	if r, _ := s.Resolve("instance:grandchild"); r != "instance:parent" {
		t.Fatalf("resolve grandchild: %s", r)
	}
	// Live instances resolve to themselves.
	if r, _ := s.Resolve("instance:someone"); r != "instance:someone" {
		t.Fatalf("resolve live: %s", r)
	}
}

func TestDoDisturbWakesQuietMerges(t *testing.T) {
	s := openTest(t)
	body := `{"summary":"done","forkSessionId":"fork","forkSessionFile":"/tmp/fork.jsonl","branchEntryId":"branch","firstEntryId":"first","lastEntryId":"last","mergedAt":"2026-01-01T00:00:00Z","turnCount":2,"forkedFurther":false}`
	day := int64(30 * time.Hour / time.Millisecond)
	d, err := s.DNDSetMode("instance:a", true, "familiar", DNDDisturb, &day)
	if err != nil || d.Mode != DNDDisturb || d.ExpiresAt-d.SetAt != int64(24*time.Hour/time.Millisecond) {
		t.Fatalf("set disturb: %#v %v", d, err)
	}
	if got, _ := s.DNDGet("instance:a"); got == nil || got.Mode != DNDDisturb {
		t.Fatalf("get disturb: %#v", got)
	}
	// Disturb is not DND: wakes pass.
	_, _, _ = s.Enqueue(Enqueue{ID: "wake", Target: "instance:a", Summary: "wake"})
	if e, _ := s.Claim("instance:a", time.Now().UnixMilli()); e == nil || e.ID != "wake" {
		t.Fatalf("wake held under disturb: %#v", e)
	}
	_, _, _ = s.Enqueue(Enqueue{ID: "merge", Target: "instance:a", Type: "merge", Summary: "done", Body: body, Urgency: "soft"})
	if e, _ := s.Claim("instance:a", time.Now().UnixMilli()); e == nil || e.ID != "merge" || e.Urgency != "wake" {
		t.Fatalf("quiet merge not upgraded: %#v", e)
	}
	if e, _ := s.Get("merge"); e.Urgency != "wake" {
		t.Fatalf("upgrade not stored: %#v", e)
	}
	// Other soft notes stay soft.
	_, _, _ = s.Enqueue(Enqueue{ID: "note", Target: "instance:a", Summary: "fyi", Urgency: "soft"})
	if e, _ := s.Claim("instance:a", time.Now().UnixMilli()); e == nil || e.Urgency != "soft" {
		t.Fatalf("soft note changed: %#v", e)
	}
	// Kev's DND replaces it; quiet merges stay quiet.
	if d, _ := s.DNDSet("instance:a", true, "user", nil); d.Mode != DNDQuiet {
		t.Fatalf("dnd did not replace disturb: %#v", d)
	}
	_, _, _ = s.Enqueue(Enqueue{ID: "merge2", Target: "instance:a", Type: "merge", Summary: "done", Body: body, Urgency: "soft"})
	if e, _ := s.Claim("instance:a", time.Now().UnixMilli()); e == nil || e.Urgency != "soft" {
		t.Fatalf("merge upgraded under DND: %#v", e)
	}
	if _, err := s.DNDSetMode("instance:a", true, "familiar", "loud", nil); err == nil {
		t.Fatal("bad mode accepted")
	}
}

// Oct 24: ~25 soft notifies queued ahead of two quiet merges under Do Disturb
// kept the merges waiting ~10 min. Turn starters now go first.
func TestWakesClaimedBeforeSoftBacklog(t *testing.T) {
	s := openTest(t)
	body := `{"summary":"done","forkSessionId":"fork","forkSessionFile":"/tmp/fork.jsonl","branchEntryId":"branch","firstEntryId":"first","lastEntryId":"last","mergedAt":"2026-01-01T00:00:00Z","turnCount":2,"forkedFurther":false}`
	now := time.Now().UnixMilli()
	for i := 0; i < 3; i++ {
		_, _, _ = s.Enqueue(Enqueue{ID: fmt.Sprintf("note%d", i), Target: "instance:a", Summary: "fyi", Urgency: "soft", DueAt: now - 60_000 + int64(i)})
	}
	_, _, _ = s.Enqueue(Enqueue{ID: "merge", Target: "instance:a", Type: "merge", Summary: "done", Body: body, Urgency: "soft"})
	_, _, _ = s.Enqueue(Enqueue{ID: "wake", Target: "instance:a", Summary: "wake"})
	// Without Do Disturb the wake jumps the notes; the quiet merge stays in line.
	if e, _ := s.Claim("instance:a", now+1000); e == nil || e.ID != "wake" {
		t.Fatalf("wake not first: %#v", e)
	}
	if e, _ := s.Claim("instance:a", now+1000); e == nil || e.ID != "note0" {
		t.Fatalf("soft order changed: %#v", e)
	}
	day := int64(time.Hour / time.Millisecond)
	if _, err := s.DNDSetMode("instance:a", true, "familiar", DNDDisturb, &day); err != nil {
		t.Fatal(err)
	}
	// While wakes are being spaced, only non-turn-starters are claimed.
	if e, _ := s.ClaimNext("instance:a", now+1000, true); e == nil || e.ID != "note1" {
		t.Fatalf("softOnly claimed: %#v", e)
	}
	// Under Do Disturb the quiet merge is a turn starter and goes next.
	if e, _ := s.Claim("instance:a", now+1000); e == nil || e.ID != "merge" || e.Urgency != "wake" {
		t.Fatalf("merge not ahead of notes: %#v", e)
	}
	if e, _ := s.ClaimNext("instance:a", now+1000, true); e == nil || e.ID != "note2" {
		t.Fatalf("last note: %#v", e)
	}
}

// Oct 24: siblings' deploy-lock notes, unread by a busy fork, were forwarded to
// the primary when the fork merged (60 of them in one morning). Notes another
// instance sent are dropped at merge and refused afterwards; the fork's own
// schedules and service events for it still forward.
func TestMergedForkDropsSiblingNotes(t *testing.T) {
	s := openTest(t)
	const fork = "01a16e85-c8cd-701d-9434-8d062cabd3ce"
	const sib = "01a16e85-de4a-7214-8a33-b240f1cc30a1"
	s.Enqueue(Enqueue{ID: "sib-note", Target: "instance:" + fork, Origin: sib, Summary: "deploy lock released", Urgency: "soft"})
	s.Enqueue(Enqueue{ID: "own-reminder", Target: "instance:" + fork, Origin: fork, Summary: "check the deploy"})
	s.Enqueue(Enqueue{ID: "golem", Target: "instance:" + fork, Origin: fork, Source: "golem", Summary: "agent settled"})
	s.Enqueue(Enqueue{ID: "svc", Target: "instance:" + fork, Summary: "service note"})
	if _, _, err := s.Enqueue(Enqueue{ID: "m", Target: "instance:parent", Type: "merge", Summary: "home", Body: mergeBody(fork)}); err != nil {
		t.Fatal(err)
	}
	if e, _ := s.Get("sib-note"); e.State != "acked" || e.Target != "instance:"+fork || !strings.HasPrefix(e.Summary, "(undelivered: fork merged) ") {
		t.Fatalf("sibling note not dropped: %#v", e)
	}
	for _, id := range []string{"own-reminder", "golem", "svc"} {
		if e, _ := s.Get(id); e.Target != "instance:parent" || e.State != "pending" || e.Summary[:31] != "(for merged fork 01a16e85-c8cd)" {
			t.Fatalf("%s not forwarded: %#v", id, e)
		}
	}
	if _, _, err := s.Enqueue(Enqueue{Target: "instance:" + fork, Origin: sib, Summary: "late lock note"}); err == nil || !strings.Contains(err.Error(), "already merged") {
		t.Fatalf("late sibling note should be refused, got %v", err)
	}
	if late, _, err := s.Enqueue(Enqueue{Target: "instance:" + fork, Source: "golem", Summary: "late settle"}); err != nil || late.Target != "instance:parent" {
		t.Fatalf("late service event should forward: %#v %v", late, err)
	}
}
