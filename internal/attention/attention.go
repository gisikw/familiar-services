// Package attention is the SQLite-backed Attention board.
package attention

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	_ "github.com/mattn/go-sqlite3"
	"strings"
	"sync"
	"time"
)

const schema = `
create table if not exists projects (slug text primary key,label text not null,default_policy text not null check(default_policy in ('ship-quiet','ship-tell','pr-evidence','talk-first')),repo text,created_at text not null,hidden integer not null default 0);
create table if not exists cards (id text primary key,project text not null references projects(slug),lane text not null check(lane in ('captured','icebox','clarified','inflight','review','settling','archived')),title text not null,summary text,owner text check(owner in ('kevin','kes')),policy text check(policy in ('ship-quiet','ship-tell','pr-evidence','talk-first')),blocked text,done integer not null default 0,captured_at text not null,moved_at text not null,updated_at text not null);
create index if not exists cards_project_lane on cards(project,lane,moved_at);
create table if not exists events (id integer primary key,card_id text not null references cards(id) on delete cascade,at text not null,actor text not null,kind text not null,data text);
create index if not exists events_card on events(card_id,id);
create table if not exists notes (id integer primary key,card_id text not null references cards(id) on delete cascade,at text not null,by text not null check(by in ('kevin','kes')),text text not null,detail text);
create table if not exists evidence (id integer primary key,card_id text not null references cards(id) on delete cascade,at text not null,kind text not null check(kind in ('pr','commit','shot','run','link')),title text not null,ref text,meta text);
create table if not exists agents (id integer primary key,card_id text not null references cards(id) on delete cascade,name text not null,model text,host text,harness text,state text not null check(state in ('running','blocked','done','failed','cancelled')),question text,started_at text not null,ended_at text);
create index if not exists agents_card on agents(card_id);
create table if not exists meta (key text primary key,value text not null);`

type Store struct {
	db  *sql.DB
	now func() time.Time
	mu  sync.Mutex
}
type CodedError struct {
	Code string
	Err  error
}

func (e *CodedError) Error() string { return e.Err.Error() }
func code(c, s string) error        { return &CodedError{c, errors.New(s)} }
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("attention db is required")
	}
	db, e := sql.Open("sqlite3", path+"?_busy_timeout=5000&_foreign_keys=on&_journal_mode=WAL")
	if e != nil {
		return nil, e
	}
	if _, e = db.Exec(schema); e != nil {
		db.Close()
		return nil, e
	}
	s := &Store{db: db, now: time.Now}
	tx, e := db.Begin()
	if e != nil {
		return nil, e
	}
	at := s.stamp()
	_, e = tx.Exec("insert or ignore into meta(key,value) values('schema_version','1')")
	if e == nil {
		_, e = tx.Exec("insert or ignore into meta(key,value) values('revision',?)", uuid())
	}
	if e == nil {
		_, e = tx.Exec("insert or ignore into projects(slug,label,default_policy,repo,created_at,hidden) values('_today','Today','ship-tell',null,?,1)", at)
	}
	if e != nil {
		tx.Rollback()
		db.Close()
		return nil, e
	}
	e = tx.Commit()
	return s, e
}
func (s *Store) Close() error  { return s.db.Close() }
func (s *Store) stamp() string { return s.now().UTC().Format(time.RFC3339Nano) }
func uuid() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&15 | 64
	b[8] = b[8]&63 | 128
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
func str(a map[string]any, k string) (string, bool) {
	v, ok := a[k]
	x, yes := v.(string)
	return x, ok && yes
}
func optstr(a map[string]any, k string) (*string, bool) {
	v, ok := a[k]
	if !ok {
		return nil, true
	}
	if v == nil {
		return nil, true
	}
	x, yes := v.(string)
	if !yes {
		return nil, false
	}
	return &x, true
}
func boolean(a map[string]any, k string, d bool) (bool, bool) {
	v, ok := a[k]
	if !ok {
		return d, true
	}
	x, yes := v.(bool)
	return x, yes
}
func one(x string, v ...string) bool {
	for _, s := range v {
		if x == s {
			return true
		}
	}
	return false
}
func allowed(a map[string]any, names ...string) bool {
	for k := range a {
		if !one(k, names...) {
			return false
		}
	}
	return true
}
func uuidLike(x string) bool {
	if len(x) != 36 {
		return false
	}
	for i, r := range x {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return false
			}
			continue
		}
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}
func validText(x string, max int, line bool) bool {
	if len(x) == 0 || len(x) > max || strings.TrimSpace(x) == "" {
		return false
	}
	for _, r := range x {
		if r == 127 || (r < 32 && (line || r != '\n')) {
			return false
		}
	}
	return true
}
func clip(v []any) any {
	if len(v) <= 64 {
		return v
	}
	return map[string]any{"items": v[:64], "total": len(v), "truncated": true}
}

