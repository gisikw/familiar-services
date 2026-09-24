# Continuity model (Milestone 0)

The continuity database is a **derived, read-only index**. Pi session JSONL is
the source of truth: Pi never waits for this service, and deleting the SQLite
file and running the importer again is always safe. M0 has no daemon or API.

## Importing

```console
familiar-services continuity import --sessions DIR --handoffs DIR --db FILE
familiar-services continuity stats --db FILE
```

The importer recursively finds `.jsonl` files. `import_state` records each
absolute source path's inode, observed size and mtime, last committed byte
offset, and last entry ID. Every complete JSONL line and its checkpoint are
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

### Known system-prompt gap

Pi's v3 session header and entry union do **not** persist the system prompt, and
the real harness session inspected for M0 did not contain it. The importer does
not invent one. It will preserve a `systemPrompt` header field as a turn-0
`system` part if a producer supplies that field, but ordinary current Pi
sessions therefore cannot reconstruct historical system prompts. This
supersedes the earlier assumption that prompts were always persisted.

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
