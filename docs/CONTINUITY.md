# Continuity model (Milestone 0)

The continuity database is a **derived, read-only index**. Pi session JSONL is
the source of truth: Pi never waits for this service, and deleting the SQLite
file and running the importer again is always safe. M0 has no daemon or API.

## Importing

```console
familiar-services continuity import --sessions DIR [--sessions DIR ...] --handoffs DIR --db FILE
familiar-services continuity stats --db FILE
```

The importer recursively finds `.jsonl` files under every `--sessions` root;
the option may be repeated (for example, for primary and fork session trees).
A file whose first complete JSON line does not have `type: "session"` is counted
as skipped rather than malformed; this excludes fork-local `log.jsonl` files.
`import_state` records each absolute source path's inode, observed size and mtime,
last committed byte offset, and last entry ID. Every complete JSONL line and its checkpoint are
committed atomically. A final unterminated line is treated as an in-progress Pi
append and retried later. A changed inode or a file shorter than its checkpoint
causes that file's session to be deleted and imported again. Sessions whose
source files disappeared are removed from the index. Re-running without changes
is a no-op.

`schema_version` is deliberately simple. A version mismatch drops the derived
schema and rebuilds it. Complete malformed lines are checkpointed in
`import_errors`; `stats` reports their count and the last successful import pass.

## Source identity and faithful storage

`sessions.source_format` identifies the producer (`pi`, later `claude-code`,
`openai-chat`, and so on). Only Pi is supported in M0. Session IDs are
`pi:<header id>`, and turn IDs are `pi:<session id>:<entry id>`, so IDs are
stable across catch-up runs and source formats cannot collide.

Every Pi tree entry is represented as a turn. Message content arrays become
ordered parts; **`parts.body_json` is the exact raw JSON slice from the source
block**, including its original whitespace and payload. The raw entry envelope
is also retained in `turns.meta_json`, preserving usage, tool metadata, model
metadata, compaction details, and fields unknown to this importer. Non-message
entries are attributed parts (`pi:model_change`, `pi:compaction`,
`pi:branch_summary`, `pi:custom`, `pi:custom_message`, `pi:label`,
`pi:session_info`, etc.) rather than being dropped. Message roles are projected
to `role`/`source`; tool results and shell executions use source `tool`.

The model was checked against Pi 0.84.2's `session-manager` implementation and
a real session produced by the Pi harness. Version 3 has a header without tree
identity, followed by entries with an eight-character `id`, nullable `parentId`,
and ISO timestamp. `/tree` does not append a navigation event by itself: the
next entry points at the selected ancestor; `branch_summary` additionally
records the abandoned tip in `fromId`. Compaction, model/thinking changes,
custom state/messages, labels, and session info all use the same tree links.
Every recorded `parentId` becomes a non-inferred `continue` edge. When a parent
already has a child, the later child also gets a non-inferred `fork` edge to
that parent: this is Pi's durable representation of `/tree` navigation. A
`branch_summary.fromId` remains faithfully available in the raw attributed part
but is not treated as ancestry (it identifies the abandoned tip, not the split).

After all files are scanned, reconciliation projects explicit peer lifecycle
records. Pi branch files copy the parent's path with the same entry IDs; entries
in that prefix are inherited and are not stored again under the fork session.
A `familiar.fork.v1` marker (or the fork header's `parentSession`) links the
first suffix turn to the recorded parent branch entry with a `fork` edge. If the
parent has not arrived, the fork is deferred and retried on a later pass rather
than guessing. A parent `familiar.merge.v1` custom message links to the recorded
fork leaf with a `merge` edge and sets `parts.from_turn`. For historical compatibility only, a `familiar.branch-close.v1` marker projects `kind='branch_close'`; current writers never emit one. References
whose session/turn has not arrived remain unresolved and are retried on every
import pass; the importer never substitutes a timestamp or nearest leaf.

### System prompts

The Familiar Pi extension records the exact system prompt in a Pi custom entry
with custom type `familiar.system-prompt.v1` and data containing its `sha256`
and `text`. It writes the entry on the session's first turn and whenever the
prompt changes. The importer keeps the raw entry envelope verbatim in
`parts.body_json`, projects the part's source as `system`, and attaches it to
the turn created by that entry. Missing or malformed prompt data remains a raw
`pi:custom` part and is also reported in `import_errors`, so source material is
never dropped. Other Pi custom entries are unchanged. Sessions created before
this mechanism was introduced have no recorded system prompt.

## Edges and leaves

| type | meaning |
|---|---|
| `continue` | exact Pi `parentId` ancestry |
| `fork` | a later Pi child branching from an already-used parent |
| `merge` | reserved for a source-recorded merge |
| `handoff` | timestamp-matched cross-session continuity |

`edges.inferred` is zero for facts in source data and one for guessed links.
The `live_branches` view is the set of turns with no `continue`/`fork` child and
which are not explicit `branch_close` turns. Metadata is intentionally visible:
the mirror does not silently discard a persisted leaf.

## Handoff heuristic

Handoff files must use filename-safe ISO UTC names such as
`2026-08-27T03-14-22-886Z.md`. For each handoff, sessions are ordered by their
header timestamps. The receiving session is the first session starting at or
after the handoff timestamp; its first persisted turn is linked to the last
persisted turn of the immediately preceding session. The `handoff` edge has
`inferred=1`, because Pi did not record it. The Markdown is stored as a JSON
string in a `handoff` part on the receiving turn unless an existing source part
contains that exact decoded string. Handoff projections are recomputed each
pass, so newly arrived older sessions can improve the match.

This is intentionally a timestamp heuristic. Operators should validate clock
ordering and filename conventions against the real archive before relying on
cross-session ancestry.
