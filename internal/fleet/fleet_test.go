package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gisikw/familiar-services/internal/scheduler"
)

// fake is a tiny Herdr: agents move through states on a script, and waits
// block the way Herdr's do.
type fake struct {
	mu      sync.Mutex
	agents  map[string]*fakeAgent
	onStart func(a *fakeAgent) // what happens after a prompt
	calls   []string
}
type fakeAgent struct {
	status string
	seq    int64
	gone   bool
}

func (f *fake) set(name, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.agents[name]
	a.status, a.seq = status, a.seq+1
}

func (f *fake) Run(ctx context.Context, node string, args ...string) (json.RawMessage, error) {
	f.mu.Lock()
	f.calls = append(f.calls, node+" "+strings.Join(args, " "))
	f.mu.Unlock()
	agentJSON := func(a *fakeAgent) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"agent":{"agent_status":%q,"state_change_seq":%d}}`, a.status, a.seq))
	}
	get := func(name string) (*fakeAgent, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		a := f.agents[name]
		if a == nil || a.gone {
			return nil, &Error{"agent_not_found", "gone"}
		}
		return a, nil
	}
	switch args[0] + " " + args[1] {
	case "workspace create":
		return json.RawMessage(`{"workspace":{"workspace_id":"w1"},"root_pane":{"pane_id":"w1:p1"}}`), nil
	case "workspace close":
		f.mu.Lock()
		for _, a := range f.agents {
			a.gone = true
		}
		f.mu.Unlock()
		return json.RawMessage(`{}`), nil
	case "agent start":
		f.mu.Lock()
		f.agents[args[2]] = &fakeAgent{status: "idle", seq: 1}
		out := agentJSON(f.agents[args[2]])
		f.mu.Unlock()
		return out, nil
	case "agent prompt":
		a, err := get(args[2])
		if err != nil {
			return nil, err
		}
		f.mu.Lock()
		out := agentJSON(a)
		f.mu.Unlock()
		if f.onStart != nil {
			go f.onStart(a)
		}
		return out, nil
	case "agent get":
		a, err := get(args[2])
		if err != nil {
			return nil, err
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		return agentJSON(a), nil
	case "agent read":
		return json.RawMessage(`"pong\n"`), nil
	case "agent send-keys":
		return json.RawMessage(`{}`), nil
	case "agent wait":
		until := map[string]bool{"idle": true, "done": true, "blocked": true}
		deadline := time.Time{}
		for i := 3; i < len(args); i++ {
			switch args[i] {
			case "--until":
				if len(until) == 3 {
					until = map[string]bool{}
				}
				until[args[i+1]] = true
				i++
			case "--timeout":
				var ms int64
				fmt.Sscan(args[i+1], &ms)
				deadline = time.Now().Add(time.Duration(ms) * time.Millisecond)
				i++
			}
		}
		for {
			a, err := get(args[2])
			if err != nil {
				return nil, err
			}
			f.mu.Lock()
			hit, out := until[a.status], agentJSON(a)
			f.mu.Unlock()
			if hit {
				return out, nil
			}
			if !deadline.IsZero() && time.Now().After(deadline) {
				return nil, &Error{"timeout", "timed out"}
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(2 * time.Millisecond):
			}
		}
	}
	return nil, fmt.Errorf("fake: unhandled %v", args)
}

type sink struct {
	mu     sync.Mutex
	events map[string]scheduler.Enqueue
	order  []string
}

func (s *sink) enqueue(e scheduler.Enqueue) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.events[e.ID]; !dup {
		s.order = append(s.order, e.ID)
	}
	s.events[e.ID] = e
	return nil
}
func (s *sink) ids() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.order...)
}

func setup(t *testing.T, dir string, f *fake) (*Service, *sink, context.CancelFunc) {
	t.Helper()
	k := &sink{events: map[string]scheduler.Enqueue{}}
	s, err := Open(dir, f, k.enqueue)
	if err != nil {
		t.Fatal(err)
	}
	s.StartGrace, s.Poll, s.Backoff = 60*time.Millisecond, 40*time.Millisecond, 5*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	for s.ctxReady() == false {
		time.Sleep(time.Millisecond)
	}
	t.Cleanup(func() { cancel(); <-done; s.Close() })
	return s, k, cancel
}

func (s *Service) ctxReady() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.ctx != nil }

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSettleDeliversOnceToOrigin(t *testing.T) {
	f := &fake{agents: map[string]*fakeAgent{}}
	f.onStart = func(a *fakeAgent) {
		time.Sleep(5 * time.Millisecond)
		f.set("worker", "working")
		time.Sleep(10 * time.Millisecond)
		f.set("worker", "idle")
	}
	s, k, _ := setup(t, t.TempDir(), f)
	a, err := s.Start(context.Background(), StartRequest{Node: "bandit", Task: "say pong\nmore detail", Name: "worker", Origin: "fork-1"})
	if err != nil || a.Runtime != "enrolled" || a.Kind != "pi" {
		t.Fatalf("start: %#v %v", a, err)
	}
	eventually(t, "settle", func() bool { return len(k.ids()) == 1 })
	time.Sleep(150 * time.Millisecond) // several idle polls: no repeats
	if ids := k.ids(); len(ids) != 1 || ids[0] != "fleet-worker-3" {
		t.Fatalf("deliveries: %v", ids)
	}
	e := k.events["fleet-worker-3"]
	if e.Target != "instance:fork-1" || !strings.Contains(e.Summary, "settled: say pong") || !strings.Contains(e.Body, "pong") || strings.Contains(e.Summary, "more detail") {
		t.Fatalf("event: %#v", e)
	}
}

func TestBlockedThenAnsweredThenSettled(t *testing.T) {
	f := &fake{agents: map[string]*fakeAgent{}}
	f.onStart = func(a *fakeAgent) {
		f.set("asker", "working")
		time.Sleep(5 * time.Millisecond)
		f.set("asker", "blocked")
	}
	s, k, _ := setup(t, t.TempDir(), f)
	if _, err := s.Start(context.Background(), StartRequest{Node: "bandit", Task: "ask me", Name: "asker", Origin: "primary"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "blocked", func() bool { return len(k.ids()) == 1 })
	if e := k.events[k.ids()[0]]; *e.Priority != 1 || !strings.Contains(e.Summary, "blocked") {
		t.Fatalf("blocked event: %#v", e)
	}
	if err := s.Keys(context.Background(), "asker", []string{"enter"}); err != nil {
		t.Fatal(err)
	}
	f.set("asker", "working")
	time.Sleep(5 * time.Millisecond)
	f.set("asker", "idle")
	eventually(t, "settled after answer", func() bool { return len(k.ids()) == 2 })
}

func TestNeverStartsIsReported(t *testing.T) {
	f := &fake{agents: map[string]*fakeAgent{}} // prompt changes nothing
	s, k, _ := setup(t, t.TempDir(), f)
	if _, err := s.Start(context.Background(), StartRequest{Task: "x", Name: "stuck", Origin: "primary"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "not-started", func() bool { return len(k.ids()) == 1 })
	if e := k.events[k.ids()[0]]; !strings.Contains(e.Summary, "didn't start") {
		t.Fatalf("event: %#v", e)
	}
	time.Sleep(150 * time.Millisecond)
	if len(k.ids()) != 1 {
		t.Fatalf("repeated: %v", k.ids())
	}
}

func TestResumeDoesNotRedeliver(t *testing.T) {
	dir := t.TempDir()
	f := &fake{agents: map[string]*fakeAgent{}}
	f.onStart = func(a *fakeAgent) {
		f.set("keeper", "working")
		time.Sleep(5 * time.Millisecond)
		f.set("keeper", "idle")
	}
	s, k, cancel := setup(t, dir, f)
	if _, err := s.Start(context.Background(), StartRequest{Task: "x", Name: "keeper", Origin: "primary"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "settle", func() bool { return len(k.ids()) == 1 })
	cancel()
	// A restarted service picks the agent back up from its recorded seq.
	_, k2, _ := setup(t, dir, f)
	time.Sleep(150 * time.Millisecond)
	if len(k2.ids()) != 0 {
		t.Fatalf("redelivered after restart: %v", k2.ids())
	}
	f.set("keeper", "working")
	time.Sleep(5 * time.Millisecond)
	f.set("keeper", "idle")
	eventually(t, "new turn after restart", func() bool { return len(k2.ids()) == 1 })
}

func TestExitAndClose(t *testing.T) {
	f := &fake{agents: map[string]*fakeAgent{}}
	f.onStart = func(a *fakeAgent) { f.set("gone", "working") }
	s, k, _ := setup(t, t.TempDir(), f)
	if _, err := s.Start(context.Background(), StartRequest{Task: "x", Name: "gone", Origin: "primary"}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.agents["gone"].gone = true
	f.mu.Unlock()
	eventually(t, "exited", func() bool { return len(k.ids()) == 1 && strings.Contains(k.events[k.ids()[0]].Summary, "exited") })
	a, _ := s.Get("gone")
	if a.ClosedAt == 0 {
		t.Fatal("exited agent not closed")
	}
	if err := s.Prompt(context.Background(), "gone", "hi"); err == nil {
		t.Fatal("prompted a closed agent")
	}
	if open, _ := s.List("", false); len(open) != 0 {
		t.Fatalf("open list: %v", open)
	}
}

func TestStartValidation(t *testing.T) {
	s, _, _ := setup(t, t.TempDir(), &fake{agents: map[string]*fakeAgent{}})
	for _, in := range []StartRequest{
		{Task: "x"}, // no origin
		{Task: "x", Origin: "o", Kind: "codex"},
		{Task: "x", Origin: "o", Kind: "claude", Runtime: "gisikw/familiar-fleet@abc"},
		{Task: "x", Origin: "o", Runtime: "gisikw/familiar-fleet@abc"},
		{Task: "x", Origin: "o", Name: "Bad Name"},
		{Task: " ", Origin: "o"},
	} {
		if _, err := s.Start(context.Background(), in); err == nil {
			t.Errorf("accepted %#v", in)
		}
	}
	a, err := s.Start(context.Background(), StartRequest{Task: "x", Origin: "o", Kind: "claude"})
	if err != nil || a.Runtime != "host" {
		t.Fatalf("claude: %#v %v", a, err)
	}
}

func TestTidy(t *testing.T) {
	in := "\n\n hello  \n\n\n────────\n\n world\n\n\n"
	if got := tidy(in); got != " hello\n\n world" {
		t.Fatalf("tidy: %q", got)
	}
}
