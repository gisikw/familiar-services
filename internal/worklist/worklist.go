// Package worklist owns the durable, file-backed worklist and DND state.
package worklist

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,159}$`)

type Item struct {
	ID                string `json:"id"`
	TS                int64  `json:"ts"`
	Priority          int    `json:"priority"`
	Type              string `json:"type"`
	Summary           string `json:"summary"`
	Body              string `json:"body"`
	Source            string `json:"source"`
	SuggestedDeadline *int64 `json:"suggested_deadline,omitempty"`
	Delivered         bool   `json:"delivered,omitempty"`
	Acked             bool   `json:"acked,omitempty"`
	SurfacedCount     int    `json:"surfacedCount,omitempty"`
	Escalated         bool   `json:"escalated,omitempty"`
	Digested          bool   `json:"digested,omitempty"`
	DigestedAt        *int64 `json:"digestedAt,omitempty"`
	SnoozedUntil      *int64 `json:"snoozedUntil,omitempty"`
	Withdrawn         bool   `json:"withdrawn,omitempty"`
}

type Enqueue struct {
	ID                string `json:"id"`
	Priority          *int   `json:"priority"`
	Type              string `json:"type"`
	Summary           string `json:"summary"`
	Body              string `json:"body"`
	Source            string `json:"source"`
	SuggestedDeadline *int64 `json:"suggested_deadline"`
}

type DND struct {
	Enabled   bool   `json:"enabled"`
	SetBy     string `json:"setBy"`
	SetAt     int64  `json:"setAt"`
	ExpiresAt int64  `json:"expiresAt"`
}

type Store struct {
	root string
	mu   sync.Mutex
	now  func() time.Time
}

func Open(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("state directory is required")
	}
	s := &Store{root: filepath.Join(root, "worklist"), now: time.Now}
	for _, d := range []string{s.items(), s.archive(), filepath.Join(s.root, "incoming"), filepath.Join(s.root, "acknowledgements")} {
		if err := os.MkdirAll(d, 0700); err != nil {
			return nil, err
		}
	}
	return s, nil
}
func (s *Store) items() string   { return filepath.Join(s.root, "items") }
func (s *Store) archive() string { return filepath.Join(s.items(), "archive") }
func validID(id string) bool     { return idPattern.MatchString(id) }
func (s *Store) path(id string, archived bool) string {
	if archived {
		return filepath.Join(s.archive(), id+".json")
	}
	return filepath.Join(s.items(), id+".json")
}

func atomicJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	cerr := f.Close()
	if err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}
func readJSON(path string, out any) error {
	b, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	return json.Unmarshal(b, out)
}
func (s *Store) get(id string, archived bool) (*Item, error) {
	if !validID(id) {
		return nil, errors.New("invalid worklist id")
	}
	var x Item
	if e := readJSON(s.path(id, archived), &x); e != nil {
		return nil, e
	}
	if !validID(x.ID) {
		return nil, errors.New("invalid worklist record")
	}
	return &x, nil
}
func (s *Store) known(id string) (*Item, bool) {
	x, e := s.get(id, true)
	if e == nil {
		return x, true
	}
	x, e = s.get(id, false)
	return x, e == nil
}
func (s *Store) put(x *Item, archived bool) error {
	if !validID(x.ID) {
		return errors.New("invalid worklist id")
	}
	return atomicJSON(s.path(x.ID, archived), x)
}

func (s *Store) Enqueue(in Enqueue) (*Item, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enqueueLocked(in)
}
func (s *Store) enqueueLocked(in Enqueue) (*Item, bool, error) {
	if in.Summary == "" {
		return nil, false, errors.New("summary is required")
	}
	if in.ID != "" {
		if !validID(in.ID) {
			return nil, false, errors.New("invalid worklist id")
		}
		if x, ok := s.known(in.ID); ok {
			return x, false, nil
		}
	}
	p := 2
	if in.Priority != nil {
		p = *in.Priority
	}
	if p < 0 || p > 3 {
		return nil, false, errors.New("priority must be 0..3")
	}
	t := in.Type
	if t == "" {
		t = "notify"
	}
	if t != "notify" && t != "question" && t != "review" {
		return nil, false, errors.New("invalid worklist type")
	}
	id := in.ID
	if id == "" {
		id = mint("wl", s.now())
	}
	body := in.Body
	if body == "" {
		body = in.Summary
	}
	source := in.Source
	if source == "" {
		source = "unknown"
	}
	x := &Item{ID: id, TS: s.now().UnixMilli(), Priority: p, Type: t, Summary: in.Summary, Body: body, Source: source, SuggestedDeadline: in.SuggestedDeadline}
	if err := s.put(x, false); err != nil {
		return nil, false, err
	}
	return x, true, nil
}
func mint(prefix string, now time.Time) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s-%s-%s", prefix, now.UTC().Format("20060102-150405"), hex.EncodeToString(b[:2]))
}

func (s *Store) List() ([]Item, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.listLocked() }
func (s *Store) listLocked() ([]Item, error) {
	names, e := os.ReadDir(s.items())
	if e != nil {
		return nil, e
	}
	out := []Item{}
	for _, n := range names {
		if n.IsDir() || filepath.Ext(n.Name()) != ".json" {
			continue
		}
		var x Item
		if readJSON(filepath.Join(s.items(), n.Name()), &x) != nil || !validID(x.ID) {
			continue
		}
		if _, e := os.Stat(s.path(x.ID, true)); e == nil {
			_ = os.Remove(s.path(x.ID, false))
			continue
		}
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS < out[j].TS })
	return out, nil
}
func (s *Store) Ack(id string) (*Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x, ok := s.known(id)
	if !ok {
		return nil, os.ErrNotExist
	}
	x.Delivered = true
	x.Acked = true
	if e := s.put(x, true); e != nil {
		return nil, e
	}
	_ = os.Remove(s.path(id, false))
	return x, nil
}
func (s *Store) Withdraw(id string) (*Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x, ok := s.known(id)
	if !ok {
		return nil, os.ErrNotExist
	}
	if x.Delivered || x.Acked {
		return nil, errors.New("already delivered")
	}
	x.Withdrawn = true
	if e := s.put(x, true); e != nil {
		return nil, e
	}
	_ = os.Remove(s.path(id, false))
	return x, nil
}

func (s *Store) DNDGet() (*DND, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.dndLocked() }
func (s *Store) dndLocked() (*DND, error) {
	var d *DND
	e := readJSON(filepath.Join(s.root, "dnd.json"), &d)
	if os.IsNotExist(e) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	now := s.now().UnixMilli()
	if d == nil || !d.Enabled || (d.SetBy != "user" && d.SetBy != "familiar") || d.ExpiresAt <= now || d.ExpiresAt <= d.SetAt {
		_ = atomicJSON(filepath.Join(s.root, "dnd.json"), nil)
		return nil, nil
	}
	if d.SetBy == "familiar" {
		cap := d.SetAt + 2*60*60*1000
		if d.ExpiresAt > cap {
			d.ExpiresAt = cap
		}
		if d.ExpiresAt > now+2*60*60*1000 {
			d.ExpiresAt = now + 2*60*60*1000
		}
		if d.ExpiresAt <= now {
			return nil, nil
		}
	}
	return d, nil
}
func (s *Store) DNDSet(enabled bool, setBy string, durationMS *int64) (*DND, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !enabled {
		if e := atomicJSON(filepath.Join(s.root, "dnd.json"), nil); e != nil {
			return nil, e
		}
		return nil, nil
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
	if e := atomicJSON(filepath.Join(s.root, "dnd.json"), d); e != nil {
		return nil, e
	}
	return d, nil
}
