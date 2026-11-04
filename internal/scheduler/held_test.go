package scheduler

import (
	"encoding/json"
	"testing"
	"time"
)

// A quiet return claimed (soft) before Do Disturb was switched on is woken by
// the switch: same id, pending again, as a wake.
func TestDoDisturbWakesMergesAlreadyHeld(t *testing.T) {
	s := openTest(t)
	now := time.Now().Add(time.Minute).UnixMilli() // past every due_at set below
	_, _, _ = s.Enqueue(Enqueue{ID: "q1", Target: "instance:p", Origin: "fork-q1", Type: "merge", Urgency: "soft", Summary: "q1", Body: inboxMergeBody("fork-q1", "quiet one", nil)})
	_, _, _ = s.Enqueue(Enqueue{ID: "note", Target: "instance:p", Urgency: "soft", Summary: "a soft note"})
	for i := 0; i < 2; i++ {
		if e, _ := s.Claim("instance:p", now); e == nil || e.Urgency != "soft" {
			t.Fatalf("claim %d: %#v", i, e)
		}
	}
	d := int64(time.Hour / time.Millisecond)
	if _, err := s.DNDSetMode("instance:p", true, "user", DNDDisturb, &d); err != nil {
		t.Fatal(err)
	}
	e, err := s.Claim("instance:p", now)
	if err != nil || e == nil || e.ID != "q1" || e.Urgency != "wake" {
		t.Fatalf("after disturb: %#v %v", e, err)
	}
	// The soft note isn't a return: it still rides the next turn.
	if e, _ := s.Claim("instance:p", now); e != nil {
		t.Fatalf("soft note requeued: %#v", e)
	}
}

// Acked merges (in Kes's context) and stubs (full return in the inbox) are
// not held returns; switching on Do Disturb leaves them alone.
func TestDoDisturbLeavesAckedAndStubs(t *testing.T) {
	s := openTest(t)
	now := time.Now().Add(time.Minute).UnixMilli() // past every due_at set below
	softOn(t, s, "instance:p")
	_, _, _ = s.Enqueue(Enqueue{ID: "stub", Target: "instance:p", Origin: "fork-s", Type: "merge", Urgency: "soft", Summary: "s", Body: inboxMergeBody("fork-s", "tasked", map[string]any{"status": "done"})})
	if e, _ := s.Claim("instance:p", now); e == nil {
		t.Fatal("stub not claimed")
	}
	_, _ = s.DNDSetMode("instance:p", false, "user", "", nil)
	_, _, _ = s.Enqueue(Enqueue{ID: "done", Target: "instance:p", Origin: "fork-d", Type: "merge", Urgency: "soft", Summary: "d", Body: inboxMergeBody("fork-d", "acked", nil)})
	_, _ = s.Claim("instance:p", now)
	_, _ = s.Ack("done", "instance:p")
	if n, err := s.WakeHeldMerges("instance:p", "", false); err != nil || n != 0 {
		t.Fatalf("woke %d, %v", n, err)
	}
}

// Kev taps a held return in Open: only that fork's wakes, marked his, so it
// goes first and DND doesn't hold it.
func TestKevTapWakesOneHeldMergePastDND(t *testing.T) {
	s := openTest(t)
	now := time.Now().Add(time.Minute).UnixMilli() // past every due_at set below
	for _, f := range []string{"fork-a", "fork-b"} {
		_, _, _ = s.Enqueue(Enqueue{ID: f, Target: "instance:p", Origin: f, Type: "merge", Urgency: "soft", Summary: f, Body: inboxMergeBody(f, "body "+f, nil)})
		_, _ = s.Claim("instance:p", now)
	}
	d := int64(time.Hour / time.Millisecond)
	_, _ = s.DNDSetMode("instance:p", true, "user", DNDQuiet, &d)
	if n, err := s.WakeHeldMerges("", "fork-b", true); err != nil || n != 1 {
		t.Fatalf("woke %d, %v", n, err)
	}
	e, _ := s.Claim("instance:p", now)
	if e == nil || e.ID != "fork-b" || e.Urgency != "wake" || e.Source != sourceInboxDeliver {
		t.Fatalf("tapped: %#v", e)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(e.Body), &body)
	if body["pulledBy"] != "kevin" || body["summary"] != "body fork-b" {
		t.Fatalf("body: %s", e.Body)
	}
	if e, _ := s.Claim("instance:p", now); e != nil {
		t.Fatalf("fork-a woke too: %#v", e)
	}
}
