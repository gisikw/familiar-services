-- Continuity graph (proposal). SQLite.
CREATE TABLE sessions (
  id          TEXT PRIMARY KEY,
  started_at  TEXT NOT NULL,
  host        TEXT,             -- where it ran (server, laptop-on-a-plane, …)
  model       TEXT,
  label       TEXT
);

CREATE TABLE turns (
  id          TEXT PRIMARY KEY,
  session_id  TEXT NOT NULL REFERENCES sessions(id),
  seq         INTEGER NOT NULL, -- order within session
  ts          TEXT NOT NULL,
  role        TEXT NOT NULL,    -- user | assistant | system
  kind        TEXT NOT NULL DEFAULT 'turn', -- turn | branch_close
  meta_json   TEXT,             -- e.g. close reason, interrupted-by-filter marker
  UNIQUE (session_id, seq)
);

CREATE TABLE parts (
  turn_id     TEXT NOT NULL REFERENCES turns(id),
  idx         INTEGER NOT NULL,
  source      TEXT NOT NULL,    -- user | assistant | merge | handoff | wake | system | tool
  from_turn   TEXT REFERENCES turns(id), -- for merge/handoff parts
  body_json   TEXT NOT NULL,    -- raw content block as seen
  PRIMARY KEY (turn_id, idx)
);

CREATE TABLE edges (
  child_id    TEXT NOT NULL REFERENCES turns(id),
  parent_id   TEXT NOT NULL REFERENCES turns(id),
  edge_type   TEXT NOT NULL CHECK (edge_type IN ('continue','fork','merge','handoff')),
  PRIMARY KEY (child_id, parent_id)
);
CREATE INDEX edges_parent ON edges(parent_id);

-- Live branches: leaves that neither merged nor closed.
CREATE VIEW live_branches AS
SELECT t.* FROM turns t
WHERE NOT EXISTS (SELECT 1 FROM edges e WHERE e.parent_id = t.id)
  AND t.kind <> 'branch_close';
