package attention

import (
	"path/filepath"
	"testing"
)

func TestImpOperationsAndExistingSchema(t *testing.T) {
	s, e := Open(filepath.Join(t.TempDir(), "attention.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	p, e := s.Handle("project.add", map[string]any{"slug": "service-test", "label": "Service test"})
	if e != nil {
		t.Fatal(e)
	}
	if p.(map[string]any)["slug"] != "service-test" {
		t.Fatalf("project: %#v", p)
	}
	c, e := s.Handle("card.add", map[string]any{"project": "service-test", "title": "Port singleton", "summary": "same semantics", "owner": "kes"})
	if e != nil {
		t.Fatal(e)
	}
	id := c.(map[string]any)["id"].(string)
	for _, tc := range []struct {
		op   string
		args map[string]any
	}{{"card.move", map[string]any{"id": id, "lane": "inflight"}}, {"note.add", map[string]any{"card": id, "text": "working"}}, {"evidence.add", map[string]any{"card": id, "kind": "commit", "title": "implementation", "ref": "abc"}}, {"agent.start", map[string]any{"card": id, "name": "worker"}}, {"agent.set", map[string]any{"card": id, "name": "worker", "state": "done"}}} {
		if _, e = s.Handle(tc.op, tc.args); e != nil {
			t.Fatalf("%s: %v", tc.op, e)
		}
	}
	full, e := s.Handle("card.get", map[string]any{"id": id})
	if e != nil {
		t.Fatal(e)
	}
	m := full.(map[string]any)
	if m["lane"] != "inflight" || len(m["notes"].([]any)) != 1 || len(m["evidence"].([]any)) != 1 || len(m["agent_list"].([]any)) != 1 {
		t.Fatalf("full card: %#v", m)
	}
}
func TestJotDoneAndClear(t *testing.T) {
	s, e := Open(filepath.Join(t.TempDir(), "a.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	j, e := s.Handle("jot.add", map[string]any{"title": "today"})
	if e != nil {
		t.Fatal(e)
	}
	id := j.(map[string]any)["id"].(string)
	if _, e = s.Handle("card.done", map[string]any{"id": id}); e != nil {
		t.Fatal(e)
	}
	x, e := s.Handle("jot.clear-done", map[string]any{})
	if e != nil || x.(map[string]any)["archived"] != 1 {
		t.Fatalf("clear: %#v %v", x, e)
	}
}
