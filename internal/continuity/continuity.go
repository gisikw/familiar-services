// Package continuity imports Pi JSONL sessions into a disposable SQLite index.
package continuity

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/gisikw/familiar-services/schema"
	_ "github.com/mattn/go-sqlite3"
)

const (
	SchemaVersion          = 2
	systemPromptCustomType = "familiar.system-prompt.v1"
)

// ErrStopped is returned by the test-only StopAfter hook, simulating termination.
var ErrStopped = errors.New("import stopped")

// ImportOptions describes one catch-up pass. StopAfter is a test hook; zero disables it.
type ImportOptions struct {
	SessionsDirs []string
	HandoffsDir  string
	DBPath       string
	StopAfter    int
}

type header struct {
	Type         string          `json:"type"`
	ID           string          `json:"id"`
	Timestamp    string          `json:"timestamp"`
	CWD          string          `json:"cwd"`
	Parent       string          `json:"parentSession"`
	SystemPrompt json.RawMessage `json:"systemPrompt"`
}

type entry struct {
	Type       string          `json:"type"`
	ID         string          `json:"id"`
	ParentID   *string         `json:"parentId"`
	Timestamp  string          `json:"timestamp"`
	Message    json.RawMessage `json:"message"`
	FromID     string          `json:"fromId"`
	Name       *string         `json:"name"`
	Provider   string          `json:"provider"`
	ModelID    string          `json:"modelId"`
	CustomType string          `json:"customType"`
	Data       json.RawMessage `json:"data"`
}

type systemPromptData struct {
	SHA256 *string `json:"sha256"`
	Text   *string `json:"text"`
}

type message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	CustomType string          `json:"customType"`
}

type fileState struct {
	Inode, Size, Mtime, Offset int64
	SessionID                  string
}

// Open opens an existing index for queries.
func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", "file:"+path+"?_foreign_keys=on&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	if err = db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Import catches the derived index up with all complete JSONL records.
func Import(opts ImportOptions) error {
	if len(opts.SessionsDirs) == 0 || opts.HandoffsDir == "" || opts.DBPath == "" {
		return errors.New("sessions, handoffs, and db paths are required")
	}
	if err := os.MkdirAll(filepath.Dir(opts.DBPath), 0o750); err != nil {
		return err
	}
	db, err := Open(opts.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureSchema(db); err != nil {
		return err
	}

	pathSet := make(map[string]struct{})
	for _, root := range opts.SessionsDirs {
		root, err = filepath.Abs(root)
		if err != nil {
			return fmt.Errorf("resolve sessions root: %w", err)
		}
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if !d.IsDir() && strings.HasSuffix(strings.ToLower(d.Name()), ".jsonl") {
				pathSet[path] = struct{}{}
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("scan sessions %s: %w", root, err)
		}
	}
	paths := make([]string, 0, len(pathSet))
	for path := range pathSet {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	if err := pruneMissing(db, paths); err != nil {
		return err
	}
	processed := 0
	for _, path := range paths {
		n, err := importFile(db, path, opts.StopAfter-processed)
		processed += n
		if err != nil {
			return err
		}
		if opts.StopAfter > 0 && processed >= opts.StopAfter {
			return ErrStopped
		}
	}
	if err := reconcileBranches(db); err != nil {
		return err
	}
	if err := importHandoffs(db, opts.HandoffsDir); err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO import_runs(singleton,last_import_at) VALUES(1,?)
		ON CONFLICT(singleton) DO UPDATE SET last_import_at=excluded.last_import_at`, now())
	return err
}

func ensureSchema(db *sql.DB) error {
	var version int
	err := db.QueryRow(`SELECT version FROM schema_version LIMIT 1`).Scan(&version)
	if err == nil && version == SchemaVersion {
		return nil
	}
	if err != nil && !strings.Contains(err.Error(), "no such table") {
		return err
	}
	rows, err := db.Query(`SELECT type,name FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type='table'`)
	if err != nil {
		return err
	}
	type object struct{ typ, name string }
	var objects []object
	for rows.Next() {
		var o object
		if err := rows.Scan(&o.typ, &o.name); err != nil {
			rows.Close()
			return err
		}
		objects = append(objects, o)
	}
	rows.Close()
	if _, err = db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	for _, o := range objects {
		kind := strings.ToUpper(o.typ)
		if kind != "TABLE" && kind != "VIEW" && kind != "INDEX" && kind != "TRIGGER" {
			continue
		}
		if _, err = db.Exec(`DROP ` + kind + ` IF EXISTS "` + strings.ReplaceAll(o.name, `"`, `""`) + `"`); err != nil {
			return err
		}
	}
	if _, err = db.Exec(schema.Continuity); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	_, err = db.Exec(`PRAGMA foreign_keys=ON`)
	return err
}

