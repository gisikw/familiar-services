package attention

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestImpOperationsAndExistingSchema(t *testing.T) {
	s, e := Open(filepath.Join(t.TempDir(), "attention.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	p, e := s.Handle("project.add", map[string]any{"slug": "service-test", "label": "Service test"})
	if e != nil {
		t.Fatal(e)
	}
	if p.(map[string]any)["slug"] != "service-test" {
		t.Fatalf("project: %#v", p)
	}
	c, e := s.Handle("card.add", map[string]any{"project": "service-test", "title": "Port singleton", "summary": "same semantics", "owner": "kes"})
	if e != nil {
		t.Fatal(e)
	}
	id := c.(map[string]any)["id"].(string)
	for _, tc := range []struct {
		op   string
		args map[string]any
	}{{"card.move", map[string]any{"id": id, "lane": "inflight"}}, {"note.add", map[string]any{"card": id, "text": "working"}}, {"evidence.add", map[string]any{"card": id, "kind": "commit", "title": "implementation", "ref": "abc"}}, {"agent.start", map[string]any{"card": id, "name": "worker"}}, {"agent.set", map[string]any{"card": id, "name": "worker", "state": "done"}}} {
		if _, e = s.Handle(tc.op, tc.args); e != nil {
			t.Fatalf("%s: %v", tc.op, e)
		}
	}
	full, e := s.Handle("card.get", map[string]any{"id": id})
	if e != nil {
		t.Fatal(e)
	}
	m := full.(map[string]any)
	if m["lane"] != "inflight" || len(m["notes"].([]any)) != 1 || len(m["evidence"].([]any)) != 1 || len(m["agent_list"].([]any)) != 1 {
		t.Fatalf("full card: %#v", m)
	}
}
func TestJotDoneAndClear(t *testing.T) {
	s, e := Open(filepath.Join(t.TempDir(), "a.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	j, e := s.Handle("jot.add", map[string]any{"title": "today"})
	if e != nil {
		t.Fatal(e)
	}
	id := j.(map[string]any)["id"].(string)
	if _, e = s.Handle("card.done", map[string]any{"id": id}); e != nil {
		t.Fatal(e)
	}
	x, e := s.Handle("jot.clear-done", map[string]any{})
	if e != nil || x.(map[string]any)["archived"] != 1 {
		t.Fatalf("clear: %#v %v", x, e)
	}
}

func TestCardPrefixResolution(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "a.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.Handle("project.add", map[string]any{"slug": "p"}); err != nil {
		t.Fatal(err)
	}
	c, err := s.Handle("card.add", map[string]any{"project": "p", "title": "t", "lane": "inflight"})
	if err != nil {
		t.Fatal(err)
	}
	id := c.(map[string]any)["id"].(string)
	for _, ref := range []string{id, id[:8], strings.ReplaceAll(id, "-", "")[:12]} {
		if _, err := s.Handle("note.add", map[string]any{"card": ref, "text": "a/b/c"}); err != nil {
			t.Fatalf("%s: %v", ref, err)
		}
	}
	if _, err := s.Handle("card.get", map[string]any{"id": id[:7]}); err == nil {
		t.Fatal("short prefix accepted")
	}
}

func TestActorAttribution(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "a.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.Handle("project.add", map[string]any{"slug": "p"}); err != nil {
		t.Fatal(err)
	}
	c, err := s.Handle("card.add", map[string]any{"project": "p", "title": "t"})
	if err != nil {
		t.Fatal(err)
	}
	id := c.(map[string]any)["id"].(string)
	if _, err = s.HandleAs("kevin", "card.move", map[string]any{"id": id, "lane": "archived"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.HandleAs("kevin", "note.add", map[string]any{"card": id, "text": "from the UI"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.HandleAs("mallory", "card.move", map[string]any{"id": id, "lane": "captured"}); err == nil {
		t.Fatal("unknown actor accepted")
	}
	var createdBy, movedBy, notedBy string
	_ = s.db.QueryRow("select actor from events where card_id=? and kind='created'", id).Scan(&createdBy)
	_ = s.db.QueryRow("select actor from events where card_id=? and kind='moved'", id).Scan(&movedBy)
	_ = s.db.QueryRow("select by from notes where card_id=?", id).Scan(&notedBy)
	if createdBy != "kes" || movedBy != "kevin" || notedBy != "kevin" {
		t.Fatalf("created=%q moved=%q noted=%q", createdBy, movedBy, notedBy)
	}
	full, err := s.Handle("card.get", map[string]any{"id": id})
	if err != nil {
		t.Fatal(err)
	}
	tl := full.(map[string]any)["timeline"].([]any)
	if last := tl[len(tl)-1].(map[string]any)["text"].(string); !strings.Contains(last, "Kevin") {
		t.Fatalf("timeline text %q", last)
	}
}

func TestSettlingIsKevinsToClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.sqlite")
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Handle("project.add", map[string]any{"slug": "p", "label": "P"}); e != nil {
		t.Fatal(e)
	}
	add := func(title string) string {
		c, e := s.Handle("card.add", map[string]any{"project": "p", "title": title, "owner": "kes"})
		if e != nil {
			t.Fatal(e)
		}
		return c.(map[string]any)["id"].(string)
	}
	get := func(id string) map[string]any {
		c, e := s.Handle("card.get", map[string]any{"id": id})
		if e != nil {
			t.Fatal(e)
		}
		return c.(map[string]any)
	}
	future := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)

	// Plain move to settling hands the card to Kevin with a note.
	a := add("plain")
	if _, e = s.Handle("card.move", map[string]any{"id": a, "lane": "settling"}); e != nil {
		t.Fatal(e)
	}
	if c := get(a); c["owner"] != "kevin" || c["hold_until"] != nil || len(c["notes"].([]any)) != 1 {
		t.Fatalf("plain settling: %#v", c)
	}

	// Kes may keep it only with a future hold.
	b := add("held")
	if _, e = s.Handle("card.move", map[string]any{"id": b, "lane": "settling", "hold_until": future}); e != nil {
		t.Fatal(e)
	}
	if c := get(b); c["owner"] != "kes" || c["hold_until"] != future {
		t.Fatalf("held settling: %#v", c)
	}
	for _, bad := range []map[string]any{
		{"id": a, "lane": "review", "hold_until": future},
		{"id": a, "lane": "settling", "hold_until": "2001-01-01T00:00:00Z"},
		{"id": a, "lane": "settling", "hold_until": "tomorrow"},
	} {
		if _, e = s.Handle("card.move", bad); e == nil {
			t.Fatalf("accepted bad hold %#v", bad)
		}
	}
	if _, e = s.HandleAs("kevin", "card.move", map[string]any{"id": a, "lane": "settling", "hold_until": future}); e == nil {
		t.Fatal("kevin holding a card should be refused")
	}
	// Kes can't take an unheld settling card back by owner alone.
	if _, e = s.Handle("card.set", map[string]any{"id": a, "owner": "kes"}); e == nil || !strings.Contains(e.Error(), "hold-until") {
		t.Fatalf("set owner kes on settling: %v", e)
	}
	// Handing a held card to Kevin ends the hold.
	if _, e = s.Handle("card.set", map[string]any{"id": b, "owner": "kevin"}); e != nil {
		t.Fatal(e)
	}
	if c := get(b); c["owner"] != "kevin" || c["hold_until"] != nil {
		t.Fatalf("released: %#v", c)
	}
	// Leaving settling clears a hold.
	if _, e = s.Handle("card.move", map[string]any{"id": b, "lane": "settling", "hold_until": future}); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Handle("card.move", map[string]any{"id": b, "lane": "archived"}); e != nil {
		t.Fatal(e)
	}
	if c := get(b); c["hold_until"] != nil || c["lane"] != "archived" {
		t.Fatalf("archived: %#v", c)
	}
	// Adding straight into settling: Kevin's, and Kes can't claim it.
	if _, e = s.Handle("card.add", map[string]any{"project": "p", "title": "x", "lane": "settling", "owner": "kes"}); e == nil {
		t.Fatal("add settling as kes should be refused")
	}
	c, e := s.Handle("card.add", map[string]any{"project": "p", "title": "y", "lane": "settling"})
	if e != nil || c.(map[string]any)["owner"] != "kevin" {
		t.Fatalf("add settling: %#v %v", c, e)
	}

	// One-time migration: a legacy kes-owned settling card becomes Kevin's on open.
	legacy := add("legacy")
	if _, e = s.db.Exec("update cards set lane='settling',owner='kes' where id=?", legacy); e != nil {
		t.Fatal(e)
	}
	if _, e = s.db.Exec("delete from meta where key='settling_owner_v1'"); e != nil {
		t.Fatal(e)
	}
	s.Close()
	s, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if c := get(legacy); c["owner"] != "kevin" || len(c["notes"].([]any)) != 1 {
		t.Fatalf("migrated: %#v", c)
	}
	// ...and only once: a later legacy row is left alone.
	if _, e = s.db.Exec("update cards set owner='kes' where id=?", legacy); e != nil {
		t.Fatal(e)
	}
	s.Close()
	s, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	if c := get(legacy); c["owner"] != "kes" {
		t.Fatalf("migration ran twice: %#v", c)
	}
}
