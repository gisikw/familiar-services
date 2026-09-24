package api_test

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gisikw/familiar-services/internal/attention"
	"github.com/gisikw/familiar-services/internal/wakes"
	"github.com/gisikw/familiar-services/internal/worklist"
)

// This compatibility test never opens production state. It first copies each
// existing store into t.TempDir (normally /tmp), then opens only the copy.
func TestCopiesOfRealStores(t *testing.T) {
	tmp := t.TempDir()
	dbSource := os.Getenv("FAMILIAR_ATTENTION_DB")
	if dbSource == "" {
		for _, p := range []string{"/var/lib/kestrel/state/familiar-ui/attention.sqlite", "/var/lib/golem/herdr/state/familiar-ui/attention.sqlite"} {
			if _, e := os.Stat(p); e == nil {
				dbSource = p
				break
			}
		}
	}
	if dbSource == "" {
		t.Skip("real Attention DB is not present")
	}
	dbCopy := filepath.Join(tmp, "attention.sqlite")
	copyFile(t, dbSource, dbCopy)
	// SQLite WAL sidecars are copied when present so the snapshot is coherent.
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, e := os.Stat(dbSource + suffix); e == nil {
			copyFile(t, dbSource+suffix, dbCopy+suffix)
		}
	}
	attn, e := attention.Open(dbCopy)
	if e != nil {
		t.Fatalf("open copied Attention DB: %v", e)
	}
	if _, e = attn.Handle("project.list", map[string]any{}); e != nil {
		t.Fatalf("read copied Attention DB: %v", e)
	}
	_ = attn.Close()

	stateSource := "/var/lib/kestrel/state"
	if _, e = os.Stat(filepath.Join(stateSource, "worklist")); e != nil {
		t.Skip("real file stores are not present")
	}
	stateCopy := filepath.Join(tmp, "state")
	if e = os.CopyFS(filepath.Join(stateCopy, "worklist"), os.DirFS(filepath.Join(stateSource, "worklist"))); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(stateSource, "wakes")); e == nil {
		if e = os.CopyFS(filepath.Join(stateCopy, "wakes"), os.DirFS(filepath.Join(stateSource, "wakes"))); e != nil {
			t.Fatal(e)
		}
	}
	work, e := worklist.Open(stateCopy)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = work.List(); e != nil {
		t.Fatalf("read copied worklist: %v", e)
	}
	wake, e := wakes.Open(stateCopy, work)
	if e != nil {
		t.Fatalf("read copied wakes: %v", e)
	}
	if _, e = wake.List(); e != nil {
		t.Fatal(e)
	}
	wake.Close()
}
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, e := os.Open(src)
	if e != nil {
		t.Fatal(e)
	}
	defer in.Close()
	out, e := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = io.Copy(out, in); e != nil {
		out.Close()
		t.Fatal(e)
	}
	if e = out.Close(); e != nil {
		t.Fatal(e)
	}
}
