package continuity

import (
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		out := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T) (ImportOptions, *sql.DB) {
	t.Helper()
	root := t.TempDir()
	sessions := filepath.Join(root, "sessions")
	handoffs := filepath.Join(root, "handoffs")
	copyTree(t, "testdata/sessions", sessions)
	copyTree(t, "testdata/handoffs", handoffs)
	opts := ImportOptions{SessionsDir: sessions, HandoffsDir: handoffs, DBPath: filepath.Join(root, "continuity.db")}
	if err := Import(opts); err != nil {
		t.Fatal(err)
	}
	db, err := Open(opts.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return opts, db
}
func count(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestImportFaithfulTreeHandoffAndIdempotency(t *testing.T) {
	opts, db := setup(t)
	if got := count(t, db, `SELECT count(*) FROM sessions`); got != 2 {
		t.Fatalf("sessions=%d", got)
	}
	if got := count(t, db, `SELECT count(*) FROM turns`); got != 10 {
		t.Fatalf("turns=%d", got)
	}
	if got := count(t, db, `SELECT count(*) FROM edges WHERE edge_type='fork' AND inferred=0`); got != 1 {
		t.Fatalf("forks=%d", got)
	}
	if got := count(t, db, `SELECT count(*) FROM edges WHERE edge_type='handoff' AND inferred=1`); got != 1 {
		t.Fatalf("handoffs=%d", got)
	}
	if got := count(t, db, `SELECT count(*) FROM parts WHERE source='pi:compaction'`); got != 1 {
		t.Fatalf("compactions=%d", got)
	}
	if got := count(t, db, `SELECT count(*) FROM parts WHERE source='pi:custom'`); got != 1 {
		t.Fatalf("custom=%d", got)
	}
	var raw string
	if err := db.QueryRow(`SELECT body_json FROM parts WHERE turn_id='pi:sess-one:user0001'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != `{"type":"text", "text":"hello"}` {
		t.Fatalf("raw block normalized: %q", raw)
	}
	before := count(t, db, `SELECT count(*) FROM parts`)
	if err := Import(opts); err != nil {
		t.Fatal(err)
	}
	if after := count(t, db, `SELECT count(*) FROM parts`); after != before {
		t.Fatalf("rerun changed parts %d -> %d", before, after)
	}
}

func TestSystemPromptCustomEntries(t *testing.T) {
	root := t.TempDir()
	sessions := filepath.Join(root, "sessions")
	handoffs := filepath.Join(root, "handoffs")
	copyTree(t, "testdata/system-prompts", sessions)
	if err := os.MkdirAll(handoffs, 0o755); err != nil {
		t.Fatal(err)
	}
	opts := ImportOptions{SessionsDir: sessions, HandoffsDir: handoffs, DBPath: filepath.Join(root, "continuity.db")}
	if err := Import(opts); err != nil {
		t.Fatal(err)
	}
	db, err := Open(opts.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if got := count(t, db, `SELECT count(*) FROM parts WHERE source='system'`); got != 2 {
		t.Fatalf("system prompt parts=%d", got)
	}
	if got := count(t, db, `SELECT count(*) FROM parts WHERE source='system' AND turn_id='pi:prompt-session:prompt01'`); got != 1 {
		t.Fatalf("first-turn system prompt parts=%d", got)
	}
	if got := count(t, db, `SELECT count(*) FROM parts WHERE source='system' AND turn_id IN ('pi:prompt-session:user0001','pi:prompt-session:asst0001')`); got != 0 {
		t.Fatalf("unchanged turns unexpectedly have %d prompt parts", got)
	}
	if got := count(t, db, `SELECT count(*) FROM edges WHERE child_id='pi:prompt-session:prompt02' AND parent_id='pi:prompt-session:asst0001' AND edge_type='continue'`); got != 1 {
		t.Fatalf("changed prompt ancestry edges=%d", got)
	}

	var raw string
	if err := db.QueryRow(`SELECT body_json FROM parts WHERE turn_id='pi:prompt-session:prompt02'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	wantRaw := `{"type":"custom","id":"prompt02","parentId":"asst0001","timestamp":"2026-09-25T10:00:03.000Z","customType":"familiar.system-prompt.v1","data":{"sha256":"2fcfb6024ff49f090300f99c2a41ad5a1555bce95bdb2e3f86172b6986d3c83a","text":"Prompt beta"}}`
	if raw != wantRaw {
		t.Fatalf("system prompt entry normalized:\n got %q\nwant %q", raw, wantRaw)
	}

	if got := count(t, db, `SELECT count(*) FROM parts WHERE source='pi:custom' AND turn_id IN ('pi:prompt-session:badtext1','pi:prompt-session:badhash1')`); got != 2 {
		t.Fatalf("malformed raw custom parts=%d", got)
	}
	if got := count(t, db, `SELECT count(*) FROM import_errors`); got != 2 {
		t.Fatalf("malformed prompt errors=%d", got)
	}
	s, err := ReadStats(db)
	if err != nil {
		t.Fatal(err)
	}
	if s.SystemPrompts != 2 {
		t.Fatalf("distinct system prompts=%d", s.SystemPrompts)
	}
	if !strings.Contains(FormatStats(s), "system prompts: 2\n") {
		t.Fatalf("stats missing system prompt count:\n%s", FormatStats(s))
	}
}

func TestSchemaVersionMismatchRebuilds(t *testing.T) {
	root := t.TempDir()
	sessions := filepath.Join(root, "sessions")
	handoffs := filepath.Join(root, "handoffs")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(handoffs, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(root, "continuity.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE schema_version(version INTEGER NOT NULL); INSERT INTO schema_version VALUES(1); CREATE TABLE stale(value TEXT);`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err = Import(ImportOptions{SessionsDir: sessions, HandoffsDir: handoffs, DBPath: dbPath}); err != nil {
		t.Fatal(err)
	}
	db, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got := count(t, db, `SELECT version FROM schema_version`); got != SchemaVersion {
		t.Fatalf("schema version=%d", got)
	}
	if got := count(t, db, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='stale'`); got != 0 {
		t.Fatal("stale version-1 schema was not rebuilt")
	}
}

func TestTruncatedTrailingLineWaitsForCompletion(t *testing.T) {
	root := t.TempDir()
	sessions := filepath.Join(root, "sessions")
	handoffs := filepath.Join(root, "handoffs")
	os.MkdirAll(sessions, 0o755)
	os.MkdirAll(handoffs, 0o755)
	b, err := os.ReadFile("testdata/truncated.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sessions, "truncated.jsonl")
	if err = os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	opts := ImportOptions{SessionsDir: sessions, HandoffsDir: handoffs, DBPath: filepath.Join(root, "db")}
	if err = Import(opts); err != nil {
		t.Fatal(err)
	}
	db, _ := Open(opts.DBPath)
	defer db.Close()
	if got := count(t, db, `SELECT count(*) FROM turns`); got != 1 {
		t.Fatalf("turns=%d", got)
	}
	if err = os.WriteFile(path, append(b, []byte(`"}]}}`+"\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err = Import(opts); err != nil {
		t.Fatal(err)
	}
	if got := count(t, db, `SELECT count(*) FROM turns`); got != 2 {
		t.Fatalf("completed turns=%d", got)
	}
}

func TestReplacementReimportsFile(t *testing.T) {
	opts, db := setup(t)
	path := filepath.Join(opts.SessionsDir, "one.jsonl")
	replacement := `{"type":"session","version":3,"id":"replacement","timestamp":"2026-08-27T02:00:00Z","cwd":"/scrubbed"}` + "\n" +
		`{"type":"message","id":"only","parentId":null,"timestamp":"2026-08-27T02:00:01Z","message":{"role":"user","content":"replacement"}}` + "\n"
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(replacement), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	if err := Import(opts); err != nil {
		t.Fatal(err)
	}
	if got := count(t, db, `SELECT count(*) FROM sessions WHERE id='pi:sess-one'`); got != 0 {
		t.Fatalf("old session remains")
	}
	if got := count(t, db, `SELECT count(*) FROM turns WHERE session_id='pi:replacement'`); got != 1 {
		t.Fatalf("replacement turns=%d", got)
	}
}

func TestDeletedSourceIsRemovedFromDerivedIndex(t *testing.T) {
	opts, db := setup(t)
	if err := os.Remove(filepath.Join(opts.SessionsDir, "one.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := Import(opts); err != nil {
		t.Fatal(err)
	}
	if got := count(t, db, `SELECT count(*) FROM sessions`); got != 1 {
		t.Fatalf("sessions after source deletion=%d", got)
	}
	if got := count(t, db, `SELECT count(*) FROM sessions WHERE id='pi:sess-one'`); got != 0 {
		t.Fatal("deleted source remains indexed")
	}
}

func TestCrashCheckpointResumes(t *testing.T) {
	root := t.TempDir()
	sessions := filepath.Join(root, "sessions")
	handoffs := filepath.Join(root, "handoffs")
	copyTree(t, "testdata/sessions", sessions)
	copyTree(t, "testdata/handoffs", handoffs)
	opts := ImportOptions{SessionsDir: sessions, HandoffsDir: handoffs, DBPath: filepath.Join(root, "db"), StopAfter: 3}
	if err := Import(opts); !errors.Is(err, ErrStopped) {
		t.Fatalf("got %v", err)
	}
	db, _ := Open(opts.DBPath)
	if got := count(t, db, `SELECT count(*) FROM turns`); got != 3 {
		t.Fatalf("checkpoint turns=%d", got)
	}
	db.Close()
	opts.StopAfter = 0
	if err := Import(opts); err != nil {
		t.Fatal(err)
	}
	db, _ = Open(opts.DBPath)
	defer db.Close()
	if got := count(t, db, `SELECT count(*) FROM turns`); got != 10 {
		t.Fatalf("resumed turns=%d", got)
	}
}

func TestCompleteMalformedLineIsReported(t *testing.T) {
	opts, db := setup(t)
	path := filepath.Join(opts.SessionsDir, "two.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("{not json}\n")
	f.Close()
	if err = Import(opts); err != nil {
		t.Fatal(err)
	}
	s, err := ReadStats(db)
	if err != nil {
		t.Fatal(err)
	}
	if s.ImportErrors != 1 {
		t.Fatalf("errors=%d", s.ImportErrors)
	}
	if !strings.Contains(FormatStats(s), "last import:") {
		t.Fatal("stats missing last import")
	}
}
