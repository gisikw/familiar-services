package scheduler

import (
	"os"
	"path/filepath"
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
