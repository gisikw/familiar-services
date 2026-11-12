package scheduler

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// Soft Disturb (DND mode "soft"): a tasked fork's return is delivered as a
// stub (who, status, headline) and the full return waits here, unclaimed,
// until the target pulls it (`imp pull`) or Kev taps it in Open. The stub
// keeps the fork's own wake choice: a loud return's stub still starts a turn,
// a quiet one rides the next. Ambient forks (morning watch, free time, ask
// restocking: set at dispatch) are never stubbed and never wait here.
//
// Kev, Oct 31: six forks returning at once became a bank run. The returns
// need to survive a /clear and be taken at our discretion, not dumped.

const DNDSoft = "soft"

// ReturnStatuses are what a fork says about its own return.
var ReturnStatuses = []string{"done", "failed", "partial", "blocked"}

func validStatus(s string) bool {
	for _, x := range ReturnStatuses {
		if s == x {
			return true
		}
	}
	return false
}

type InboxItem struct {
	ID        string `json:"id"`
	Target    string `json:"target"`
	Fork      string `json:"fork"`
	Label     string `json:"label,omitempty"`
	Status    string `json:"status"`
	Headline  string `json:"headline"`
	CreatedAt int64  `json:"createdAt"`
	ClaimedAt int64  `json:"claimedAt,omitempty"`
	// Held: a stored return not yet in the inbox (pull files it).
	Held bool `json:"held,omitempty"`
	// Body is the full merge body, only on claim.
	Body string `json:"body,omitempty"`
}

