package attention

// Boards rethink (spec: kestrel notes/2026-11-08-boards-rethink-spec.md, rev 3.1).
//
// The card is the one primitive. Lanes say who moves next:
//   todo     Kev's (decide / tap / do; fresh capture lands here)
//   queue    Kes may pick up; `queue` says how (just-do-it, build-but-wait, help-me-think)
//   running  an agent holds it
//   done     the agent's work is finished; Proving and Review are projections over it
//   archived
// Proving = done ∧ confidence < high. Review = done ∧ confidence > low ∧ ¬approved.
// Icebox is a projection too: snoozed cards (snoozed ∧ revisit in the future).
// The legacy lanes (captured, icebox, clarified, inflight, review, settling) stay
// readable until the lanes-v3 migration moves their cards.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	legacyLanes = []string{"captured", "icebox", "clarified", "inflight", "review", "settling"}
	v3Lanes     = []string{"todo", "queue", "running", "done"}
	allLanes    = append(append(append([]string{}, legacyLanes...), v3Lanes...), "archived")
	queues      = []string{"just-do-it", "build-but-wait", "help-me-think"}
	confidences = []string{"low", "medium", "high"}
)

func validLane(x string) bool { return one(x, allLanes...) }

// cardsV3DDL is the cards table after the boards rethink. The lane check admits
// both the legacy and the new lanes; the new columns carry the card attributes.
const cardsV3DDL = `create table %s (id text primary key,project text not null references projects(slug),lane text not null check(lane in ('captured','icebox','clarified','inflight','review','settling','todo','queue','running','done','archived')),title text not null,summary text,owner text check(owner in ('kevin','kes')),policy text check(policy in ('ship-quiet','ship-tell','pr-evidence','talk-first')),blocked text,done integer not null default 0,captured_at text not null,moved_at text not null,updated_at text not null,hold_until text,due text,maybe_on text,queue text check(queue in ('just-do-it','build-but-wait','help-me-think')),revisit text,snoozed integer not null default 0,confidence text check(confidence in ('low','medium','high')),approved integer not null default 0,exit text,woke_at text,actions text)`

// upgradeCardsV3 rebuilds the cards table once so its lane check admits the new
// lanes and it has the new attribute columns. SQLite can't alter a check
// constraint, so this is the documented 12-step rebuild: foreign keys off on one
// connection, copy, drop, rename, re-index, check, foreign keys back on.
// Data is copied unchanged; no card moves lane here.
func upgradeCardsV3(db *sql.DB) error {
	var ddl string
	if e := db.QueryRow("select sql from sqlite_master where type='table' and name='cards'").Scan(&ddl); e != nil {
		return e
	}
	if strings.Contains(ddl, "'todo'") {
		return nil
	}
	ctx := context.Background()
	conn, e := db.Conn(ctx)
	if e != nil {
		return e
	}
	defer conn.Close()
	if _, e = conn.ExecContext(ctx, "pragma foreign_keys=off"); e != nil {
		return e
	}
	defer conn.ExecContext(ctx, "pragma foreign_keys=on")
	tx, e := conn.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	cols := "id,project,lane,title,summary,owner,policy,blocked,done,captured_at,moved_at,updated_at,hold_until,due,maybe_on"
	steps := []string{
		"drop table if exists cards_v3",
		fmt.Sprintf(cardsV3DDL, "cards_v3"),
		"insert into cards_v3(" + cols + ") select " + cols + " from cards",
		"drop table cards",
		"alter table cards_v3 rename to cards",
		"create index if not exists cards_project_lane on cards(project,lane,moved_at)",
		"create index if not exists cards_snooze on cards(snoozed,revisit)",
	}
	for _, q := range steps {
		if _, e = tx.ExecContext(ctx, q); e != nil {
			tx.Rollback()
			return fmt.Errorf("cards v3 upgrade: %s: %w", q, e)
		}
	}
	rs, e := tx.QueryContext(ctx, "pragma foreign_key_check")
	if e != nil {
		tx.Rollback()
		return e
	}
	bad := rs.Next()
	rs.Close()
	if bad {
		tx.Rollback()
		return errors.New("cards v3 upgrade: foreign key check failed")
	}
	return tx.Commit()
}

