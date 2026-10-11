package attention

import (
	"encoding/json"
	"errors"
	"fmt"
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

type askFixture struct {
	t *testing.T
	s *Store
}

func (f askFixture) do(actor, op string, args map[string]any) any {
	f.t.Helper()
	x, err := f.s.HandleAs(actor, op, args)
	if err != nil {
		f.t.Fatalf("%s %v: %v", op, args, err)
	}
	return x
}
func (f askFixture) fails(code, op string, args map[string]any) {
	f.t.Helper()
	_, err := f.s.Handle(op, args)
	var ce *CodedError
	if !errors.As(err, &ce) || ce.Code != code {
		f.t.Fatalf("%s %v: want %s, got %v", op, args, code, err)
	}
}
func (f askFixture) card(title string) string {
	f.t.Helper()
	return f.do("kes", "card.add", map[string]any{"project": "p", "title": title, "lane": "settling"}).(map[string]any)["id"].(string)
}
func (f askFixture) add(card, q string, stage bool) int64 {
	f.t.Helper()
	x := f.do("kes", "ask.add", map[string]any{"card": card[:8], "question": q, "stage": stage, "actions": []any{map[string]any{"key": "ok", "label": "OK"}}})
	return x.(map[string]any)["id"].(int64)
}
func (f askFixture) notes(card string) []string {
	f.t.Helper()
	rs, err := f.s.db.Query("select by||': '||text from notes where card_id=? order by id", card)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rs.Close()
	out := []string{}
	for rs.Next() {
		var x string
		_ = rs.Scan(&x)
		out = append(out, x)
	}
	return out
}
func (f askFixture) list() (surfaced, staged []int64) {
	f.t.Helper()
	m := f.do("kevin", "ask.list", map[string]any{}).(map[string]any)
	for _, x := range m["surfaced"].([]any) {
		surfaced = append(surfaced, x.(map[string]any)["id"].(int64))
	}
	for _, x := range m["staged"].([]any) {
		staged = append(staged, x.(map[string]any)["id"].(int64))
	}
	return
}

func newAskFixture(t *testing.T) askFixture {
	s, err := Open(filepath.Join(t.TempDir(), "a.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	f := askFixture{t, s}
	f.do("kes", "project.add", map[string]any{"slug": "p", "label": "P"})
	return f
}

func TestAsksSurfaceCapStagingAndMore(t *testing.T) {
	f := newAskFixture(t)
	c := f.card("M4")
	ids := []int64{}
	for i := 0; i < 5; i++ {
		ids = append(ids, f.add(c, fmt.Sprintf("q%d?", i), false))
	}
	staged := f.add(c, "explicitly staged?", true)
	sur, stg := f.list()
	if fmt.Sprint(sur) != fmt.Sprint(ids[:3]) || fmt.Sprint(stg) != fmt.Sprint(append(ids[3:], staged)) {
		t.Fatalf("surfaced %v staged %v", sur, stg)
	}
	st := f.do("kevin", "status", map[string]any{}).(map[string]any)
	if st["needs_attention"] != 3 || fmt.Sprint(st["asks"]) != "map[staged:3 surfaced:3]" {
		t.Fatalf("status %#v", st)
	}
	// Surfacing past the cap is refused; nothing auto-refills.
	f.fails("conflict", "ask.surface", map[string]any{"id": ids[3]})
	f.do("kevin", "ask.resolve", map[string]any{"id": ids[0], "choice": "not-now"})
	if sur, _ = f.list(); len(sur) != 2 {
		t.Fatalf("auto-refilled: %v", sur)
	}
	// Kes may force one up when there's room.
	if x := f.do("kes", "ask.surface", map[string]any{"id": staged}).(map[string]any); x["state"] != "surfaced" || x["surfaced_at"] == nil {
		t.Fatalf("surface %#v", x)
	}
	f.do("kevin", "ask.resolve", map[string]any{"id": ids[1], "choice": "ok"})
	f.do("kevin", "ask.resolve", map[string]any{"id": ids[2], "choice": "ok"})
	more := f.do("kevin", "ask.more", map[string]any{}).(map[string]any)
	if more["surfaced"] != int64(2) || more["staged_left"] != 0 {
		t.Fatalf("more %#v", more)
	}
	if sur, stg = f.list(); fmt.Sprint(sur) != fmt.Sprint([]int64{ids[3], ids[4], staged}) || len(stg) != 0 {
		t.Fatalf("after more: %v %v", sur, stg)
	}
	all := f.do("kevin", "ask.list", map[string]any{"all": true}).(map[string]any)["resolved"].([]any)
	if len(all) != 3 || all[0].(map[string]any)["id"] != ids[2] {
		t.Fatalf("resolved list %#v", all)
	}
}

func TestAskResolveRunsOpsAsActor(t *testing.T) {
	f := newAskFixture(t)
	m4 := f.card("M4 — store")
	sib := f.card("M4 — sibling")
	link := "https://example.com/pr/1"
	x := f.do("kes", "ask.add", map[string]any{
		"card": m4, "question": "Close M4 and its sibling?", "steps": "look at the PR\n\ncheck the deploy",
		"link": link, "link_label": "PR ↗",
		"actions": []any{
			map[string]any{"key": "close", "label": "Close both", "primary": true, "ops": []any{
				map[string]any{"op": "card.move", "id": m4[:8], "lane": "archived"},
				map[string]any{"op": "card.move", "id": sib, "lane": "archived"},
				map[string]any{"op": "note.add", "card": sib, "text": "closed with M4"},
			}},
			map[string]any{"key": "keep", "label": "Keep open"},
		},
	}).(map[string]any)
	id := x["id"].(int64)
	card := x["card"].(map[string]any)
	if x["state"] != "surfaced" || card["id"] != m4 || card["project"] != "p" || card["title"] != "M4 — store" || x["link"] != link || x["link_label"] != "PR ↗" || x["resolution"] != nil {
		t.Fatalf("ask %#v", x)
	}
	b, _ := json.Marshal(x["actions"])
	if string(b) != `[{"key":"close","label":"Close both","primary":true},{"key":"keep","label":"Keep open"}]` {
		t.Fatalf("actions on the wire: %s", b)
	}
	// Ops are stored but never sent to clients.
	all, _ := json.Marshal(f.do("kevin", "ask.list", map[string]any{"all": true}))
	if strings.Contains(string(all), "ops") || strings.Contains(string(b), "ops") {
		t.Fatalf("ops leaked: %s", all)
	}
	r := f.do("kevin", "ask.resolve", map[string]any{"id": json.Number(fmt.Sprint(id)), "choice": "close"}).(map[string]any)
	res := r["resolved"].(map[string]any)
	if res["state"] != "resolved" || res["resolution"] != "close" || res["resolved_at"] == nil || r["card"].(map[string]any)["lane"] != "archived" {
		t.Fatalf("resolve %#v", r)
	}
	var lane, mover string
	_ = f.s.db.QueryRow("select c.lane,e.actor from cards c join events e on e.card_id=c.id and e.kind='moved' where c.id=?", sib).Scan(&lane, &mover)
	if lane != "archived" || mover != "kevin" {
		t.Fatalf("sibling lane=%s moved by %s", lane, mover)
	}
	if got := strings.Join(f.notes(m4), " | "); got != "kes: Asked Kev: Close M4 and its sibling? | kevin: Kev chose “Close both” on: Close M4 and its sibling?" {
		t.Fatalf("m4 notes: %s", got)
	}
	if got := f.notes(sib); got[len(got)-1] != "kevin: closed with M4" {
		t.Fatalf("sibling notes %v", got)
	}
	f.fails("conflict", "ask.resolve", map[string]any{"id": id, "choice": "close"})
	f.fails("not_found", "ask.resolve", map[string]any{"id": 999, "choice": "close"})
}

func TestAskResolveIsAtomic(t *testing.T) {
	f := newAskFixture(t)
	c := f.card("x")
	other := f.card("y")
	// icebox is only reachable from captured: the second op fails at resolve time.
	id := f.do("kes", "ask.add", map[string]any{"card": c, "question": "Q?", "actions": []any{map[string]any{"key": "go", "label": "Go", "ops": []any{
		map[string]any{"op": "card.move", "id": other, "lane": "archived"},
		map[string]any{"op": "card.move", "id": c, "lane": "icebox"},
	}}}}).(map[string]any)["id"].(int64)
	f.fails("invalid_request", "ask.resolve", map[string]any{"id": id, "choice": "go"})
	var lane, state string
	_ = f.s.db.QueryRow("select lane from cards where id=?", other).Scan(&lane)
	_ = f.s.db.QueryRow("select state from asks where id=?", id).Scan(&state)
	if lane != "settling" || state != "surfaced" {
		t.Fatalf("partial resolve: lane=%s state=%s", lane, state)
	}
	f.fails("invalid_request", "ask.resolve", map[string]any{"id": id, "choice": "nope"})
}

func TestAskNotNowTalkDrop(t *testing.T) {
	f := newAskFixture(t)
	c := f.card("c")
	a := f.add(c, "Ship it?", false)
	b := f.add(c, "Rename it?", false)
	d := f.add(c, "Split it?", true)
	if x := f.do("kevin", "ask.talk", map[string]any{"id": a}).(map[string]any); x["state"] != "surfaced" || x["resolution"] != nil {
		t.Fatalf("talk resolved: %#v", x)
	}
	r := f.do("kevin", "ask.resolve", map[string]any{"id": a, "choice": "not-now"}).(map[string]any)["resolved"].(map[string]any)
	if r["resolution"] != "not-now" || r["state"] != "resolved" {
		t.Fatalf("not-now %#v", r)
	}
	x := f.do("kes", "ask.drop", map[string]any{"id": d}).(map[string]any)
	if x["state"] != "dropped" || x["resolution"] != "dropped" {
		t.Fatalf("drop %#v", x)
	}
	f.fails("conflict", "ask.drop", map[string]any{"id": d})
	f.fails("conflict", "ask.talk", map[string]any{"id": a})
	f.fails("conflict", "ask.surface", map[string]any{"id": d})
	f.fails("not_found", "ask.talk", map[string]any{"id": 4242})
	f.fails("invalid_request", "ask.talk", map[string]any{"id": "one"})
	want := []string{
		"kes: Asked Kev: Ship it?", "kes: Asked Kev: Rename it?", "kes: Asked Kev: Split it?",
		"kevin: Kev opened a conversation about: Ship it?",
		"kevin: Kev: not now on “Ship it?” (wrong moment — not a judgment on the ask)",
		"kes: Withdrew ask: Split it?",
	}
	if got := f.notes(c); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("notes:\n%s", strings.Join(got, "\n"))
	}
	_ = b
	// A long question keeps the generated note within the 300-unit note bound.
	long := strings.Repeat("é", 290) + "?"
	l := f.add(c, long, false)
	f.do("kevin", "ask.resolve", map[string]any{"id": l, "choice": "not-now"})
	var text, detail string
	_ = f.s.db.QueryRow("select text,detail from notes where card_id=? order by id desc limit 1", c).Scan(&text, &detail)
	if units(text) > 300 || detail != long || !strings.Contains(text, "…") {
		t.Fatalf("long note %d %q", units(text), text)
	}
}

func TestAskAddRejectsInvalid(t *testing.T) {
	f := newAskFixture(t)
	c := f.card("c")
	act := func(m map[string]any) map[string]any {
		x := map[string]any{"key": "go", "label": "Go"}
		for k, v := range m {
			x[k] = v
		}
		return x
	}
	ops := func(o ...any) map[string]any { return act(map[string]any{"ops": o}) }
	base := func(over map[string]any) map[string]any {
		x := map[string]any{"card": c, "question": "Q?", "actions": []any{act(nil)}}
		for k, v := range over {
			x[k] = v
		}
		return x
	}
	nine := []any{}
	for i := 0; i < 9; i++ {
		nine = append(nine, map[string]any{"op": "card.unblock", "id": c})
	}
	cases := map[string]map[string]any{
		"no actions":        base(map[string]any{"actions": []any{}}),
		"five actions":      base(map[string]any{"actions": []any{act(map[string]any{"key": "a"}), act(map[string]any{"key": "b"}), act(map[string]any{"key": "c"}), act(map[string]any{"key": "d"}), act(map[string]any{"key": "e"})}}),
		"reserved not-now":  base(map[string]any{"actions": []any{act(map[string]any{"key": "not-now"})}}),
		"reserved talk":     base(map[string]any{"actions": []any{act(map[string]any{"key": "talk"})}}),
		"bad key":           base(map[string]any{"actions": []any{act(map[string]any{"key": "Go!"})}}),
		"dup key":           base(map[string]any{"actions": []any{act(nil), act(nil)}}),
		"two primaries":     base(map[string]any{"actions": []any{act(map[string]any{"primary": true}), act(map[string]any{"key": "b", "primary": true})}}),
		"long label":        base(map[string]any{"actions": []any{act(map[string]any{"label": strings.Repeat("x", 121)})}}),
		"multiline q":       base(map[string]any{"question": "a\nb"}),
		"long q":            base(map[string]any{"question": strings.Repeat("q", 301)}),
		"relative link":     base(map[string]any{"link": "docs/x.md"}),
		"ftp link":          base(map[string]any{"link": "ftp://x/y"}),
		"unknown field":     base(map[string]any{"priority": 1}),
		"unknown card":      base(map[string]any{"card": "deadbeef"}),
		"disallowed op":     base(map[string]any{"actions": []any{ops(map[string]any{"op": "card.add", "project": "p", "title": "t"})}}),
		"agent op":          base(map[string]any{"actions": []any{ops(map[string]any{"op": "agent.start", "card": c, "name": "x"})}}),
		"missing lane":      base(map[string]any{"actions": []any{ops(map[string]any{"op": "card.move", "id": c})}}),
		"bad lane":          base(map[string]any{"actions": []any{ops(map[string]any{"op": "card.move", "id": c, "lane": "done"})}}),
		"hold_until":        base(map[string]any{"actions": []any{ops(map[string]any{"op": "card.move", "id": c, "lane": "settling", "hold_until": "2030-01-01T00:00:00Z"})}}),
		"missing reason":    base(map[string]any{"actions": []any{ops(map[string]any{"op": "card.block", "id": c})}}),
		"missing note text": base(map[string]any{"actions": []any{ops(map[string]any{"op": "note.add", "card": c})}}),
		"op id unknown":     base(map[string]any{"actions": []any{ops(map[string]any{"op": "card.unblock", "id": "0123456789abcdef"})}}),
		"set due via ask":   base(map[string]any{"actions": []any{ops(map[string]any{"op": "card.set", "id": c, "due": "2026-01-01"})}}),
		"nine ops":          base(map[string]any{"actions": []any{ops(nine...)}}),
	}
	for name, args := range cases {
		if _, err := f.s.Handle("ask.add", args); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	var n int
	_ = f.s.db.QueryRow("select count(*) from asks").Scan(&n)
	if n != 0 {
		t.Fatalf("%d asks stored", n)
	}
	// An absolute path is a fine link.
	f.do("kes", "ask.add", base(map[string]any{"link": "/home/kev/docs/plan.md"}))
}
