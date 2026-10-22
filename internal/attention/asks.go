package attention

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// undoWindow is how long after a resolve Kev's undo is honored. The UI shows
// the collapsed row for 5s; the server allows slack for a slow round trip.
const undoWindow = 2 * time.Minute

// Asks are Kes's inbox to Kev: bounded decisions about one card. At most
// maxSurfaced are surfaced at a time; the rest are staged until Kev asks for
// more. Nothing auto-refills.
const maxSurfaced = 3

// askOp lists, per allowed action op, the accepted and the required fields.
var askOps = map[string]struct{ fields, required []string }{
	"card.move":    {[]string{"id", "lane"}, []string{"id", "lane"}},
	"card.set":     {[]string{"id", "owner", "title", "summary"}, []string{"id"}},
	"card.done":    {[]string{"id", "done"}, []string{"id"}},
	"card.block":   {[]string{"id", "reason"}, []string{"id", "reason"}},
	"card.unblock": {[]string{"id"}, []string{"id"}},
	"note.add":     {[]string{"card", "text", "detail"}, []string{"card", "text"}},
}

type askAction struct {
	Key     string           `json:"key"`
	Label   string           `json:"label"`
	Done    string           `json:"done,omitempty"` // past-tense outcome, e.g. "Closed"
	Primary bool             `json:"primary,omitempty"`
	Ops     []map[string]any `json:"ops,omitempty"`
	Fork    *askFork         `json:"fork,omitempty"`
}

// askFork is a one-click fork: picking the action starts a fork of Target
// (the primary the ask was written from) with Task. A started fork can't be
// taken back, so an action with a fork has no undo.
type askFork struct {
	Task   string `json:"task"`
	Label  string `json:"label,omitempty"`
	Fresh  bool   `json:"fresh,omitempty"`
	Target string `json:"target"`
}

// ForkSpawn is what SpawnFork receives: a stable ID (one fork per ask, even if
// a delivery is retried), the instance to fork from, and the fork's brief.
type ForkSpawn struct {
	ID, Target, Task, Label string
	Fresh                   bool
	Ask                     int64
	Choice                  string
}

func validAskFork(raw any) (*askFork, error) {
	bad := func(msg string) error { return code("invalid_request", "invalid action fork: "+msg) }
	m, ok := raw.(map[string]any)
	if !ok || !allowed(m, "task", "label", "fresh", "target") {
		return nil, bad("a fork is {task, label?, fresh?, target}")
	}
	task, ok := str(m, "task")
	if !ok || strings.TrimSpace(task) == "" || !validUnits(task, 8000, false) {
		return nil, bad("task must be prose of at most 8000 characters")
	}
	label, ok := optstr(m, "label")
	if !ok || (label != nil && !validUnits(*label, 60, true)) {
		return nil, bad("label must be one line of at most 60 characters")
	}
	fresh, ok := boolean(m, "fresh", false)
	if !ok {
		return nil, bad("invalid fresh")
	}
	target, ok := str(m, "target")
	if !ok || !targetRE.MatchString(target) {
		return nil, bad("target must be instance:<session id> (imp attn ask add fills it in)")
	}
	f := &askFork{Task: task, Fresh: fresh, Target: target}
	if label != nil {
		f.Label = *label
	}
	return f, nil
}

var targetRE = regexp.MustCompile(`^instance:[A-Za-z0-9._-]{1,128}$`)

