package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/gisikw/familiar-services/internal/attention"
	"github.com/gisikw/familiar-services/internal/scheduler"
)

func testServer(t *testing.T) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	root := t.TempDir()
	attn, e := attention.Open(filepath.Join(root, "attention.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	sched, e := scheduler.Open(root)
	if e != nil {
		t.Fatal(e)
	}
	socket := filepath.Join(root, "service.sock")
	server := New(socket, Services{Attention: attn, Scheduler: sched})
	if e = server.Listen(); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx); attn.Close(); sched.Close() }()
	return socket, cancel, done
}
func TestUnixProtocolAndPushAck(t *testing.T) {
	socket, cancel, done := testServer(t)
	conn, e := net.Dial("unix", socket)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(bufio.NewReader(conn))
	call := func(op string, args map[string]any) map[string]any {
		t.Helper()
		if e := enc.Encode(map[string]any{"op": op, "args": args}); e != nil {
			t.Fatal(e)
		}
		var r map[string]any
		if e := dec.Decode(&r); e != nil {
			t.Fatal(e)
		}
		return r
	}
	if r := call("hello", map[string]any{"instance": "session-a"}); r["ok"] != true {
		t.Fatalf("hello: %#v", r)
	}
	if r := call("schedule.enqueue", map[string]any{"id": "socket-item", "origin": "session-a", "summary": "hello"}); r["ok"] != true {
		t.Fatalf("enqueue: %#v", r)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var pushed map[string]any
	if e = dec.Decode(&pushed); e != nil {
		t.Fatal(e)
	}
	event, ok := pushed["event"].(map[string]any)
	if !ok || event["id"] != "socket-item" {
		t.Fatalf("push: %#v", pushed)
	}
	if r := call("schedule.ack", map[string]any{"id": "socket-item"}); r["ok"] != true {
		t.Fatalf("ack: %#v", r)
	}
	conn.Close()
	cancel()
	if e = <-done; e != nil {
		t.Fatal(e)
	}
}
func TestDefaultRoutesToMostRecentHello(t *testing.T) {
	socket, cancel, done := testServer(t)
	receiver, e := net.Dial("unix", socket)
	if e != nil {
		t.Fatal(e)
	}
	defer receiver.Close()
	enc := json.NewEncoder(receiver)
	dec := json.NewDecoder(bufio.NewReader(receiver))
	enc.Encode(map[string]any{"op": "hello", "args": map[string]any{"instance": "latest"}})
	var r map[string]any
	dec.Decode(&r)
	sender, _ := net.Dial("unix", socket)
	json.NewEncoder(sender).Encode(map[string]any{"op": "schedule.enqueue", "args": map[string]any{"summary": "unaddressed"}})
	json.NewDecoder(bufio.NewReader(sender)).Decode(&r)
	sender.Close()
	receiver.SetReadDeadline(time.Now().Add(2 * time.Second))
	if e = dec.Decode(&r); e != nil {
		t.Fatal(e)
	}
	if r["event"].(map[string]any)["target"] != "instance:latest" {
		t.Fatalf("route: %#v", r)
	}
	receiver.Close()
	cancel()
	<-done
}