// ---- dates and durations ------------------------------------------------

var durRe = regexp.MustCompile(`^(\d{1,3})(d|w|mo|y)$`)

// parseDur reads "10d", "2w", "6mo", "1y" into days.
func parseDur(x string) (int, bool) {
	m := durRe.FindStringSubmatch(strings.TrimSpace(strings.ToLower(x)))
	if m == nil {
		return 0, false
	}
	n, _ := strconv.Atoi(m[1])
	switch m[2] {
	case "w":
		n *= 7
	case "mo":
		n *= 30
	case "y":
		n *= 365
	}
	return n, n > 0 && n <= 5*365
}

// defaultSnoozeDays is ~6 months with ±3 weeks of uniform jitter, so a pile of
// cards snoozed on one afternoon doesn't wake on one morning.
func defaultSnoozeDays(r *rand.Rand) int { return 182 - 21 + r.Intn(43) }

func (s *Store) addDays(days int) string {
	return s.now().In(s.tz).AddDate(0, 0, days).Format(dateLayout)
}

// ---- wake ---------------------------------------------------------------

// wake clears `snoozed` on every card whose revisit has come, marking it woke
// ("back from Icebox"). Cheap no-op when nothing is due, so reads don't churn
// the revision.
func (s *Store) wake() error {
	today := s.today()
	var n int
	if e := s.db.QueryRow("select count(*) from cards where snoozed=1 and revisit<=?", today).Scan(&n); e != nil || n == 0 {
		return e
	}
	_, e := s.write(func(tx *sql.Tx, at string) (any, error) {
		rs, e := tx.Query("select id,revisit from cards where snoozed=1 and revisit<=?", today)
		if e != nil {
			return nil, e
		}
		type w struct{ id, rv string }
		ws := []w{}
		for rs.Next() {
			var x w
			_ = rs.Scan(&x.id, &x.rv)
			ws = append(ws, x)
		}
		rs.Close()
		for _, x := range ws {
			if _, e = tx.Exec("update cards set snoozed=0,woke_at=?,updated_at=? where id=?", at, at, x.id); e == nil {
				e = event(tx, x.id, "system", "woke", at, map[string]any{"revisit": x.rv})
			}
			if e != nil {
				return nil, e
			}
		}
		return nil, nil
	})
	return e
}

// ---- snooze -------------------------------------------------------------

// snooze parks a card until a date: snoozed=1, revisit set. The card keeps its
// lane (the lane says who it wakes to). Running/done cards first go back to
// their queue (or Todo): snoozing means stopping work. Kes may snooze any card;
// by convention Kes doesn't snooze Kev's unprompted (guidance, not code).
func (s *Store) snoozeTx(tx *sql.Tx, at, actor, id string, a map[string]any) error {
	var lane string
	var queue sql.NullString
	if e := tx.QueryRow("select lane,queue from cards where id=?", id).Scan(&lane, &queue); e != nil {
		return code("not_found", "card not found")
	}
	if lane == "archived" {
		return code("invalid_request", "an archived card can't be snoozed")
	}
	until, hasUntil := str(a, "until")
	dur, hasFor := str(a, "for")
	if hasUntil && hasFor {
		return code("invalid_request", "use until or for, not both")
	}
	var rv string
	switch {
	case hasUntil:
		if !validDate(until) || until <= s.today() {
			return code("invalid_request", "until must be a future YYYY-MM-DD")
		}
		rv = until
	case hasFor:
		days, ok := parseDur(dur)
		if !ok {
			return code("invalid_request", "for must be like 10d, 2w, 6mo, 1y")
		}
		rv = s.addDays(days)
	default:
		rv = s.addDays(defaultSnoozeDays(rand.New(rand.NewSource(time.Now().UnixNano()))))
	}
	if one(lane, "running", "done", "inflight", "settling") {
		to := "todo"
		if queue.Valid {
			to = "queue"
		}
		if _, e := tx.Exec("update cards set lane=?,confidence=null,exit=null,moved_at=? where id=?", to, at, id); e != nil {
			return e
		}
		if e := event(tx, id, actor, "moved", at, map[string]any{"from": lane, "to": to, "why": "snoozed: work stops"}); e != nil {
			return e
		}
	}
	if _, e := tx.Exec("update cards set snoozed=1,revisit=?,woke_at=null,updated_at=? where id=?", rv, at, id); e != nil {
		return e
	}
	return event(tx, id, actor, "snoozed", at, map[string]any{"until": rv})
}

