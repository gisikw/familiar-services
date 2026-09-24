// Package wakes owns durable wake timers. Due wakes become worklist items.
package wakes

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

	"github.com/gisikw/familiar-services/internal/worklist"
)

type Record struct {
	Version     int    `json:"version"`
	ID          string `json:"id"`
	Mode        string `json:"mode"`
	Reason      string `json:"reason"`
	ScheduledAt int64  `json:"scheduledAt"`
	FireAt      int64  `json:"fireAt"`
}
type Store struct {
	root    string
	work    *worklist.Store
	mu      sync.Mutex
	timers  map[string]*time.Timer
	stopped bool
	now     func() time.Time
}

var idRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,159}$`)

func Open(stateDir string, w *worklist.Store) (*Store, error) {
	s := &Store{root: filepath.Join(stateDir, "wakes"), work: w, timers: map[string]*time.Timer{}, now: time.Now}
	for _, d := range []string{s.pending(), s.fired(), filepath.Join(s.root, "quarantine")} {
		if e := os.MkdirAll(d, 0700); e != nil {
			return nil, e
		}
	}
	if e := s.restore(); e != nil {
		return nil, e
	}
	return s, nil
}
func (s *Store) pending() string { return filepath.Join(s.root, "pending") }
func (s *Store) fired() string   { return filepath.Join(s.root, "fired") }
func (s *Store) file(id string, fired bool) string {
	if fired {
		return filepath.Join(s.fired(), id+".json")
	}
	return filepath.Join(s.pending(), id+".json")
}
func atomic(path string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	_ = f.Chmod(0600)
	if _, e = f.Write(append(b, '\n')); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	return os.Rename(name, path)
}
func valid(r Record) bool {
	return r.Version == 1 && idRE.MatchString(r.ID) && one(r.Mode, "always", "unless_wakened") && len(r.Reason) <= 16384 && r.ScheduledAt >= 0 && r.FireAt >= r.ScheduledAt
}
func one(x string, v ...string) bool {
	for _, s := range v {
		if x == s {
			return true
		}
	}
	return false
}
func (s *Store) restore() error {
	es, e := os.ReadDir(s.pending())
	if e != nil {
		return e
	}
	for _, x := range es {
		if x.IsDir() || filepath.Ext(x.Name()) != ".json" {
			continue
		}
		var r Record
		b, e := os.ReadFile(filepath.Join(s.pending(), x.Name()))
		if e != nil || json.Unmarshal(b, &r) != nil || !valid(r) || x.Name() != r.ID+".json" {
			continue
		}
		if _, e = os.Stat(s.file(r.ID, true)); e == nil {
			_ = os.Remove(s.file(r.ID, false))
			continue
		}
		s.arm(r)
	}
	return nil
}
func randID() string { var b [16]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }
func (s *Store) Schedule(mode, reason string, fireAt int64) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !one(mode, "always", "unless_wakened") {
		return Record{}, errors.New("invalid wake mode")
	}
	if reason == "" || len(reason) > 16384 {
		return Record{}, errors.New("reason is required")
	}
	now := s.now().UnixMilli()
	if fireAt < now+6000 {
		fireAt = now + 6000
	}
	r := Record{1, fmt.Sprintf("wake-%d-%s", now, randID()), mode, reason, now, fireAt}
	if e := atomic(s.file(r.ID, false), r); e != nil {
		return Record{}, e
	}
	s.arm(r)
	return r, nil
}
func (s *Store) arm(r Record) {
	d := time.Duration(r.FireAt-s.now().UnixMilli()) * time.Millisecond
	if d < 0 {
		d = 0
	}
	const max = time.Duration(2147000000) * time.Millisecond
	if d > max {
		d = max
	}
	s.timers[r.ID] = time.AfterFunc(d, func() {
		if s.now().UnixMilli() < r.FireAt {
			s.mu.Lock()
			delete(s.timers, r.ID)
			s.arm(r)
			s.mu.Unlock()
			return
		}
		s.fire(r)
	})
}
func (s *Store) fire(r Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.timers, r.ID)
	if s.stopped {
		return
	}
	if e := os.Rename(s.file(r.ID, false), s.file(r.ID, true)); e != nil {
		return
	}
	p := 1
	_, _, _ = s.work.Enqueue(worklist.Enqueue{ID: r.ID, Priority: &p, Type: "notify", Summary: "Scheduled wake: " + r.Reason, Body: fmt.Sprintf("<system-reminder>Scheduled wake %s firing (mode: %s). Reason: %s</system-reminder>", r.ID, r.Mode, r.Reason), Source: "wake"})
}
func (s *Store) Cancel(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !idRE.MatchString(id) {
		return false, errors.New("invalid wake id")
	}
	if t := s.timers[id]; t != nil {
		t.Stop()
		delete(s.timers, id)
	}
	e := os.Remove(s.file(id, false))
	if os.IsNotExist(e) {
		return false, nil
	}
	return e == nil, e
}
func (s *Store) List() ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	es, e := os.ReadDir(s.pending())
	if e != nil {
		return nil, e
	}
	out := []Record{}
	for _, x := range es {
		var r Record
		b, e := os.ReadFile(filepath.Join(s.pending(), x.Name()))
		if e == nil && json.Unmarshal(b, &r) == nil && valid(r) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FireAt < out[j].FireAt })
	return out, nil
}

// FreshInput durably cancels interruptible naps scheduled before the activity.
func (s *Store) FreshInput(at int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	es, _ := os.ReadDir(s.pending())
	for _, x := range es {
		var r Record
		b, e := os.ReadFile(filepath.Join(s.pending(), x.Name()))
		if e == nil && json.Unmarshal(b, &r) == nil && r.Mode == "unless_wakened" && r.ScheduledAt < at {
			if t := s.timers[r.ID]; t != nil {
				t.Stop()
				delete(s.timers, r.ID)
			}
			_ = os.Remove(s.file(r.ID, false))
		}
	}
}
func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
	for _, t := range s.timers {
		t.Stop()
	}
	s.timers = map[string]*time.Timer{}
}