type cardRow struct {
	ID, Project, Lane, Title          string
	Summary, Owner, Policy, Blocked   sql.NullString
	Done                              int
	Captured, Moved, Updated, Default string
	Hidden                            int
}

func scanCard(sc interface{ Scan(...any) error }) (cardRow, error) {
	var r cardRow
	e := sc.Scan(&r.ID, &r.Project, &r.Lane, &r.Title, &r.Summary, &r.Owner, &r.Policy, &r.Blocked, &r.Done, &r.Captured, &r.Moved, &r.Updated, &r.Default, &r.Hidden)
	return r, e
}

const cardCols = "c.id,c.project,c.lane,c.title,c.summary,c.owner,c.policy,c.blocked,c.done,c.captured_at,c.moved_at,c.updated_at,p.default_policy,p.hidden"

func (s *Store) card(id string) (cardRow, error) {
	return scanCard(s.db.QueryRow("select "+cardCols+" from cards c join projects p on p.slug=c.project where c.id=?", id))
}
func (s *Store) cards(where string, args ...any) ([]cardRow, error) {
	rs, e := s.db.Query("select "+cardCols+" from cards c join projects p on p.slug=c.project where "+where, args...)
	if e != nil {
		return nil, e
	}
	defer rs.Close()
	out := []cardRow{}
	for rs.Next() {
		r, e := scanCard(rs)
		if e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, rs.Err()
}
func n(v sql.NullString) any {
	if v.Valid {
		return v.String
	}
	return nil
}
func parseMS(x string) int64 { t, _ := time.Parse(time.RFC3339Nano, x); return t.UnixMilli() }
func (s *Store) concise(r cardRow) (map[string]any, error) {
	var running, blocked, notes, evidence int
	if e := s.db.QueryRow("select count(*) filter(where state='running'),count(*) filter(where state='blocked') from agents where card_id=?", r.ID).Scan(&running, &blocked); e != nil {
		return nil, e
	}
	_ = s.db.QueryRow("select count(*) from notes where card_id=?", r.ID).Scan(&notes)
	_ = s.db.QueryRow("select count(*) from evidence where card_id=?", r.ID).Scan(&evidence)
	edge := any(nil)
	if r.Blocked.Valid || blocked > 0 {
		edge = "blocked"
	} else if r.Lane == "review" {
		edge = "review"
	} else if running > 0 {
		edge = "live"
	}
	effective := r.Default
	if r.Policy.Valid {
		effective = r.Policy.String
	}
	now := s.now().UnixMilli()
	return map[string]any{"id": r.ID, "project": r.Project, "lane": r.Lane, "title": r.Title, "owner": n(r.Owner), "policy": n(r.Policy), "effective_policy": effective, "diverges": r.Policy.Valid && r.Policy.String != r.Default, "blocked": n(r.Blocked), "edge": edge, "dispatched": r.Lane == "inflight" && running+blocked > 0, "stale": r.Project == "_today" && r.Done == 0 && now-parseMS(r.Captured) > 36*60*60*1000, "fading": r.Lane == "captured" && now-parseMS(r.Captured) > 14*24*60*60*1000, "done": r.Done != 0, "age_s": max64(0, (now-parseMS(r.Captured))/1000), "moved_s": max64(0, (now-parseMS(r.Moved))/1000), "agents": map[string]any{"running": running, "blocked": blocked}, "notes": notes, "evidence": evidence}, nil
}
func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
func (s *Store) concises(rows []cardRow) ([]any, error) {
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		x, e := s.concise(r)
		if e != nil {
			return nil, e
		}
		out = append(out, x)
	}
	return out, nil
}
func (s *Store) full(id string) (map[string]any, error) {
	r, e := s.card(id)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, code("not_found", "card not found")
	}
	if e != nil {
		return nil, e
	}
	out, e := s.concise(r)
	if e != nil {
		return nil, e
	}
	out["summary"] = n(r.Summary)
	out["captured_at"] = r.Captured
	out["moved_at"] = r.Moved
	out["updated_at"] = r.Updated
	notes := []any{}
	rs, e := s.db.Query("select id,at,by,text,detail from notes where card_id=? order by id desc", id)
	if e != nil {
		return nil, e
	}
	for rs.Next() {
		var i int
		var at, by, text string
		var d sql.NullString
		_ = rs.Scan(&i, &at, &by, &text, &d)
		notes = append(notes, map[string]any{"id": i, "at": at, "by": by, "text": text, "detail": n(d)})
	}
	rs.Close()
	out["notes"] = notes
	evs := []any{}
	rs, e = s.db.Query("select id,at,kind,title,ref,meta from evidence where card_id=? order by id", id)
	if e != nil {
		return nil, e
	}
	for rs.Next() {
		var i int
		var at, k, t string
		var ref, meta sql.NullString
		_ = rs.Scan(&i, &at, &k, &t, &ref, &meta)
		var m any
		if meta.Valid {
			_ = json.Unmarshal([]byte(meta.String), &m)
		}
		evs = append(evs, map[string]any{"id": i, "at": at, "kind": k, "title": t, "ref": n(ref), "meta": m})
	}
	rs.Close()
	out["evidence"] = evs
	ag := []any{}
	rs, e = s.db.Query("select id,name,model,host,harness,state,question,started_at,ended_at from agents where card_id=? order by id", id)
	if e != nil {
		return nil, e
	}
	for rs.Next() {
		var i int
		var name, state, started string
		var model, host, harness, q, ended sql.NullString
		_ = rs.Scan(&i, &name, &model, &host, &harness, &state, &q, &started, &ended)
		ag = append(ag, map[string]any{"id": i, "name": name, "model": n(model), "host": n(host), "harness": n(harness), "state": state, "question": n(q), "started_at": started, "ended_at": n(ended)})
	}
	rs.Close()
	out["agent_list"] = ag
	timeline := []any{}
	var count int
	_ = s.db.QueryRow("select count(*) from events where card_id=?", id).Scan(&count)
	rs, e = s.db.Query("select id,at,actor,kind,data from events where card_id=? order by id desc limit 12", id)
	if e != nil {
		return nil, e
	}
	for rs.Next() {
		var i int
		var at, actor, kind string
		var data sql.NullString
		_ = rs.Scan(&i, &at, &actor, &kind, &data)
		var d map[string]any
		if data.Valid {
			_ = json.Unmarshal([]byte(data.String), &d)
		}
		text := timelineText(actor, kind, d, r.Default)
		runes := []rune(text)
		if len(runes) > 512 {
			text = string(runes[:512])
		}
		timeline = append([]any{map[string]any{"id": i, "at": at, "actor": actor, "kind": kind, "text": text}}, timeline...)
	}
	rs.Close()
	out["timeline"] = timeline
	out["event_count"] = count
	return out, nil
}
func who(x string) string {
	if x == "kevin" {
		return "Kevin"
	}
	if x == "kes" {
		return "Kes"
	}
	return "system"
}
func timelineText(actor, kind string, d map[string]any, def string) string {
	g := func(k string) string { x, _ := d[k].(string); return x }
	suffix := " — " + who(actor)
	switch kind {
	case "created":
		if g("from") == "plate" {
			return "captured — migrated from Plate"
		}
		text := "captured" + suffix
		if g("owner") != "" {
			text += ", for " + who(g("owner"))
		}
		return text
	case "moved":
		to := g("to")
		if to == "clarified" {
			return "clarified" + suffix
		}
		if to == "inflight" {
			return "in flight" + suffix
		}
		return "moved to " + to + suffix
	case "edited":
		keys := []string{}
		for _, k := range []string{"title", "summary"} {
			if _, ok := d[k]; ok {
				keys = append(keys, k)
			}
		}
		what := strings.Join(keys, ", ")
		if what == "" {
			what = "card"
		}
		return "edited " + what + suffix
	case "assigned":
		if g("owner") != "" {
			return who(g("owner")) + " has this" + suffix
		}
		return "unassigned" + suffix
	case "policy":
		if p := g("policy"); p != "" {
			text := "policy " + p
			if p != def {
				text += " (diverges: project default is " + def + ")"
			}
			return text + suffix
		}
		return "policy back to project default" + suffix
	case "dispatched":
		text := "dispatched — " + g("name")
		if g("host") != "" {
			text += " on " + g("host")
		}
		return text
	case "agent":
		text := g("name") + " " + g("state")
		if g("question") != "" {
			text += " — " + g("question")
		}
		return text
	case "blocked":
		return strings.TrimSpace("blocked — " + g("reason"))
	case "unblocked":
		return "unblocked" + suffix
	case "evidence":
		label := g("kind") + " added"
		if g("kind") == "pr" {
			label = "PR opened"
		}
		return strings.TrimSpace(label + " — " + g("title"))
	case "note":
		return strings.TrimSpace("note — " + who(actor) + ": " + g("text"))
	case "done":
		if done, ok := d["done"].(bool); ok && !done {
			return "reopened" + suffix
		}
		return "done" + suffix
	}
	return kind
}