// ---- projections --------------------------------------------------------

// views are the board's columns as queries. Each returns rows already ordered.
func (s *Store) viewRows(view, project, queue string) ([]cardRow, error) {
	w := []string{"p.hidden=0"}
	args := []any{}
	if project != "" {
		w = []string{"c.project=?"}
		args = append(args, project)
	}
	order := "c.moved_at desc"
	switch view {
	case "queue":
		w = append(w, "c.lane='queue'", "c.snoozed=0")
		if queue != "" {
			w = append(w, "c.queue=?")
			args = append(args, queue)
		}
		order = "c.moved_at asc,c.captured_at asc" // oldest-first: what Kes pulls next
	case "running":
		w = append(w, "c.lane='running'")
		order = "c.moved_at asc"
	case "proving":
		w = append(w, "c.lane='done'", "c.confidence in ('low','medium')")
		order = "c.revisit is null,c.revisit asc,c.moved_at asc"
	case "review":
		w = append(w, "c.lane='done'", "c.confidence in ('medium','high')", "c.approved=0")
	case "todo":
		w = append(w, "c.lane='todo'", "c.snoozed=0", "c.done=0")
		order = "c.woke_at is null,c.due is null,c.due asc,c.moved_at desc"
	case "icebox":
		w = append(w, "((c.snoozed=1 and c.lane<>'archived') or c.lane='icebox')")
		order = "c.revisit is null,c.revisit asc,c.moved_at desc"
	case "archived":
		w = append(w, "c.lane='archived'")
	default:
		// "legacy:<lane>" reads a raw legacy lane. Bare names still work for
		// the ones that aren't also v3 views ("review" is both: bare means v3).
		lane := strings.TrimPrefix(view, "legacy:")
		if one(lane, legacyLanes...) && lane != "icebox" {
			w = append(w, "c.lane=?")
			args = append(args, lane)
			order = "c.moved_at asc"
			break
		}
		return nil, code("invalid_request", "unknown view")
	}
	return s.cards(strings.Join(w, " and ")+" order by "+order, args...)
}

var boardViews = []string{"queue", "running", "proving", "review", "todo", "icebox", "archived"}

func page(x []any, offset, limit int) map[string]any {
	total := len(x)
	lo := min(offset, total)
	hi := min(lo+limit, total)
	return map[string]any{"items": x[lo:hi], "total": total, "offset": lo, "truncated": hi < total}
}

// cardView pages one column: {view, project?, queue?, offset?, limit?}.
func (s *Store) cardView(a map[string]any) (any, error) {
	if e := s.wake(); e != nil {
		return nil, e
	}
	view, ok := str(a, "view")
	if !ok {
		return nil, code("invalid_request", "view required")
	}
	project, _ := str(a, "project")
	q, _ := str(a, "queue")
	if q != "" && !one(q, queues...) {
		return nil, code("invalid_request", "invalid queue")
	}
	offset, ok := whole(a, "offset", 0, 0, 1<<30)
	limit, lok := whole(a, "limit", 64, 1, 200)
	if !ok || !lok {
		return nil, code("invalid_request", "invalid offset or limit")
	}
	rows, e := s.viewRows(view, project, q)
	if e != nil {
		return nil, e
	}
	x, e := s.concises(rows)
	if e != nil {
		return nil, e
	}
	return page(x, offset, limit), nil
}

