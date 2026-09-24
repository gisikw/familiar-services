package wakes

import (
	"github.com/gisikw/familiar-services/internal/worklist"
	"testing"
	"time"
)

func TestScheduleCancelAndFireIntoWorklist(t *testing.T) {
	root := t.TempDir()
	work, e := worklist.Open(root)
	if e != nil {
		t.Fatal(e)
	}
	s, e := Open(root, work)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	r, e := s.Schedule("always", "check deployment", time.Now().Add(time.Hour).UnixMilli())
	if e != nil {
		t.Fatal(e)
	}
	list, e := s.List()
	if e != nil || len(list) != 1 || list[0].ID != r.ID {
		t.Fatalf("list: %#v %v", list, e)
	}
	ok, e := s.Cancel(r.ID)
	if e != nil || !ok {
		t.Fatalf("cancel: %v %v", ok, e)
	}
	r, e = s.Schedule("always", "deliver me", time.Now().Add(time.Hour).UnixMilli())
	if e != nil {
		t.Fatal(e)
	}
	if timer := s.timers[r.ID]; timer != nil {
		timer.Stop()
	}
	s.fire(r)
	items, e := work.List()
	if e != nil || len(items) != 1 || items[0].ID != r.ID || items[0].Source != "wake" {
		t.Fatalf("fired worklist: %#v %v", items, e)
	}
}
