// Package api serves the local newline-delimited JSON protocol and scheduler stream.
package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gisikw/familiar-services/internal/attention"
	"github.com/gisikw/familiar-services/internal/push"
	"github.com/gisikw/familiar-services/internal/scheduler"
)

type Services struct {
	Attention     *attention.Store
	Scheduler     *scheduler.Store
	Push          *push.Service
	DefaultTarget string
}
type request struct {
	Op   string         `json:"op"`
	Args map[string]any `json:"args"`
}
type response struct {
	OK     bool       `json:"ok"`
	Result any        `json:"result"`
	Error  *wireError `json:"error,omitempty"`
}
type wireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type client struct {
	net.Conn
	target string
	enc    *json.Encoder
	mu     sync.Mutex
}

func (c *client) send(v any) error { c.mu.Lock(); defer c.mu.Unlock(); return c.enc.Encode(v) }

type Server struct {
	socket       string
	services     Services
	ln           net.Listener
	wg           sync.WaitGroup
	mu           sync.Mutex
	clients      map[string]*client
	lastDelivery map[string]time.Time
	latest       string
	wake         chan struct{}
}

func New(socket string, services Services) *Server {
	return &Server{socket: socket, services: services, clients: map[string]*client{}, lastDelivery: map[string]time.Time{}, wake: make(chan struct{}, 1)}
}
func (s *Server) Listen() error {
	if s.socket == "" {
		return errors.New("socket path is required")
	}
	if err := os.MkdirAll(filepath.Dir(s.socket), 0700); err != nil {
		return err
	}
	if st, err := os.Lstat(s.socket); err == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("refusing to replace non-socket %s", s.socket)
		}
		if err = os.Remove(s.socket); err != nil {
			return err
		}
	}
	ln, err := net.Listen("unix", s.socket)
	if err != nil {
		return err
	}
	_ = os.Chmod(s.socket, 0600)
	s.ln = ln
	return nil
}
func (s *Server) Serve(ctx context.Context) error {
	if s.ln == nil {
		if err := s.Listen(); err != nil {
			return err
		}
	}
	go s.deliveryLoop(ctx)
	go func() {
		<-ctx.Done()
		_ = s.ln.Close()
		s.mu.Lock()
		for _, c := range s.clients {
			_ = c.Close()
		}
		s.mu.Unlock()
	}()
	defer os.Remove(s.socket)
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				s.wg.Wait()
				return nil
			}
			return err
		}
		s.wg.Add(1)
		go func() { defer s.wg.Done(); s.connection(conn) }()
	}
}
func (s *Server) connection(conn net.Conn) {
	c := &client{Conn: conn, enc: json.NewEncoder(conn)}
	defer func() { s.unregister(c); conn.Close() }()
	scan := bufio.NewScanner(conn)
	scan.Buffer(make([]byte, 4096), 1024*1024)
	for scan.Scan() {
		var r request
		dec := json.NewDecoder(strings.NewReader(scan.Text()))
		dec.UseNumber()
		if err := dec.Decode(&r); err != nil {
			_ = c.send(failure("invalid_request", err.Error()))
			continue
		}
		if r.Args == nil {
			r.Args = map[string]any{}
		}
		if r.Op == "hello" {
			instance, _ := r.Args["instance"].(string)
			if !scheduler.ValidTarget("instance:" + instance) {
				_ = c.send(failure("invalid_request", "valid instance is required"))
				continue
			}
			s.register(c, "instance:"+instance)
			_ = s.services.Scheduler.Requeue(c.target)
			_ = c.send(response{OK: true, Result: map[string]any{"target": c.target}})
			s.signal()
			continue
		}
		v, err := s.dispatch(r, c.target)
		if err != nil {
			code := "unavailable"
			var attentionError *attention.CodedError
			if errors.As(err, &attentionError) {
				code = attentionError.Code
			}
			var pushError *push.CodedError
			if errors.As(err, &pushError) {
				code = pushError.Code
			}
			if errors.Is(err, os.ErrNotExist) {
				code = "not_found"
			}
			_ = c.send(failure(code, err.Error()))
			continue
		}
		_ = c.send(response{OK: true, Result: v})
		if r.Op == "schedule.enqueue" || r.Op == "notify" || r.Op == "schedule.ack" || r.Op == "dnd.set" {
			s.signal()
		}
	}
}
func (s *Server) register(c *client, target string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old := s.clients[target]; old != nil && old != c {
		_ = old.Close()
	}
	c.target = target
	s.clients[target] = c
	delete(s.lastDelivery, target) // reconnect redelivery is immediate
	s.latest = target
}
func (s *Server) unregister(c *client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clients[c.target] == c {
		delete(s.clients, c.target)
	}
}
func (s *Server) defaultTarget() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if x := scheduler.NormalizeTarget(s.services.DefaultTarget); x != "" {
		return x
	}
	return s.latest
}
func (s *Server) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *Server) deliveryLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.wake:
		}
		s.pump()
	}
}
func (s *Server) pump() {
	s.mu.Lock()
	clients := make([]*client, 0, len(s.clients))
	for _, c := range s.clients {
		if time.Since(s.lastDelivery[c.target]) >= 15*time.Second {
			clients = append(clients, c)
		}
	}
	s.mu.Unlock()
	for _, c := range clients {
		e, err := s.services.Scheduler.Claim(c.target, time.Now().UnixMilli())
		if err != nil {
			log.Printf("scheduler delivery: %v", err)
			continue
		}
		if e != nil {
			s.mu.Lock()
			s.lastDelivery[c.target] = time.Now()
			s.mu.Unlock()
			if c.send(map[string]any{"event": e}) != nil {
				_ = c.Close()
			}
		}
	}
}
func failure(code, msg string) response { return response{OK: false, Error: &wireError{code, msg}} }