func (s *Store) initInbox() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS inbox (
		id TEXT PRIMARY KEY, target TEXT NOT NULL, fork TEXT NOT NULL, label TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL, headline TEXT NOT NULL, body TEXT NOT NULL,
		created_at INTEGER NOT NULL, claimed_at INTEGER NOT NULL DEFAULT 0)`)
	if err != nil {
		return err
	}
	return s.reheadInbox()
}

// reheadInbox re-derives clipped headlines on unclaimed rows from their
// stored merge body. Headlines used to be cut at 140 runes even though imp
// merge allows 160, so a fork's own line could arrive with a stray "…".
func (s *Store) reheadInbox() error {
	rows, err := s.db.Query(`SELECT id, headline, body FROM inbox WHERE claimed_at=0 AND headline LIKE '%…'`)
	if err != nil {
		return err
	}
	type fix struct{ id, h string }
	var fixes []fix
	for rows.Next() {
		var id, h, body string
		if err := rows.Scan(&id, &h, &body); err != nil {
			rows.Close()
			return err
		}
		var m mergeFacts
		if json.Unmarshal([]byte(body), &m) != nil {
			continue
		}
		if nh := headlineOf(m); nh != h {
			fixes = append(fixes, fix{id, nh})
		}
	}
	rows.Close()
	for _, f := range fixes {
		if _, err := s.db.Exec(`UPDATE inbox SET headline=? WHERE id=?`, f.h, f.id); err != nil {
			return err
		}
	}
	return nil
}

type mergeFacts struct {
	Summary       string `json:"summary"`
	ForkSessionID string `json:"forkSessionId"`
	Label         string `json:"label"`
	Status        string `json:"status"`
	Headline      string `json:"headline"`
	Ambient       bool   `json:"ambient"`
	Stub          bool   `json:"stub"`
	Claimed       bool   `json:"claimed"`
}

// Headline is the fork's own line, kept whole (imp merge already caps it at
// 160 runes; 400 is only a backstop), or, for older forks with no headline,
// the first line of the return cut short at 140.
func headlineOf(m mergeFacts) string {
	h, limit := strings.TrimSpace(m.Headline), 400
	if h == "" {
		h, limit = strings.TrimSpace(strings.SplitN(strings.TrimSpace(m.Summary), "\n", 2)[0]), 140
	}
	if r := []rune(h); len(r) > limit {
		h = string(r[:limit-1]) + "…"
	}
	return h
}

func statusOf(m mergeFacts) string {
	if validStatus(m.Status) {
		return m.Status
	}
	return "done"
}

// stubMerge files a tasked merge in the inbox and rewrites the delivered
// event to a stub. Returns false (no change) for ambient, already-stubbed or
// already-claimed merges and for bodies it cannot read.
func stubMerge(tx *sql.Tx, e *Event, now int64) (bool, error) {
	return stubMergeOpt(tx, e, now, false)
}

// stubMergeOpt is stubMerge; allowAmbient files ambient merges too (an
// explicit pull asked for this one by name).
func stubMergeOpt(tx *sql.Tx, e *Event, now int64, allowAmbient bool) (bool, error) {
	var m mergeFacts
	if json.Unmarshal([]byte(e.Body), &m) != nil || m.ForkSessionID == "" || (m.Ambient && !allowAmbient) || m.Stub || m.Claimed {
		return false, nil
	}
	item := InboxItem{ID: e.ID, Target: e.Target, Fork: m.ForkSessionID, Label: m.Label, Status: statusOf(m), Headline: headlineOf(m), CreatedAt: now}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO inbox(id,target,fork,label,status,headline,body,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		item.ID, item.Target, item.Fork, item.Label, item.Status, item.Headline, e.Body, item.CreatedAt); err != nil {
		return false, err
	}
	// The stub keeps every merge field the Pi relies on (ids, file, entries)
	// except the return itself.
	var raw map[string]any
	if err := json.Unmarshal([]byte(e.Body), &raw); err != nil {
		return false, nil
	}
	raw["summary"] = item.Headline
	raw["status"] = item.Status
	raw["headline"] = item.Headline
	raw["stub"] = true
	raw["inboxId"] = item.ID
	b, _ := json.Marshal(raw)
	e.Body = string(b)
	e.Summary = item.Status + ": " + item.Headline
	_, err := tx.Exec(`UPDATE events SET body=?, summary=? WHERE id=?`, e.Body, e.Summary, e.ID)
	return err == nil, err
}

// heldMerges are stored returns that never reached the inbox and aren't in
// Kes's turn yet: merges still pending (quiet DND holds wakes) or a quiet
// merge delivered soft that the Pi holds for the next turn. They are as real as an inbox row, so pull sees them
// (Nov 12: a wake return held by quiet DND said not_found to both imp pull
// and Kev's "Bring return to Kes" while its full text sat in events).
// ref "" lists every held tasked return for target; otherwise ref matches
// an event id, a fork id, or a unique fork-id prefix (8+ chars), ambient
// returns included.
func heldMerges(q interface {
	Query(string, ...any) (*sql.Rows, error)
}, target, ref string) ([]Event, error) {
	query := `SELECT ` + eventColumns + ` FROM events WHERE type='merge' AND (state='pending' OR (state='delivered' AND urgency='soft'))
		AND id NOT IN (SELECT id FROM inbox) AND id NOT LIKE '%-pulled'`
	args := []any{}
	if target != "" && target != "*" {
		query += ` AND target=?`
		args = append(args, NormalizeTarget(target))
	}
	if ref != "" {
		query += ` AND (id=? OR origin=? OR (length(?)>=8 AND origin LIKE ?||'%'))`
		args = append(args, ref, ref, ref, ref)
	}
	rows, err := q.Query(query+` ORDER BY created_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		var m mergeFacts
		if json.Unmarshal([]byte(e.Body), &m) != nil || m.ForkSessionID == "" || m.Stub || m.Claimed || (m.Ambient && ref == "") {
			continue
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// fileHeld moves one held return into the inbox so it can be claimed: the
// full body goes to the inbox and the event becomes a soft stub requeued
// under the same id (a Pi holding the full copy swaps it for the stub), so
// the return reaches Kes once, by the pull, and the stub only notes it.
func (s *Store) fileHeld(tx *sql.Tx, ref string) (bool, error) {
	held, err := heldMerges(tx, "", ref)
	if err != nil || len(held) == 0 {
		return false, err
	}
	forks := map[string]bool{}
	for _, e := range held {
		forks[e.Origin] = true
	}
	if len(forks) > 1 {
		return false, errors.New("ambiguous: " + ref + " matches more than one fork")
	}
	for _, e := range held {
		ok, err := stubMergeOpt(tx, &e, s.now().UnixMilli(), true)
		if err != nil {
			return false, err
		}
		if ok {
			if _, err := tx.Exec(`UPDATE events SET state='pending', urgency='soft' WHERE id=?`, e.ID); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}

// InboxList returns unclaimed returns for target ("*" for all), oldest first:
// the inbox, then stored returns still held outside it (Held).
func (s *Store) InboxList(target string) ([]InboxItem, error) {
	items, err := s.inboxRows(target)
	if err != nil {
		return nil, err
	}
	held, err := heldMerges(s.db, target, "")
	if err != nil {
		return nil, err
	}
	for _, e := range held {
		var m mergeFacts
		_ = json.Unmarshal([]byte(e.Body), &m)
		items = append(items, InboxItem{ID: e.ID, Target: e.Target, Fork: m.ForkSessionID, Label: m.Label, Status: statusOf(m), Headline: headlineOf(m), CreatedAt: e.CreatedAt, Held: true})
	}
	return items, nil
}

func (s *Store) inboxRows(target string) ([]InboxItem, error) {
	q := `SELECT id,target,fork,label,status,headline,created_at FROM inbox WHERE claimed_at=0`
	args := []any{}
	if target != "" && target != "*" {
		q += ` AND target=?`
		args = append(args, NormalizeTarget(target))
	}
	rows, err := s.db.Query(q+` ORDER BY created_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []InboxItem{}
	for rows.Next() {
		var x InboxItem
		if err := rows.Scan(&x.ID, &x.Target, &x.Fork, &x.Label, &x.Status, &x.Headline, &x.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// find matches an inbox id, a fork id, or a unique fork-id prefix (8+ chars).
// A stored return held outside the inbox is filed first, so it is found too.
func (s *Store) inboxFind(q *sql.Tx, ref string) (InboxItem, error) {
	return s.inboxFindOnce(q, ref, false)
}

func (s *Store) inboxFindOnce(q *sql.Tx, ref string, refiled bool) (InboxItem, error) {
	ref = strings.TrimPrefix(strings.TrimSpace(ref), "instance:")
	if ref == "" {
		return InboxItem{}, errors.New("which return? give a fork id or inbox id")
	}
	rows, err := q.Query(`SELECT id,target,fork,label,status,headline,body,created_at,claimed_at FROM inbox
		WHERE id=? OR fork=? OR (length(?)>=8 AND fork LIKE ?||'%') ORDER BY claimed_at=0 DESC, created_at DESC`, ref, ref, ref, ref)
	if err != nil {
		return InboxItem{}, err
	}
	defer rows.Close()
	var found []InboxItem
	for rows.Next() {
		var x InboxItem
		if err := rows.Scan(&x.ID, &x.Target, &x.Fork, &x.Label, &x.Status, &x.Headline, &x.Body, &x.CreatedAt, &x.ClaimedAt); err != nil {
			return InboxItem{}, err
		}
		found = append(found, x)
	}
	if len(found) == 0 && !refiled {
		rows.Close()
		if ok, err := s.fileHeld(q, ref); err != nil {
			return InboxItem{}, err
		} else if ok {
			return s.inboxFindOnce(q, ref, true)
		}
	}
	if len(found) == 0 {
		return InboxItem{}, &notFound{"no return for " + ref}
	}
	forks := map[string]bool{}
	for _, f := range found {
		forks[f.Fork] = true
	}
	if len(forks) > 1 {
		return InboxItem{}, errors.New("ambiguous: " + ref + " matches more than one fork")
	}
	return found[0], rows.Err()
}

type notFound struct{ msg string }

func (n *notFound) Error() string { return n.msg }
func (n *notFound) Is(target error) bool {
	return target == errNotFound
}

var errNotFound = errors.New("not found")

// IsNotFound reports an unknown inbox reference.
func IsNotFound(err error) bool { return errors.Is(err, errNotFound) }

// Forbidden: a caller asked to consume a return addressed to someone else.
type Forbidden struct{ msg string }

func (f *Forbidden) Error() string { return f.msg }

// IsForbidden reports a pull by an instance the return isn't addressed to.
func IsForbidden(err error) bool { var f *Forbidden; return errors.As(err, &f) }

// InboxClaim marks a return received and hands back its full body. Claiming
// twice returns the body again (a pull can be re-read), flagged by ClaimedAt.
func (s *Store) InboxClaim(ref string) (InboxItem, error) { return s.InboxClaimAs(ref, "") }

// InboxClaimAs is InboxClaim for a known caller: only the instance a return
// is addressed to may consume it (Nov 11: a fork pulled a sibling's return
// and it vanished from Kev's Open unread). Forks read the record instead.
// caller "" (Kev's own Open, a service) is not checked.
func (s *Store) InboxClaimAs(ref, caller string) (InboxItem, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return InboxItem{}, err
	}
	defer tx.Rollback()
	x, err := s.inboxFind(tx, ref)
	if err != nil {
		return InboxItem{}, err
	}
	if caller = strings.TrimSpace(caller); caller != "" && NormalizeTarget(caller) != x.Target {
		return InboxItem{}, &Forbidden{"that return is addressed to " + x.Target + "; only it may pull it. Read the fork's record instead: state/forks/" + x.Fork + "/"}
	}
	wasClaimed := x.ClaimedAt
	if x.ClaimedAt == 0 {
		x.ClaimedAt = s.now().UnixMilli()
		if _, err = tx.Exec(`UPDATE inbox SET claimed_at=? WHERE id=?`, x.ClaimedAt, x.ID); err != nil {
			return InboxItem{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return InboxItem{}, err
	}
	if wasClaimed != 0 {
		x.ClaimedAt = wasClaimed
	}
	return x, nil
}

// sourceInboxDeliver marks a return Kev tapped in Open. ClaimNext puts it
// first, past wake spacing and DND, and always delivers it as a wake.
const sourceInboxDeliver = "inbox.deliver"

// InboxDeliver claims a return and sends it in full to its target as a wake:
// Kev tapped it in Open. While that delivery is in flight the merge shows as
// undelivered (settled), so the row stays until Kes has it.
func (s *Store) InboxDeliver(ref string) (Event, error) { return s.InboxDeliverAs(ref, "") }

// InboxDeliverAs is InboxDeliver for a known caller (see InboxClaimAs).
func (s *Store) InboxDeliverAs(ref, caller string) (Event, error) {
	x, err := s.InboxClaimAs(ref, caller)
	if err != nil {
		return Event{}, err
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(x.Body), &raw); err != nil {
		return Event{}, err
	}
	raw["claimed"] = true
	raw["pulledBy"] = "kevin"
	b, _ := json.Marshal(raw)
	summary, _ := raw["summary"].(string)
	if strings.TrimSpace(summary) == "" {
		summary = x.Headline
	}
	e, _, err := s.Enqueue(Enqueue{ID: x.ID + "-pulled", Target: x.Target, Origin: x.Fork, Source: sourceInboxDeliver, Type: "merge", Urgency: "wake", Summary: summary, Body: string(b)})
	return e, err
}

// WakeHeldMerges turns quiet fork returns that are waiting for the target's
// next turn into wakes and queues them again, so they start a turn now.
// A quiet merge is delivered soft: the Pi holds it until Kev next speaks, and
// it stays unacked ("merged · waiting to reach Kes" in Open) until then.
// Requeued under the same id, the Pi swaps its held copy for the wake.
//
// fork "" takes every held merge for target (Do Disturb switched on: Nov 4,
// two returns queued before the switch sat until Kev typed). A fork id takes
// that fork's (Kev tapped it in Open); byKev marks it his, so it goes first
// and DND doesn't hold it, like a tapped Soft Disturb return. Stubs are left
// alone: their full return is in the inbox, and InboxDeliver brings that.
func (s *Store) WakeHeldMerges(target, fork string, byKev bool) (int, error) {
	q := `SELECT ` + eventColumns + ` FROM events WHERE type='merge' AND urgency='soft' AND state IN ('pending','delivered')`
	args := []any{}
	if target != "" {
		q += ` AND target=?`
		args = append(args, NormalizeTarget(target))
	}
	if fork != "" {
		q += ` AND origin=?`
		args = append(args, strings.TrimPrefix(strings.TrimSpace(fork), "instance:"))
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(q, args...)
	if err != nil {
		return 0, err
	}
	var held []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			rows.Close()
			return 0, err
		}
		held = append(held, e)
	}
	rows.Close()
	n := 0
	for _, e := range held {
		var raw map[string]any
		if json.Unmarshal([]byte(e.Body), &raw) == nil {
			if stub, _ := raw["stub"].(bool); stub {
				continue
			}
		}
		source := e.Source
		if byKev {
			source = sourceInboxDeliver
			if raw != nil {
				raw["pulledBy"] = "kevin"
				if b, err := json.Marshal(raw); err == nil {
					e.Body = string(b)
				}
			}
		}
		if _, err := tx.Exec(`UPDATE events SET state='pending', urgency='wake', source=?, body=? WHERE id=?`, source, e.Body, e.ID); err != nil {
			return 0, err
		}
		n++
	}
	return n, tx.Commit()
}
