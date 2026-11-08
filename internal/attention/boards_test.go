package attention

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type bclock struct{ t time.Time }

func (c *bclock) now() time.Time { return c.t }

func boardStore(t *testing.T) (*Store, *bclock) {
	t.Helper()
	tz, _ := time.LoadLocation("America/Chicago")
	c := &bclock{time.Date(2026, 11, 8, 15, 0, 0, 0, tz)}
	s, e := open(filepath.Join(t.TempDir(), "a.sqlite"), c.now, tz)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	if _, e = s.Handle("project.add", map[string]any{"slug": "p"}); e != nil {
		t.Fatal(e)
	}
	return s, c
}

func must(t *testing.T, v any, e error) map[string]any {
	t.Helper()
	if e != nil {
		t.Fatal(e)
	}
	m, _ := v.(map[string]any)
	return m
}

func ids(v any) []string {
	m := v.(map[string]any)
	out := []string{}
	for _, it := range m["items"].([]any) {
		out = append(out, it.(map[string]any)["id"].(string))
	}
	return out
}

func has(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func TestUpgradeFromLegacySchemaKeepsCards(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.sqlite")
	db, e := sql.Open("sqlite3", path+"?_foreign_keys=on")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(schema); e != nil {
		t.Fatal(e)
	}
	for _, q := range []string{
		"alter table cards add column hold_until text", "alter table cards add column due text", "alter table cards add column maybe_on text",
		"insert into projects values('p','P','ship-tell',null,'2026-01-01T00:00:00Z',0)",
		"insert into cards(id,project,lane,title,owner,done,captured_at,moved_at,updated_at) values('11111111-0000-4000-8000-000000000000','p','captured','old card','kes',0,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')",
		"insert into notes(card_id,at,by,text) values('11111111-0000-4000-8000-000000000000','2026-01-01T00:00:00Z','kes','hello')",
	} {
		if _, e = db.Exec(q); e != nil {
			t.Fatal(q, e)
		}
	}
	db.Close()
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	c := h(t, s, "kes", "card.get", map[string]any{"id": "11111111"})
	if c["lane"] != "captured" || c["owner"] != "kes" || len(c["notes"].([]any)) != 1 || c["snoozed"] != false || c["confidence"] != nil {
		t.Fatalf("card after upgrade: %#v", c)
	}
	// New lanes are admitted now, and cascades still hold.
	if _, e = s.Handle("card.move", map[string]any{"id": "11111111", "lane": "queue", "queue": "just-do-it"}); e != nil {
		t.Fatal(e)
	}
	var fk int
	_ = s.db.QueryRow("select count(*) from pragma_foreign_key_list('notes') where \"table\"='cards'").Scan(&fk)
	if fk != 1 {
		t.Fatal("notes lost its foreign key to cards")
	}
	// Reopening is a no-op.
	s.Close()
	if s, e = Open(path); e != nil {
		t.Fatal(e)
	}
}

func TestQueueDoneProvingReviewAndAutoArchive(t *testing.T) {
	s, _ := boardStore(t)
	c := h(t, s, "kes", "card.add", map[string]any{"project": "p", "title": "GPU unlock", "queue": "just-do-it"})
	id := c["id"].(string)
	if c["lane"] != "queue" || c["queue"] != "just-do-it" || c["side"] != "kes" {
		t.Fatalf("queued: %#v", c)
	}
	if _, e := s.Handle("card.add", map[string]any{"project": "p", "title": "x", "lane": "queue"}); e == nil {
		t.Fatal("queue lane without a queue accepted")
	}
	h(t, s, "kes", "card.move", map[string]any{"id": id, "lane": "running"})
	if _, e := s.Handle("card.move", map[string]any{"id": id, "lane": "done", "confidence": "medium"}); e == nil || !strings.Contains(e.Error(), "exit") {
		t.Fatalf("proving without exit: %v", e)
	}
	c = h(t, s, "kes", "card.move", map[string]any{"id": id, "lane": "done", "confidence": "medium", "exit": "7 days no Xid", "revisit": "2026-11-14"})
	if c["proving"] != true || c["review"] != true || c["revisit"] != "2026-11-14" || c["side"] != "kevin" {
		t.Fatalf("medium done: %#v", c)
	}
	prov := ids(h(t, s, "kes", "card.view", map[string]any{"view": "proving", "project": "p"}))
	rev := ids(h(t, s, "kes", "card.view", map[string]any{"view": "review", "project": "p"}))
	if !has(prov, id) || !has(rev, id) {
		t.Fatalf("one card, two places: proving %v review %v", prov, rev)
	}
	if _, e := s.Handle("card.set", map[string]any{"id": id, "approved": true}); e == nil {
		t.Fatal("Kes approved a card")
	}
	h(t, s, "kevin", "card.set", map[string]any{"id": id, "approved": true})
	rev = ids(h(t, s, "kes", "card.view", map[string]any{"view": "review", "project": "p"}))
	if has(rev, id) {
		t.Fatal("approved card still in review")
	}
	// New work on an approved card must say whether it's material.
	if _, e := s.Handle("evidence.add", map[string]any{"card": id, "kind": "commit", "title": "dst fix"}); e == nil {
		t.Fatal("evidence on approved card without approval decision")
	}
	h(t, s, "kes", "evidence.add", map[string]any{"card": id, "kind": "commit", "title": "dst fix", "approval": "keep", "approval_reason": "DST-only edge-case fixes"})
	c = h(t, s, "kes", "card.get", map[string]any{"id": id})
	if c["approved"] != true {
		t.Fatal("keep reset approval")
	}
	tl := timelineJoined(c)
	if !strings.Contains(tl, "approval kept: DST-only edge-case fixes") {
		t.Fatalf("timeline: %s", tl)
	}
	// Exit met: high + approved → archived.
	c = h(t, s, "kes", "card.set", map[string]any{"id": id, "confidence": "high"})
	if c["lane"] != "archived" {
		t.Fatalf("not archived: %v", c["lane"])
	}
}

func TestApprovalResetAndReopen(t *testing.T) {
	s, _ := boardStore(t)
	id := h(t, s, "kes", "card.add", map[string]any{"project": "p", "title": "t", "queue": "build-but-wait"})["id"].(string)
	h(t, s, "kes", "card.move", map[string]any{"id": id, "lane": "done", "confidence": "high"})
	h(t, s, "kevin", "card.set", map[string]any{"id": id, "approved": true})
	// high+approved archived immediately; reopen a medium one instead
	id = h(t, s, "kes", "card.add", map[string]any{"project": "p", "title": "t2", "queue": "just-do-it"})["id"].(string)
	h(t, s, "kes", "card.move", map[string]any{"id": id, "lane": "done", "confidence": "medium", "exit": "a week clean"})
	h(t, s, "kevin", "card.set", map[string]any{"id": id, "approved": true})
	if _, e := s.Handle("card.set", map[string]any{"id": id, "approved": false}); e == nil {
		t.Fatal("reset without reason")
	}
	c := h(t, s, "kes", "evidence.add", map[string]any{"card": id, "kind": "pr", "title": "new approach", "approval": "reset", "approval_reason": "different rollback"})
	if c["approved"] != false || c["review"] != true {
		t.Fatalf("reset: %#v", c)
	}
	// Breaks while proving: back to running clears confidence.
	c = h(t, s, "kes", "card.move", map[string]any{"id": id, "lane": "running"})
	if c["confidence"] != nil || c["exit"] != nil || c["proving"] != false {
		t.Fatalf("reopened: %#v", c)
	}
}

func TestSnoozeIceboxWakeAndWokenActions(t *testing.T) {
	s, clk := boardStore(t)
	id := h(t, s, "kes", "card.add", map[string]any{"project": "p", "title": "multiplayer minecraft", "lane": "todo"})["id"].(string)
	c := h(t, s, "kevin", "card.snooze", map[string]any{"id": id})
	rv := c["revisit"].(string)
	lo, hi := s.addDays(161), s.addDays(203)
	if c["snoozed"] != true || rv < lo || rv > hi || c["lane"] != "todo" {
		t.Fatalf("default snooze %s not in [%s,%s]: %#v", rv, lo, hi, c)
	}
	if has(ids(h(t, s, "kes", "card.view", map[string]any{"view": "todo", "project": "p"})), id) {
		t.Fatal("snoozed card in todo")
	}
	if !has(ids(h(t, s, "kes", "card.view", map[string]any{"view": "icebox", "project": "p"})), id) {
		t.Fatal("snoozed card not in icebox")
	}
	// A Kes queue card snoozed for a month: wakes to Kes.
	ram := h(t, s, "kes", "card.add", map[string]any{"project": "p", "title": "is RAM cheap yet", "queue": "help-me-think"})["id"].(string)
	c = h(t, s, "kes", "card.snooze", map[string]any{"id": ram, "for": "1mo"})
	if c["side"] != "kes" || c["revisit"] != s.addDays(30) {
		t.Fatalf("ram snooze: %#v", c)
	}
	// Running cards can't stay running when snoozed.
	run := h(t, s, "kes", "card.add", map[string]any{"project": "p", "title": "r", "queue": "just-do-it"})["id"].(string)
	h(t, s, "kes", "card.move", map[string]any{"id": run, "lane": "running"})
	if c = h(t, s, "kes", "card.snooze", map[string]any{"id": run, "for": "2w"}); c["lane"] != "queue" {
		t.Fatalf("snoozed running card: %v", c["lane"])
	}
	// Time passes.
	clk.t = clk.t.AddDate(0, 7, 0)
	todo := h(t, s, "kes", "todo.list", map[string]any{})
	found := false
	for _, it := range todo["items"].([]any) {
		m := it.(map[string]any)
		if m["id"] == id {
			found = m["why"] == "woke"
		}
		if m["id"] == ram {
			t.Fatal("Kes's woken queue card showed in Kev's todo")
		}
	}
	if !found {
		t.Fatalf("woken card not in todo as woke: %#v", todo)
	}
	c = h(t, s, "kes", "card.get", map[string]any{"id": id})
	if c["woke_at"] == nil || c["snoozed"] != false {
		t.Fatalf("not woke: %#v", c)
	}
	// Still someday → snoozed again; then archive needs Kev.
	h(t, s, "kevin", "card.act", map[string]any{"id": id, "key": "still-someday"})
	c = h(t, s, "kes", "card.get", map[string]any{"id": id})
	if c["snoozed"] != true || c["woke_at"] != nil {
		t.Fatalf("still someday: %#v", c)
	}
	clk.t = clk.t.AddDate(0, 8, 0)
	h(t, s, "kes", "todo.list", map[string]any{})
	if _, e := s.Handle("card.act", map[string]any{"id": id, "key": "archive"}); e == nil {
		t.Fatal("Kes archived via action")
	}
	c = h(t, s, "kevin", "card.act", map[string]any{"id": id, "key": "archive"})
	if c["card"].(map[string]any)["lane"] != "archived" {
		t.Fatal("archive action")
	}
	// Drag back out of icebox (a move) wakes and clears revisit.
	x := h(t, s, "kes", "card.add", map[string]any{"project": "p", "title": "x", "lane": "todo", "snooze": "2w"})
	if x["snoozed"] != true {
		t.Fatal("capture snoozed")
	}
	c = h(t, s, "kevin", "card.move", map[string]any{"id": x["id"], "lane": "queue", "queue": "just-do-it"})
	if c["snoozed"] != false || c["revisit"] != nil {
		t.Fatalf("move out of icebox: %#v", c)
	}
}

func TestCardActionsForkGetsOnlyTheClickLine(t *testing.T) {
	s, _ := boardStore(t)
	var got []ForkSpawn
	s.SpawnFork = func(f ForkSpawn) error { got = append(got, f); return nil }
	s.SplitTarget = "instance:primary"
	id := h(t, s, "kes", "card.add", map[string]any{"project": "p", "title": "Consolidate the docs", "lane": "todo"})["id"].(string)
	acts := []any{
		map[string]any{"key": "go", "label": "Consolidate them", "primary": true, "effects": []any{map[string]any{"type": "move", "lane": "running"}, map[string]any{"type": "fork", "fresh": true}}},
		map[string]any{"key": "later", "label": "Later", "effects": []any{map[string]any{"type": "snooze", "for": "2w"}}},
	}
	if _, e := s.Handle("card.set", map[string]any{"id": id, "actions": []any{map[string]any{"key": "x", "label": "x", "effects": []any{map[string]any{"type": "fork", "task": "secret"}}}}}); e == nil {
		t.Fatal("fork effect with hidden task accepted")
	}
	if _, e := s.Handle("card.set", map[string]any{"id": id, "actions": []any{map[string]any{"key": "talk", "label": "x", "effects": []any{map[string]any{"type": "reply"}}}}}); e == nil {
		t.Fatal("reserved key accepted")
	}
	c := h(t, s, "kes", "card.set", map[string]any{"id": id, "actions": acts})
	if len(c["actions"].([]any)) != 2 {
		t.Fatalf("actions: %#v", c["actions"])
	}
	r := h(t, s, "kevin", "card.act", map[string]any{"id": id, "key": "go"})
	if len(got) != 1 || got[0].Task != ForkClickLine("Consolidate them", id, "Consolidate the docs") || got[0].Target != "instance:primary" || !got[0].Fresh {
		t.Fatalf("spawn: %#v", got)
	}
	if !strings.HasPrefix(got[0].Task, `Kevin clicked "Consolidate them" on card `+id) {
		t.Fatalf("click line: %s", got[0].Task)
	}
	if r["card"].(map[string]any)["lane"] != "running" {
		t.Fatal("move effect")
	}
	if _, e := s.HandleAs("kevin", "card.act", map[string]any{"id": id, "key": "go"}); e == nil {
		t.Fatal("second fork from the same action")
	}
}

func TestTodoListProjection(t *testing.T) {
	s, _ := boardStore(t)
	todo := h(t, s, "kes", "card.add", map[string]any{"project": "p", "title": "email teacher", "lane": "todo"})["id"].(string)
	due := h(t, s, "kes", "card.add", map[string]any{"project": "p", "title": "spectrum", "queue": "help-me-think", "due": "2026-11-07"})["id"].(string)
	fut := h(t, s, "kes", "jot.add", map[string]any{"title": "later", "due": "2026-12-01"})["id"].(string)
	rev := h(t, s, "kes", "card.add", map[string]any{"project": "p", "title": "review me", "queue": "just-do-it"})["id"].(string)
	h(t, s, "kes", "card.move", map[string]any{"id": rev, "lane": "done", "confidence": "high"})
	low := h(t, s, "kes", "card.add", map[string]any{"project": "p", "title": "tentative", "queue": "just-do-it"})["id"].(string)
	h(t, s, "kes", "card.move", map[string]any{"id": low, "lane": "done", "confidence": "low", "exit": "see if it works"})
	l := h(t, s, "kes", "todo.list", map[string]any{})
	why := map[string]string{}
	for _, it := range l["items"].([]any) {
		m := it.(map[string]any)
		why[m["id"].(string)] = m["why"].(string)
	}
	if why[todo] != "todo" || why[due] != "overdue" || why[rev] != "review" {
		t.Fatalf("todo feed: %v", why)
	}
	if _, ok := why[fut]; ok {
		t.Fatal("future jot in todo")
	}
	if _, ok := why[low]; ok {
		t.Fatal("tentative (low) work landed in Kev's lane")
	}
}

func TestBoardViewColumns(t *testing.T) {
	s, _ := boardStore(t)
	h(t, s, "kes", "card.add", map[string]any{"project": "p", "title": "a", "queue": "just-do-it"})
	h(t, s, "kes", "card.add", map[string]any{"project": "p", "title": "b", "queue": "help-me-think"})
	h(t, s, "kes", "card.add", map[string]any{"project": "p", "title": "legacy"})
	b := h(t, s, "kes", "board.view", map[string]any{"project": "p"})
	cols := b["columns"].(map[string]any)
	q := cols["queue"].(map[string]any)
	if q["just-do-it"].(map[string]any)["total"] != 1 || q["help-me-think"].(map[string]any)["total"] != 1 || q["build-but-wait"].(map[string]any)["total"] != 0 {
		t.Fatalf("queues: %#v", q)
	}
	if cols["legacy"].(map[string]any)["captured"].(map[string]any)["total"] != 1 {
		t.Fatalf("legacy: %#v", cols["legacy"])
	}
	for _, k := range []string{"running", "proving", "review", "todo", "icebox", "archived"} {
		if _, ok := cols[k]; !ok {
			t.Fatalf("missing column %s", k)
		}
	}
}

func TestLegacyReviewIsNotV3Review(t *testing.T) {
	// "review" names both a legacy lane and the v3 Review projection. A v3
	// done/medium card must not be reported as an unmigrated legacy card.
	s, _ := boardStore(t)
	v3 := h(t, s, "kes", "card.add", map[string]any{"project": "p", "title": "v3"})["id"].(string)
	old := h(t, s, "kes", "card.add", map[string]any{"project": "p", "title": "old"})["id"].(string)
	if _, e := s.db.Exec("update cards set lane='done',confidence='medium',approved=0 where id=?", v3); e != nil {
		t.Fatal(e)
	}
	if _, e := s.db.Exec("update cards set lane='review' where id=?", old); e != nil {
		t.Fatal(e)
	}
	cols := h(t, s, "kes", "board.view", map[string]any{"project": "p"})["columns"].(map[string]any)
	leg := cols["legacy"].(map[string]any)
	r, ok := leg["review"].(map[string]any)
	if !ok || r["total"] != 1 || r["items"].([]any)[0].(map[string]any)["id"] != old {
		t.Fatalf("legacy review: %#v", leg)
	}
	if rv := cols["review"].(map[string]any); rv["total"] != 1 || rv["items"].([]any)[0].(map[string]any)["id"] != v3 {
		t.Fatalf("v3 review: %#v", rv)
	}
	// With the legacy card gone, nothing is reported as unmigrated.
	if _, e := s.db.Exec("update cards set lane='todo' where id=?", old); e != nil {
		t.Fatal(e)
	}
	cols = h(t, s, "kes", "board.view", map[string]any{"project": "p"})["columns"].(map[string]any)
	if leg := cols["legacy"].(map[string]any); len(leg) != 0 {
		t.Fatalf("legacy should be empty: %#v", leg)
	}
}

func TestLanesMigrationDryRunThenRun(t *testing.T) {
	s, _ := boardStore(t)
	kev := h(t, s, "kes", "card.add", map[string]any{"project": "p", "title": "kev's", "owner": "kevin"})["id"].(string)
	kes := h(t, s, "kes", "card.add", map[string]any{"project": "p", "title": "kes's", "owner": "kes"})["id"].(string)
	clar := h(t, s, "kes", "card.add", map[string]any{"project": "p", "title": "clar", "lane": "clarified"})["id"].(string)
	jot := h(t, s, "kes", "jot.add", map[string]any{"title": "septic"})["id"].(string)
	dry := h(t, s, "kes", "migrate.lanes", map[string]any{})
	if dry["ran"] != false || dry["cards"] != 4 {
		t.Fatalf("dry: %#v", dry)
	}
	if c := h(t, s, "kes", "card.get", map[string]any{"id": kev}); c["lane"] != "captured" {
		t.Fatal("dry run moved a card")
	}
	if _, e := s.Handle("migrate.lanes", map[string]any{"run": true}); e == nil {
		t.Fatal("run without token")
	}
	run := h(t, s, "kes", "migrate.lanes", map[string]any{"run": true, "token": dry["token"]})
	if run["ran"] != true {
		t.Fatal("not ran")
	}
	a := h(t, s, "kes", "card.get", map[string]any{"id": kev})
	b := h(t, s, "kes", "card.get", map[string]any{"id": kes})
	if a["lane"] != "todo" || a["snoozed"] != true || b["lane"] != "todo" || b["snoozed"] != true {
		t.Fatalf("snoozed into todo: %v %v", a, b)
	}
	if a["revisit"].(string) > s.addDays(105) || a["revisit"].(string) < s.addDays(28) || b["revisit"].(string) < s.addDays(105) {
		t.Fatalf("stagger: kev %v kes %v", a["revisit"], b["revisit"])
	}
	if c := h(t, s, "kes", "card.get", map[string]any{"id": clar}); c["lane"] != "todo" || c["snoozed"] != false {
		t.Fatalf("clarified: %#v", c)
	}
	if c := h(t, s, "kes", "card.get", map[string]any{"id": jot}); c["lane"] != "todo" || c["snoozed"] != false {
		t.Fatalf("jot: %#v", c)
	}
	// After migration capture lands in Todo, and Icebox means snooze.
	n := h(t, s, "kes", "card.add", map[string]any{"project": "p", "title": "fresh"})
	if n["lane"] != "todo" {
		t.Fatalf("capture lane %v", n["lane"])
	}
	c := h(t, s, "kes", "card.move", map[string]any{"id": n["id"], "lane": "icebox"})
	if c["lane"] != "todo" || c["snoozed"] != true {
		t.Fatalf("icebox move: %#v", c)
	}
}

// TestMigrationDryRunOnRealData runs the dry run against a copy of the live
// board when one is reachable. It never writes the live file.
func TestMigrationDryRunOnRealData(t *testing.T) {
	src := os.Getenv("ATTENTION_REAL_COPY")
	if src == "" {
		t.Skip("ATTENTION_REAL_COPY unset")
	}
	dst := filepath.Join(t.TempDir(), "real.sqlite")
	b, e := os.ReadFile(src)
	if e != nil {
		t.Skip(e)
	}
	if e = os.WriteFile(dst, b, 0o600); e != nil {
		t.Fatal(e)
	}
	s, e := Open(dst)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	dry := h(t, s, "kes", "migrate.lanes", map[string]any{})
	t.Logf("real dry run: cards=%v moves=%v snoozed=%v", dry["cards"], dry["moves"], dry["snoozed"])
	for _, w := range dry["weeks"].([]any) {
		t.Logf("  %v", w)
	}
}

func timelineJoined(c map[string]any) string {
	parts := []string{}
	for _, it := range c["timeline"].([]any) {
		parts = append(parts, it.(map[string]any)["text"].(string))
	}
	return strings.Join(parts, "\n")
}

func h(t *testing.T, s *Store, actor, op string, a map[string]any) map[string]any {
	t.Helper()
	v, e := s.HandleAs(actor, op, a)
	if e != nil {
		t.Fatalf("%s: %v", op, e)
	}
	m, _ := v.(map[string]any)
	return m
}
