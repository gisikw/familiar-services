package scheduler

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Nov 12: a wake return held by quiet DND was invisible to imp pull and to
// Kev's "Bring return to Kes" (both said not_found) while its full text sat
// in events. A stored return is pullable in every mode, reaches Kes once,
// and only the instance it is addressed to may consume it.

func quietOn(t *testing.T, s *Store, target string) {
	t.Helper()
	d := int64(time.Hour / time.Millisecond)
	if _, err := s.DNDSetMode(target, true, "user", DNDQuiet, &d); err != nil {
		t.Fatal(err)
	}
}

const heldFull = "LAN DNS is healthy.\n\nDetails: forty lookups each."

func enqueueHeldWake(t *testing.T, s *Store) {
	t.Helper()
	if _, _, err := s.Enqueue(Enqueue{ID: "merge-fork-held1234-x", Target: "instance:p", Origin: "fork-held1234", Type: "merge", Summary: heldFull,
		Body: inboxMergeBody("fork-held1234", heldFull, map[string]any{"status": "done", "headline": "LAN DNS healthy", "label": "dns"})}); err != nil {
		t.Fatal(err)
	}
}

func TestPullFindsWakeReturnHeldByQuietDND(t *testing.T) {
	s := openTest(t)
	quietOn(t, s, "instance:p")
	enqueueHeldWake(t, s)
	now := time.Now().Add(time.Minute).UnixMilli()
	if e, _ := s.Claim("instance:p", now); e != nil {
		t.Fatalf("quiet DND should hold the wake: %#v", e)
	}
	items, err := s.InboxList("instance:p")
	if err != nil || len(items) != 1 || !items[0].Held || items[0].Fork != "fork-held1234" || items[0].Headline != "LAN DNS healthy" {
		t.Fatalf("list: %#v %v", items, err)
	}
	got, err := s.InboxClaimAs("fork-he", "instance:p") // too short a prefix
	if !IsNotFound(err) {
		t.Fatalf("short prefix: %#v %v", got, err)
	}
	got, err = s.InboxClaimAs("fork-held", "instance:p")
	if err != nil || !strings.Contains(got.Body, "forty lookups") || got.ClaimedAt == 0 {
		t.Fatalf("claim: %#v %v", got, err)
	}
	// Pulled: off the unclaimed list, and the held event is now a soft stub
	// that rides through quiet DND without the full text.
	if items, _ = s.InboxList("instance:p"); len(items) != 0 {
		t.Fatalf("still listed: %#v", items)
	}
	e, _ := s.Claim("instance:p", now)
	if e == nil || e.ID != "merge-fork-held1234-x" || e.Urgency != "soft" || strings.Contains(e.Body, "forty lookups") {
		t.Fatalf("stub: %#v", e)
	}
	var stub map[string]any
	_ = json.Unmarshal([]byte(e.Body), &stub)
	if stub["stub"] != true {
		t.Fatalf("not a stub: %s", e.Body)
	}
	if again, err := s.InboxClaim("fork-held1234"); err != nil || !strings.Contains(again.Body, "forty lookups") {
		t.Fatalf("reread: %#v %v", again, err)
	}
}

func TestOpenTapDeliversWakeReturnHeldByQuietDND(t *testing.T) {
	s := openTest(t)
	quietOn(t, s, "instance:p")
	enqueueHeldWake(t, s)
	now := time.Now().Add(time.Minute).UnixMilli()
	pulled, err := s.InboxDeliver("fork-held1234")
	if err != nil || pulled.ID != "merge-fork-held1234-x-pulled" {
		t.Fatalf("deliver: %#v %v", pulled, err)
	}
	// The full return first, past quiet DND; then only the stub.
	e, _ := s.Claim("instance:p", now)
	if e == nil || e.ID != pulled.ID || e.Urgency != "wake" || !strings.Contains(e.Body, "forty lookups") {
		t.Fatalf("first: %#v", e)
	}
	e, _ = s.Claim("instance:p", now)
	if e == nil || e.ID != "merge-fork-held1234-x" || strings.Contains(e.Body, "forty lookups") {
		t.Fatalf("second: %#v", e)
	}
	if e, _ = s.Claim("instance:p", now); e != nil {
		t.Fatalf("delivered twice: %#v", e)
	}
}

func TestPullSwapsAQuietReturnAlreadyHeldByThePi(t *testing.T) {
	s := openTest(t)
	now := time.Now().Add(time.Minute).UnixMilli()
	_, _, _ = s.Enqueue(Enqueue{ID: "q1", Target: "instance:p", Origin: "fork-quiet123", Type: "merge", Urgency: "soft", Summary: "q", Body: inboxMergeBody("fork-quiet123", "quiet full return", nil)})
	if e, _ := s.Claim("instance:p", now); e == nil || e.ID != "q1" {
		t.Fatalf("claim: %#v", e)
	}
	if x, err := s.InboxClaim("fork-quiet123"); err != nil || !strings.Contains(x.Body, "quiet full return") {
		t.Fatalf("pull: %#v %v", x, err)
	}
	// Requeued under the same id as a stub, so the Pi's held full copy is
	// replaced rather than shown again.
	e, _ := s.Claim("instance:p", now)
	if e == nil || e.ID != "q1" || !strings.Contains(e.Body, `"stub":true`) {
		t.Fatalf("swap: %#v", e)
	}
}

func TestOnlyTheAddresseePullsAReturn(t *testing.T) {
	s := openTest(t)
	quietOn(t, s, "instance:p")
	enqueueHeldWake(t, s)
	if _, err := s.InboxClaimAs("fork-held1234", "fork-sibling1"); !IsForbidden(err) {
		t.Fatalf("sibling pull: %v", err)
	}
	if _, err := s.InboxDeliverAs("fork-held1234", "instance:fork-sibling1"); !IsForbidden(err) {
		t.Fatalf("sibling deliver: %v", err)
	}
	// Refused without side effects: still held, still listed, full body intact.
	items, _ := s.InboxList("instance:p")
	if len(items) != 1 || !items[0].Held {
		t.Fatalf("after refusal: %#v", items)
	}
	if x, err := s.InboxClaimAs("fork-held1234", "p"); err != nil || !strings.Contains(x.Body, "forty lookups") {
		t.Fatalf("addressee: %#v %v", x, err)
	}
}

func TestAmbientReturnsArePullableByNameButNotListed(t *testing.T) {
	s := openTest(t)
	quietOn(t, s, "instance:p")
	_, _, _ = s.Enqueue(Enqueue{ID: "amb", Target: "instance:p", Origin: "fork-ambient1", Type: "merge", Summary: "watch", Body: inboxMergeBody("fork-ambient1", "morning watch notes", map[string]any{"ambient": true})})
	if items, _ := s.InboxList("instance:p"); len(items) != 0 {
		t.Fatalf("ambient listed: %#v", items)
	}
	if x, err := s.InboxClaim("fork-ambient1"); err != nil || !strings.Contains(x.Body, "morning watch") {
		t.Fatalf("ambient pull: %#v %v", x, err)
	}
}