func (s *Server) dispatch(r request, connectedTarget string) (any, error) {
	op := strings.TrimPrefix(r.Op, "attn.")
	if isAttention(op) {
		return s.services.Attention.Handle(op, r.Args)
	}
	if r.Op == "push.register" || r.Op == "push.send" {
		if s.services.Push == nil {
			return nil, &push.CodedError{Code: "unavailable", Err: errors.New("APNs is unavailable")}
		}
		return s.services.Push.Handle(context.Background(), r.Op, r.Args)
	}
	switch r.Op {
	case "schedule.enqueue", "notify":
		var in scheduler.Enqueue
		if err := decode(r.Args, &in); err != nil {
			return nil, err
		}
		if in.Target == "" {
			if in.Origin != "" {
				in.Target = "instance:" + in.Origin
			} else if connectedTarget != "" {
				in.Target = connectedTarget
			} else {
				in.Target = s.defaultTarget()
			}
		}
		e, created, err := s.services.Scheduler.Enqueue(in)
		if err == nil && strings.HasPrefix(e.Target, "spawn:") {
			log.Printf("spawn targets land in M3: %s", e.Target)
		}
		return map[string]any{"event": e, "created": created}, err
	case "schedule.list":
		target, _ := r.Args["target"].(string)
		if all, _ := r.Args["all"].(bool); all {
			target = "*"
		}
		if target == "" {
			origin, _ := r.Args["origin"].(string)
			if origin != "" {
				target = "instance:" + origin
			} else if connectedTarget != "" {
				target = connectedTarget
			} else {
				target = s.defaultTarget()
			}
		}
		return s.services.Scheduler.List(target)
	case "schedule.cancel":
		id, _ := r.Args["id"].(string)
		if id == "" {
			return nil, errors.New("id is required")
		}
		ok, err := s.services.Scheduler.Cancel(id)
		return map[string]any{"cancelled": ok}, err
	case "schedule.ack":
		id, _ := r.Args["id"].(string)
		if id == "" || connectedTarget == "" {
			return nil, errors.New("ack requires hello and id")
		}
		ok, err := s.services.Scheduler.Ack(id, connectedTarget)
		if err == nil && !ok {
			return nil, os.ErrNotExist
		}
		return map[string]any{"acked": ok}, err
	case "dnd.get":
		target := requestTarget(r.Args, connectedTarget, s.defaultTarget())
		return s.services.Scheduler.DNDGet(target)
	case "dnd.set":
		target := requestTarget(r.Args, connectedTarget, s.defaultTarget())
		enabled := true
		if x, ok := r.Args["enabled"].(bool); ok {
			enabled = x
		}
		setBy, _ := r.Args["set_by"].(string)
		var duration *int64
		if x, ok := number(r.Args["duration_ms"]); ok {
			duration = &x
		}
		return s.services.Scheduler.DNDSet(target, enabled, setBy, duration)
	default:
		return nil, &attention.CodedError{Code: "invalid_request", Err: errors.New("unknown operation")}
	}
}
func requestTarget(args map[string]any, connected, fallback string) string {
	if x, _ := args["target"].(string); x != "" {
		return x
	}
	if x, _ := args["origin"].(string); x != "" {
		return "instance:" + x
	}
	if connected != "" {
		return connected
	}
	return fallback
}
func decode(v, out any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
func number(v any) (int64, bool) {
	switch x := v.(type) {
	case json.Number:
		n, e := x.Int64()
		return n, e == nil
	case float64:
		return int64(x), x == float64(int64(x))
	case int64:
		return x, true
	case int:
		return int64(x), true
	}
	return 0, false
}
func isAttention(op string) bool {
	switch op {
	case "project.list", "project.get", "project.add", "project.set", "card.list", "card.get", "card.add", "card.set", "card.move", "card.block", "card.unblock", "card.done", "note.add", "evidence.add", "agent.start", "agent.set", "jot.add", "jot.list", "jot.clear-done", "status":
		return true
	}
	return false
}
