package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"

	"github.com/gisikw/familiar-services/internal/attention"
	"github.com/gisikw/familiar-services/internal/wakes"
	"github.com/gisikw/familiar-services/internal/worklist"
)

func TestUnixNDJSONProtocol(t *testing.T) {
	root := t.TempDir()
	attn, e := attention.Open(filepath.Join(root, "attention.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	defer attn.Close()
	work, e := worklist.Open(root)
	if e != nil {
		t.Fatal(e)
	}
	wake, e := wakes.Open(root, work)
	if e != nil {
		t.Fatal(e)
	}
	defer wake.Close()
	socket := filepath.Join(root, "service.sock")
	server := New(socket, Services{attn, work, wake})
	if e = server.Listen(); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	conn, e := net.Dial("unix", socket)
	if e != nil {
		t.Fatal(e)
	}
	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(bufio.NewReader(conn))
	requests := []map[string]any{{"op": "attn.project.add", "args": map[string]any{"slug": "socket-test"}}, {"op": "worklist.enqueue", "args": map[string]any{"id": "socket-item", "summary": "hello"}}, {"op": "dnd.get", "args": map[string]any{}}}
	for _, request := range requests {
		if e = enc.Encode(request); e != nil {
			t.Fatal(e)
		}
		var response map[string]any
		if e = dec.Decode(&response); e != nil {
			t.Fatal(e)
		}
		if response["ok"] != true {
			t.Fatalf("response: %#v", response)
		}
		if _, present := response["result"]; !present {
			t.Fatalf("successful response omitted result: %#v", response)
		}
	}
	_ = conn.Close()
	cancel()
	if e = <-done; e != nil {
		t.Fatal(e)
	}
}
