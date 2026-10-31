package scheduler

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func inboxMergeBody(fork, summary string, extra map[string]any) string {
	m := map[string]any{"summary": summary, "forkSessionId": fork, "forkSessionFile": "/tmp/" + fork + ".jsonl", "branchEntryId": "b", "firstEntryId": "f", "lastEntryId": "l", "mergedAt": "2026-10-31T12:00:00Z", "turnCount": 3, "forkedFurther": false}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func softOn(t *testing.T, s *Store, target string) {
	t.Helper()
	d := int64(time.Hour / time.Millisecond)
	if _, err := s.DNDSetMode(target, true, "user", DNDSoft, &d); err != nil {
		t.Fatal(err)
	}
}

func TestSoftStubsTaskedReturnsAndKeepsThemUnclaimed(t *testing.T) {
	s := openTest(t)
	softOn(t, s, "instance:p")
	full := "Fixed the CVE on three hosts.\n\nDetails: lots of them."
	if _, _, err := s.Enqueue(Enqueue{ID: "m1", Target: "instance:p", Type: "merge", Summary: full, Body: inboxMergeBody("fork-aaaaaaaa1", full, map[string]any{"status": "blocked", "headline": "CVE fixed on 3 of 5; need your hands on 2", "label": "cve"})}); err != nil {
		t.Fatal(err)
	}
	e, err := s.Claim("instance:p", time.Now().UnixMilli())
	if err != nil || e == nil {
		t.Fatalf("claim: %v %v", e, err)
	}
	// Loud stays loud: the fork asked to wake, so the stub wakes.
	if e.Urgency != "wake" {
		t.Fatalf("stub urgency %q", e.Urgency)
	}
	var stub map[string]any
	_ = json.Unmarshal([]byte(e.Body), &stub)
	if stub["stub"] != true || stub["status"] != "blocked" || strings.Contains(e.Body, "Details") || stub["summary"] != "CVE fixed on 3 of 5; need your hands on 2" {
		t.Fatalf("not a stub: %s", e.Body)
	}
	// Redelivery after a reconnect is the stub too.
	_ = s.Requeue("instance:p")
	again, _ := s.Claim("instance:p", time.Now().UnixMilli())
	if again == nil || again.Body != e.Body {
		t.Fatalf("redelivery: %#v", again)
	}
	items, _ := s.InboxList("instance:p")
	if len(items) != 1 || items[0].Fork != "fork-aaaaaaaa1" || items[0].Status != "blocked" || items[0].Label != "cve" {
		t.Fatalf("inbox: %#v", items)
	}
	// Ack the stub: it is still unclaimed (survives a /clear).
	_, _ = s.Ack("m1", "instance:p")
	if items, _ = s.InboxList("instance:p"); len(items) != 1 {
		t.Fatal("ack claimed the return")
	}
	got, err := s.InboxClaim("fork-aaaa") // prefix
	if err != nil || !strings.Contains(got.Body, "Details: lots of them") {
		t.Fatalf("claim: %#v %v", got, err)
	}
	if items, _ = s.InboxList("instance:p"); len(items) != 0 {
		t.Fatal("claimed return still listed")
	}
	if _, err := s.InboxClaim("nope-nope-nope"); !IsNotFound(err) {
		t.Fatalf("unknown ref: %v", err)
	}
}

func TestSoftLeavesAmbientAndQuietUrgency(t *testing.T) {
	s := openTest(t)
	softOn(t, s, "instance:p")
	_, _, _ = s.Enqueue(Enqueue{ID: "watch", Target: "instance:p", Type: "merge", Urgency: "soft", Summary: "watch", Body: inboxMergeBody("fork-watch", "all quiet", map[string]any{"ambient": true, "status": "done"})})
	_, _, _ = s.Enqueue(Enqueue{ID: "quiet", Target: "instance:p", Type: "merge", Urgency: "soft", Summary: "q", Body: inboxMergeBody("fork-quiet", "did a thing", nil)})
	seen := map[string]*Event{}
	for i := 0; i < 2; i++ {
		e, _ := s.Claim("instance:p", time.Now().UnixMilli())
		if e == nil {
			t.Fatal("missing delivery")
		}
		seen[e.ID] = e
	}
	if strings.Contains(seen["watch"].Body, `"stub"`) {
		t.Fatal("ambient return was stubbed")
	}
	if !strings.Contains(seen["quiet"].Body, `"stub":true`) || seen["quiet"].Urgency != "soft" || !strings.Contains(seen["quiet"].Body, `"status":"done"`) {
		t.Fatalf("quiet tasked: %#v", seen["quiet"])
	}
	items, _ := s.InboxList("*")
	if len(items) != 1 || items[0].Fork != "fork-quiet" {
		t.Fatalf("inbox: %#v", items)
	}
}

func TestNormalModeDoesNotStub(t *testing.T) {
	s := openTest(t)
	_, _, _ = s.Enqueue(Enqueue{ID: "m", Target: "instance:p", Type: "merge", Summary: "x", Body: inboxMergeBody("fork-n", "full return", map[string]any{"status": "done"})})
	e, _ := s.Claim("instance:p", time.Now().UnixMilli())
	if e == nil || strings.Contains(e.Body, `"stub"`) {
		t.Fatalf("%#v", e)
	}
	if items, _ := s.InboxList("*"); len(items) != 0 {
		t.Fatal("normal mode filled the inbox")
	}
}

func TestInboxDeliverSendsFullReturnAsWake(t *testing.T) {
	s := openTest(t)
	softOn(t, s, "instance:p")
	_, _, _ = s.Enqueue(Enqueue{ID: "m", Target: "instance:p", Type: "merge", Urgency: "soft", Summary: "x", Body: inboxMergeBody("fork-d", "the whole story", map[string]any{"status": "partial"})})
	stub, _ := s.Claim("instance:p", time.Now().UnixMilli())
	_, _ = s.Ack(stub.ID, "instance:p")
	e, err := s.InboxDeliver("fork-d")
	if err != nil || e.Urgency != "wake" || e.Origin != "fork-d" {
		t.Fatalf("deliver: %#v %v", e, err)
	}
	// Still soft mode: a pulled return is not stubbed again.
	got, _ := s.Claim("instance:p", time.Now().UnixMilli())
	if got == nil || got.ID != "m-pulled" || !strings.Contains(got.Body, "the whole story") || strings.Contains(got.Body, `"stub"`) {
		t.Fatalf("pulled: %#v", got)
	}
	// A second tap is the same delivery, not another.
	if again, err := s.InboxDeliver("fork-d"); err != nil || again.ID != "m-pulled" {
		t.Fatalf("again: %#v %v", again, err)
	}
}

func TestSoftModeCappedLikeDisturb(t *testing.T) {
	s := openTest(t)
	d := int64(48 * time.Hour / time.Millisecond)
	got, err := s.DNDSetMode("instance:p", true, "familiar", DNDSoft, &d)
	if err != nil || got.ExpiresAt-got.SetAt != 24*60*60*1000 || got.Mode != "soft" {
		t.Fatalf("%#v %v", got, err)
	}
	// Soft is not a hold: an ordinary wake still goes through.
	_, _, _ = s.Enqueue(Enqueue{ID: "w", Target: "instance:p", Summary: "wake"})
	if e, _ := s.Claim("instance:p", time.Now().UnixMilli()); e == nil || e.ID != "w" {
		t.Fatalf("wake held under soft: %#v", e)
	}
}