// boardView returns every column of one project's board, each paged
// independently (first `limit` rows plus a total). Queue splits by sub-lane.
// Legacy lanes still holding cards appear under "legacy" until migrated.
func (s *Store) boardView(a map[string]any) (any, error) {
	if e := s.wake(); e != nil {
		return nil, e
	}
	project, ok := str(a, "project")
	if !ok || !validSlug(project) && project != "_today" {
		return nil, code("invalid_request", "project required")
	}
	limit, lok := whole(a, "limit", 64, 1, 200)
	if !lok {
		return nil, code("invalid_request", "invalid limit")
	}
	cols := map[string]any{}
	for _, v := range boardViews {
		if v == "queue" {
			sub := map[string]any{}
			for _, q := range queues {
				rows, e := s.viewRows("queue", project, q)
				if e != nil {
					return nil, e
				}
				x, e := s.concises(rows)
				if e != nil {
					return nil, e
				}
				sub[q] = page(x, 0, limit)
			}
			cols["queue"] = sub
			continue
		}
		rows, e := s.viewRows(v, project, "")
		if e != nil {
			return nil, e
		}
		if v == "archived" {
			cols[v] = map[string]any{"items": []any{}, "total": len(rows), "offset": 0, "truncated": len(rows) > 0}
			continue
		}
		x, e := s.concises(rows)
		if e != nil {
			return nil, e
		}
		cols[v] = page(x, 0, limit)
	}
	legacy := map[string]any{}
	for _, l := range legacyLanes {
		if l == "icebox" {
			continue
		}
		rows, e := s.viewRows("legacy:"+l, project, "")
		if e != nil {
			return nil, e
		}
		if len(rows) == 0 {
			continue
		}
		x, e := s.concises(rows)
		if e != nil {
			return nil, e
		}
		legacy[l] = page(x, 0, limit)
	}
	cols["legacy"] = legacy
	return map[string]any{"project": project, "columns": cols}, nil
}

// todoList is Kev's Todo feed (attention sidebar and desk): unsnoozed lane-todo
// cards, anything due today or overdue, cards woken back to him, and Review.
// Uncapped by design (Kev, Nov 8). Each item says why it's here.
func (s *Store) todoList(a map[string]any) (any, error) {
	if e := s.wake(); e != nil {
		return nil, e
	}
	today := s.today()
	rows, e := s.cards("c.lane not in ('archived') and c.done=0 and (p.hidden=0 or c.project='_today') and ((c.lane='todo' and c.snoozed=0) or (c.due is not null and c.due<=?) or (c.lane='done' and c.confidence in ('medium','high') and c.approved=0)) order by c.due is null,c.due asc,c.moved_at desc", today)
	if e != nil {
		return nil, e
	}
	jots := map[string]bool{}
	for _, r := range s.visibleJots(filterProject(rows, "_today")) {
		jots[r.ID] = true
	}
	out := []any{}
	for _, r := range rows {
		if r.Project == "_today" && !jots[r.ID] {
			continue
		}
		x, e := s.concise(r)
		if e != nil {
			return nil, e
		}
		why := "todo"
		switch {
		case r.Lane == "done":
			why = "review"
		case r.WokeAt.Valid:
			why = "woke"
		case r.Due.Valid && r.Due.String < today:
			why = "overdue"
		case r.Due.Valid && r.Due.String == today:
			why = "due"
		case r.Revisit.Valid && r.Revisit.String <= today:
			why = "revisit"
		}
		x["why"] = why
		out = append(out, x)
	}
	offset, ok := whole(a, "offset", 0, 0, 1<<30)
	limit, lok := whole(a, "limit", 200, 1, 500)
	if !ok || !lok {
		return nil, code("invalid_request", "invalid offset or limit")
	}
	return page(out, offset, limit), nil
}

func filterProject(rows []cardRow, p string) []cardRow {
	out := []cardRow{}
	for _, r := range rows {
		if r.Project == p {
			out = append(out, r)
		}
	}
	return out
}

// side says whose a card is right now: who moves next.
func side(r cardRow) string {
	switch r.Lane {
	case "queue", "running", "clarified", "inflight":
		return "kes"
	case "done":
		if r.Confidence.Valid && r.Confidence.String == "low" {
			return "kes"
		}
		return "kevin"
	}
	return "kevin"
}

// ---- lanes-v3 migration -------------------------------------------------

type migPlan struct {
	ID, From, To, Owner string
	Queue, Revisit      string
	Snooze              bool
	Project, Title      string
}

