// Package api serves the local newline-delimited JSON protocol.
package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gisikw/familiar-services/internal/attention"
	"github.com/gisikw/familiar-services/internal/wakes"
	"github.com/gisikw/familiar-services/internal/worklist"
)

type Services struct {
	Attention *attention.Store
	Worklist  *worklist.Store
	Wakes     *wakes.Store
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
type Server struct {
	socket   string
	services Services
	ln       net.Listener
	wg       sync.WaitGroup
}

func New(socket string, s Services) *Server { return &Server{socket: socket, services: s} }
func (s *Server) Listen() error {
	if s.socket == "" {
		return errors.New("socket path is required")
	}
	if e := os.MkdirAll(filepath.Dir(s.socket), 0700); e != nil {
		return e
	}
	if st, e := os.Lstat(s.socket); e == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("refusing to replace non-socket %s", s.socket)
		}
		if e = os.Remove(s.socket); e != nil {
			return e
		}
	}
	ln, e := net.Listen("unix", s.socket)
	if e != nil {
		return e
	}
	_ = os.Chmod(s.socket, 0600)
	s.ln = ln
	return nil
}
func (s *Server) Serve(ctx context.Context) error {
	if s.ln == nil {
		if e := s.Listen(); e != nil {
			return e
		}
	}
	go func() { <-ctx.Done(); _ = s.ln.Close() }()
	defer os.Remove(s.socket)
	for {
		c, e := s.ln.Accept()
		if e != nil {
			if ctx.Err() != nil {
				s.wg.Wait()
				return nil
			}
			return e
		}
		s.wg.Add(1)
		go func() { defer s.wg.Done(); defer c.Close(); s.connection(c) }()
	}
}
func (s *Server) connection(c net.Conn) {
	scan := bufio.NewScanner(c)
	scan.Buffer(make([]byte, 4096), 1024*1024)
	enc := json.NewEncoder(c)
	for scan.Scan() {
		var r request
		dec := json.NewDecoder(strings.NewReader(scan.Text()))
		dec.UseNumber()
		if e := dec.Decode(&r); e != nil {
			_ = enc.Encode(failure("invalid_request", e.Error()))
			continue
		}
		if r.Args == nil {
			r.Args = map[string]any{}
		}
		v, e := s.dispatch(r)
		if e != nil {
			code := "unavailable"
			var ce *attention.CodedError
			if errors.As(e, &ce) {
				code = ce.Code
			}
			if errors.Is(e, os.ErrNotExist) {
				code = "not_found"
			}
			_ = enc.Encode(failure(code, e.Error()))
			continue
		}
		_ = enc.Encode(response{OK: true, Result: v})
	}
}
func failure(code, msg string) response { return response{OK: false, Error: &wireError{code, msg}} }
func (s *Server) dispatch(r request) (any, error) {
	op := strings.TrimPrefix(r.Op, "attn.")
	if isAttention(op) {
		return s.services.Attention.Handle(op, r.Args)
	}
	switch r.Op {
	case "worklist.list":
		return s.services.Worklist.List()
	case "worklist.enqueue":
		var in worklist.Enqueue
		if e := decode(r.Args, &in); e != nil {
			return nil, e
		}
		x, created, e := s.services.Worklist.Enqueue(in)
		if e == nil {
			s.services.Wakes.FreshInput(time.Now().UnixMilli())
		}
		return map[string]any{"item": x, "created": created}, e
	case "worklist.ack":
		id, _ := r.Args["id"].(string)
		if id == "" {
			return nil, errors.New("id is required")
		}
		return s.services.Worklist.Ack(id)
	case "worklist.withdraw":
		id, _ := r.Args["id"].(string)
		if id == "" {
			return nil, errors.New("id is required")
		}
		return s.services.Worklist.Withdraw(id)
	case "dnd.get":
		return s.services.Worklist.DNDGet()
	case "dnd.set":
		enabled := true
		if x, ok := r.Args["enabled"].(bool); ok {
			enabled = x
		}
		setBy, _ := r.Args["set_by"].(string)
		if setBy == "" {
			setBy, _ = r.Args["setBy"].(string)
		}
		var d *int64
		if x, ok := number(r.Args["duration_ms"]); ok {
			d = &x
		}
		return s.services.Worklist.DNDSet(enabled, setBy, d)
	case "wake.schedule", "wakes.schedule":
		mode, _ := r.Args["mode"].(string)
		reason, _ := r.Args["reason"].(string)
		fire, ok := number(r.Args["fire_at"])
		if !ok {
			mins, mok := float(r.Args["duration_minutes"])
			if !mok {
				return nil, errors.New("duration_minutes or fire_at is required")
			}
			fire = time.Now().UnixMilli() + int64(mins*60000)
		}
		return s.services.Wakes.Schedule(mode, reason, fire)
	case "wake.cancel", "wakes.cancel":
		id, _ := r.Args["id"].(string)
		ok, e := s.services.Wakes.Cancel(id)
		return map[string]any{"cancelled": ok}, e
	case "wake.list", "wakes.list":
		return s.services.Wakes.List()
	default:
		return nil, &attention.CodedError{Code: "invalid_request", Err: errors.New("unknown operation")}
	}
}
func decode(v any, out any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
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
func float(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		n, e := x.Float64()
		return n, e == nil
	case float64:
		return x, true
	case int:
		return float64(x), true
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
