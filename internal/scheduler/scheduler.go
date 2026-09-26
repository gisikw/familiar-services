// Package scheduler owns durable events, delivery state, and per-instance DND.
package scheduler

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

var idRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,159}$`)

type Event struct {
	ID        string `json:"id"`
	DueAt     int64  `json:"due_at"`
	Target    string `json:"target"`
	Origin    string `json:"origin"`
	Source    string `json:"source"`
	Priority  int    `json:"priority"`
	Type      string `json:"type"`
	Summary   string `json:"summary"`
	Body      string `json:"body"`
	Urgency   string `json:"urgency"`
	State     string `json:"state"`
	CreatedAt int64  `json:"created_at"`
	Rule      string `json:"rule,omitempty"`
	Series    string `json:"series,omitempty"`
}
type Enqueue struct {
	ID       string `json:"id"`
	DueAt    int64  `json:"due_at"`
	Target   string `json:"target"`
	Origin   string `json:"origin"`
	Source   string `json:"source"`
	Priority *int   `json:"priority"`
	Type     string `json:"type"`
	Summary  string `json:"summary"`
	Body     string `json:"body"`
	Urgency  string `json:"urgency"`
	// Rule makes the event recurring (see ParseRule). Each delivery enqueues
	// the next occurrence in the same transaction; the series id is the first
	// occurrence's id, and cancelling any occurrence cancels the series.
	Rule string `json:"rule"`
}
type DND struct {
	Enabled   bool   `json:"enabled"`
	SetBy     string `json:"setBy"`
	SetAt     int64  `json:"setAt"`
	ExpiresAt int64  `json:"expiresAt"`
}
type Store struct {
	db  *sql.DB
	now func() time.Time
}

func Open(stateDir string) (*Store, error) {
	if stateDir == "" {
		return nil, errors.New("state directory is required")
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", filepath.Join(stateDir, "scheduler.sqlite")+"?_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		`PRAGMA journal_mode=WAL`,
		`CREATE TABLE IF NOT EXISTS events (
			id TEXT PRIMARY KEY, due_at INTEGER NOT NULL, target TEXT NOT NULL, origin TEXT NOT NULL,
			source TEXT NOT NULL, priority INTEGER NOT NULL, type TEXT NOT NULL, summary TEXT NOT NULL,
			body TEXT NOT NULL, urgency TEXT NOT NULL DEFAULT 'wake' CHECK(urgency IN ('wake','soft')),
			state TEXT NOT NULL CHECK(state IN ('pending','delivered','acked')), created_at INTEGER NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS events_delivery ON events(target,state,due_at,priority,created_at)`,
		`CREATE TABLE IF NOT EXISTS dnd (target TEXT PRIMARY KEY, set_by TEXT NOT NULL, set_at INTEGER NOT NULL, expires_at INTEGER NOT NULL)`,
		// A merged fork's address forwards to what it merged into, so late
		// deliveries (an agent it dispatched settling, a wake it scheduled) reach
		// someone who can act on them.
		`CREATE TABLE IF NOT EXISTS merged (fork TEXT PRIMARY KEY, parent TEXT NOT NULL, merged_at INTEGER NOT NULL)`,
	} {
		if _, err = db.Exec(q); err != nil {
			db.Close()
			return nil, err
		}
	}
	// Existing M2 databases predate urgency; SQLite has no ADD COLUMN IF NOT EXISTS.
	for _, q := range []string{
		`ALTER TABLE events ADD COLUMN urgency TEXT NOT NULL DEFAULT 'wake' CHECK(urgency IN ('wake','soft'))`,
		`ALTER TABLE events ADD COLUMN rule TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE events ADD COLUMN series TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err = db.Exec(q); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, err
		}
	}
	if _, err = db.Exec(`CREATE INDEX IF NOT EXISTS events_series ON events(series) WHERE series != ''`); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, now: time.Now}, nil
}
func (s *Store) Close() error { return s.db.Close() }

func ValidTarget(target string) bool {
	if strings.HasPrefix(target, "instance:") {
		return len(target) > 9 && idRE.MatchString(strings.TrimPrefix(target, "instance:"))
	}
	if strings.HasPrefix(target, "spawn:") {
		return len(target) > 6 && len(target) <= 200 && !strings.ContainsAny(target, "\x00\n\r")
	}
	return false
}
func NormalizeTarget(target string) string {
	if target != "" && !strings.Contains(target, ":") {
		return "instance:" + target
	}
	return target
}
func mint(now time.Time) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("event-%d-%s", now.UnixMilli(), hex.EncodeToString(b[:]))
}