// migrationPlan maps every open legacy card to its v3 lane. Captured and legacy
// icebox cards (outside _today) are snoozed into Todo with staggered wake dates:
// Kev's across weeks 4–15, Kes's across weeks 15–26, random within the window
// (seeded, so a dry run and the real run agree). All wake to Kev: putting a card
// in a Kes queue takes his drag, and a migration doesn't grant it (rev 3.1).
func (s *Store) migrationPlan(seed int64) ([]migPlan, error) {
	rows, e := s.cards("c.lane in ('captured','icebox','clarified','inflight','review','settling') order by c.captured_at asc,c.id asc")
	if e != nil {
		return nil, e
	}
	r := rand.New(rand.NewSource(seed))
	out := []migPlan{}
	for _, c := range rows {
		owner := "kevin"
		if c.Owner.Valid && c.Owner.String == "kes" {
			owner = "kes"
		}
		p := migPlan{ID: c.ID, From: c.Lane, Owner: owner, Project: c.Project, Title: c.Title}
		switch {
		case c.Project == "_today":
			p.To = "todo" // jots stay live: due / maybe-today keep their meaning
		case c.Lane == "captured" || c.Lane == "icebox":
			p.To, p.Snooze = "todo", true
			lo, hi := 28, 105
			if owner == "kes" {
				lo, hi = 105, 182
			}
			p.Revisit = s.addDays(lo + r.Intn(hi-lo+1))
		case c.Lane == "inflight":
			var running int
			_ = s.db.QueryRow("select count(*) from agents where card_id=? and state in ('running','blocked')", c.ID).Scan(&running)
			if running > 0 {
				p.To = "running"
			} else {
				p.To = "todo"
			}
		case c.Lane == "settling":
			p.To = "done"
		default: // clarified, review: Kev decides what happens next
			p.To = "todo"
		}
		out = append(out, p)
	}
	return out, nil
}

