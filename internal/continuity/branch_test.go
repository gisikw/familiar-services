package continuity

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func syntheticBranchFiles(parentPath string) (string, string) {
	parent := `{"type":"session","version":3,"id":"parent","timestamp":"2026-01-01T00:00:00Z","cwd":"/tmp"}
{"type":"message","id":"branch01","parentId":null,"timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","content":"base"}}
{"type":"message","id":"merge001","parentId":"branch01","timestamp":"2026-01-01T00:00:05Z","message":{"role":"custom","customType":"familiar.merge.v1","content":"merged","display":true,"details":{"forkSessionId":"fork","forkSessionFile":"/tmp/fork.jsonl","branchEntryId":"branch01","firstEntryId":"first001","lastEntryId":"last0001","turnCount":2,"forkedFurther":false}}}
`
	fork := `{"type":"session","version":3,"id":"fork","timestamp":"2026-01-01T00:00:02Z","cwd":"/tmp","parentSession":"` + parentPath + `"}
{"type":"message","id":"branch01","parentId":null,"timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","content":"base"}}
{"type":"custom","id":"marker01","parentId":"branch01","timestamp":"2026-01-01T00:00:02Z","customType":"familiar.fork.v1","data":{"parentSessionId":"parent","branchEntryId":"branch01"}}
{"type":"message","id":"first001","parentId":"marker01","timestamp":"2026-01-01T00:00:03Z","message":{"role":"user","content":"task"}}
{"type":"message","id":"last0001","parentId":"first001","timestamp":"2026-01-01T00:00:04Z","message":{"role":"assistant","content":"done"}}
{"type":"custom","id":"close001","parentId":"last0001","timestamp":"2026-01-01T00:00:05Z","customType":"familiar.branch-close.v1","data":{"reason":"closed"}}
`
	return parent, fork
}

func TestHeaderOnlyForkDropsCopiedPrefix(t *testing.T) {
	root := t.TempDir()
	sessions, handoffs := filepath.Join(root, "sessions"), filepath.Join(root, "handoffs")
	os.MkdirAll(sessions, 0755)
	os.MkdirAll(handoffs, 0755)
	parentPath := filepath.Join(sessions, "parent.jsonl")
	parent := `{"type":"session","id":"parent","timestamp":"2026-01-01T00:00:00Z"}
{"type":"message","id":"branch01","parentId":null,"timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","content":"base"}}
`
	fork := `{"type":"session","id":"fork","timestamp":"2026-01-01T00:00:02Z","parentSession":"` + parentPath + `"}
{"type":"message","id":"branch01","parentId":null,"timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","content":"base"}}
{"type":"message","id":"own00001","parentId":"branch01","timestamp":"2026-01-01T00:00:03Z","message":{"role":"user","content":"task"}}
`
	os.WriteFile(parentPath, []byte(parent), 0644)
	os.WriteFile(filepath.Join(sessions, "fork.jsonl"), []byte(fork), 0644)
	opts := ImportOptions{SessionsDirs: []string{sessions}, HandoffsDir: handoffs, DBPath: filepath.Join(root, "db")}
	if err := Import(opts); err != nil {
		t.Fatal(err)
	}
	db, _ := Open(opts.DBPath)
	defer db.Close()
	if got := count(t, db, `SELECT count(*) FROM turns WHERE session_id='pi:fork'`); got != 1 {
		t.Fatalf("fork turns=%d", got)
	}
	if got := count(t, db, `SELECT count(*) FROM edges WHERE child_id='pi:fork:own00001' AND parent_id='pi:parent:branch01' AND edge_type='fork'`); got != 1 {
		t.Fatalf("fork edges=%d", got)
	}
}

