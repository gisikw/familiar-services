package continuity

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const forkMarkerCustomType = "familiar.fork.v1"

// CompactOptions describes one fork-compaction pass.
type CompactOptions struct {
	ForksDir string
	DBPath   string
	MinAge   time.Duration
	DryRun   bool
	Now      func() time.Time // test hook; nil means time.Now
}

// CompactResult reports what a pass did (or would do, in a dry run).
type CompactResult struct {
	Compacted      []string
	Skipped        map[string]string
	BytesReclaimed int64
}

// lockIndex serialises writers of one index (import and fork compaction). The
// lock file lives beside the database; the returned func releases it.
func lockIndex(dbPath string) (func(), error) {
	f, err := os.OpenFile(dbPath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// CompactForks rewrites merged fork session files to drop the parent prefix
// that Pi's SessionManager copies into every branched session.
//
// A fork file is <header><copied parent path><familiar.fork.v1 marker><own
// entries>. The index already stores only the marker and what follows (the
// prefix is removed by reconcileForkPrefixes), so dropping the copied path
// from the source loses nothing the index or the parent session does not
// already hold. The file stays the source of truth: the result is exactly the
// shape a fresh import treats as "already cleaned".
//
// A file is compacted only when every check passes, otherwise it is left
// untouched and the reason recorded:
//   - it is older than MinAge and fully imported, unchanged since (import_state
//     matches its inode, size and mtime, and the offset reached EOF);
//   - the index holds a merge edge from a parent turn into this session, so it
//     is merged and its return is linked;
//   - its own suffix is self-contained (every parentId after the marker points
//     inside the suffix), and the index's turns for the session are exactly the
//     suffix's entry IDs.
//
// The rewrite is atomic (temp file, fsync, rename, directory fsync) and the
// import_state row is updated in the same locked pass, so the importer sees an
// unchanged file rather than a replacement to re-derive.
func CompactForks(opts CompactOptions) (CompactResult, error) {
	res := CompactResult{Skipped: map[string]string{}}
	if opts.ForksDir == "" || opts.DBPath == "" {
		return res, errors.New("forks and db paths are required")
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	unlock, err := lockIndex(opts.DBPath)
	if err != nil {
		return res, err
	}
	defer unlock()
	db, err := Open(opts.DBPath)
	if err != nil {
		return res, err
	}
	defer db.Close()

	root, err := filepath.Abs(opts.ForksDir)
	if err != nil {
		return res, err
	}
	paths, err := filepath.Glob(filepath.Join(root, "*", "sessions", "*.jsonl"))
	if err != nil {
		return res, err
	}
	sort.Strings(paths)
	for _, path := range paths {
		reason, saved, err := compactOne(db, path, now(), opts.MinAge, opts.DryRun)
		if err != nil {
			return res, fmt.Errorf("%s: %w", path, err)
		}
		if reason != "" {
			res.Skipped[path] = reason
			continue
		}
		res.Compacted = append(res.Compacted, path)
		res.BytesReclaimed += saved
	}
	return res, nil
}

func compactOne(db *sql.DB, path string, now time.Time, minAge time.Duration, dryRun bool) (string, int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", 0, err
	}
	if now.Sub(info.ModTime()) < minAge {
		return "younger than min age", 0, nil
	}
	head, second, err := firstTwoLines(path)
	if err != nil {
		return "", 0, err
	}
	var h header
	if json.Unmarshal(head, &h) != nil || h.Type != "session" || h.ID == "" {
		return "not a Pi session file", 0, nil
	}
	if h.Parent == "" {
		return "no parentSession header", 0, nil
	}
	var e2 entry
	if json.Unmarshal(second, &e2) == nil && e2.CustomType == forkMarkerCustomType {
		return "already compact", 0, nil
	}
	sid := "pi:" + h.ID
	state, exists, err := getState(db, path)
	if err != nil {
		return "", 0, err
	}
	if !exists || state.SessionID != sid {
		return "not in index", 0, nil
	}
	if state.Inode != statIdentity(info) || state.Size != info.Size() || state.Mtime != info.ModTime().UnixNano() || state.Offset < info.Size() {
		return "index not caught up with file", 0, nil
	}
	var merged int
	if err = db.QueryRow(`SELECT count(*) FROM edges WHERE edge_type='merge' AND parent_id >= ? AND parent_id < ?`, sid+":", sid+";").Scan(&merged); err != nil {
		return "", 0, err
	}
	if merged == 0 {
		return "no merge edge (active, unmerged, or unlinked)", 0, nil
	}

	lines, markerAt, err := readForkLines(path)
	if err != nil {
		return "", 0, err
	}
	if markerAt < 0 {
		return "no familiar.fork.v1 marker", 0, nil
	}
	if markerAt == 1 {
		return "already compact", 0, nil
	}
	suffix := lines[markerAt:]
	own := make(map[string]bool, len(suffix))
	for i, raw := range suffix {
		var e entry
		if err := json.Unmarshal(bytes.TrimRight(raw, "\r\n"), &e); err != nil || e.ID == "" {
			return "unparseable entry in suffix", 0, nil
		}
		if i > 0 && (e.ParentID == nil || !own[*e.ParentID]) {
			return "suffix is not self-contained", 0, nil
		}
		own[sid+":"+e.ID] = true
		own[e.ID] = true
	}
	rows, err := db.Query(`SELECT id FROM turns WHERE session_id=? AND id<>?`, sid, sid+":system")
	if err != nil {
		return "", 0, err
	}
	indexed := 0
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return "", 0, err
		}
		if !own[id] {
			rows.Close()
			return "index holds turns outside the suffix", 0, nil
		}
		indexed++
	}
	if err = rows.Close(); err != nil {
		return "", 0, err
	}
	if indexed != len(suffix) {
		return fmt.Sprintf("index has %d turns, suffix has %d", indexed, len(suffix)), 0, nil
	}

	var newSize int64 = int64(len(lines[0]))
	for _, l := range suffix {
		newSize += int64(len(l))
	}
	saved := info.Size() - newSize
	if dryRun {
		return "", saved, nil
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".compact-*.jsonl.tmp")
	if err != nil {
		return "", 0, err
	}
	tmpName := tmp.Name()
	cleanup := func() { tmp.Close(); os.Remove(tmpName) }
	w := bufio.NewWriterSize(tmp, 1<<20)
	for _, l := range append([][]byte{lines[0]}, suffix...) {
		if _, err = w.Write(l); err != nil {
			cleanup()
			return "", 0, err
		}
	}
	if err = w.Flush(); err == nil {
		err = tmp.Chmod(info.Mode().Perm())
	}
	if err == nil {
		err = tmp.Sync()
	}
	if err != nil {
		cleanup()
		return "", 0, err
	}
	if err = tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", 0, err
	}
	if err = os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return "", 0, err
	}
	if d, e := os.Open(dir); e == nil {
		_ = d.Sync()
		d.Close()
	}
	ni, err := os.Stat(path)
	if err != nil {
		return "", 0, err
	}
	_, err = db.Exec(`UPDATE import_state SET inode=?,size=?,mtime_ns=?,byte_offset=?,imported_at=? WHERE path=?`,
		statIdentity(ni), ni.Size(), ni.ModTime().UnixNano(), ni.Size(), now_(), path)
	if err != nil {
		return "", 0, fmt.Errorf("rewrote file but failed to update import_state (next import will re-derive it): %w", err)
	}
	return "", saved, nil
}

