-- Continuity derived index. SQLite. Schema version 1.
PRAGMA foreign_keys = ON;

CREATE TABLE schema_version (
  version INTEGER NOT NULL
);
INSERT INTO schema_version(version) VALUES (1);

CREATE TABLE sessions (
  id            TEXT PRIMARY KEY, -- source-format namespaced session ID
  source_format TEXT NOT NULL,    -- pi | claude-code | openai-chat | ...
  source_path   TEXT NOT NULL UNIQUE,
  started_at    TEXT NOT NULL,
  host          TEXT,
  model         TEXT,
  label         TEXT,
  meta_json     TEXT
);

CREATE TABLE turns (
  id          TEXT PRIMARY KEY,   -- source entry ID, namespaced by session
  session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  seq         INTEGER NOT NULL,   -- append order within the source file
  ts          TEXT NOT NULL,
  role        TEXT NOT NULL,      -- user | assistant | system | tool | metadata
  kind        TEXT NOT NULL DEFAULT 'turn',
  meta_json   TEXT,               -- raw source entry envelope
  UNIQUE (session_id, seq)
);

CREATE TABLE parts (
  turn_id     TEXT NOT NULL REFERENCES turns(id) ON DELETE CASCADE,
  idx         INTEGER NOT NULL,
  source      TEXT NOT NULL,      -- user | assistant | handoff | system | tool | pi:<entry type>
  from_turn   TEXT REFERENCES turns(id) ON DELETE SET NULL,
  body_json   TEXT NOT NULL,      -- raw source content block, byte-for-byte
  PRIMARY KEY (turn_id, idx)
);

CREATE TABLE edges (
  child_id    TEXT NOT NULL REFERENCES turns(id) ON DELETE CASCADE,
  parent_id   TEXT NOT NULL REFERENCES turns(id) ON DELETE CASCADE,
  edge_type   TEXT NOT NULL CHECK (edge_type IN ('continue','fork','merge','handoff')),
  inferred    INTEGER NOT NULL DEFAULT 0 CHECK (inferred IN (0,1)),
  PRIMARY KEY (child_id, parent_id, edge_type)
);
CREATE INDEX edges_parent ON edges(parent_id);

CREATE TABLE import_state (
  path          TEXT PRIMARY KEY,
  inode         INTEGER NOT NULL,
  size          INTEGER NOT NULL,
  mtime_ns      INTEGER NOT NULL,
  byte_offset   INTEGER NOT NULL,
  last_entry_id TEXT,
  session_id    TEXT REFERENCES sessions(id) ON DELETE SET NULL,
  imported_at   TEXT NOT NULL
);

CREATE TABLE import_errors (
  path       TEXT NOT NULL,
  byte_offset INTEGER NOT NULL,
  error      TEXT NOT NULL,
  occurred_at TEXT NOT NULL,
  PRIMARY KEY (path, byte_offset)
);

CREATE TABLE import_runs (
  singleton       INTEGER PRIMARY KEY CHECK (singleton = 1),
  last_import_at  TEXT NOT NULL
);

-- Leaves in Pi's persisted entry tree. Metadata entries remain visible because
-- dropping them would make the mirror lossy.
CREATE VIEW live_branches AS
SELECT t.* FROM turns t
WHERE NOT EXISTS (SELECT 1 FROM edges e WHERE e.parent_id = t.id AND e.edge_type IN ('continue','fork'))
  AND t.kind <> 'branch_close';
