package continuity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBranchReconciliationIsOrderIndependent(t *testing.T) {
	root := t.TempDir()
	sessions := filepath.Join(root, "sessions")
	handoffs := filepath.Join(root, "handoffs")
	os.MkdirAll(sessions, 0755)
	os.MkdirAll(handoffs, 0755)
	parent := `{"type":"session","version":3,"id":"parent","timestamp":"2026-01-01T00:00:00Z","cwd":"/tmp"}
{"type":"message","id":"branch01","parentId":null,"timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","content":"base"}}
{"type":"message","id":"merge001","parentId":"branch01","timestamp":"2026-01-01T00:00:05Z","message":{"role":"custom","customType":"familiar.merge.v1","content":"merged","display":true,"details":{"forkSessionId":"fork","forkSessionFile":"/tmp/fork.jsonl","branchEntryId":"branch01","firstEntryId":"first001","lastEntryId":"last0001","turnCount":2,"forkedFurther":false}}}
`
	fork := `{"type":"session","version":3,"id":"fork","timestamp":"2026-01-01T00:00:02Z","cwd":"/tmp","parentSession":"` + filepath.Join(sessions, "parent.jsonl") + `"}
{"type":"message","id":"branch01","parentId":null,"timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","content":"base"}}
{"type":"custom","id":"marker01","parentId":"branch01","timestamp":"2026-01-01T00:00:02Z","customType":"familiar.fork.v1","data":{"parentSessionId":"parent","branchEntryId":"branch01"}}
{"type":"message","id":"first001","parentId":"marker01","timestamp":"2026-01-01T00:00:03Z","message":{"role":"user","content":"task"}}
{"type":"message","id":"last0001","parentId":"first001","timestamp":"2026-01-01T00:00:04Z","message":{"role":"assistant","content":"done"}}
{"type":"custom","id":"close001","parentId":"last0001","timestamp":"2026-01-01T00:00:05Z","customType":"familiar.branch-close.v1","data":{"reason":"closed"}}
`
	// Parent sorts/imports before fork: the first reconciliation leaves its merge
	// pending, then a second no-op pass resolves it without guessing.
	os.WriteFile(filepath.Join(sessions, "parent.jsonl"), []byte(parent), 0644)
	dbp := filepath.Join(root, "db")
	opts := ImportOptions{SessionsDir: sessions, HandoffsDir: handoffs, DBPath: dbp}
	if err := Import(opts); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(sessions, "fork.jsonl"), []byte(fork), 0644)
	if err := Import(opts); err != nil {
		t.Fatal(err)
	}
	db, _ := Open(dbp)
	defer db.Close()
	if got := count(t, db, `SELECT count(*) FROM edges WHERE child_id='pi:fork:marker01' AND parent_id='pi:parent:branch01' AND edge_type='fork'`); got != 1 {
		t.Fatalf("fork edges=%d", got)
	}
	if got := count(t, db, `SELECT count(*) FROM edges WHERE child_id='pi:parent:merge001' AND parent_id='pi:fork:last0001' AND edge_type='merge'`); got != 1 {
		t.Fatalf("merge edges=%d", got)
	}
	if got := count(t, db, `SELECT count(*) FROM parts WHERE turn_id='pi:parent:merge001' AND from_turn='pi:fork:last0001'`); got != 1 {
		t.Fatalf("from_turn=%d", got)
	}
	if got := count(t, db, `SELECT count(*) FROM turns WHERE id='pi:fork:close001' AND kind='branch_close'`); got != 1 {
		t.Fatalf("close=%d", got)
	}
}
