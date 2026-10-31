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
	// Body is the full merge body, only on claim.
	Body string `json:"body,omitempty"`
}

func (s *Store) initInbox() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS inbox (
		id TEXT PRIMARY KEY, target TEXT NOT NULL, fork TEXT NOT NULL, label TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL, headline TEXT NOT NULL, body TEXT NOT NULL,
		created_at INTEGER NOT NULL, claimed_at INTEGER NOT NULL DEFAULT 0)`)
	return err
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

// Headline is the fork's own line, or the first line of its return cut short.
func headlineOf(m mergeFacts) string {
	h := strings.TrimSpace(m.Headline)
	if h == "" {
		h = strings.TrimSpace(strings.SplitN(strings.TrimSpace(m.Summary), "\n", 2)[0])
	}
	if r := []rune(h); len(r) > 140 {
		h = string(r[:139]) + "…"
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
	var m mergeFacts
	if json.Unmarshal([]byte(e.Body), &m) != nil || m.ForkSessionID == "" || m.Ambient || m.Stub || m.Claimed {
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

// InboxList returns unclaimed returns for target ("*" for all), oldest first.
func (s *Store) InboxList(target string) ([]InboxItem, error) {
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
func (s *Store) inboxFind(q *sql.Tx, ref string) (InboxItem, error) {
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

// InboxClaim marks a return received and hands back its full body. Claiming
// twice returns the body again (a pull can be re-read), flagged by ClaimedAt.
func (s *Store) InboxClaim(ref string) (InboxItem, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return InboxItem{}, err
	}
	defer tx.Rollback()
	x, err := s.inboxFind(tx, ref)
	if err != nil {
		return InboxItem{}, err
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

// InboxDeliver claims a return and sends it in full to its target as a wake:
// Kev tapped it in Open. While that delivery is in flight the merge shows as
// undelivered (settled), so the row stays until Kes has it.
func (s *Store) InboxDeliver(ref string) (Event, error) {
	x, err := s.InboxClaim(ref)
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
	e, _, err := s.Enqueue(Enqueue{ID: x.ID + "-pulled", Target: x.Target, Origin: x.Fork, Source: "inbox.deliver", Type: "merge", Summary: summary, Body: string(b)})
	return e, err
}
