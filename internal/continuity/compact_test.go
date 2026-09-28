package continuity

import (
	"database/sql"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

type compactFixture struct {
	root, sessions, forks, handoffs, forkPath string
}

func newCompactFixture(t *testing.T, merged bool) compactFixture {
	t.Helper()
	root := t.TempDir()
	f := compactFixture{
		root:     root,
		sessions: filepath.Join(root, "sessions"),
		forks:    filepath.Join(root, "forks"),
		handoffs: filepath.Join(root, "handoffs"),
	}
	forkDir := filepath.Join(f.forks, "fork", "sessions")
	for _, d := range []string{f.sessions, forkDir, f.handoffs} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	parentPath := filepath.Join(f.sessions, "parent.jsonl")
	parent, fork := syntheticBranchFiles(parentPath)
	if !merged {
		parent = strings.Join(strings.SplitAfter(parent, "\n")[:2], "")
	}
	f.forkPath = filepath.Join(forkDir, "fork.jsonl")
	if err := os.WriteFile(parentPath, []byte(parent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.forkPath, []byte(fork), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f compactFixture) importTo(t *testing.T, db string) {
	t.Helper()
	if err := Import(ImportOptions{SessionsDirs: []string{f.sessions, f.forks}, HandoffsDir: f.handoffs, DBPath: db}); err != nil {
		t.Fatal(err)
	}
}

type forkSnapshot struct {
	turns        []string
	fork, merge  int
	importErrors int
}

func snapshot(t *testing.T, path string) forkSnapshot {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id FROM turns WHERE session_id='pi:fork' AND id<>'pi:fork:system' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var s forkSnapshot
	for rows.Next() {
		var id string
		rows.Scan(&id)
		s.turns = append(s.turns, id)
	}
	rows.Close()
	sort.Strings(s.turns)
	s.fork = count(t, db, `SELECT count(*) FROM edges WHERE child_id='pi:fork:marker01' AND parent_id='pi:parent:branch01' AND edge_type='fork'`)
	s.merge = count(t, db, `SELECT count(*) FROM edges WHERE child_id='pi:parent:merge001' AND parent_id='pi:fork:last0001' AND edge_type='merge'`)
	s.importErrors = count(t, db, `SELECT count(*) FROM import_errors`)
	return s
}

func later() time.Time { return time.Now().Add(time.Hour) }

func TestCompactForksDropsPrefixLosslessly(t *testing.T) {
	f := newCompactFixture(t, true)
	dbPath := filepath.Join(f.root, "a.db")
	f.importTo(t, dbPath)
	before := snapshot(t, dbPath)
	if len(before.turns) != 4 || before.fork != 1 || before.merge != 1 {
		t.Fatalf("unexpected baseline: %+v", before)
	}
	origSize := fileSize(t, f.forkPath)

	res, err := CompactForks(CompactOptions{ForksDir: f.forks, DBPath: dbPath, MinAge: time.Minute, Now: later})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Compacted) != 1 || res.BytesReclaimed <= 0 {
		t.Fatalf("result: %+v", res)
	}
	data, _ := os.ReadFile(f.forkPath)
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 5 || !strings.Contains(lines[0], `"type":"session"`) || !strings.Contains(lines[1], "familiar.fork.v1") {
		t.Fatalf("compacted file:\n%s", data)
	}
	if got := origSize - fileSize(t, f.forkPath); got != res.BytesReclaimed {
		t.Fatalf("reclaimed %d, reported %d", got, res.BytesReclaimed)
	}
	if info, _ := os.Stat(f.forkPath); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode not preserved: %v", info.Mode())
	}

	// The live index treats the rewritten file as unchanged: nothing is lost.
	f.importTo(t, dbPath)
	if after := snapshot(t, dbPath); !equalSnap(before, after) {
		t.Fatalf("live index changed:\nbefore %+v\nafter  %+v", before, after)
	}
	// A rebuild from the compacted sources derives the same fork.
	fresh := filepath.Join(f.root, "fresh.db")
	f.importTo(t, fresh)
	if got := snapshot(t, fresh); !equalSnap(before, got) {
		t.Fatalf("rebuilt index differs:\nbefore %+v\nfresh  %+v", before, got)
	}
	// Idempotent.
	res, err = CompactForks(CompactOptions{ForksDir: f.forks, DBPath: dbPath, MinAge: time.Minute, Now: later})
	if err != nil || len(res.Compacted) != 0 || res.Skipped[f.forkPath] != "already compact" {
		t.Fatalf("second pass: %+v %v", res, err)
	}
}

func TestCompactForksRefusesUnmergedYoungAndStale(t *testing.T) {
	f := newCompactFixture(t, false)
	dbPath := filepath.Join(f.root, "a.db")
	f.importTo(t, dbPath)
	orig, _ := os.ReadFile(f.forkPath)

	res, err := CompactForks(CompactOptions{ForksDir: f.forks, DBPath: dbPath, MinAge: time.Minute, Now: later})
	if err != nil || len(res.Compacted) != 0 || !strings.HasPrefix(res.Skipped[f.forkPath], "no merge edge") {
		t.Fatalf("unmerged: %+v %v", res, err)
	}

	m := newCompactFixture(t, true)
	mdb := filepath.Join(m.root, "a.db")
	m.importTo(t, mdb)
	res, _ = CompactForks(CompactOptions{ForksDir: m.forks, DBPath: mdb, MinAge: 24 * time.Hour})
	if res.Skipped[m.forkPath] != "younger than min age" {
		t.Fatalf("young: %+v", res)
	}
	// Appended after the last import: the index is behind, so leave it alone.
	fh, _ := os.OpenFile(m.forkPath, os.O_APPEND|os.O_WRONLY, 0)
	fh.WriteString(`{"type":"message","id":"late0001","parentId":"close001","timestamp":"2026-01-01T00:00:06Z","message":{"role":"assistant","content":"late"}}` + "\n")
	fh.Close()
	res, _ = CompactForks(CompactOptions{ForksDir: m.forks, DBPath: mdb, MinAge: time.Minute, Now: later})
	if res.Skipped[m.forkPath] != "index not caught up with file" {
		t.Fatalf("stale: %+v", res)
	}
	if now, _ := os.ReadFile(f.forkPath); string(now) != string(orig) {
		t.Fatal("unmerged fork was modified")
	}
	res, _ = CompactForks(CompactOptions{ForksDir: m.forks, DBPath: mdb, MinAge: time.Minute, Now: later, DryRun: true})
	_ = res
}

func TestCompactForksDryRunWritesNothing(t *testing.T) {
	f := newCompactFixture(t, true)
	dbPath := filepath.Join(f.root, "a.db")
	f.importTo(t, dbPath)
	orig, _ := os.ReadFile(f.forkPath)
	res, err := CompactForks(CompactOptions{ForksDir: f.forks, DBPath: dbPath, MinAge: time.Minute, Now: later, DryRun: true})
	if err != nil || len(res.Compacted) != 1 || res.BytesReclaimed <= 0 {
		t.Fatalf("dry run: %+v %v", res, err)
	}
	if now, _ := os.ReadFile(f.forkPath); string(now) != string(orig) {
		t.Fatal("dry run modified the file")
	}
}

func fileSize(t *testing.T, p string) int64 {
	t.Helper()
	i, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return i.Size()
}

func equalSnap(a, b forkSnapshot) bool {
	if a.fork != b.fork || a.merge != b.merge || a.importErrors != b.importErrors || len(a.turns) != len(b.turns) {
		return false
	}
	for i := range a.turns {
		if a.turns[i] != b.turns[i] {
			return false
		}
	}
	return true
}

var _ = sql.ErrNoRows
