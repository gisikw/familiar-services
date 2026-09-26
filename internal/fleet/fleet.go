// Package fleet runs agents in Herdr on enrolled machines and reports their
// settled and blocked states to whoever dispatched them, via the scheduler.
//
// Herdr is the executor and the source of truth for agent state; this package
// keeps only what Herdr can't: which instance is waiting on each agent, and
// the last state change it already reported. One goroutine per live agent
// runs `herdr [--machine NODE] agent wait NAME`, and state_change_seq makes
// each transition deliver exactly once.
package fleet

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gisikw/familiar-services/internal/scheduler"
)

// Local is the node name for the Herdr session on this host (no --machine).
const Local = "local"

var (
	nameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	nodeRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._ -]{0,63}$`)
)

// Herdr runs one herdr CLI command against a node and returns its result
// object, or a *Error carrying Herdr's error code.
type Herdr interface {
	Run(ctx context.Context, node string, args ...string) (json.RawMessage, error)
}

type Error struct{ Code, Message string }

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func code(err error) string {
	var he *Error
	if errors.As(err, &he) {
		return he.Code
	}
	return ""
}

// Exec is the real Herdr: remote nodes go through saved machine profiles
// (`herdr --machine NODE`), the local node through a named session.
type Exec struct {
	Bin          string
	LocalSession string
}

func (x Exec) Run(ctx context.Context, node string, args ...string) (json.RawMessage, error) {
	bin := x.Bin
	if bin == "" {
		bin = "herdr"
	}
	argv := args
	if node != Local {
		argv = append([]string{"--machine", node}, args...)
	}
	cmd := exec.CommandContext(ctx, bin, argv...)
	cmd.Env = os.Environ()
	if node == Local && x.LocalSession != "" {
		cmd.Env = append(cmd.Env, "HERDR_SESSION="+x.LocalSession)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	// Herdr prints its JSON response on stdout, or a JSON error on stderr.
	for _, out := range []string{stdout.String(), stderr.String()} {
		out = strings.TrimSpace(out)
		if i := strings.LastIndex(out, "\n{"); i >= 0 {
			out = out[i+1:]
		}
		var r struct {
			Result json.RawMessage `json:"result"`
			Error  *Error          `json:"error"`
		}
		if json.Unmarshal([]byte(out), &r) == nil {
			if r.Error != nil {
				return nil, r.Error
			}
			if r.Result != nil {
				return r.Result, nil
			}
		}
	}
	if runErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 400 {
			msg = msg[len(msg)-400:]
		}
		return nil, fmt.Errorf("herdr %s: %v: %s", strings.Join(args[:min(2, len(args))], " "), runErr, msg)
	}
	// Reads print terminal text directly, not JSON.
	b, _ := json.Marshal(stdout.String())
	return b, nil
}

// Agent is one dispatched agent and who is waiting on it.
type Agent struct {
	Name      string `json:"name"`
	Node      string `json:"node"`
	Kind      string `json:"kind"`
	Runtime   string `json:"runtime"`
	Origin    string `json:"origin"`
	Cwd       string `json:"cwd"`
	Task      string `json:"task"`
	Workspace string `json:"workspace"`
	Pane      string `json:"pane"`
	State     string `json:"state"`
	Seq       int64  `json:"seq"`
	CreatedAt int64  `json:"created_at"`
	ClosedAt  int64  `json:"closed_at,omitempty"`
}

type StartRequest struct {
	Node    string `json:"node"`
	Kind    string `json:"kind"`
	Runtime string `json:"runtime"`
	Cwd     string `json:"cwd"`
	Task    string `json:"task"`
	Name    string `json:"name"`
	Origin  string `json:"origin"`
}

type Service struct {
	db      *sql.DB
	herdr   Herdr
	enqueue func(scheduler.Enqueue) error
	now     func() time.Time

	mu       sync.Mutex
	ctx      context.Context
	watchers map[string]context.CancelFunc
	wg       sync.WaitGroup

	// Timings are fields so tests can shrink them.
	StartGrace time.Duration // how long a new agent has to begin working
	Poll       time.Duration // bound on each wait for work to resume
	Backoff    time.Duration // first retry delay after a transport failure
}

func Open(stateDir string, herdr Herdr, enqueue func(scheduler.Enqueue) error) (*Service, error) {
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", filepath.Join(stateDir, "fleet.sqlite")+"?_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		`PRAGMA journal_mode=WAL`,
		`CREATE TABLE IF NOT EXISTS agents (
			name TEXT PRIMARY KEY, node TEXT NOT NULL, kind TEXT NOT NULL, runtime TEXT NOT NULL,
			origin TEXT NOT NULL, cwd TEXT NOT NULL, task TEXT NOT NULL, workspace TEXT NOT NULL,
			pane TEXT NOT NULL, state TEXT NOT NULL, seq INTEGER NOT NULL, created_at INTEGER NOT NULL,
			closed_at INTEGER NOT NULL DEFAULT 0)`,
	} {
		if _, err = db.Exec(q); err != nil {
			db.Close()
			return nil, err
		}
	}
	return &Service{db: db, herdr: herdr, enqueue: enqueue, now: time.Now, watchers: map[string]context.CancelFunc{},
		StartGrace: time.Minute, Poll: 10 * time.Minute, Backoff: 5 * time.Second}, nil
}

// Run resumes watchers for every open agent and keeps new ones under ctx.
func (s *Service) Run(ctx context.Context) error {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
	open, err := s.list(`closed_at=0`)
	if err != nil {
		return err
	}
	for _, a := range open {
		s.watch(a, false)
	}
	<-ctx.Done()
	s.wg.Wait()
	return nil
}

func (s *Service) Close() error { return s.db.Close() }

const cols = `name,node,kind,runtime,origin,cwd,task,workspace,pane,state,seq,created_at,closed_at`

func scan(r interface{ Scan(...any) error }) (Agent, error) {
	var a Agent
	err := r.Scan(&a.Name, &a.Node, &a.Kind, &a.Runtime, &a.Origin, &a.Cwd, &a.Task, &a.Workspace, &a.Pane, &a.State, &a.Seq, &a.CreatedAt, &a.ClosedAt)
	return a, err
}

func (s *Service) list(where string, args ...any) ([]Agent, error) {
	rows, err := s.db.Query(`SELECT `+cols+` FROM agents WHERE `+where+` ORDER BY created_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Agent{}
	for rows.Next() {
		a, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Service) Get(name string) (Agent, error) {
	a, err := scan(s.db.QueryRow(`SELECT `+cols+` FROM agents WHERE name=?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return a, os.ErrNotExist
	}
	return a, err
}

// List returns open agents (all of them, or only those an origin dispatched).
func (s *Service) List(origin string, closed bool) ([]Agent, error) {
	where, args := `1=1`, []any{}
	if !closed {
		where += ` AND closed_at=0`
	}
	if origin != "" {
		where += ` AND origin=?`
		args = append(args, origin)
	}
	return s.list(where, args...)
}

func mintName() string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return "a" + hex.EncodeToString(b[:])
}

// Start opens a workspace on the node, starts the agent there, submits the
// task, and begins watching. Anything half-made is closed on failure.
func (s *Service) Start(ctx context.Context, in StartRequest) (Agent, error) {
	in.Node, in.Kind, in.Task = strings.TrimSpace(in.Node), strings.TrimSpace(in.Kind), strings.TrimSpace(in.Task)
	if in.Node == "" {
		in.Node = Local
	}
	if in.Kind == "" {
		in.Kind = "pi"
	}
	switch {
	case !nodeRE.MatchString(in.Node):
		return Agent{}, errors.New("invalid node")
	case in.Kind != "pi" && in.Kind != "claude":
		return Agent{}, errors.New("kind must be pi or claude")
	case in.Task == "":
		return Agent{}, errors.New("task is required")
	case !scheduler.ValidTarget("instance:" + in.Origin):
		return Agent{}, errors.New("origin instance is required")
	}
	switch {
	case in.Kind == "claude" && in.Runtime != "" && in.Runtime != "host":
		return Agent{}, errors.New("claude runs on the node's own install; runtime must be empty or host")
	case in.Kind == "claude":
		in.Runtime = "host"
	case in.Runtime != "":
		return Agent{}, errors.New("per-dispatch runtime pinning is not built yet; omit --runtime to use the node's enrolled runtime")
	default:
		in.Runtime = "enrolled"
	}
	if in.Name == "" {
		in.Name = mintName()
	}
	if !nameRE.MatchString(in.Name) {
		return Agent{}, errors.New("name must match [a-z][a-z0-9_-]{0,31}")
	}
	if _, err := s.Get(in.Name); err == nil {
		return Agent{}, fmt.Errorf("agent %s already exists", in.Name)
	}
	if in.Cwd == "" {
		in.Cwd = "~"
	}
	res, err := s.herdr.Run(ctx, in.Node, "workspace", "create", "--cwd", in.Cwd, "--label", in.Name, "--no-focus")
	if err != nil {
		return Agent{}, fmt.Errorf("creating workspace on %s: %w", in.Node, err)
	}
	var ws struct {
		Workspace struct {
			ID string `json:"workspace_id"`
		} `json:"workspace"`
		Root struct {
			ID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	if json.Unmarshal(res, &ws) != nil || ws.Workspace.ID == "" || ws.Root.ID == "" {
		return Agent{}, errors.New("herdr returned no workspace")
	}
	fail := func(err error) (Agent, error) {
		_, _ = s.herdr.Run(context.Background(), in.Node, "workspace", "close", ws.Workspace.ID)
		return Agent{}, err
	}
	res, err = s.herdr.Run(ctx, in.Node, "agent", "start", in.Name, "--kind", in.Kind, "--pane", ws.Root.ID, "--timeout", "60000")
	if err != nil {
		return fail(fmt.Errorf("starting %s: %w", in.Kind, err))
	}
	st, _ := parseAgent(res)
	a := Agent{Name: in.Name, Node: in.Node, Kind: in.Kind, Runtime: in.Runtime, Origin: in.Origin, Cwd: in.Cwd, Task: in.Task,
		Workspace: ws.Workspace.ID, Pane: ws.Root.ID, State: st.Status, Seq: st.Seq, CreatedAt: s.now().UnixMilli()}
	if _, err = s.db.Exec(`INSERT INTO agents(`+cols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,0)`, a.Name, a.Node, a.Kind, a.Runtime, a.Origin, a.Cwd, a.Task, a.Workspace, a.Pane, a.State, a.Seq, a.CreatedAt); err != nil {
		return fail(err)
	}
	if _, err = s.herdr.Run(ctx, in.Node, "agent", "prompt", in.Name, in.Task); err != nil {
		_, _ = s.db.Exec(`UPDATE agents SET closed_at=? WHERE name=?`, s.now().UnixMilli(), a.Name)
		return fail(fmt.Errorf("submitting task: %w", err))
	}
	s.watch(a, true)
	return a, nil
}

type status struct {
	Status string
	Seq    int64
}

func parseAgent(res json.RawMessage) (status, error) {
	var r struct {
		Agent struct {
			Status string `json:"agent_status"`
			Seq    int64  `json:"state_change_seq"`
		} `json:"agent"`
	}
	if err := json.Unmarshal(res, &r); err != nil || r.Agent.Status == "" {
		return status{}, errors.New("herdr returned no agent")
	}
	return status{r.Agent.Status, r.Agent.Seq}, nil
}

func (s *Service) watch(a Agent, fresh bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx == nil || s.watchers[a.Name] != nil {
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.watchers[a.Name] = cancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() { s.mu.Lock(); delete(s.watchers, a.Name); s.mu.Unlock() }()
		s.loop(ctx, a, fresh)
	}()
}

// loop alternates two waits: for work to begin, then for it to settle.
// A settled or blocked state is reported once per state_change_seq.
func (s *Service) loop(ctx context.Context, a Agent, fresh bool) {
	delay := s.Backoff
	retry := func(err error) bool {
		if ctx.Err() != nil {
			return false
		}
		if code(err) == "agent_not_found" {
			s.deliver(a, "exited", a.Seq+1, "")
			s.markClosed(a.Name)
			return false
		}
		log.Printf("fleet: %s on %s: %v (retrying in %s)", a.Name, a.Node, err, delay)
		select {
		case <-ctx.Done():
			return false
		case <-time.After(delay):
		}
		delay = min(delay*2, 2*time.Minute)
		return true
	}
	grace := fresh
	for ctx.Err() == nil {
		// Wait for work. A fresh agent that never starts is reported once;
		// an idle one just re-checks on the poll interval.
		bound := s.Poll
		if grace {
			bound = s.StartGrace
		}
		_, err := s.herdr.Run(ctx, a.Node, "agent", "wait", a.Name, "--until", "working", "--timeout", fmt.Sprint(bound.Milliseconds()))
		if err != nil && code(err) != "timeout" {
			if retry(err) {
				continue
			}
			return
		}
		if err != nil && grace {
			if cur, e := s.herdr.Run(ctx, a.Node, "agent", "get", a.Name); e == nil {
				if st, e := parseAgent(cur); e == nil && st.Seq <= a.Seq {
					s.deliver(a, "not-started", st.Seq, st.Status)
					a.Seq = st.Seq
				}
			}
		}
		grace = false
		// Wait for it to settle (idle, done, or blocked).
		res, err := s.herdr.Run(ctx, a.Node, "agent", "wait", a.Name)
		if err != nil {
			if retry(err) {
				continue
			}
			return
		}
		delay = s.Backoff
		st, err := parseAgent(res)
		if err != nil || st.Seq <= a.Seq {
			continue
		}
		s.deliver(a, st.Status, st.Seq, st.Status)
		a.Seq, a.State = st.Seq, st.Status
		_, _ = s.db.Exec(`UPDATE agents SET seq=?, state=? WHERE name=?`, a.Seq, a.State, a.Name)
	}
}

func (s *Service) markClosed(name string) {
	_, _ = s.db.Exec(`UPDATE agents SET closed_at=? WHERE name=? AND closed_at=0`, s.now().UnixMilli(), name)
}

// deliver wakes the origin with the agent's state and the tail of its pane.
// The event id is derived from the transition, so a retry can't double-send.
func (s *Service) deliver(a Agent, what string, seq int64, herdrState string) {
	tail := ""
	if what != "exited" {
		if res, err := s.herdr.Run(context.Background(), a.Node, "agent", "read", a.Name, "--source", "recent-unwrapped", "--lines", "40"); err == nil {
			tail = readText(res)
		}
	}
	verb := map[string]string{
		"blocked":     "is blocked and needs an answer",
		"idle":        "settled",
		"done":        "settled",
		"exited":      "exited",
		"not-started": "didn't start working",
		"unknown":     "stopped in a state Herdr can't classify",
	}[what]
	if verb == "" {
		verb = what
	}
	task := a.Task
	if i := strings.IndexByte(task, '\n'); i >= 0 {
		task = task[:i]
	}
	if len(task) > 120 {
		task = task[:120] + "…"
	}
	priority := 2
	if what == "blocked" {
		priority = 1
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<fleet-agent name=%q node=%q kind=%q state=%q>\n", a.Name, a.Node, a.Kind, herdrState)
	fmt.Fprintf(&b, "task: %s\n", task)
	if tail != "" {
		fmt.Fprintf(&b, "\nlast output:\n%s\n", tidy(tail))
	}
	fmt.Fprintf(&b, "</fleet-agent>\n(imp agent read|prompt|keys|close %s)", a.Name)
	err := s.enqueue(scheduler.Enqueue{
		ID: fmt.Sprintf("fleet-%s-%d", a.Name, seq), Target: "instance:" + a.Origin, Origin: a.Origin, Source: "fleet",
		Priority: &priority, Type: "notify", Summary: fmt.Sprintf("agent %s on %s %s: %s", a.Name, a.Node, verb, task), Body: b.String(),
	})
	if err != nil {
		log.Printf("fleet: delivering %s %s: %v", a.Name, what, err)
	}
}

func readText(res json.RawMessage) string {
	var r struct {
		Text    string `json:"text"`
		Content string `json:"content"`
		Output  string `json:"output"`
	}
	if json.Unmarshal(res, &r) == nil {
		for _, t := range []string{r.Text, r.Content, r.Output} {
			if t != "" {
				return t
			}
		}
	}
	var s string
	if json.Unmarshal(res, &s) == nil {
		return s
	}
	return string(res)
}

func (s *Service) open(name string) (Agent, error) {
	a, err := s.Get(name)
	if err != nil {
		return a, err
	}
	if a.ClosedAt != 0 {
		return a, fmt.Errorf("agent %s is closed", name)
	}
	return a, nil
}

func (s *Service) Read(ctx context.Context, name string, lines int) (string, error) {
	a, err := s.Get(name)
	if err != nil {
		return "", err
	}
	if lines <= 0 || lines > 2000 {
		lines = 80
	}
	res, err := s.herdr.Run(ctx, a.Node, "agent", "read", name, "--source", "recent-unwrapped", "--lines", fmt.Sprint(lines))
	if err != nil {
		return "", err
	}
	return readText(res), nil
}

func (s *Service) Prompt(ctx context.Context, name, text string) error {
	a, err := s.open(name)
	if err != nil {
		return err
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("text is required")
	}
	_, err = s.herdr.Run(ctx, a.Node, "agent", "prompt", name, text)
	return err
}

func (s *Service) Keys(ctx context.Context, name string, keys []string) error {
	a, err := s.open(name)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return errors.New("keys are required")
	}
	_, err = s.herdr.Run(ctx, a.Node, append([]string{"agent", "send-keys", name}, keys...)...)
	return err
}

// Close ends the agent's workspace and stops watching it. Closing is the one
// deliberate end; an agent that exits on its own is reported as exited.
func (s *Service) CloseAgent(ctx context.Context, name string) error {
	a, err := s.Get(name)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if cancel := s.watchers[name]; cancel != nil {
		cancel()
	}
	s.mu.Unlock()
	s.markClosed(name)
	if _, err = s.herdr.Run(ctx, a.Node, "workspace", "close", a.Workspace); err != nil && code(err) != "workspace_not_found" {
		return err
	}
	return nil
}

// Handle serves fleet.* socket operations. origin is the caller's instance.
func (s *Service) Handle(ctx context.Context, op string, args map[string]any, origin string) (any, error) {
	str := func(k string) string { v, _ := args[k].(string); return v }
	switch op {
	case "fleet.start":
		var in StartRequest
		b, _ := json.Marshal(args)
		if err := json.Unmarshal(b, &in); err != nil {
			return nil, err
		}
		if in.Origin == "" {
			in.Origin = origin
		}
		return s.Start(ctx, in)
	case "fleet.list":
		all, _ := args["all"].(bool)
		o := str("origin")
		if o == "" && !all {
			o = origin
		}
		closed, _ := args["closed"].(bool)
		return s.List(o, closed)
	case "fleet.read":
		lines := 0
		if n, ok := args["lines"].(json.Number); ok {
			v, _ := n.Int64()
			lines = int(v)
		} else if f, ok := args["lines"].(float64); ok {
			lines = int(f)
		}
		text, err := s.Read(ctx, str("name"), lines)
		return map[string]any{"text": text}, err
	case "fleet.prompt":
		return map[string]any{"sent": true}, s.Prompt(ctx, str("name"), str("text"))
	case "fleet.keys":
		raw, _ := args["keys"].([]any)
		keys := make([]string, 0, len(raw))
		for _, k := range raw {
			if v, ok := k.(string); ok {
				keys = append(keys, v)
			}
		}
		return map[string]any{"sent": true}, s.Keys(ctx, str("name"), keys)
	case "fleet.close":
		return map[string]any{"closed": true}, s.CloseAgent(ctx, str("name"))
	}
	return nil, errors.New("unknown fleet operation")
}

// tidy trims a terminal tail for a wake: trailing spaces off, rule lines and
// runs of blank lines collapsed, so the part worth reading fits.
func tidy(tail string) string {
	var out []string
	blank := false
	for _, line := range strings.Split(tail, "\n") {
		line = strings.TrimRight(line, " \t\r")
		if strings.Trim(line, "─━-=_ ") == "" {
			if !blank && len(out) > 0 {
				out = append(out, "")
			}
			blank = true
			continue
		}
		blank = false
		out = append(out, line)
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}