func pruneMissing(db *sql.DB, paths []string) error {
	present := make(map[string]bool, len(paths))
	for _, path := range paths {
		present[path] = true
	}
	rows, err := db.Query(`SELECT path,coalesce(session_id,'') FROM import_state`)
	if err != nil {
		return err
	}
	type stale struct{ path, sessionID string }
	var remove []stale
	for rows.Next() {
		var s stale
		if err := rows.Scan(&s.path, &s.sessionID); err != nil {
			rows.Close()
			return err
		}
		if !present[s.path] {
			remove = append(remove, s)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, s := range remove {
		if s.sessionID != "" {
			if _, err = tx.Exec(`DELETE FROM sessions WHERE id=?`, s.sessionID); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(`DELETE FROM import_state WHERE path=?`, s.path); err != nil {
			return err
		}
		if _, err = tx.Exec(`DELETE FROM import_errors WHERE path=?`, s.path); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func statIdentity(info fs.FileInfo) (inode int64) {
	v := reflect.ValueOf(info.Sys())
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.IsValid() {
		f := v.FieldByName("Ino")
		if f.IsValid() {
			return int64(f.Uint())
		}
	}
	return 0
}

func getState(db *sql.DB, path string) (fileState, bool, error) {
	var s fileState
	err := db.QueryRow(`SELECT inode,size,mtime_ns,byte_offset,coalesce(session_id,'') FROM import_state WHERE path=?`, path).
		Scan(&s.Inode, &s.Size, &s.Mtime, &s.Offset, &s.SessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return s, false, nil
	}
	return s, err == nil, err
}

func importFile(db *sql.DB, path string, remaining int) (int, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	inode, size, mtime := statIdentity(info), info.Size(), info.ModTime().UnixNano()
	state, exists, err := getState(db, path)
	if err != nil {
		return 0, err
	}
	replaced := exists && (state.Inode != inode || size < state.Size)
	if replaced {
		tx, e := db.Begin()
		if e != nil {
			return 0, e
		}
		if state.SessionID != "" {
			if _, e = tx.Exec(`DELETE FROM sessions WHERE id=?`, state.SessionID); e != nil {
				tx.Rollback()
				return 0, e
			}
		}
		if _, e = tx.Exec(`DELETE FROM import_state WHERE path=?`, path); e != nil {
			tx.Rollback()
			return 0, e
		}
		if _, e = tx.Exec(`DELETE FROM import_errors WHERE path=?`, path); e != nil {
			tx.Rollback()
			return 0, e
		}
		if e = tx.Commit(); e != nil {
			return 0, e
		}
		state = fileState{}
		exists = false
	}
	if exists && size == state.Size && mtime == state.Mtime && state.Offset >= size {
		return 0, nil
	}
	// A skipped non-session file is reconsidered from its first line if changed.
	if exists && state.SessionID == "" {
		state.Offset = 0
	}

	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if _, err = f.Seek(state.Offset, io.SeekStart); err != nil {
		return 0, err
	}
	r := bufio.NewReader(f)
	offset := state.Offset
	count := 0
	sessionID := state.SessionID
	for {
		start := offset
		line, readErr := r.ReadBytes('\n')
		if readErr == io.EOF && len(line) > 0 {
			break
		} // Pi may be halfway through an append.
		if readErr != nil && readErr != io.EOF {
			return count, readErr
		}
		if len(line) == 0 {
			break
		}
		offset += int64(len(line))
		raw := bytes.TrimSuffix(line, []byte{'\n'})
		raw = bytes.TrimSuffix(raw, []byte{'\r'})
		if len(bytes.TrimSpace(raw)) == 0 {
			if err = checkpoint(db, path, inode, size, mtime, offset, "", sessionID); err != nil {
				return count, err
			}
			continue
		}
		if sessionID == "" {
			var h header
			decodeErr := json.Unmarshal(raw, &h)
			if decodeErr != nil || h.Type != "session" {
				return count, skipFile(db, path, inode, size, mtime)
			}
			if h.ID == "" {
				if err = recordError(db, path, start, "missing or invalid Pi session header", inode, size, mtime, offset, sessionID); err != nil {
					return count, err
				}
				continue
			}
			sessionID = "pi:" + h.ID
			if err = insertHeader(db, path, h, raw, sessionID, inode, size, mtime, offset); err != nil {
				return count, err
			}
			continue
		}
		var e entry
		if err = json.Unmarshal(raw, &e); err != nil || e.ID == "" || e.Type == "session" {
			msg := "invalid Pi entry"
			if err != nil {
				msg = err.Error()
			}
			if err = recordError(db, path, start, msg, inode, size, mtime, offset, sessionID); err != nil {
				return count, err
			}
			continue
		}
		if err = insertEntry(db, path, sessionID, e, raw, start, inode, size, mtime, offset); err != nil {
			return count, err
		}
		count++
		if remaining > 0 && count >= remaining {
			return count, ErrStopped
		}
		if readErr == io.EOF {
			break
		}
	}
	// Keep observed file metadata current without advancing over an incomplete line.
	_, err = db.Exec(`UPDATE import_state SET inode=?,size=?,mtime_ns=?,imported_at=? WHERE path=?`, inode, size, mtime, now(), path)
	return count, err
}

func insertHeader(db *sql.DB, path string, h header, raw []byte, sid string, inode, size, mtime, offset int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	meta := string(raw)
	_, err = tx.Exec(`INSERT INTO sessions(id,source_format,source_path,started_at,host,meta_json) VALUES(?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET source_path=excluded.source_path,started_at=excluded.started_at,meta_json=excluded.meta_json`, sid, "pi", path, h.Timestamp, "", meta)
	if err != nil {
		return err
	}
	if len(h.SystemPrompt) > 0 && string(h.SystemPrompt) != "null" {
		tid := sid + ":system"
		if _, err = tx.Exec(`INSERT OR IGNORE INTO turns(id,session_id,seq,ts,role,kind,meta_json) VALUES(?,?,?,?,?,'turn',?)`, tid, sid, 0, h.Timestamp, "system", meta); err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT OR IGNORE INTO parts(turn_id,idx,source,body_json) VALUES(?,0,'system',?)`, tid, string(h.SystemPrompt)); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`INSERT INTO import_state(path,inode,size,mtime_ns,byte_offset,last_entry_id,session_id,imported_at) VALUES(?,?,?,?,?,NULL,?,?)`, path, inode, size, mtime, offset, sid, now())
	if err != nil {
		return err
	}
	return tx.Commit()
}

func insertEntry(db *sql.DB, path, sid string, e entry, raw []byte, lineOffset, inode, size, mtime, offset int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var seq int
	if err = tx.QueryRow(`SELECT coalesce(max(seq),-1)+1 FROM turns WHERE session_id=?`, sid).Scan(&seq); err != nil {
		return err
	}
	role, source := "metadata", "pi:"+e.Type
	var blocks []json.RawMessage
	if e.Type == "message" {
		var m message
		if json.Unmarshal(e.Message, &m) == nil {
			role = m.Role
			source = role
			if role == "toolResult" || role == "bashExecution" {
				source = "tool"
			}
			if m.CustomType != "" {
				role = "custom"
				source = "pi:custom_message"
			}
			if len(m.Content) > 0 {
				if m.Content[0] == '[' {
					_ = json.Unmarshal(m.Content, &blocks)
				} else {
					blocks = []json.RawMessage{m.Content}
				}
			}
		}
	}
	if e.Type == "custom_message" {
		role = "custom"
		source = "pi:custom_message"
	}
	var entryError string
	if e.Type == "custom" && e.CustomType == systemPromptCustomType {
		if err := validateSystemPrompt(e.Data); err != nil {
			entryError = "invalid " + systemPromptCustomType + " entry: " + err.Error()
		} else {
			source = "system"
		}
	}
	if e.Type == "branch_summary" {
		role = "metadata"
		source = "pi:branch_summary"
	}
	if len(blocks) == 0 {
		blocks = []json.RawMessage{append(json.RawMessage(nil), raw...)}
	}
	tid := sid + ":" + e.ID
	if _, err = tx.Exec(`INSERT OR IGNORE INTO turns(id,session_id,seq,ts,role,kind,meta_json) VALUES(?,?,?,?,?,'turn',?)`, tid, sid, seq, e.Timestamp, role, string(raw)); err != nil {
		return err
	}
	for i, b := range blocks {
		if _, err = tx.Exec(`INSERT OR IGNORE INTO parts(turn_id,idx,source,body_json) VALUES(?,?,?,?)`, tid, i, source, string(b)); err != nil {
			return err
		}
	}
	if e.ParentID != nil && *e.ParentID != "" {
		pid := sid + ":" + *e.ParentID
		var existingChildren int
		if err = tx.QueryRow(`SELECT count(*) FROM edges WHERE parent_id=? AND edge_type='continue'`, pid).Scan(&existingChildren); err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT OR IGNORE INTO edges(child_id,parent_id,edge_type,inferred) VALUES(?,?,'continue',0)`, tid, pid); err != nil {
			return fmt.Errorf("parent %s for %s: %w", *e.ParentID, e.ID, err)
		}
		// A second child is Pi's durable representation of /tree navigation.
		if existingChildren > 0 {
			if _, err = tx.Exec(`INSERT OR IGNORE INTO edges(child_id,parent_id,edge_type,inferred) VALUES(?,?,'fork',0)`, tid, pid); err != nil {
				return err
			}
		}
	}
	if e.Type == "model_change" {
		_, err = tx.Exec(`UPDATE sessions SET model=? WHERE id=?`, e.Provider+"/"+e.ModelID, sid)
		if err != nil {
			return err
		}
	}
	if e.Type == "session_info" && e.Name != nil {
		_, err = tx.Exec(`UPDATE sessions SET label=? WHERE id=?`, *e.Name, sid)
		if err != nil {
			return err
		}
	}
	if entryError != "" {
		if _, err = tx.Exec(`INSERT INTO import_errors(path,byte_offset,error,occurred_at) VALUES(?,?,?,?)
			ON CONFLICT(path,byte_offset) DO UPDATE SET error=excluded.error,occurred_at=excluded.occurred_at`, path, lineOffset, entryError, now()); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`UPDATE import_state SET inode=?,size=?,mtime_ns=?,byte_offset=?,last_entry_id=?,imported_at=? WHERE path=?`, inode, size, mtime, offset, e.ID, now(), path)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// reconcileBranches projects cross-file facts after every scan. Unresolved
// references are deliberately left absent; a later import pass retries them.
func reconcileBranches(db *sql.DB) error {
	if err := reconcileForkPrefixes(db); err != nil {
		return err
	}
	rows, err := db.Query(`SELECT t.id,t.session_id,t.seq,t.meta_json,s.meta_json FROM turns t JOIN sessions s ON s.id=t.session_id ORDER BY t.session_id,t.seq`)
	if err != nil {
		return err
	}
	type item struct {
		id, sid       string
		seq           int
		entry, header map[string]any
	}
	var items []item
	for rows.Next() {
		var x item
		var eraw, hraw string
		if err = rows.Scan(&x.id, &x.sid, &x.seq, &eraw, &hraw); err != nil {
			rows.Close()
			return err
		}
		_ = json.Unmarshal([]byte(eraw), &x.entry)
		_ = json.Unmarshal([]byte(hraw), &x.header)
		items = append(items, x)
	}
	rows.Close()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, x := range items {
		ct, _ := x.entry["customType"].(string)
		if ct == "familiar.branch-close.v1" {
			if _, err = tx.Exec(`UPDATE turns SET kind='branch_close' WHERE id=?`, x.id); err != nil {
				return err
			}
			continue
		}
		if ct == "familiar.fork.v1" {
			// A nested fork inherits older markers in its immutable prefix; only
			// this session's final marker describes its own cross-session edge.
			newer := false
			for j := i + 1; j < len(items) && items[j].sid == x.sid; j++ {
				if c, _ := items[j].entry["customType"].(string); c == "familiar.fork.v1" {
					newer = true
					break
				}
			}
			if newer {
				continue
			}
			data, _ := x.entry["data"].(map[string]any)
			parent, _ := data["parentSessionId"].(string)
			branch, _ := data["branchEntryId"].(string)
			if parent == "" {
				if p, _ := x.header["parentSession"].(string); p != "" {
					_ = tx.QueryRow(`SELECT id FROM sessions WHERE source_path=?`, p).Scan(&parent)
					parent = strings.TrimPrefix(parent, "pi:")
				}
			}
			if parent != "" && branch != "" {
				if _, err = tx.Exec(`INSERT OR IGNORE INTO edges(child_id,parent_id,edge_type,inferred) SELECT ?,?,'fork',0 WHERE EXISTS(SELECT 1 FROM turns WHERE id=?)`, x.id, "pi:"+parent+":"+branch, "pi:"+parent+":"+branch); err != nil {
					return err
				}
			}
			continue
		}
		if typ, _ := x.entry["type"].(string); typ == "message" || typ == "custom_message" {
			msg, _ := x.entry["message"].(map[string]any)
			if typ == "custom_message" && msg == nil {
				msg = x.entry
			}
			if c, _ := msg["customType"].(string); c != "familiar.merge.v1" {
				continue
			}
			details, _ := msg["details"].(map[string]any)
			fork, _ := details["forkSessionId"].(string)
			last, _ := details["lastEntryId"].(string)
			from := "pi:" + fork + ":" + last
			if fork != "" && last != "" {
				var exists int
				_ = tx.QueryRow(`SELECT count(*) FROM turns WHERE id=?`, from).Scan(&exists)
				if exists > 0 {
					if _, err = tx.Exec(`INSERT OR IGNORE INTO edges(child_id,parent_id,edge_type,inferred) VALUES(?,?,'merge',0)`, x.id, from); err != nil {
						return err
					}
					if _, err = tx.Exec(`UPDATE parts SET from_turn=? WHERE turn_id=?`, from, x.id); err != nil {
						return err
					}
				}
			}
		}
	}
	return tx.Commit()
}

type forkTurn struct {
	id, entryID, parentID, customType string
	data                              map[string]any
}

// reconcileForkPrefixes removes the path copied by SessionManager when it
// creates a branched session. The parent must be present: otherwise the fork is
// removed from the derived index and retried on a later pass.
func reconcileForkPrefixes(db *sql.DB) error {
	rows, err := db.Query(`SELECT id,source_path,meta_json FROM sessions ORDER BY id`)
	if err != nil {
		return err
	}
	type session struct{ id, path, meta string }
	var sessions []session
	for rows.Next() {
		var s session
		if err = rows.Scan(&s.id, &s.path, &s.meta); err != nil {
			rows.Close()
			return err
		}
		sessions = append(sessions, s)
	}
	if err = rows.Close(); err != nil {
		return err
	}

	for _, s := range sessions {
		var h header
		_ = json.Unmarshal([]byte(s.meta), &h)
		turnRows, err := db.Query(`SELECT id,meta_json FROM turns WHERE session_id=? ORDER BY seq`, s.id)
		if err != nil {
			return err
		}
		var turns []forkTurn
		marker := -1
		for turnRows.Next() {
			var t forkTurn
			var raw string
			if err = turnRows.Scan(&t.id, &raw); err != nil {
				turnRows.Close()
				return err
			}
			var e entry
			if json.Unmarshal([]byte(raw), &e) == nil && e.Type != "session" {
				t.entryID, t.customType = e.ID, e.CustomType
				if e.ParentID != nil {
					t.parentID = *e.ParentID
				}
				_ = json.Unmarshal(e.Data, &t.data)
			}
			turns = append(turns, t)
			if t.customType == "familiar.fork.v1" {
				marker = len(turns) - 1 // The final marker belongs to this fork.
			}
		}
		if err = turnRows.Close(); err != nil {
			return err
		}
		if marker < 0 && h.Parent == "" {
			continue
		}

		parentSID := ""
		if marker >= 0 {
			if parent, _ := turns[marker].data["parentSessionId"].(string); parent != "" {
				parentSID = "pi:" + parent
			}
		}
		var parentPath string
		if parentSID != "" {
			err = db.QueryRow(`SELECT source_path FROM sessions WHERE id=?`, parentSID).Scan(&parentPath)
		} else {
			err = db.QueryRow(`SELECT id,source_path FROM sessions WHERE source_path=?`, h.Parent).Scan(&parentSID, &parentPath)
		}
		if errors.Is(err, sql.ErrNoRows) {
			// Do not guess which copied entries are inherited. Removing both the
			// session and checkpoint makes the unchanged file retryable.
			tx, e := db.Begin()
			if e != nil {
				return e
			}
			if _, e = tx.Exec(`DELETE FROM sessions WHERE id=?`, s.id); e == nil {
				_, e = tx.Exec(`DELETE FROM import_state WHERE path=?`, s.path)
			}
			if e == nil {
				_, e = tx.Exec(`DELETE FROM import_errors WHERE path=?`, s.path)
			}
			if e != nil {
				tx.Rollback()
				return e
			}
			if e = tx.Commit(); e != nil {
				return e
			}
			continue
		}
		if err != nil {
			return err
		}
		if marker >= 0 {
			hasPrefix := false
			for i := 0; i < marker; i++ {
				hasPrefix = hasPrefix || turns[i].entryID != ""
			}
			if !hasPrefix { // Already cleaned on an earlier reconciliation.
				continue
			}
		}
		parentIDs, err := piEntryIDs(parentPath)
		if err != nil {
			return fmt.Errorf("read parent session %s: %w", parentPath, err)
		}

		limit := len(turns)
		if marker >= 0 {
			limit = marker
		}
		var inherited []string
		firstOwn := marker
		for i := 0; i < limit; i++ {
			if turns[i].entryID == "" { // Header system-prompt pseudo-turn.
				continue
			}
			if parentIDs[turns[i].entryID] {
				inherited = append(inherited, turns[i].id)
				continue
			}
			if marker < 0 { // Without a marker, only the shared leading path is inherited.
				firstOwn = i
				break
			}
		}

		tx, err := db.Begin()
		if err != nil {
			return err
		}
		for _, id := range inherited {
			if _, err = tx.Exec(`DELETE FROM turns WHERE id=?`, id); err != nil {
				tx.Rollback()
				return err
			}
		}
		// Header-only Pi forks have no Familiar marker. Their first non-shared
		// entry's parentId is the recorded branch point.
		if marker < 0 && firstOwn >= 0 && firstOwn < len(turns) && turns[firstOwn].parentID != "" {
			parent := parentSID + ":" + turns[firstOwn].parentID
			if _, err = tx.Exec(`INSERT OR IGNORE INTO edges(child_id,parent_id,edge_type,inferred) SELECT ?,?,'fork',0 WHERE EXISTS(SELECT 1 FROM turns WHERE id=?)`, turns[firstOwn].id, parent, parent); err != nil {
				tx.Rollback()
				return err
			}
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func piEntryIDs(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ids := make(map[string]bool)
	r := bufio.NewReader(f)
	for {
		line, readErr := r.ReadBytes('\n')
		if readErr == io.EOF && len(line) > 0 {
			break // Ignore the source's incomplete trailing record.
		}
		if readErr != nil && readErr != io.EOF {
			return nil, readErr
		}
		if len(line) == 0 {
			break
		}
		var e entry
		if json.Unmarshal(bytes.TrimSpace(line), &e) == nil && e.Type != "session" && e.ID != "" {
			ids[e.ID] = true
		}
		if readErr == io.EOF {
			break
		}
	}
	return ids, nil
}

func validateSystemPrompt(raw json.RawMessage) error {
	var data systemPromptData
	if len(raw) == 0 || json.Unmarshal(raw, &data) != nil {
		return errors.New("data must be an object")
	}
	if data.SHA256 == nil {
		return errors.New("data.sha256 is required")
	}
	if len(*data.SHA256) != 64 || strings.IndexFunc(*data.SHA256, func(r rune) bool {
		return !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F'))
	}) != -1 {
		return errors.New("data.sha256 must be a 64-character hexadecimal string")
	}
	if data.Text == nil {
		return errors.New("data.text is required")
	}
	return nil
}

func skipFile(db *sql.DB, path string, inode, size, mtime int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`DELETE FROM import_errors WHERE path=?`, path); err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO import_state(path,inode,size,mtime_ns,byte_offset,last_entry_id,session_id,imported_at) VALUES(?,?,?,?,?,NULL,NULL,?)
		ON CONFLICT(path) DO UPDATE SET inode=excluded.inode,size=excluded.size,mtime_ns=excluded.mtime_ns,byte_offset=excluded.byte_offset,last_entry_id=NULL,session_id=NULL,imported_at=excluded.imported_at`, path, inode, size, mtime, size, now())
	if err != nil {
		return err
	}
	return tx.Commit()
}

func checkpoint(db *sql.DB, path string, inode, size, mtime, offset int64, last, sid string) error {
	_, err := db.Exec(`UPDATE import_state SET inode=?,size=?,mtime_ns=?,byte_offset=?,last_entry_id=coalesce(nullif(?,''),last_entry_id),imported_at=? WHERE path=?`, inode, size, mtime, offset, last, now(), path)
	return err
}
func recordError(db *sql.DB, path string, at int64, msg string, inode, size, mtime, offset int64, sid string) error {
	tx, e := db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.Exec(`INSERT INTO import_errors(path,byte_offset,error,occurred_at) VALUES(?,?,?,?) ON CONFLICT(path,byte_offset) DO UPDATE SET error=excluded.error,occurred_at=excluded.occurred_at`, path, at, msg, now()); e != nil {
		return e
	}
	if sid != "" {
		if _, e = tx.Exec(`UPDATE import_state SET inode=?,size=?,mtime_ns=?,byte_offset=?,imported_at=? WHERE path=?`, inode, size, mtime, offset, now(), path); e != nil {
			return e
		}
	}
	return tx.Commit()
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// Stats is the compact set printed by the stats command.
type Stats struct {
	Sessions, Turns, Parts, SystemPrompts, LiveLeaves, Skipped, ImportErrors int64
	Edges                                                                    map[string]int64
	LastImport                                                               string
}

func ReadStats(db *sql.DB) (Stats, error) {
	s := Stats{Edges: map[string]int64{}}
	for _, q := range []struct {
		sql string
		dst *int64
	}{{`SELECT count(*) FROM sessions`, &s.Sessions}, {`SELECT count(*) FROM turns`, &s.Turns}, {`SELECT count(*) FROM parts`, &s.Parts},
		{`SELECT count(DISTINCT json_extract(body_json,'$.data.sha256')) FROM parts WHERE source='system' AND json_extract(body_json,'$.customType')='familiar.system-prompt.v1'`, &s.SystemPrompts},
		{`SELECT count(*) FROM live_branches`, &s.LiveLeaves}, {`SELECT count(*) FROM import_state WHERE session_id IS NULL`, &s.Skipped}, {`SELECT count(*) FROM import_errors`, &s.ImportErrors}} {
		if err := db.QueryRow(q.sql).Scan(q.dst); err != nil {
			return s, err
		}
	}
	rows, err := db.Query(`SELECT edge_type,count(*) FROM edges GROUP BY edge_type ORDER BY edge_type`)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var n int64
		if err = rows.Scan(&k, &n); err != nil {
			return s, err
		}
		s.Edges[k] = n
	}
	err = db.QueryRow(`SELECT coalesce((SELECT last_import_at FROM import_runs WHERE singleton=1),'never')`).Scan(&s.LastImport)
	return s, err
}

func parseHandoffTime(name string) (time.Time, error) {
	base := strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
	// ISO timestamp with filename-safe punctuation: 2026-08-27T03-14-22-886Z.
	if len(base) != 24 || base[10] != 'T' || base[13] != '-' || base[16] != '-' || base[19] != '-' || base[23] != 'Z' {
		return time.Time{}, errors.New("bad handoff timestamp")
	}
	iso := base[:13] + ":" + base[14:16] + ":" + base[17:19] + "." + base[20:]
	return time.Parse(time.RFC3339Nano, iso)
}

type handoff struct {
	path, body string
	ts         time.Time
}
type sessionPoint struct {
	id, started, first, last string
	t                        time.Time
}

func importHandoffs(db *sql.DB, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("scan handoffs: %w", err)
	}
	var hs []handoff
	for _, de := range entries {
		if de.IsDir() || strings.ToLower(filepath.Ext(de.Name())) != ".md" {
			continue
		}
		t, e := parseHandoffTime(de.Name())
		if e != nil {
			continue
		}
		p := filepath.Join(dir, de.Name())
		b, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		hs = append(hs, handoff{p, string(b), t})
	}
	sort.Slice(hs, func(i, j int) bool { return hs[i].ts.Before(hs[j].ts) })
	rows, err := db.Query(`SELECT id,started_at FROM sessions WHERE source_format='pi' ORDER BY started_at,id`)
	if err != nil {
		return err
	}
	var ss []sessionPoint
	for rows.Next() {
		var s sessionPoint
		if err = rows.Scan(&s.id, &s.started); err != nil {
			rows.Close()
			return err
		}
		s.t, _ = time.Parse(time.RFC3339Nano, s.started)
		if err = db.QueryRow(`SELECT id FROM turns WHERE session_id=? ORDER BY seq LIMIT 1`, s.id).Scan(&s.first); err != nil && err != sql.ErrNoRows {
			rows.Close()
			return err
		}
		if err = db.QueryRow(`SELECT id FROM turns WHERE session_id=? ORDER BY seq DESC LIMIT 1`, s.id).Scan(&s.last); err != nil && err != sql.ErrNoRows {
			rows.Close()
			return err
		}
		ss = append(ss, s)
	}
	rows.Close()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`DELETE FROM edges WHERE edge_type='handoff' AND inferred=1`); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM parts WHERE source='handoff'`); err != nil {
		return err
	}
	// Most handoffs are recorded in-session: /clear writes a Pi compaction whose
	// summary is the handoff text, and the parent chain already carries
	// continuity. Index those (and any custom entry carrying the text) once, so a
	// recorded handoff is never re-attached or re-linked by the heuristic.
	recorded := map[string]bool{}
	cr, err := tx.Query(`SELECT body_json FROM parts WHERE source IN ('pi:compaction','pi:custom_message','pi:custom','user')`)
	if err != nil {
		return err
	}
	for cr.Next() {
		var raw string
		_ = cr.Scan(&raw)
		if !strings.Contains(raw, "Handoff") && !strings.Contains(raw, "handoff") {
			continue
		}
		collectStrings(raw, recorded)
	}
	cr.Close()
	for _, h := range hs {
		if recorded[h.body] || recorded[strings.TrimSpace(h.body)] {
			continue
		}
		recv := -1
		for i := range ss {
			if !ss[i].t.Before(h.ts) && ss[i].first != "" {
				recv = i
				break
			}
		}
		if recv < 0 {
			continue
		}
		var prev = -1
		for i := 0; i < recv; i++ {
			if ss[i].last != "" {
				prev = i
			}
		}
		bodyJSON, _ := json.Marshal(h.body)
		{
			var idx int
			if err = tx.QueryRow(`SELECT coalesce(max(idx),-1)+1 FROM parts WHERE turn_id=?`, ss[recv].first).Scan(&idx); err != nil {
				return err
			}
			if _, err = tx.Exec(`INSERT INTO parts(turn_id,idx,source,body_json) VALUES(?,?,'handoff',?)`, ss[recv].first, idx, string(bodyJSON)); err != nil {
				return err
			}
		}
		if prev >= 0 {
			if _, err = tx.Exec(`INSERT OR IGNORE INTO edges(child_id,parent_id,edge_type,inferred) VALUES(?,?,'handoff',1)`, ss[recv].first, ss[prev].last); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// collectStrings adds every decoded string value in raw (and its trimmed form) to set.
func collectStrings(raw string, set map[string]bool) {
	var v any
	if json.Unmarshal([]byte(raw), &v) != nil {
		return
	}
	var walk func(any)
	walk = func(x any) {
		switch z := x.(type) {
		case string:
			set[z] = true
			set[strings.TrimSpace(z)] = true
		case []any:
			for _, y := range z {
				walk(y)
			}
		case map[string]any:
			for _, y := range z {
				walk(y)
			}
		}
	}
	walk(v)
}

func jsonContains(raw, want string) bool {
	var v any
	if json.Unmarshal([]byte(raw), &v) != nil {
		return false
	}
	var walk func(any) bool
	walk = func(x any) bool {
		switch z := x.(type) {
		case string:
			return z == want
		case []any:
			for _, y := range z {
				if walk(y) {
					return true
				}
			}
		case map[string]any:
			for _, y := range z {
				if walk(y) {
					return true
				}
			}
		}
		return false
	}
	return walk(v)
}

// FormatStats returns the intentionally small human-readable report.
func FormatStats(s Stats) string {
	var b strings.Builder
	fmt.Fprintf(&b, "sessions: %d\nturns: %d\nparts: %d\nsystem prompts: %d\n", s.Sessions, s.Turns, s.Parts, s.SystemPrompts)
	keys := make([]string, 0, len(s.Edges))
	for k := range s.Edges {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		b.WriteString("edges: 0\n")
	} else {
		b.WriteString("edges:")
		for _, k := range keys {
			fmt.Fprintf(&b, " %s=%d", k, s.Edges[k])
		}
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "live leaves: %d\nskipped files: %d\nimport errors: %d\nlast import: %s\n", s.LiveLeaves, s.Skipped, s.ImportErrors, s.LastImport)
	return b.String()
}