// migrateLanes runs or previews the lanes-v3 migration. Dry run (default)
// reports counts, the wake-date histogram and a token; `run` with that token
// applies exactly the previewed plan.
func (s *Store) migrateLanes(actor string, a map[string]any) (any, error) {
	run, ok := boolean(a, "run", false)
	if !ok {
		return nil, code("invalid_request", "invalid run")
	}
	var seed int64
	if t, has := str(a, "token"); has {
		x, err := strconv.ParseInt(t, 36, 64)
		if err != nil {
			return nil, code("invalid_request", "invalid token")
		}
		seed = x
	} else if run {
		return nil, code("invalid_request", "run needs the token from a dry run")
	} else {
		seed = time.Now().UnixNano() & (1<<53 - 1)
	}
	plan, e := s.migrationPlan(seed)
	if e != nil {
		return nil, e
	}
	moves := map[string]int{}
	weeks := map[int]map[string]int{}
	snoozed := map[string]int{}
	for _, p := range plan {
		k := p.From + "→" + p.To
		if p.Snooze {
			k += " (snoozed)"
			snoozed[p.Owner]++
			t, _ := time.ParseInLocation(dateLayout, p.Revisit, s.tz)
			wk := int(t.Sub(s.now().In(s.tz)).Hours()/24/7) + 1
			if weeks[wk] == nil {
				weeks[wk] = map[string]int{}
			}
			weeks[wk][p.Owner]++
		}
		moves[k]++
	}
	hist := []any{}
	keys := []int{}
	for k := range weeks {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	for _, k := range keys {
		hist = append(hist, map[string]any{"week": k, "kevin": weeks[k]["kevin"], "kes": weeks[k]["kes"]})
	}
	report := map[string]any{"cards": len(plan), "moves": moves, "snoozed": snoozed, "weeks": hist, "token": strconv.FormatInt(seed, 36), "ran": false}
	if !run {
		return report, nil
	}
	_, e = s.write(func(tx *sql.Tx, at string) (any, error) {
		for _, p := range plan {
			var lane string
			if er := tx.QueryRow("select lane from cards where id=?", p.ID).Scan(&lane); er != nil || lane != p.From {
				continue // moved since the dry run: leave it alone
			}
			q := "update cards set lane=?,moved_at=?,updated_at=? where id=?"
			args := []any{p.To, at, at, p.ID}
			if p.Snooze {
				q = "update cards set lane=?,moved_at=?,updated_at=?,snoozed=1,revisit=? where id=?"
				args = []any{p.To, at, at, p.Revisit, p.ID}
			}
			if p.To == "done" {
				q = "update cards set lane=?,moved_at=?,updated_at=?,confidence='medium',exit=coalesce(exit,'Kev confirms it has settled') where id=?"
			}
			if _, er := tx.Exec(q, args...); er != nil {
				return nil, er
			}
			data := map[string]any{"from": p.From, "to": p.To, "why": "boards rethink migration"}
			if p.Snooze {
				data["revisit"] = p.Revisit
			}
			if er := event(tx, p.ID, actor, "moved", at, data); er != nil {
				return nil, er
			}
		}
		_, er := tx.Exec("insert into meta(key,value) values('lanes_v3_migrated',?) on conflict(key) do update set value=excluded.value", at)
		return nil, er
	})
	if e != nil {
		return nil, e
	}
	report["ran"] = true
	return report, nil
}

// jsonText is a small helper for optional JSON columns.
func jsonText(v any) any {
	if v == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// ---- v3 attribute edits, moves, approval --------------------------------

// lanesV3 reports whether the lanes-v3 migration has run (capture then lands
// in Todo, and Icebox means snooze).
func (s *Store) lanesV3() bool {
	var n int
	_ = s.db.QueryRow("select count(*) from meta where key='lanes_v3_migrated'").Scan(&n)
	return n > 0
}

var confRank = map[string]int{"low": 1, "medium": 2, "high": 3}

// setV3 applies the v3 attributes of card.set: queue, revisit, confidence,
// exit, approved (+approval_reason), actions. Reports whether anything changed.
func (s *Store) setV3(tx *sql.Tx, at, actor, id string, a map[string]any) (bool, error) {
	var lane string
	var queue, revisit, conf, exit sql.NullString
	var approved, snoozed int
	if e := tx.QueryRow("select lane,queue,revisit,confidence,exit,approved,snoozed from cards where id=?", id).Scan(&lane, &queue, &revisit, &conf, &exit, &approved, &snoozed); e != nil {
		return false, e
	}
	changed := false
	ed := map[string]any{}
	if _, has := a["queue"]; has {
		x, ok := optstr(a, "queue")
		if !ok || (x != nil && !one(*x, queues...)) || (x == nil && lane == "queue") {
			return false, code("invalid_request", "invalid queue")
		}
		if !sameNull(ptrArg(x), queue) {
			if _, e := tx.Exec("update cards set queue=? where id=?", ptrArg(x), id); e != nil {
				return false, e
			}
			ed["queue"] = ptrArg(x)
		}
	}
	if _, has := a["revisit"]; has {
		x, ok := optstr(a, "revisit")
		if !ok || (x != nil && !validDate(*x)) {
			return false, code("invalid_request", "revisit must be YYYY-MM-DD or null")
		}
		if !sameNull(ptrArg(x), revisit) {
			q := "update cards set revisit=? where id=?"
			if x == nil {
				// No date left to wake on: clearing revisit unsnoozes.
				q = "update cards set revisit=?,snoozed=0 where id=?"
			}
			if _, e := tx.Exec(q, ptrArg(x), id); e != nil {
				return false, e
			}
			ed["revisit"] = ptrArg(x)
		}
	}
	if _, has := a["exit"]; has {
		x, ok := optstr(a, "exit")
		if !ok || (x != nil && !validText(*x, 300, true)) {
			return false, code("invalid_request", "exit is one line of at most 300 characters")
		}
		if !sameNull(ptrArg(x), exit) {
			if _, e := tx.Exec("update cards set exit=? where id=?", ptrArg(x), id); e != nil {
				return false, e
			}
			ed["exit"] = ptrArg(x)
			exit = sql.NullString{String: deref(x), Valid: x != nil}
		}
	}
	if len(ed) > 0 {
		if e := event(tx, id, actor, "edited", at, ed); e != nil {
			return false, e
		}
		changed = true
	}
	if _, has := a["confidence"]; has {
		x, ok := optstr(a, "confidence")
		if !ok || (x != nil && !one(*x, confidences...)) {
			return false, code("invalid_request", "confidence is low, medium or high")
		}
		if lane == "done" && x == nil {
			return false, code("invalid_request", "a done card has a confidence; move it back to running instead")
		}
		if lane == "done" && x != nil && *x != "high" && !exit.Valid {
			return false, code("invalid_request", "Proving needs an exit: say what proves it (--exit)")
		}
		if !sameNull(ptrArg(x), conf) {
			if _, e := tx.Exec("update cards set confidence=? where id=?", ptrArg(x), id); e != nil {
				return false, e
			}
			d := map[string]any{"confidence": ptrArg(x)}
			if conf.Valid {
				d["from"] = conf.String
			}
			if e := event(tx, id, actor, "confidence", at, d); e != nil {
				return false, e
			}
			changed = true
		}
	}
	if v, has := a["approved"]; has {
		x, ok := v.(bool)
		reason, _ := str(a, "approval_reason")
		if !ok {
			return false, code("invalid_request", "approved is true or false")
		}
		if x && actor != "kevin" {
			return false, code("invalid_request", "approval is Kev's")
		}
		if !x && actor == "kes" && strings.TrimSpace(reason) == "" {
			return false, code("invalid_request", "resetting approval needs approval_reason: why Kev should look again")
		}
		if (approved != 0) != x {
			if _, e := tx.Exec("update cards set approved=? where id=?", x, id); e != nil {
				return false, e
			}
			d := map[string]any{"approved": x}
			if reason != "" {
				d["reason"] = reason
			}
			if e := event(tx, id, actor, "approval", at, d); e != nil {
				return false, e
			}
			changed = true
		}
	} else if _, has := a["approval_reason"]; has {
		return false, code("invalid_request", "approval_reason goes with approved")
	}
	if v, has := a["actions"]; has {
		acts, e := validCardActions(v)
		if e != nil {
			return false, e
		}
		var val any
		if len(acts) > 0 {
			val = jsonText(acts)
		}
		if _, e = tx.Exec("update cards set actions=? where id=?", val, id); e != nil {
			return false, e
		}
		if e = event(tx, id, actor, "edited", at, map[string]any{"actions": len(acts)}); e != nil {
			return false, e
		}
		changed = true
	}
	if changed {
		return true, s.autoArchive(tx, at, id)
	}
	return false, nil
}

func deref(x *string) string {
	if x == nil {
		return ""
	}
	return *x
}

// approvalGate: once Kev has approved a card, new commits, PRs or runs from
// Kes must say whether that's material. "reset" clears approval (he looks
// again); "keep" records why it isn't ("DST-only edge-case fixes"). Either way
// the timeline holds the reason, so "didn't I already look at this?" has an answer.
func (s *Store) approvalGate(tx *sql.Tx, at, actor, id, kind string, a map[string]any) error {
	var approved int
	_ = tx.QueryRow("select approved from cards where id=?", id).Scan(&approved)
	decision, has := str(a, "approval")
	reason, _ := str(a, "approval_reason")
	if approved == 0 || actor != "kes" || !one(kind, "pr", "commit", "run") {
		if has {
			return code("invalid_request", "approval applies only to new work on a card Kev has approved")
		}
		return nil
	}
	if !one(decision, "keep", "reset") || strings.TrimSpace(reason) == "" {
		return code("invalid_request", "Kev approved this card: say whether this is material with approval keep|reset and a one-line approval_reason")
	}
	d := map[string]any{"approved": decision == "keep", "kept": decision == "keep", "reason": reason}
	if decision == "reset" {
		if _, e := tx.Exec("update cards set approved=0 where id=?", id); e != nil {
			return e
		}
	}
	return event(tx, id, actor, "approval", at, d)
}

// moveV3 moves a card into a v3 lane (or archives it). Any lane change wakes
// it (a snooze ends when the card moves) and clears the woke mark. Into queue
// needs a queue; into done needs a confidence, and Proving (below high) needs
// an exit. Leaving done reopens the work: confidence, exit and approval clear.
func (s *Store) moveV3(tx *sql.Tx, at, actor, id, lane string, a map[string]any) error {
	var old string
	var queue, conf, exit sql.NullString
	var approved, snoozed int
	if e := tx.QueryRow("select lane,queue,confidence,exit,approved,snoozed from cards where id=?", id).Scan(&old, &queue, &conf, &exit, &approved, &snoozed); e != nil {
		return e
	}
	if _, has := a["hold_until"]; has && a["hold_until"] != nil {
		return code("invalid_request", "hold_until applies only to settling")
	}
	q, qok := optstr(a, "queue")
	if !qok || (q != nil && !one(*q, queues...)) {
		return code("invalid_request", "invalid queue")
	}
	if lane == "queue" {
		if q == nil && !queue.Valid {
			return code("invalid_request", "a queue card needs a queue (just-do-it, build-but-wait, help-me-think)")
		}
	} else if q != nil {
		return code("invalid_request", "queue goes with lane queue")
	}
	c, cok := optstr(a, "confidence")
	if !cok || (c != nil && !one(*c, confidences...)) {
		return code("invalid_request", "confidence is low, medium or high")
	}
	x, xok := optstr(a, "exit")
	if !xok || (x != nil && !validText(*x, 300, true)) {
		return code("invalid_request", "exit is one line of at most 300 characters")
	}
	rv, rok := optstr(a, "revisit")
	if !rok || (rv != nil && !validDate(*rv)) {
		return code("invalid_request", "revisit must be YYYY-MM-DD")
	}
	if lane != "done" && (c != nil || x != nil) {
		return code("invalid_request", "confidence and exit go with lane done")
	}
	if lane == "done" {
		if c == nil && !conf.Valid {
			return code("invalid_request", "moving to done needs a confidence (low, medium, high)")
		}
		eff := conf.String
		if c != nil {
			eff = *c
		}
		if eff != "high" && x == nil && !exit.Valid {
			return code("invalid_request", "Proving needs an exit: say what proves it (exit), and set revisit to when it can be judged")
		}
	}
	if old != lane {
		if _, e := tx.Exec("update cards set lane=?,moved_at=?,updated_at=?,woke_at=null where id=?", lane, at, at, id); e != nil {
			return e
		}
		d := map[string]any{"from": old, "to": lane}
		if q != nil {
			d["queue"] = *q
		}
		if e := event(tx, id, actor, "moved", at, d); e != nil {
			return e
		}
		if snoozed != 0 {
			if _, e := tx.Exec("update cards set snoozed=0,revisit=null where id=?", id); e != nil {
				return e
			}
		}
		if old == "done" {
			if _, e := tx.Exec("update cards set confidence=null,exit=null,approved=0 where id=?", id); e != nil {
				return e
			}
			if approved != 0 {
				if e := event(tx, id, actor, "approval", at, map[string]any{"approved": false, "reason": "work reopened"}); e != nil {
					return e
				}
			}
		}
	}
	set := map[string]any{}
	if q != nil {
		set["queue"] = *q
	}
	if c != nil {
		set["confidence"] = *c
	}
	if x != nil {
		set["exit"] = *x
	}
	if rv != nil {
		set["revisit"] = *rv
	}
	if len(set) > 0 {
		// exit first so a below-high confidence finds it
		if x != nil {
			if _, e := s.setV3(tx, at, actor, id, map[string]any{"exit": *x}); e != nil {
				return e
			}
			delete(set, "exit")
		}
		if len(set) > 0 {
			if _, e := s.setV3(tx, at, actor, id, set); e != nil {
				return e
			}
		}
	}
	return s.autoArchive(tx, at, id)
}

// autoArchive: done ∧ high ∧ approved means proven and seen. Archive it.
func (s *Store) autoArchive(tx *sql.Tx, at, id string) error {
	var lane string
	var conf sql.NullString
	var approved int
	if e := tx.QueryRow("select lane,confidence,approved from cards where id=?", id).Scan(&lane, &conf, &approved); e != nil {
		return e
	}
	if lane != "done" || !conf.Valid || conf.String != "high" || approved == 0 {
		return nil
	}
	if _, e := tx.Exec("update cards set lane='archived',moved_at=?,updated_at=? where id=?", at, at, id); e != nil {
		return e
	}
	return event(tx, id, "system", "moved", at, map[string]any{"from": "done", "to": "archived", "why": "proven and approved"})
}