func (s *Store) write(fn func(*sql.Tx, string) (any, error)) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, e := s.db.Begin()
	if e != nil {
		return nil, e
	}
	at := s.stamp()
	v, e := fn(tx, at)
	if e == nil {
		_, e = tx.Exec("insert into meta(key,value) values('revision',?) on conflict(key) do update set value=excluded.value", uuid())
	}
	if e != nil {
		tx.Rollback()
		return nil, e
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	return v, nil
}
func event(tx *sql.Tx, id, actor, kind, at string, data any) error {
	var x any
	if data != nil {
		b, _ := json.Marshal(data)
		x = string(b)
	}
	_, e := tx.Exec("insert into events(card_id,at,actor,kind,data) values(?,?,?,?,?)", id, at, actor, kind, x)
	return e
}

// Handle applies one existing imp attn operation. Authorship is always Kes.
func (s *Store) Handle(op string, a map[string]any) (any, error) {
	fields := map[string][]string{
		"project.list": {"hidden"}, "project.get": {"slug"}, "project.add": {"slug", "label", "default_policy", "repo"}, "project.set": {"slug", "label", "default_policy", "repo"},
		"card.list": {"project", "lane", "owner", "edge", "q"}, "card.get": {"id"}, "card.add": {"project", "title", "lane", "summary", "owner", "policy"}, "card.set": {"id", "title", "summary", "owner", "policy"},
		"card.move": {"id", "lane"}, "card.block": {"id", "reason"}, "card.unblock": {"id"}, "card.done": {"id", "done"}, "note.add": {"card", "text", "detail"},
		"evidence.add": {"card", "kind", "title", "ref", "meta"}, "agent.start": {"card", "name", "model", "host", "harness"}, "agent.set": {"card", "name", "state", "question"},
		"jot.add": {"title", "owner"}, "jot.list": {}, "jot.clear-done": {}, "status": {},
	}
	if names, ok := fields[op]; !ok || !allowed(a, names...) {
		return nil, code("invalid_request", "invalid arguments")
	}
	switch op {
	case "project.list":
		hidden, ok := boolean(a, "hidden", false)
		if !ok {
			return nil, code("invalid_request", "invalid hidden")
		}
		q := "select slug,label,default_policy,repo from projects"
		if !hidden {
			q += " where hidden=0"
		}
		q += " order by lower(label),slug"
		rs, e := s.db.Query(q)
		if e != nil {
			return nil, e
		}
		defer rs.Close()
		out := []any{}
		for rs.Next() {
			var slug, label, p string
			var repo sql.NullString
			_ = rs.Scan(&slug, &label, &p, &repo)
			counts := map[string]any{"captured": 0, "clarified": 0, "inflight": 0, "review": 0, "settling": 0}
			cr, _ := s.db.Query("select lane,count(*) from cards where project=? and done=0 group by lane", slug)
			for cr.Next() {
				var l string
				var n int
				_ = cr.Scan(&l, &n)
				if _, ok := counts[l]; ok {
					counts[l] = n
				}
			}
			cr.Close()
			out = append(out, map[string]any{"slug": slug, "label": label, "default_policy": p, "repo": n(repo), "counts": counts})
		}
		return clip(out), nil
	case "project.get":
		slug, ok := str(a, "slug")
		if !ok || !validSlug(slug) {
			return nil, code("invalid_request", "invalid slug")
		}
		var label, policy string
		var repo sql.NullString
		if e := s.db.QueryRow("select label,default_policy,repo from projects where slug=? and hidden=0", slug).Scan(&label, &policy, &repo); errors.Is(e, sql.ErrNoRows) {
			return nil, code("not_found", "project not found")
		} else if e != nil {
			return nil, e
		}
		counts := map[string]any{"captured": 0, "clarified": 0, "inflight": 0, "review": 0, "settling": 0}
		cr, e := s.db.Query("select lane,count(*) from cards where project=? and done=0 group by lane", slug)
		if e != nil {
			return nil, e
		}
		for cr.Next() {
			var lane string
			var count int
			_ = cr.Scan(&lane, &count)
			if _, ok := counts[lane]; ok {
				counts[lane] = count
			}
		}
		cr.Close()
		return map[string]any{"slug": slug, "label": label, "default_policy": policy, "repo": n(repo), "counts": counts}, nil
	case "card.get":
		id, ok := str(a, "id")
		if !ok || !uuidLike(id) {
			return nil, code("invalid_request", "id required")
		}
		return s.full(id)
	case "card.list":
		return s.cardList(a)
	case "jot.list":
		rows, e := s.cards("c.project='_today' and c.lane<>'archived' order by c.done asc,c.captured_at asc")
		if e != nil {
			return nil, e
		}
		x, e := s.concises(rows)
		return clip(x), e
	case "status":
		return s.status()
	case "project.add":
		return s.projectAdd(a)
	case "project.set":
		return s.projectSet(a)
	case "card.add":
		return s.cardAdd(a, false)
	case "jot.add":
		return s.cardAdd(a, true)
	case "card.set", "card.move", "card.block", "card.unblock", "card.done", "note.add", "evidence.add", "agent.start", "agent.set", "jot.clear-done":
		return s.mutate(op, a)
	default:
		return nil, code("invalid_request", "unknown attention operation")
	}
}
func (s *Store) cardList(a map[string]any) (any, error) {
	w := []string{"p.hidden=0"}
	args := []any{}
	if x, ok := str(a, "project"); ok {
		w = append(w, "c.project=?")
		args = append(args, x)
	}
	if x, ok := str(a, "lane"); ok {
		w = append(w, "c.lane=?")
		args = append(args, x)
	}
	if len(w) == 1 {
		return nil, code("invalid_request", "project or lane required")
	}
	if x, ok := str(a, "owner"); ok {
		w = append(w, "c.owner=?")
		args = append(args, x)
	}
	if x, ok := str(a, "q"); ok {
		w = append(w, "(instr(lower(c.title),?)>0 or instr(lower(coalesce(c.summary,'')),?)>0)")
		args = append(args, strings.ToLower(x), strings.ToLower(x))
	}
	rows, e := s.cards(strings.Join(w, " and ")+" order by c.lane,c.moved_at asc,c.captured_at asc", args...)
	if e != nil {
		return nil, e
	}
	x, e := s.concises(rows)
	if edge, ok := str(a, "edge"); ok {
		z := []any{}
		for _, v := range x {
			if v.(map[string]any)["edge"] == edge {
				z = append(z, v)
			}
		}
		x = z
	}
	return clip(x), e
}
func (s *Store) projectAdd(a map[string]any) (any, error) {
	slug, ok := str(a, "slug")
	if !ok || !validSlug(slug) {
		return nil, code("invalid_request", "invalid slug")
	}
	label := slug
	if x, ok := str(a, "label"); ok {
		label = x
	}
	if !validText(label, 200, true) {
		return nil, code("invalid_request", "invalid label")
	}
	p := "ship-tell"
	if x, ok := str(a, "default_policy"); ok {
		p = x
	}
	if !one(p, "ship-quiet", "ship-tell", "pr-evidence", "talk-first") {
		return nil, code("invalid_request", "invalid policy")
	}
	repo, rok := optstr(a, "repo")
	if !rok || (repo != nil && !validText(*repo, 4096, true)) {
		return nil, code("invalid_request", "invalid repo")
	}
	_, e := s.write(func(tx *sql.Tx, at string) (any, error) {
		_, e := tx.Exec("insert into projects(slug,label,default_policy,repo,created_at,hidden) values(?,?,?,?,?,0)", slug, label, p, repo, at)
		return nil, e
	})
	if e != nil {
		return nil, code("conflict", "project exists")
	}
	return s.Handle("project.get", map[string]any{"slug": slug})
}
func validSlug(x string) bool {
	if len(x) < 1 || len(x) > 64 {
		return false
	}
	for i, c := range x {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || (c == '-' && i > 0)) {
			return false
		}
	}
	return true
}
func (s *Store) projectSet(a map[string]any) (any, error) {
	slug, ok := str(a, "slug")
	if !ok || !validSlug(slug) {
		return nil, code("invalid_request", "invalid slug")
	}
	_, e := s.write(func(tx *sql.Tx, at string) (any, error) {
		var label, p string
		var repo sql.NullString
		if e := tx.QueryRow("select label,default_policy,repo from projects where slug=? and hidden=0", slug).Scan(&label, &p, &repo); e != nil {
			return nil, code("not_found", "project not found")
		}
		if x, ok := str(a, "label"); ok {
			label = x
		}
		if x, ok := str(a, "default_policy"); ok {
			p = x
		}
		rv := any(nil)
		if repo.Valid {
			rv = repo.String
		}
		if x, ok := optstr(a, "repo"); ok {
			if _, exists := a["repo"]; exists {
				if x != nil {
					rv = *x
				} else {
					rv = nil
				}
			}
		} else {
			return nil, code("invalid_request", "invalid repo")
		}
		if !validText(label, 200, true) || !one(p, "ship-quiet", "ship-tell", "pr-evidence", "talk-first") {
			return nil, code("invalid_request", "invalid project")
		}
		if x, ok := rv.(string); ok && !validText(x, 4096, true) {
			return nil, code("invalid_request", "invalid repo")
		}
		_, e := tx.Exec("update projects set label=?,default_policy=?,repo=? where slug=?", label, p, rv, slug)
		return nil, e
	})
	if e != nil {
		return nil, e
	}
	return s.Handle("project.get", map[string]any{"slug": slug})
}
func (s *Store) cardAdd(a map[string]any, jot bool) (any, error) {
	title, ok := str(a, "title")
	if !ok || !validText(title, 200, true) {
		return nil, code("invalid_request", "invalid title")
	}
	project := "_today"
	lane := "captured"
	if owner, exists := a["owner"]; exists {
		if x, ok := owner.(string); !ok || !one(x, "kevin", "kes") {
			return nil, code("invalid_request", "invalid owner")
		}
	}
	if policy, exists := a["policy"]; exists {
		if x, ok := policy.(string); !ok || !one(x, "ship-quiet", "ship-tell", "pr-evidence", "talk-first") {
			return nil, code("invalid_request", "invalid policy")
		}
	}
	if summary, exists := a["summary"]; exists {
		if x, ok := summary.(string); !ok || !validText(x, 4096, false) {
			return nil, code("invalid_request", "invalid summary")
		}
	}
	if jot {
		if x, ok := str(a, "owner"); ok && x == "kes" {
			lane = "clarified"
		}
	} else {
		project, ok = str(a, "project")
		if !ok {
			return nil, code("invalid_request", "project required")
		}
		if x, ok := str(a, "lane"); ok {
			lane = x
		}
		if !one(lane, "captured", "icebox", "clarified", "inflight", "review", "settling", "archived") {
			return nil, code("invalid_request", "invalid lane")
		}
	}
	id := uuid()
	_, e := s.write(func(tx *sql.Tx, at string) (any, error) {
		var n int
		if e := tx.QueryRow("select count(*) from projects where slug=? and (?='_today' or hidden=0)", project, project).Scan(&n); e != nil || n == 0 {
			return nil, code("not_found", "project not found")
		}
		summary, _ := optstr(a, "summary")
		owner, _ := optstr(a, "owner")
		if jot && owner == nil {
			x := "kevin"
			owner = &x
		}
		policy, _ := optstr(a, "policy")
		_, e := tx.Exec("insert into cards(id,project,lane,title,summary,owner,policy,blocked,done,captured_at,moved_at,updated_at) values(?,?,?,?,?,?,?,null,0,?,?,?)", id, project, lane, title, summary, owner, policy, at, at, at)
		if e == nil {
			data := map[string]any{"lane": lane}
			if owner != nil {
				data["owner"] = *owner
			}
			if policy != nil {
				data["policy"] = *policy
			}
			e = event(tx, id, "kes", "created", at, data)
		}
		return nil, e
	})
	if e != nil {
		return nil, e
	}
	return s.full(id)
}
func (s *Store) mutate(op string, a map[string]any) (any, error) {
	var id string
	if op != "jot.clear-done" {
		if op == "note.add" || op == "evidence.add" || op == "agent.start" || op == "agent.set" {
			id, _ = str(a, "card")
		} else {
			id, _ = str(a, "id")
		}
		if !uuidLike(id) {
			return nil, code("invalid_request", "valid card id required")
		}
	}
	v, e := s.write(func(tx *sql.Tx, at string) (any, error) {
		exists := func() error {
			var n int
			if er := tx.QueryRow("select count(*) from cards where id=?", id).Scan(&n); er != nil || n == 0 {
				return code("not_found", "card not found")
			}
			return nil
		}
		if op != "jot.clear-done" {
			if er := exists(); er != nil {
				return nil, er
			}
		}
		touch := func(moved bool) error {
			q := "update cards set updated_at=? where id=?"
			args := []any{at, id}
			if moved {
				q = "update cards set updated_at=?,moved_at=? where id=?"
				args = []any{at, at, id}
			}
			_, er := tx.Exec(q, args...)
			return er
		}
		switch op {
		case "card.set":
			var oldTitle, defaultPolicy string
			var oldSummary, oldOwner, oldPolicy sql.NullString
			if er := tx.QueryRow("select c.title,c.summary,c.owner,c.policy,p.default_policy from cards c join projects p on p.slug=c.project where c.id=?", id).Scan(&oldTitle, &oldSummary, &oldOwner, &oldPolicy, &defaultPolicy); er != nil {
				return nil, er
			}
			title := oldTitle
			if _, has := a["title"]; has {
				x, ok := str(a, "title")
				if !ok || !validText(x, 200, true) {
					return nil, code("invalid_request", "invalid title")
				}
				title = x
			}
			sv, ov, pv := nullArg(oldSummary), nullArg(oldOwner), nullArg(oldPolicy)
			if _, has := a["summary"]; has {
				x, ok := optstr(a, "summary")
				if !ok || (x != nil && !validText(*x, 4096, false)) {
					return nil, code("invalid_request", "invalid summary")
				}
				sv = ptrArg(x)
			}
			if _, has := a["owner"]; has {
				x, ok := optstr(a, "owner")
				if !ok || (x != nil && !one(*x, "kevin", "kes")) {
					return nil, code("invalid_request", "invalid owner")
				}
				ov = ptrArg(x)
			}
			if _, has := a["policy"]; has {
				x, ok := optstr(a, "policy")
				if !ok || (x != nil && !one(*x, "ship-quiet", "ship-tell", "pr-evidence", "talk-first")) {
					return nil, code("invalid_request", "invalid policy")
				}
				pv = ptrArg(x)
			}
			_, er := tx.Exec("update cards set title=?,summary=?,owner=?,policy=? where id=?", title, sv, ov, pv, id)
			if er != nil {
				return nil, er
			}
			changed := false
			edited := map[string]any{}
			if title != oldTitle {
				edited["title"] = true
			}
			if !sameNull(sv, oldSummary) {
				edited["summary"] = true
			}
			if len(edited) > 0 {
				er = event(tx, id, "kes", "edited", at, edited)
				changed = true
			}
			if er == nil && !sameNull(ov, oldOwner) {
				er = event(tx, id, "kes", "assigned", at, map[string]any{"owner": ov})
				changed = true
			}
			if er == nil && !sameNull(pv, oldPolicy) {
				er = event(tx, id, "kes", "policy", at, map[string]any{"policy": pv, "default": defaultPolicy})
				changed = true
			}
			if er == nil && changed {
				er = touch(false)
			}
			return id, er
		case "card.move":
			lane, ok := str(a, "lane")
			if !ok || !one(lane, "captured", "icebox", "clarified", "inflight", "review", "settling", "archived") {
				return nil, code("invalid_request", "invalid lane")
			}
			var old string
			_ = tx.QueryRow("select lane from cards where id=?", id).Scan(&old)
			if lane == "icebox" && old != "captured" {
				return nil, code("invalid_request", "icebox only from captured")
			}
			if old != lane {
				_, e := tx.Exec("update cards set lane=? where id=?", lane, id)
				if e == nil {
					e = event(tx, id, "kes", "moved", at, map[string]any{"from": old, "to": lane})
				}
				if e == nil {
					e = touch(true)
				}
				return id, e
			}
			return id, nil
		case "card.block":
			reason, ok := str(a, "reason")
			if !ok || !validText(reason, 300, true) {
				return nil, code("invalid_request", "invalid reason")
			}
			_, e := tx.Exec("update cards set blocked=? where id=?", reason, id)
			if e == nil {
				e = event(tx, id, "kes", "blocked", at, map[string]any{"reason": reason})
			}
			if e == nil {
				e = touch(false)
			}
			return id, e
		case "card.unblock":
			var blocked sql.NullString
			_ = tx.QueryRow("select blocked from cards where id=?", id).Scan(&blocked)
			if !blocked.Valid {
				return id, nil
			}
			_, e := tx.Exec("update cards set blocked=null where id=?", id)
			if e == nil {
				e = event(tx, id, "kes", "unblocked", at, nil)
			}
			if e == nil {
				e = touch(false)
			}
			return id, e
		case "card.done":
			var project string
			var oldDone bool
			_ = tx.QueryRow("select project,done<>0 from cards where id=?", id).Scan(&project, &oldDone)
			if project != "_today" {
				return nil, code("invalid_request", "only jots can be done")
			}
			done, ok := boolean(a, "done", true)
			if !ok {
				return nil, code("invalid_request", "invalid done")
			}
			if oldDone == done {
				return id, nil
			}
			_, e := tx.Exec("update cards set done=? where id=?", done, id)
			if e == nil {
				e = event(tx, id, "kes", "done", at, map[string]any{"done": done})
			}
			if e == nil {
				e = touch(false)
			}
			return id, e
		case "note.add":
			text, ok := str(a, "text")
			if !ok || !validText(text, 300, true) {
				return nil, code("invalid_request", "invalid note")
			}
			detail, _ := optstr(a, "detail")
			_, e := tx.Exec("insert into notes(card_id,at,by,text,detail) values(?,?,'kes',?,?)", id, at, text, detail)
			if e == nil {
				e = event(tx, id, "kes", "note", at, map[string]any{"text": text})
			}
			if e == nil {
				e = touch(false)
			}
			return id, e
		case "evidence.add":
			kind, ok := str(a, "kind")
			title, tok := str(a, "title")
			if !ok || !tok || !one(kind, "pr", "commit", "shot", "run", "link") {
				return nil, code("invalid_request", "invalid evidence")
			}
			ref, _ := optstr(a, "ref")
			var meta any
			if x, has := a["meta"]; has {
				b, er := json.Marshal(x)
				if er != nil || len(b) > 2048 {
					return nil, code("invalid_request", "invalid evidence meta")
				}
				meta = string(b)
			}
			_, e := tx.Exec("insert into evidence(card_id,at,kind,title,ref,meta) values(?,?,?,?,?,?)", id, at, kind, title, ref, meta)
			if e == nil {
				e = event(tx, id, "kes", "evidence", at, map[string]any{"kind": kind, "title": title})
			}
			if e == nil {
				e = touch(false)
			}
			return id, e
		case "agent.start":
			name, ok := str(a, "name")
			if !ok {
				return nil, code("invalid_request", "name required")
			}
			var n int
			_ = tx.QueryRow("select count(*) from agents where card_id=? and name=? and ended_at is null", id, name).Scan(&n)
			if n > 0 {
				return nil, code("conflict", "agent already open")
			}
			model, _ := optstr(a, "model")
			host, _ := optstr(a, "host")
			harness, _ := optstr(a, "harness")
			_, e := tx.Exec("insert into agents(card_id,name,model,host,harness,state,question,started_at,ended_at) values(?,?,?,?,?,'running',null,?,null)", id, name, model, host, harness, at)
			if e == nil {
				e = event(tx, id, "kes", "dispatched", at, map[string]any{"name": name})
			}
			if e == nil {
				e = touch(false)
			}
			return id, e
		case "agent.set":
			name, ok := str(a, "name")
			state, sok := str(a, "state")
			if !ok || !sok || !one(state, "running", "blocked", "done", "failed", "cancelled") {
				return nil, code("invalid_request", "invalid agent state")
			}
			var aid int
			var oldq, ended sql.NullString
			if er := tx.QueryRow("select id,question,ended_at from agents where card_id=? and name=? order by (ended_at is null) desc,id desc limit 1", id, name).Scan(&aid, &oldq, &ended); errors.Is(er, sql.ErrNoRows) {
				return nil, code("not_found", "agent not found")
			} else if er != nil {
				return nil, er
			}
			q := any(nil)
			if state == "blocked" {
				if x, ok := str(a, "question"); ok {
					q = x
				} else if oldq.Valid {
					q = oldq.String
				}
			}
			end := any(nil)
			if one(state, "done", "failed", "cancelled") {
				if ended.Valid {
					end = ended.String
				} else {
					end = at
				}
			}
			_, e := tx.Exec("update agents set state=?,question=?,ended_at=? where id=?", state, q, end, aid)
			if e == nil {
				e = event(tx, id, "kes", "agent", at, map[string]any{"name": name, "state": state, "question": q})
			}
			if e == nil {
				e = touch(false)
			}
			return id, e
		case "jot.clear-done":
			rs, e := tx.Query("select id from cards where project='_today' and done=1 and lane<>'archived'")
			if e != nil {
				return nil, e
			}
			ids := []string{}
			for rs.Next() {
				var x string
				_ = rs.Scan(&x)
				ids = append(ids, x)
			}
			rs.Close()
			for _, x := range ids {
				_, e = tx.Exec("update cards set lane='archived',updated_at=?,moved_at=? where id=?", at, at, x)
				if e == nil {
					e = event(tx, x, "kes", "moved", at, map[string]any{"to": "archived"})
				}
				if e != nil {
					return nil, e
				}
			}
			return map[string]any{"archived": len(ids)}, nil
		}
		return nil, code("invalid_request", "unknown mutation")
	})
	if e != nil {
		return nil, e
	}
	if op == "jot.clear-done" {
		return v, nil
	}
	return s.full(id)
}
func nullArg(x sql.NullString) any {
	if x.Valid {
		return x.String
	}
	return nil
}
func ptrArg(x *string) any {
	if x == nil {
		return nil
	}
	return *x
}
func sameNull(value any, old sql.NullString) bool {
	if value == nil {
		return !old.Valid
	}
	x, ok := value.(string)
	return ok && old.Valid && x == old.String
}
func (s *Store) status() (any, error) {
	rows, e := s.cards("p.hidden=0 and c.lane in ('captured','clarified','inflight','review','settling')")
	if e != nil {
		return nil, e
	}
	needs, inflight := 0, 0
	for _, r := range rows {
		x, e := s.concise(r)
		if e != nil {
			return nil, e
		}
		if x["edge"] == "blocked" || r.Lane == "review" {
			needs++
		}
		if r.Lane == "inflight" {
			inflight++
		}
	}
	var running, blocked int
	_ = s.db.QueryRow("select count(*) filter(where a.state='running'),count(*) filter(where a.state='blocked') from agents a join cards c on c.id=a.card_id join projects p on p.slug=c.project where p.hidden=0 and c.lane<>'archived'").Scan(&running, &blocked)
	jots, e := s.cards("c.project='_today' and c.lane<>'archived'")
	if e != nil {
		return nil, e
	}
	open, stale := 0, 0
	for _, r := range jots {
		if r.Done == 0 {
			open++
			if s.now().UnixMilli()-parseMS(r.Captured) > 36*60*60*1000 {
				stale++
			}
		}
	}
	return map[string]any{"agents": map[string]any{"running": running, "blocked": blocked}, "needs_attention": needs, "inflight": inflight, "jots": map[string]any{"open": open, "stale": stale}}, nil
}