// now_ matches the importer's timestamp format.
func now_() string { return now() }

func firstTwoLines(path string) ([]byte, []byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	a, err := r.ReadBytes('\n')
	if err != nil && err != io.EOF {
		return nil, nil, err
	}
	b, err := r.ReadBytes('\n')
	if err != nil && err != io.EOF {
		return nil, nil, err
	}
	return bytes.TrimRight(a, "\r\n"), bytes.TrimRight(b, "\r\n"), nil
}

// readForkLines returns every complete line (with its newline) and the index
// of the final familiar.fork.v1 marker, or -1. A file whose last line is not
// newline-terminated is refused, since Pi may still be appending.
func readForkLines(path string) ([][]byte, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, -1, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	var lines [][]byte
	marker := -1
	for {
		l, err := r.ReadBytes('\n')
		if len(l) > 0 {
			if l[len(l)-1] != '\n' {
				return nil, -1, errors.New("final line is not newline-terminated")
			}
			if len(lines) > 0 && bytes.Contains(l, []byte(forkMarkerCustomType)) {
				var e entry
				if json.Unmarshal(bytes.TrimRight(l, "\r\n"), &e) == nil && e.Type == "custom" && e.CustomType == forkMarkerCustomType {
					marker = len(lines)
				}
			}
			lines = append(lines, l)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, -1, err
		}
	}
	return lines, marker, nil
}

// FormatCompactResult renders a pass for humans and journald.
func FormatCompactResult(r CompactResult, dryRun bool) string {
	var b strings.Builder
	verb := "compacted"
	if dryRun {
		verb = "would compact"
	}
	fmt.Fprintf(&b, "%s %d fork session(s), %d bytes reclaimed\n", verb, len(r.Compacted), r.BytesReclaimed)
	for _, p := range r.Compacted {
		fmt.Fprintf(&b, "  %s %s\n", verb, p)
	}
	keys := make([]string, 0, len(r.Skipped))
	for k := range r.Skipped {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if r.Skipped[k] == "already compact" || r.Skipped[k] == "younger than min age" {
			continue
		}
		fmt.Fprintf(&b, "  skipped %s: %s\n", k, r.Skipped[k])
	}
	return b.String()
}