// units counts UTF-16 code units, the unit the contract's bounds use.
func units(x string) int {
	n := 0
	for _, r := range x {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// validUnits is validText bounded in UTF-16 code units.
func validUnits(x string, max int, line bool) bool {
	if !utf8.ValidString(x) || units(x) > max {
		return false
	}
	return validText(x, len(x), line)
}

func validKey(x string) bool {
	if len(x) < 1 || len(x) > 32 {
		return false
	}
	for _, c := range x {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return false
		}
	}
	return true
}

func validLink(x string) bool {
	if !validUnits(x, 4096, true) {
		return false
	}
	if strings.HasPrefix(x, "/") {
		return true
	}
	u, e := url.Parse(x)
	return e == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// intArg reads a positive integer id (json.Number from the socket, float64
// from generic JSON, or a Go int in tests).
func intArg(a map[string]any, k string) (int64, bool) {
	var v int64
	switch x := a[k].(type) {
	case json.Number:
		n, e := strconv.ParseInt(string(x), 10, 64)
		if e != nil {
			return 0, false
		}
		v = n
	case float64:
		if x != math.Trunc(x) || x > math.MaxInt32*1e6 {
			return 0, false
		}
		v = int64(x)
	case int:
		v = int64(x)
	case int64:
		v = x
	default:
		return 0, false
	}
	return v, v > 0
}

// validAskOp checks one action op's shape and resolves its card reference to
// a full id. The returned op is what is stored.
func (s *Store) validAskOp(raw any) (map[string]any, error) {
	bad := func(msg string) error { return code("invalid_request", "invalid action op: "+msg) }
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, bad("must be an object")
	}
	name, _ := str(m, "op")
	spec, ok := askOps[name]
	if !ok {
		return nil, bad("op must be one of card.move, card.set, card.done, card.block, card.unblock, note.add")
	}
	args := map[string]any{}
	for k, v := range m {
		if k != "op" {
			args[k] = v
		}
	}
	if !allowed(args, spec.fields...) {
		return nil, bad(name + " accepts only " + strings.Join(spec.fields, ", "))
	}
	for _, k := range spec.required {
		if _, has := args[k]; !has {
			return nil, bad(name + " requires " + k)
		}
	}
	ref := "id"
	if name == "note.add" {
		ref = "card"
	}
	x, ok := str(args, ref)
	if !ok {
		return nil, bad(ref + " must be a card id")
	}
	id, e := s.resolveCardID(x)
	if e != nil {
		return nil, e
	}
	args[ref] = id
	switch name {
	case "card.move":
		if lane, ok := str(args, "lane"); !ok || !one(lane, "captured", "icebox", "clarified", "inflight", "review", "settling", "archived") {
			return nil, bad("invalid lane")
		}
	case "card.set":
		if _, has := args["title"]; has {
			if t, ok := str(args, "title"); !ok || !validText(t, 200, true) {
				return nil, bad("invalid title")
			}
		}
		if x, ok := optstr(args, "summary"); !ok || (x != nil && !validText(*x, 4096, false)) {
			return nil, bad("invalid summary")
		}
		if x, ok := optstr(args, "owner"); !ok || (x != nil && !one(*x, "kevin", "kes")) {
			return nil, bad("invalid owner")
		}
	case "card.done":
		if _, ok := boolean(args, "done", true); !ok {
			return nil, bad("invalid done")
		}
		// The store only lets jots be done; catch it here, not when Kev clicks.
		var project string
		_ = s.db.QueryRow("select project from cards where id=?", id).Scan(&project)
		if project != "_today" {
			return nil, bad("card.done applies only to jots; to close a card use card.move with lane archived")
		}
	case "card.block":
		if r, ok := str(args, "reason"); !ok || !validText(r, 300, true) {
			return nil, bad("invalid reason")
		}
	case "note.add":
		if t, ok := str(args, "text"); !ok || !validText(t, 300, true) {
			return nil, bad("invalid note text")
		}
		if x, ok := optstr(args, "detail"); !ok || (x != nil && !validText(*x, 4096, false)) {
			return nil, bad("invalid note detail")
		}
	}
	args["op"] = name
	return args, nil
}

func (s *Store) askAdd(a map[string]any) (any, error) {
	ref, ok := str(a, "card")
	if !ok {
		return nil, code("invalid_request", "card required")
	}
	question, ok := str(a, "question")
	if !ok || !validUnits(question, 300, true) {
		return nil, code("invalid_request", "question must be one line of at most 300 characters")
	}
	steps, ok := optstr(a, "steps")
	if !ok || (steps != nil && !validUnits(*steps, 4096, false)) {
		return nil, code("invalid_request", "steps must be prose of at most 4096 characters")
	}
	link, ok := optstr(a, "link")
	if !ok || (link != nil && !validLink(*link)) {
		return nil, code("invalid_request", "link must be an http(s) URL or an absolute path of at most 4096 characters")
	}
	label, ok := optstr(a, "link_label")
	if !ok || (label != nil && !validUnits(*label, 200, true)) {
		return nil, code("invalid_request", "link_label must be one line of at most 200 characters")
	}
	stage, ok := boolean(a, "stage", false)
	if !ok {
		return nil, code("invalid_request", "invalid stage")
	}
	raw, ok := a["actions"].([]any)
	if !ok || len(raw) < 1 || len(raw) > 4 {
		return nil, code("invalid_request", "actions must be an array of 1 to 4 actions")
	}
	actions := []askAction{}
	keys := map[string]bool{}
	primaries := 0
	for _, r := range raw {
		m, ok := r.(map[string]any)
		if !ok || !allowed(m, "key", "label", "done", "primary", "ops", "fork") {
			return nil, code("invalid_request", "an action is {key, label, done?, primary?, ops?, fork?}")
		}
		key, ok := str(m, "key")
		if !ok || !validKey(key) {
			return nil, code("invalid_request", "action key must match [a-z0-9-]{1,32}")
		}
		if one(key, "not-now", "talk") {
			return nil, code("invalid_request", "action key "+key+" is reserved")
		}
		if keys[key] {
			return nil, code("invalid_request", "duplicate action key "+key)
		}
		keys[key] = true
		lbl, ok := str(m, "label")
		if !ok || !validUnits(lbl, 120, true) {
			return nil, code("invalid_request", "action label must be one line of at most 120 characters")
		}
		primary, ok := boolean(m, "primary", false)
		if !ok {
			return nil, code("invalid_request", "invalid primary")
		}
		if primary {
			primaries++
		}
		done, ok := optstr(m, "done")
		if !ok || (done != nil && !validUnits(*done, 60, true)) {
			return nil, code("invalid_request", "action done must be one line of at most 60 characters")
		}
		act := askAction{Key: key, Label: lbl, Primary: primary}
		if done != nil {
			act.Done = *done
		}
		if x, has := m["ops"]; has {
			ops, ok := x.([]any)
			if !ok || len(ops) > 8 {
				return nil, code("invalid_request", "action ops must be an array of at most 8 ops")
			}
			for _, o := range ops {
				op, e := s.validAskOp(o)
				if e != nil {
					return nil, e
				}
				act.Ops = append(act.Ops, op)
			}
		}
		if x, has := m["fork"]; has {
			f, e := validAskFork(x)
			if e != nil {
				return nil, e
			}
			act.Fork = f
		}
		actions = append(actions, act)
	}
	if primaries > 1 {
		return nil, code("invalid_request", "at most one action may be primary")
	}
	card, e := s.resolveCardID(ref)
	if e != nil {
		return nil, e
	}
	body, e := json.Marshal(actions)
	if e != nil {
		return nil, e
	}
	v, e := s.write(func(tx *sql.Tx, at string) (any, error) {
		var surfaced, position int64
		if e := tx.QueryRow("select count(*) filter(where state='surfaced'),coalesce(max(position),0)+1 from asks").Scan(&surfaced, &position); e != nil {
			return nil, e
		}
		state, surfacedAt := "staged", any(nil)
		if !stage && surfaced < maxSurfaced {
			state, surfacedAt = "surfaced", at
		}
		r, e := tx.Exec("insert into asks(card_id,question,steps,link,link_label,actions,state,position,created_at,surfaced_at) values(?,?,?,?,?,?,?,?,?,?)", card, question, steps, link, label, string(body), state, position, at, surfacedAt)
		if e != nil {
			return nil, e
		}
		id, e := r.LastInsertId()
		if e == nil {
			e = addNote(tx, card, "kes", at, "Asked Kev: ", question, "")
		}
		return id, e
	})
	if e != nil {
		return nil, e
	}
	return s.ask(v.(int64))
}

// addNote writes a generated card note (prefix + question + suffix), with
// its note event, by `by`. A note that would exceed the 300-unit note bound
// elides the question in the text and keeps it whole in the detail.
func addNote(tx *sql.Tx, card, by, at, prefix, question, suffix string) error {
	text, detail := prefix+question+suffix, any(nil)
	if units(text) > 300 {
		budget := 300 - units(prefix) - units(suffix) - 1
		q := []rune(question)
		for units(string(q)) > budget {
			q = q[:len(q)-1]
		}
		text, detail = prefix+strings.TrimRight(string(q), " ")+"…"+suffix, question
	}
	_, e := tx.Exec("insert into notes(card_id,at,by,text,detail) values(?,?,?,?,?)", card, at, by, text, detail)
	if e == nil {
		e = event(tx, card, by, "note", at, map[string]any{"text": text})
	}
	if e == nil {
		_, e = tx.Exec("update cards set updated_at=? where id=?", at, card)
	}
	return e
}

const askCols = "a.id,a.card_id,c.project,c.title,a.question,a.steps,a.link,a.link_label,a.actions,a.state,a.created_at,a.surfaced_at,a.resolved_at,a.resolution"

type askRow struct {
	ID                             int64
	Card, Project, Title, Question string
	Steps, Link, Label             sql.NullString
	Actions                        []askAction
	State, Created                 string
	Surfaced, Resolved, Resolution sql.NullString
}

func scanAsk(sc interface{ Scan(...any) error }) (askRow, error) {
	var r askRow
	var actions string
	e := sc.Scan(&r.ID, &r.Card, &r.Project, &r.Title, &r.Question, &r.Steps, &r.Link, &r.Label, &actions, &r.State, &r.Created, &r.Surfaced, &r.Resolved, &r.Resolution)
	if e == nil {
		e = json.Unmarshal([]byte(actions), &r.Actions)
	}
	return r, e
}

// wire is the client shape of an ask: actions without their ops.
func (r askRow) wire() map[string]any {
	actions := []any{}
	for _, x := range r.Actions {
		act := map[string]any{"key": x.Key, "label": x.Label}
		if x.Done != "" {
			act["done"] = x.Done
		}
		if x.Primary {
			act["primary"] = true
		}
		if x.Fork != nil {
			act["fork"] = true
		}
		actions = append(actions, act)
	}
	return map[string]any{"id": r.ID, "card": map[string]any{"id": r.Card, "project": r.Project, "title": r.Title}, "question": r.Question, "steps": n(r.Steps), "link": n(r.Link), "link_label": n(r.Label), "actions": actions, "state": r.State, "created_at": r.Created, "surfaced_at": n(r.Surfaced), "resolved_at": n(r.Resolved), "resolution": n(r.Resolution)}
}

type querier interface {
	QueryRow(string, ...any) *sql.Row
}

func loadAsk(q querier, id int64) (askRow, error) {
	r, e := scanAsk(q.QueryRow("select "+askCols+" from asks a join cards c on c.id=a.card_id where a.id=?", id))
	if errors.Is(e, sql.ErrNoRows) {
		return r, code("not_found", "ask not found")
	}
	return r, e
}

func (s *Store) ask(id int64) (any, error) {
	r, e := loadAsk(s.db, id)
	if e != nil {
		return nil, e
	}
	return r.wire(), nil
}

func (s *Store) asks(where string, args ...any) ([]any, error) {
	rs, e := s.db.Query("select "+askCols+" from asks a join cards c on c.id=a.card_id where "+where, args...)
	if e != nil {
		return nil, e
	}
	defer rs.Close()
	out := []any{}
	for rs.Next() {
		r, e := scanAsk(rs)
		if e != nil {
			return nil, e
		}
		out = append(out, r.wire())
	}
	return out, rs.Err()
}

func (s *Store) askList(a map[string]any) (any, error) {
	all, ok := boolean(a, "all", false)
	if !ok {
		return nil, code("invalid_request", "invalid all")
	}
	surfaced, e := s.asks("a.state='surfaced' order by a.position,a.id")
	if e != nil {
		return nil, e
	}
	staged, e := s.asks("a.state='staged' order by a.position,a.id")
	if e != nil {
		return nil, e
	}
	out := map[string]any{"surfaced": clip(surfaced), "staged": clip(staged)}
	if all {
		resolved, e := s.asks("a.state in ('resolved','dropped') order by a.resolved_at desc,a.id desc limit 50")
		if e != nil {
			return nil, e
		}
		out["resolved"] = resolved
	}
	return out, nil
}

// openAsk loads an ask that is still staged or surfaced, inside tx.
func openAsk(tx *sql.Tx, a map[string]any) (askRow, error) {
	id, ok := intArg(a, "id")
	if !ok {
		return askRow{}, code("invalid_request", "id must be a positive integer")
	}
	r, e := loadAsk(tx, id)
	if e != nil {
		return r, e
	}
	if !one(r.State, "staged", "surfaced") {
		return r, code("conflict", "ask is already "+r.State)
	}
	return r, nil
}

func (s *Store) askResolve(actor string, a map[string]any) (any, error) {
	choice, ok := str(a, "choice")
	if !ok || choice == "" {
		return nil, code("invalid_request", "choice required")
	}
	v, e := s.write(func(tx *sql.Tx, at string) (any, error) {
		r, e := openAsk(tx, a)
		if e != nil {
			return nil, e
		}
		var snaps []cardSnap
		forked := false
		resolve := func() error {
			undo, e := json.Marshal(askUndoRec{Prev: r.State, Cards: snaps, Fork: forked})
			if e != nil {
				return e
			}
			_, e = tx.Exec("update asks set state='resolved',resolved_at=?,resolution=?,undo=? where id=?", at, choice, string(undo), r.ID)
			return e
		}
		if choice == "not-now" {
			if e = resolve(); e == nil {
				e = addNote(tx, r.Card, actor, at, "Kev: not now on “", r.Question, "” (wrong moment — not a judgment on the ask)")
			}
			return r, e
		}
		var act *askAction
		for i := range r.Actions {
			if r.Actions[i].Key == choice {
				act = &r.Actions[i]
			}
		}
		if act == nil {
			return nil, code("invalid_request", "choice must be one of the ask's action keys or not-now")
		}
		if snaps, e = snapCards(tx, act.Ops); e != nil {
			return nil, e
		}
		for _, op := range act.Ops {
			name, _ := str(op, "op")
			args := map[string]any{}
			for k, v := range op {
				if k != "op" {
					args[k] = v
				}
			}
			id, _ := str(args, "id")
			if name == "note.add" {
				id, _ = str(args, "card")
			}
			if _, e = s.apply(tx, at, actor, name, id, args); e != nil {
				return nil, e
			}
		}
		if f := act.Fork; f != nil {
			// Last, inside the transaction: if the scheduler refuses, nothing
			// above commits and the ask stays up for another try.
			if s.SpawnFork == nil {
				return nil, code("unavailable", "starting a fork is unavailable here")
			}
			label := f.Label
			if label == "" {
				label = act.Label
			}
			if e = s.SpawnFork(ForkSpawn{ID: askForkID(r), Target: f.Target, Task: f.Task, Label: label, Fresh: f.Fresh, Ask: r.ID, Choice: act.Key}); e != nil {
				return nil, e
			}
			forked = true
			if e = addNote(tx, r.Card, "kes", at, "Started a fork ("+label+") for: ", r.Question, ""); e != nil {
				return nil, e
			}
		}
		if e = resolve(); e == nil {
			e = addNote(tx, r.Card, actor, at, "Kev chose “"+act.Label+"” on: ", r.Question, "")
		}
		return r, e
	})
	if e != nil {
		return nil, e
	}
	r := v.(askRow)
	resolved, e := s.ask(r.ID)
	if e != nil {
		return nil, e
	}
	card, e := s.full(r.Card)
	if e != nil {
		return nil, e
	}
	return map[string]any{"resolved": resolved, "card": card}, nil
}

func (s *Store) askTalk(actor string, a map[string]any) (any, error) {
	v, e := s.write(func(tx *sql.Tx, at string) (any, error) {
		r, e := openAsk(tx, a)
		if e == nil {
			e = addNote(tx, r.Card, actor, at, "Kev opened a conversation about: ", r.Question, "")
		}
		return r.ID, e
	})
	if e != nil {
		return nil, e
	}
	return s.ask(v.(int64))
}

func (s *Store) askDrop(actor string, a map[string]any) (any, error) {
	v, e := s.write(func(tx *sql.Tx, at string) (any, error) {
		r, e := openAsk(tx, a)
		if e != nil {
			return nil, e
		}
		if _, e = tx.Exec("update asks set state='dropped',resolved_at=?,resolution='dropped' where id=?", at, r.ID); e == nil {
			e = addNote(tx, r.Card, actor, at, "Withdrew ask: ", r.Question, "")
		}
		return r.ID, e
	})
	if e != nil {
		return nil, e
	}
	return s.ask(v.(int64))
}

func (s *Store) askSurface(a map[string]any) (any, error) {
	v, e := s.write(func(tx *sql.Tx, at string) (any, error) {
		r, e := openAsk(tx, a)
		if e != nil || r.State == "surfaced" {
			return r.ID, e
		}
		var surfaced int
		if e = tx.QueryRow("select count(*) from asks where state='surfaced'").Scan(&surfaced); e != nil {
			return nil, e
		}
		if surfaced >= maxSurfaced {
			return nil, code("conflict", "3 asks are already surfaced")
		}
		_, e = tx.Exec("update asks set state='surfaced',surfaced_at=? where id=?", at, r.ID)
		return r.ID, e
	})
	if e != nil {
		return nil, e
	}
	return s.ask(v.(int64))
}

// askMore surfaces staged asks in order until maxSurfaced are surfaced.
func (s *Store) askMore() (any, error) {
	return s.write(func(tx *sql.Tx, at string) (any, error) {
		var surfaced int
		if e := tx.QueryRow("select count(*) from asks where state='surfaced'").Scan(&surfaced); e != nil {
			return nil, e
		}
		added := int64(0)
		if room := maxSurfaced - surfaced; room > 0 {
			r, e := tx.Exec("update asks set state='surfaced',surfaced_at=? where id in (select id from asks where state='staged' order by position,id limit ?)", at, room)
			if e != nil {
				return nil, e
			}
			if added, e = r.RowsAffected(); e != nil {
				return nil, e
			}
		}
		var left int
		if e := tx.QueryRow("select count(*) from asks where state='staged'").Scan(&left); e != nil {
			return nil, e
		}
		return map[string]any{"surfaced": added, "staged_left": left}, nil
	})
}

func (s *Store) askCounts() (surfaced, staged int, e error) {
	e = s.db.QueryRow("select count(*) filter(where state='surfaced'),count(*) filter(where state='staged') from asks").Scan(&surfaced, &staged)
	return
}

// cardSnap is a card's mutable columns before a resolve's ops ran. Undo
// restores them exactly; notes the ops added stay, and undo appends its own.
type cardSnap struct {
	ID                                              string
	Lane, Title                                     string
	Summary, Owner, Policy, Blocked, Hold, Due, May sql.NullString
	Done                                            int
	Moved                                           string
}

type askUndoRec struct {
	Prev  string     `json:"prev"` // state before resolve: surfaced or staged
	Cards []cardSnap `json:"cards,omitempty"`
	Fork  bool       `json:"fork,omitempty"` // a fork started: not undoable
}

const snapCols = "id,lane,title,summary,owner,policy,blocked,hold_until,due,maybe_on,done,moved_at"

func snapCards(tx *sql.Tx, ops []map[string]any) ([]cardSnap, error) {
	seen := map[string]bool{}
	out := []cardSnap{}
	for _, op := range ops {
		name, _ := str(op, "op")
		if name == "note.add" {
			continue
		}
		id, _ := str(op, "id")
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		var c cardSnap
		if e := tx.QueryRow("select "+snapCols+" from cards where id=?", id).Scan(&c.ID, &c.Lane, &c.Title, &c.Summary, &c.Owner, &c.Policy, &c.Blocked, &c.Hold, &c.Due, &c.May, &c.Done, &c.Moved); e != nil {
			if errors.Is(e, sql.ErrNoRows) {
				return nil, code("not_found", "card not found")
			}
			return nil, e
		}
		out = append(out, c)
	}
	return out, nil
}

// askUndo reverses a resolve within undoWindow: card columns go back to their
// snapshot, a note records the undo, and the ask returns to where it was
// (surfaced if there is room, else staged).
func (s *Store) askUndo(actor string, a map[string]any) (any, error) {
	id, ok := intArg(a, "id")
	if !ok {
		return nil, code("invalid_request", "id must be a positive integer")
	}
	v, e := s.write(func(tx *sql.Tx, at string) (any, error) {
		r, e := loadAsk(tx, id)
		if e != nil {
			return nil, e
		}
		var raw sql.NullString
		if e = tx.QueryRow("select undo from asks where id=?", id).Scan(&raw); e != nil {
			return nil, e
		}
		if r.State != "resolved" || !raw.Valid {
			return nil, code("conflict", "ask is not undoable")
		}
		when, e := time.Parse(time.RFC3339Nano, r.Resolved.String)
		if e != nil || s.now().Sub(when) > undoWindow {
			return nil, code("conflict", "undo window has passed")
		}
		var rec askUndoRec
		if e = json.Unmarshal([]byte(raw.String), &rec); e != nil {
			return nil, e
		}
		if rec.Fork {
			return nil, code("conflict", "that started a fork, which can't be undone")
		}
		for _, c := range rec.Cards {
			if _, e = tx.Exec("update cards set lane=?,title=?,summary=?,owner=?,policy=?,blocked=?,hold_until=?,due=?,maybe_on=?,done=?,moved_at=?,updated_at=? where id=?",
				c.Lane, c.Title, c.Summary, c.Owner, c.Policy, c.Blocked, c.Hold, c.Due, c.May, c.Done, c.Moved, at, c.ID); e != nil {
				return nil, e
			}
			if e = event(tx, c.ID, actor, "undo", at, map[string]any{"ask": r.ID, "lane": c.Lane}); e != nil {
				return nil, e
			}
		}
		state := "staged"
		if rec.Prev != "staged" {
			var surfaced int
			if e = tx.QueryRow("select count(*) from asks where state='surfaced'").Scan(&surfaced); e != nil {
				return nil, e
			}
			if surfaced < maxSurfaced {
				state = "surfaced"
			}
		}
		if _, e = tx.Exec("update asks set state=?,resolved_at=null,resolution=null,undo=null where id=?", state, r.ID); e != nil {
			return nil, e
		}
		label := r.Resolution.String
		for _, x := range r.Actions {
			if x.Key == label {
				label = x.Label
			}
		}
		if label == "not-now" {
			label = "Not now"
		}
		return r.ID, addNote(tx, r.Card, actor, at, "Kev undid “"+label+"” on: ", r.Question, "")
	})
	if e != nil {
		return nil, e
	}
	return s.ask(v.(int64))
}

// askForkID is stable for one ask (a retried resolve finds the same event)
// and unique across asks, even if ids were ever reused: it carries the ask's
// creation time.
func askForkID(r askRow) string {
	stamp := strings.Map(func(c rune) rune {
		if c >= '0' && c <= '9' {
			return c
		}
		return -1
	}, r.Created)
	return fmt.Sprintf("ask-%d-%s", r.ID, stamp)
}
