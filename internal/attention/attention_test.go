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

// clock is a settable test clock for the now seam.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func openTest(t *testing.T, path string, c *clock) *Store {
	t.Helper()
	tz, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	s, err := open(path, c.now, tz)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func jotTitles(t *testing.T, s *Store, all bool) []string {
	t.Helper()
	args := map[string]any{}
	if all {
		args["all"] = true
	}
	x, err := s.Handle("jot.list", args)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, v := range x.([]any) {
		out = append(out, v.(map[string]any)["title"].(string))
	}
	return out
}

func TestJotsTodayAndMaybeAcrossLocalMidnight(t *testing.T) {
	// 23:30 Oct 11 in Chicago (CDT, UTC-5) is already Oct 12 in UTC.
	chi, _ := time.LoadLocation("America/Chicago")
	c := &clock{time.Date(2026, 10, 11, 23, 30, 0, 0, chi)}
	s := openTest(t, filepath.Join(t.TempDir(), "a.sqlite"), c)
	defer s.Close()
	add := func(args map[string]any) map[string]any {
		x, err := s.Handle("jot.add", args)
		if err != nil {
			t.Fatal(err)
		}
		return x.(map[string]any)
	}
	maybe := add(map[string]any{"title": "maybe walk"})
	if maybe["maybe_on"] != "2026-10-11" || maybe["due"] != nil {
		t.Fatalf("maybe uses local date: %#v", maybe)
	}
	add(map[string]any{"title": "due today", "due": "2026-10-11"})
	add(map[string]any{"title": "overdue", "due": "2026-10-09"})
	add(map[string]any{"title": "tomorrow", "due": "2026-10-12"})
	legacy := add(map[string]any{"title": "legacy"})
	if _, err := s.db.Exec("update cards set maybe_on=null where id=?", legacy["id"]); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []any{"2026-10-32", "10/11/2026", "today", 7} {
		if _, err := s.Handle("jot.add", map[string]any{"title": "x", "due": bad}); err == nil {
			t.Fatalf("accepted due %#v", bad)
		}
	}
	got := strings.Join(jotTitles(t, s, false), ",")
	if got != "overdue,due today,maybe walk,legacy" {
		t.Fatalf("before midnight: %s", got)
	}
	// Cross local midnight: the maybes fade, tomorrow's task appears.
	c.t = time.Date(2026, 10, 12, 0, 30, 0, 0, chi)
	if got = strings.Join(jotTitles(t, s, false), ","); got != "overdue,due today,tomorrow" {
		t.Fatalf("after midnight: %s", got)
	}
	if got = strings.Join(jotTitles(t, s, true), ","); got != "overdue,due today,tomorrow,maybe walk,legacy" {
		t.Fatalf("all: %s", got)
	}
	// Done dated jots stay visible (sorted last) until clear-done.
	if _, err := s.Handle("card.done", map[string]any{"id": maybe["id"]}); err != nil {
		t.Fatal(err)
	}
	dueToday := jotID(t, s, "due today")
	if _, err := s.Handle("card.done", map[string]any{"id": dueToday}); err != nil {
		t.Fatal(err)
	}
	if got = strings.Join(jotTitles(t, s, false), ","); got != "overdue,tomorrow,due today" {
		t.Fatalf("done: %s", got)
	}
}

