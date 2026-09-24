package worklist

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExistingFormatAndOperations(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "worklist", "items")
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	raw := `{"id":"existing","ts":10,"priority":1,"type":"question","summary":"s","body":"b","source":"test","surfacedCount":0}`
	if e := os.WriteFile(filepath.Join(dir, "existing.json"), []byte(raw), 0600); e != nil {
		t.Fatal(e)
	}
	s, e := Open(root)
	if e != nil {
		t.Fatal(e)
	}
	items, e := s.List()
	if e != nil || len(items) != 1 || items[0].ID != "existing" {
		t.Fatalf("list: %#v %v", items, e)
	}
	x, e := s.Ack("existing")
	if e != nil || !x.Acked {
		t.Fatalf("ack: %#v %v", x, e)
	}
	if _, e = os.Stat(filepath.Join(dir, "archive", "existing.json")); e != nil {
		t.Fatal(e)
	}
}
func TestDNDMatchesFileFormatAndFamiliarCap(t *testing.T) {
	s, e := Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now().UnixMilli()
	d := DND{true, "familiar", now, now + 24*60*60*1000}
	if e = atomicJSON(filepath.Join(s.root, "dnd.json"), d); e != nil {
		t.Fatal(e)
	}
	got, e := s.DNDGet()
	if e != nil {
		t.Fatal(e)
	}
	if got == nil || got.ExpiresAt > now+2*60*60*1000+1000 {
		t.Fatalf("uncapped DND: %#v", got)
	}
	b, _ := os.ReadFile(filepath.Join(s.root, "dnd.json"))
	var persisted DND
	if json.Unmarshal(b, &persisted) != nil || persisted.SetBy != "familiar" {
		t.Fatalf("bad persisted DND: %s", b)
	}
}
func TestEnqueueIdempotentAndWithdraw(t *testing.T) {
	s, _ := Open(t.TempDir())
	p := 1
	x, created, e := s.Enqueue(Enqueue{ID: "stable", Priority: &p, Summary: "hello"})
	if e != nil || !created {
		t.Fatal(e)
	}
	y, created, e := s.Enqueue(Enqueue{ID: "stable", Summary: "different"})
	if e != nil || created || y.Summary != x.Summary {
		t.Fatalf("dedupe failed: %#v %v", y, e)
	}
	if _, e = s.Withdraw("stable"); e != nil {
		t.Fatal(e)
	}
	items, _ := s.List()
	if len(items) != 0 {
		t.Fatalf("withdraw remained live: %#v", items)
	}
}