func (s *Store) Enqueue(in Enqueue) (Event, bool, error) {
	in.Target = NormalizeTarget(in.Target)
	if !ValidTarget(in.Target) {
		return Event{}, false, errors.New("target must be instance:<session id> or spawn:<systemd unit>")
	}
	if in.ID != "" && !idRE.MatchString(in.ID) {
		return Event{}, false, errors.New("invalid event id")
	}
	if strings.TrimSpace(in.Summary) == "" {
		return Event{}, false, errors.New("summary is required")
	}
	if in.Type == "merge" {
		var m struct {
			Summary, ForkSessionID, ForkSessionFile, BranchEntryID, FirstEntryID, LastEntryID, MergedAt string
			TurnCount                                                                                   *int  `json:"turnCount"`
			ForkedFurther                                                                               *bool `json:"forkedFurther"`
		}
		if json.Unmarshal([]byte(in.Body), &m) != nil || strings.TrimSpace(m.Summary) == "" || m.ForkSessionID == "" || m.ForkSessionFile == "" || m.BranchEntryID == "" || m.FirstEntryID == "" || m.LastEntryID == "" || m.MergedAt == "" || m.TurnCount == nil || *m.TurnCount < 0 || m.ForkedFurther == nil {
			return Event{}, false, errors.New("merge requires summary, forkSessionId/file, branch/first/last entry ids, mergedAt, turnCount, and forkedFurther")
		}
		if resolved, err := s.Resolve(in.Target); err != nil {
			return Event{}, false, err
		} else {
			// A fork whose parent already came home returns to the grandparent.
			in.Target = resolved
		}
		if err := s.recordMerge("instance:"+m.ForkSessionID, in.Target); err != nil {
			return Event{}, false, err
		}
	} else {
		resolved, err := s.Resolve(in.Target)
		if err != nil {
			return Event{}, false, err
		}
		if resolved != in.Target {
			in.Summary = "(for merged fork " + shortID(in.Target) + ") " + in.Summary
			in.Target = resolved
		}
	}
	if in.Type == "fork" && !strings.HasPrefix(in.Target, "instance:") {
		return Event{}, false, errors.New("a fork event targets the instance to fork from (instance:<session id>)")
	}
	var rule Rule
	if in.Rule != "" {
		var err error
		if rule, err = ParseRule(in.Rule); err != nil {
			return Event{}, false, err
		}
	}
	priority := 2
	if in.Priority != nil {
		priority = *in.Priority
	}
	if priority < 0 || priority > 3 {
		return Event{}, false, errors.New("priority must be 0..3")
	}
	if in.Urgency == "" {
		in.Urgency = "wake"
	}
	if in.Urgency != "wake" && in.Urgency != "soft" {
		return Event{}, false, errors.New("urgency must be wake or soft")
	}
	now := s.now().UnixMilli()
	if in.DueAt == 0 && in.Rule != "" {
		in.DueAt = rule.First(s.now()).UnixMilli()
	}
	if in.DueAt == 0 {
		in.DueAt = now
	}
	if in.ID == "" {
		in.ID = mint(s.now())
	}
	if in.Source == "" {
		in.Source = "unknown"
	}
	if in.Type == "" {
		in.Type = "notify"
	}
	if in.Body == "" {
		in.Body = in.Summary
	}
	e := Event{ID: in.ID, DueAt: in.DueAt, Target: in.Target, Origin: in.Origin, Source: in.Source, Priority: priority, Type: in.Type, Summary: in.Summary, Body: in.Body, Urgency: in.Urgency, State: "pending", CreatedAt: now}
	if in.Rule != "" {
		e.Rule, e.Series = strings.ToLower(strings.TrimSpace(in.Rule)), in.ID
	}
	res, err := insertEvent(s.db, e)
	if err != nil {
		return Event{}, false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		old, err := s.Get(e.ID)
		return old, false, err
	}
	return e, true, nil
}

const eventColumns = `id,due_at,target,origin,source,priority,type,summary,body,urgency,state,created_at,rule,series`