// benchmarkForkDB builds the cardinality that exposed the original quadratic
// foreign-key work: 40,000 source turns plus a fork containing those 40,000
// copied turns and its own marker. Setup is excluded from benchmark timing.
func benchmarkForkDB(b *testing.B) ImportOptions {
	b.Helper()
	root := b.TempDir()
	sessions, handoffs := filepath.Join(root, "sessions"), filepath.Join(root, "handoffs")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.MkdirAll(handoffs, 0o755); err != nil {
		b.Fatal(err)
	}
	parentPath, forkPath := filepath.Join(sessions, "parent.jsonl"), filepath.Join(sessions, "fork.jsonl")
	var parent strings.Builder
	parent.WriteString(`{"type":"session","id":"parent","timestamp":"2026-01-01T00:00:00Z"}` + "\n")
	for i := 0; i < 40000; i++ {
		fmt.Fprintf(&parent, `{"type":"message","id":"e%07d","parentId":null,"timestamp":"2026-01-01T00:00:01Z","message":{"role":"user","content":"x"}}`+"\n", i)
	}
	if err := os.WriteFile(parentPath, []byte(parent.String()), 0o644); err != nil {
		b.Fatal(err)
	}
	forkHeader := fmt.Sprintf(`{"type":"session","id":"fork","timestamp":"2026-01-01T00:00:02Z","parentSession":%q}`+"\n", parentPath)
	if err := os.WriteFile(forkPath, []byte(forkHeader), 0o644); err != nil {
		b.Fatal(err)
	}

	dbPath := filepath.Join(root, "continuity.db")
	db, err := Open(dbPath)
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	if err = ensureSchema(db); err != nil {
		b.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	parentMeta := `{"type":"session","id":"parent","timestamp":"2026-01-01T00:00:00Z"}`
	forkMeta := fmt.Sprintf(`{"type":"session","id":"fork","timestamp":"2026-01-01T00:00:02Z","parentSession":%q}`, parentPath)
	if _, err = tx.Exec(`INSERT INTO sessions(id,source_format,source_path,started_at,meta_json) VALUES
		('pi:parent','pi',?,'2026-01-01T00:00:00Z',?),('pi:fork','pi',?,'2026-01-01T00:00:02Z',?)`, parentPath, parentMeta, forkPath, forkMeta); err != nil {
		b.Fatal(err)
	}
	turnStmt, _ := tx.Prepare(`INSERT INTO turns(id,session_id,seq,ts,role,meta_json) VALUES(?,?,?,'2026-01-01T00:00:01Z','user',?)`)
	partStmt, _ := tx.Prepare(`INSERT INTO parts(turn_id,idx,source,body_json) VALUES(?,0,'user','{}')`)
	for i := 0; i < 40000; i++ {
		entryID := fmt.Sprintf("e%07d", i)
		raw := fmt.Sprintf(`{"type":"message","id":%q,"parentId":null,"timestamp":"2026-01-01T00:00:01Z","message":{"role":"user","content":"x"}}`, entryID)
		for _, sid := range []string{"pi:parent", "pi:fork"} {
			tid := sid + ":" + entryID
			if _, err = turnStmt.Exec(tid, sid, i, raw); err != nil {
				b.Fatal(err)
			}
			if _, err = partStmt.Exec(tid); err != nil {
				b.Fatal(err)
			}
		}
	}
	turnStmt.Close()
	partStmt.Close()
	marker := `{"type":"custom","id":"marker01","parentId":"e0039999","timestamp":"2026-01-01T00:00:02Z","customType":"familiar.fork.v1","data":{"parentSessionId":"parent","branchEntryId":"e0039999"}}`
	if _, err = tx.Exec(`INSERT INTO turns(id,session_id,seq,ts,role,meta_json) VALUES('pi:fork:marker01','pi:fork',40000,'2026-01-01T00:00:02Z','metadata',?);
		INSERT INTO parts(turn_id,idx,source,body_json) VALUES('pi:fork:marker01',0,'pi:custom','{}');
		INSERT INTO branch_reconcile_pending(session_id) VALUES('pi:fork')`, marker); err != nil {
		b.Fatal(err)
	}
	for _, state := range []struct{ path, sid string }{{parentPath, "pi:parent"}, {forkPath, "pi:fork"}} {
		info, e := os.Stat(state.path)
		if e != nil {
			b.Fatal(e)
		}
		if _, err = tx.Exec(`INSERT INTO import_state(path,inode,size,mtime_ns,byte_offset,session_id,imported_at) VALUES(?,?,?,?,?,?,?)`, state.path, statIdentity(info), info.Size(), info.ModTime().UnixNano(), info.Size(), state.sid, now()); err != nil {
			b.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		b.Fatal(err)
	}
	return ImportOptions{SessionsDirs: []string{sessions}, HandoffsDir: handoffs, DBPath: dbPath}
}

func BenchmarkForkPrefixReconciliation80K(b *testing.B) {
	b.Run("cleanup", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			opts := benchmarkForkDB(b)
			b.StartTimer()
			if err := Import(opts); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("idle-incremental", func(b *testing.B) {
		b.StopTimer()
		opts := benchmarkForkDB(b)
		if err := Import(opts); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		for i := 0; i < b.N; i++ {
			if err := Import(opts); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func TestBranchReconciliationAcrossSessionRoots(t *testing.T) {
	for _, parentFirst := range []bool{true, false} {
		name := "fork-first"
		if parentFirst {
			name = "parent-first"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			sessions := filepath.Join(root, "sessions")
			forks := filepath.Join(root, "state", "forks")
			forkSessions := filepath.Join(forks, "fork-uuid", "sessions")
			handoffs := filepath.Join(root, "handoffs")
			os.MkdirAll(sessions, 0755)
			os.MkdirAll(forkSessions, 0755)
			os.MkdirAll(handoffs, 0755)
			parentPath := filepath.Join(sessions, "parent.jsonl")
			forkPath := filepath.Join(forkSessions, "fork.jsonl")
			parent, fork := syntheticBranchFiles(parentPath)
			dbp := filepath.Join(root, "db")
			opts := ImportOptions{SessionsDirs: []string{sessions, forks}, HandoffsDir: handoffs, DBPath: dbp}

			if parentFirst {
				os.WriteFile(parentPath, []byte(parent), 0644)
			} else {
				os.WriteFile(forkPath, []byte(fork), 0644)
			}
			if err := Import(opts); err != nil {
				t.Fatal(err)
			}
			if !parentFirst {
				db, _ := Open(dbp)
				if got := count(t, db, `SELECT count(*) FROM sessions WHERE id='pi:fork'`); got != 0 {
					t.Fatalf("unresolved fork sessions=%d", got)
				}
				db.Close()
				os.WriteFile(parentPath, []byte(parent), 0644)
			} else {
				os.WriteFile(forkPath, []byte(fork), 0644)
			}
			if err := Import(opts); err != nil {
				t.Fatal(err)
			}

			db, _ := Open(dbp)
			defer db.Close()
			if got := count(t, db, `SELECT count(*) FROM turns WHERE session_id='pi:fork'`); got != 4 {
				t.Fatalf("fork turns=%d", got)
			}
			if got := count(t, db, `SELECT count(*) FROM turns WHERE id='pi:fork:branch01'`); got != 0 {
				t.Fatalf("inherited turns=%d", got)
			}
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
		})
	}
}