func jotID(t *testing.T, s *Store, title string) string {
	t.Helper()
	var id string
	if err := s.db.QueryRow("select id from cards where title=?", title).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestJotPromotionViaCardSet(t *testing.T) {
	chi, _ := time.LoadLocation("America/Chicago")
	c := &clock{time.Date(2026, 10, 11, 9, 0, 0, 0, chi)}
	s := openTest(t, filepath.Join(t.TempDir(), "a.sqlite"), c)
	defer s.Close()
	j, err := s.Handle("jot.add", map[string]any{"title": "call mom"})
	if err != nil {
		t.Fatal(err)
	}
	id := j.(map[string]any)["id"].(string)
	x, err := s.HandleAs("kevin", "card.set", map[string]any{"id": id, "due": "2026-10-13"})
	if err != nil {
		t.Fatal(err)
	}
	m := x.(map[string]any)
	if m["due"] != "2026-10-13" || m["maybe_on"] != nil {
		t.Fatalf("promoted: %#v", m)
	}
	tl := m["timeline"].([]any)
	if last := tl[len(tl)-1].(map[string]any)["text"].(string); last != "edited due, maybe_on — Kevin" {
		t.Fatalf("timeline %q", last)
	}
	var data string
	_ = s.db.QueryRow("select data from events where card_id=? and kind='edited'", id).Scan(&data)
	if data != `{"due":"2026-10-13","maybe_on":null}` {
		t.Fatalf("edited event %s", data)
	}
	if got := jotTitles(t, s, false); len(got) != 0 {
		t.Fatalf("future task visible: %v", got)
	}
	// Back to a maybe for today.
	if x, err = s.Handle("card.set", map[string]any{"id": id, "due": nil, "maybe_on": "2026-10-11"}); err != nil {
		t.Fatal(err)
	}
	if m = x.(map[string]any); m["due"] != nil || m["maybe_on"] != "2026-10-11" {
		t.Fatalf("demoted: %#v", m)
	}
	if _, err = s.Handle("card.set", map[string]any{"id": id, "due": "tomorrow"}); err == nil {
		t.Fatal("bad due accepted")
	}
	if _, err = s.Handle("project.add", map[string]any{"slug": "p"}); err != nil {
		t.Fatal(err)
	}
	card, err := s.Handle("card.add", map[string]any{"project": "p", "title": "real card"})
	if err != nil {
		t.Fatal(err)
	}
	if card.(map[string]any)["due"] != nil {
		t.Fatalf("card due: %#v", card)
	}
	if _, err = s.Handle("card.set", map[string]any{"id": card.(map[string]any)["id"], "due": "2026-10-11"}); err == nil {
		t.Fatal("due on a non-jot accepted")
	}
}

func TestJotsV2Migration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.sqlite")
	chi, _ := time.LoadLocation("America/Chicago")
	c := &clock{time.Date(2026, 10, 11, 21, 0, 0, 0, chi)} // 02:00 UTC Oct 12
	s := openTest(t, path, c)
	ids := map[string]string{}
	for _, title := range []string{"Pump the Septic", "send Thinking Summaries", "Trash night", "read a book", "finished"} {
		j, err := s.Handle("jot.add", map[string]any{"title": title})
		if err != nil {
			t.Fatal(err)
		}
		ids[title] = j.(map[string]any)["id"].(string)
	}
	if _, err := s.Handle("card.done", map[string]any{"id": ids["finished"]}); err != nil {
		t.Fatal(err)
	}
	// Simulate a pre-v2 database.
	if _, err := s.db.Exec("update cards set due=null,maybe_on=null; delete from meta where key='jots_v2_migrated'"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = openTest(t, path, c)
	read := func(title string) (due, maybe *string) {
		if err := s.db.QueryRow("select due,maybe_on from cards where id=?", ids[title]).Scan(&due, &maybe); err != nil {
			t.Fatal(err)
		}
		return
	}
	for _, title := range []string{"Pump the Septic", "send Thinking Summaries", "Trash night"} {
		if due, maybe := read(title); due == nil || *due != "2026-10-11" || maybe != nil {
			t.Fatalf("%s: due=%v maybe=%v", title, due, maybe)
		}
	}
	if due, maybe := read("read a book"); due != nil || maybe == nil || *maybe != "2026-10-11" {
		t.Fatalf("maybe: due=%v maybe=%v", due, maybe)
	}
	if due, maybe := read("finished"); due != nil || maybe != nil {
		t.Fatalf("done jot migrated: due=%v maybe=%v", due, maybe)
	}
	// Only once.
	if _, err := s.db.Exec("update cards set due=null,maybe_on=null"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = openTest(t, path, c)
	defer s.Close()
	if due, maybe := read("Trash night"); due != nil || maybe != nil {
		t.Fatal("migration ran twice")
	}
}

func TestDueColumnsAddedToLegacyTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Rebuild cards without due/maybe_on, as an older store had it.
	if _, err = s.db.Exec("alter table cards drop column due; alter table cards drop column maybe_on"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	j, err := s.Handle("jot.add", map[string]any{"title": "x", "due": "2026-01-02"})
	if err != nil || j.(map[string]any)["due"] != "2026-01-02" {
		t.Fatalf("%#v %v", j, err)
	}
}