func shortID(target string) string {
	id := strings.TrimPrefix(target, "instance:")
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// Resolve follows merged-fork forwarding: an event for a fork that has come
// home is delivered to the instance it merged into (transitively, for a fork
// of a fork). Non-instance targets and live instances resolve to themselves.
func (s *Store) Resolve(target string) (string, error) {
	target = NormalizeTarget(target)
	for hops := 0; hops < 8 && strings.HasPrefix(target, "instance:"); hops++ {
		var parent string
		err := s.db.QueryRow(`SELECT parent FROM merged WHERE fork=?`, target).Scan(&parent)
		if errors.Is(err, sql.ErrNoRows) {
			return target, nil
		}
		if err != nil {
			return "", err
		}
		target = parent
	}
	return target, nil
}

// recordMerge remembers where a fork went and forwards anything still waiting
// for it (pending, or delivered to a Pi that is shutting down) to its parent.
// The fork's own merge event is enqueued to the parent, so it is unaffected.
func (s *Store) recordMerge(fork, parent string) error {
	if fork == parent || !strings.HasPrefix(parent, "instance:") {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO merged(fork,parent,merged_at) VALUES(?,?,?) ON CONFLICT(fork) DO UPDATE SET parent=excluded.parent,merged_at=excluded.merged_at`, fork, parent, s.now().UnixMilli()); err != nil {
		return err
	}
	prefix := "(for merged fork " + shortID(fork) + ") "
	if _, err = tx.Exec(`UPDATE events SET target=?, state='pending', summary=?||summary WHERE target=? AND state IN ('pending','delivered') AND type!='fork'`, parent, prefix, fork); err != nil {
		return err
	}
	// A scheduled fork set by the fork keeps its schedule: spawn it from the parent.
	if _, err = tx.Exec(`UPDATE events SET target=? WHERE target=? AND state='pending' AND type='fork'`, parent, fork); err != nil {
		return err
	}
	return tx.Commit()
}

type execer interface {
	Exec(string, ...any) (sql.Result, error)
}

func insertEvent(db execer, e Event) (sql.Result, error) {
	return db.Exec(`INSERT OR IGNORE INTO events(`+eventColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,'pending',?,?,?)`, e.ID, e.DueAt, e.Target, e.Origin, e.Source, e.Priority, e.Type, e.Summary, e.Body, e.Urgency, e.CreatedAt, e.Rule, e.Series)
}

func scanEvent(row interface{ Scan(...any) error }) (Event, error) {
	var e Event
	err := row.Scan(&e.ID, &e.DueAt, &e.Target, &e.Origin, &e.Source, &e.Priority, &e.Type, &e.Summary, &e.Body, &e.Urgency, &e.State, &e.CreatedAt, &e.Rule, &e.Series)
	return e, err
}
func (s *Store) Get(id string) (Event, error) {
	return scanEvent(s.db.QueryRow(`SELECT `+eventColumns+` FROM events WHERE id=?`, id))
}

// List returns undelivered and in-flight events. target "*" lists every target.
func (s *Store) List(target string) ([]Event, error) {
	q := `SELECT ` + eventColumns + ` FROM events WHERE state!='acked'`
	args := []any{}
	if target != "" && target != "*" {
		q += ` AND target=?`
		args = append(args, NormalizeTarget(target))
	}
	q += ` ORDER BY due_at,priority,created_at`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Cancel stops an event; for a recurring event it stops the whole series
// (any occurrence id, or the series id itself, works).
func (s *Store) Cancel(id string) (bool, error) {
	var series string
	_ = s.db.QueryRow(`SELECT series FROM events WHERE id=?`, id).Scan(&series)
	if series == "" {
		series = id
	}
	r, err := s.db.Exec(`UPDATE events SET state='acked' WHERE (id=? OR series=?) AND state!='acked'`, id, series)
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n > 0, nil
}
func (s *Store) Ack(id, target string) (bool, error) {
	r, err := s.db.Exec(`UPDATE events SET state='acked' WHERE id=? AND target=? AND state='delivered'`, id, NormalizeTarget(target))
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n > 0, nil
}
func (s *Store) Requeue(target string) error {
	_, err := s.db.Exec(`UPDATE events SET state='pending' WHERE target=? AND state='delivered'`, NormalizeTarget(target))
	return err
}
func (s *Store) Claim(target string, now int64) (*Event, error) {
	target = NormalizeTarget(target)
	if strings.HasPrefix(target, "spawn:") {
		return nil, nil
	}
	dnd, err := s.DNDGet(target)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	query := `SELECT ` + eventColumns + ` FROM events WHERE target=? AND state='pending' AND due_at<=?`
	if dnd != nil {
		// DND holds interruptions. Soft notes ride the next user turn, and a
		// scheduled fork runs in the background without a turn in the target,
		// so neither interrupts; its merge is what DND governs.
		query += ` AND (urgency='soft' OR type='fork')`
	}
	e, err := scanEvent(tx.QueryRow(query+` ORDER BY priority,due_at,created_at LIMIT 1`, target, now))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(`UPDATE events SET state='delivered' WHERE id=? AND state='pending'`, e.ID); err != nil {
		return nil, err
	}
	if e.Rule != "" {
		// Enqueue the next occurrence with the delivery, so a series never has
		// zero or two pending occurrences. After() never lands at or before
		// now: a late delivery fires once, then resumes the cadence.
		rule, perr := ParseRule(e.Rule)
		if perr == nil {
			next := e
			next.DueAt = rule.After(time.UnixMilli(e.DueAt), time.UnixMilli(now)).UnixMilli()
			next.ID = fmt.Sprintf("%s-at-%d", e.Series, next.DueAt)
			next.CreatedAt = s.now().UnixMilli()
			if len(next.ID) > 160 {
				next.ID = mint(s.now())
			}
			if _, err = insertEvent(tx, next); err != nil {
				return nil, err
			}
		} else {
			log.Printf("scheduler: series %s has an unparseable rule %q; not rescheduling", e.Series, e.Rule)
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	e.State = "delivered"
	return &e, nil
}
func (s *Store) DNDGet(target string) (*DND, error) {
	var d DND
	err := s.db.QueryRow(`SELECT set_by,set_at,expires_at FROM dnd WHERE target=?`, NormalizeTarget(target)).Scan(&d.SetBy, &d.SetAt, &d.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	now := s.now().UnixMilli()
	if d.ExpiresAt <= now {
		_, _ = s.db.Exec(`DELETE FROM dnd WHERE target=?`, NormalizeTarget(target))
		return nil, nil
	}
	d.Enabled = true
	return &d, nil
}
func (s *Store) DNDSet(target string, enabled bool, setBy string, durationMS *int64) (*DND, error) {
	target = NormalizeTarget(target)
	if !ValidTarget(target) || strings.HasPrefix(target, "spawn:") {
		return nil, errors.New("DND requires an instance target")
	}
	if !enabled {
		_, err := s.db.Exec(`DELETE FROM dnd WHERE target=?`, target)
		return nil, err
	}
	if setBy == "" {
		setBy = "familiar"
	}
	if setBy != "user" && setBy != "familiar" {
		return nil, errors.New("set_by must be user or familiar")
	}
	dur := int64(30 * 60 * 1000)
	if durationMS != nil {
		dur = *durationMS
	}
	if dur <= 0 {
		return nil, errors.New("duration_ms must be positive")
	}
	if setBy == "familiar" && dur > 2*60*60*1000 {
		dur = 2 * 60 * 60 * 1000
	}
	now := s.now().UnixMilli()
	d := &DND{true, setBy, now, now + dur}
	_, err := s.db.Exec(`INSERT INTO dnd(target,set_by,set_at,expires_at) VALUES(?,?,?,?) ON CONFLICT(target) DO UPDATE SET set_by=excluded.set_by,set_at=excluded.set_at,expires_at=excluded.expires_at`, target, setBy, d.SetAt, d.ExpiresAt)
	return d, err
}

// Migrate imports live M2-v1 worklist items and pending wakes. Stable IDs make it safe to rerun.
func (s *Store) Migrate(stateDir, defaultTarget string) (int, error) {
	defaultTarget = NormalizeTarget(defaultTarget)
	if !ValidTarget(defaultTarget) {
		return 0, errors.New("a valid --default-target is required for migration")
	}
	count := 0
	items, _ := filepath.Glob(filepath.Join(stateDir, "worklist", "items", "*.json"))
	sort.Strings(items)
	for _, path := range items {
		var x struct {
			ID                          string `json:"id"`
			TS                          int64  `json:"ts"`
			Priority                    int    `json:"priority"`
			Type, Summary, Body, Source string
		}
		b, e := os.ReadFile(path)
		if e != nil || json.Unmarshal(b, &x) != nil || x.ID == "" || x.Summary == "" {
			continue
		}
		p := x.Priority
		_, created, e := s.Enqueue(Enqueue{ID: x.ID, DueAt: x.TS, Target: defaultTarget, Source: x.Source, Priority: &p, Type: x.Type, Summary: x.Summary, Body: x.Body})
		if e != nil {
			return count, e
		}
		if created {
			count++
		}
	}
	wakes, _ := filepath.Glob(filepath.Join(stateDir, "wakes", "pending", "*.json"))
	sort.Strings(wakes)
	for _, path := range wakes {
		var x struct {
			ID, Reason string
			FireAt     int64 `json:"fireAt"`
		}
		b, e := os.ReadFile(path)
		if e != nil || json.Unmarshal(b, &x) != nil || x.ID == "" || x.Reason == "" {
			continue
		}
		p := 1
		_, created, e := s.Enqueue(Enqueue{ID: x.ID, DueAt: x.FireAt, Target: defaultTarget, Source: "wake", Priority: &p, Summary: "Scheduled wake: " + x.Reason, Body: "<system-reminder>Scheduled wake: " + x.Reason + "</system-reminder>"})
		if e != nil {
			return count, e
		}
		if created {
			count++
		}
	}
	return count, nil
}
