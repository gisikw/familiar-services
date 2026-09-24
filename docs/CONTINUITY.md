# Continuity model (decided 2026-09-23)

The continuity store is an **append-only historical record** of every
conversational turn across every Familiar session and fork. It is the substrate
for Kes's continuity, not (yet) a source Pi builds context from.

## Tables

See `schema/continuity.sql`.

- **turns** — one row per turn: session, timestamp, role, kind.
- **parts** — ordered, attributed pieces of a turn. A turn is not a blob of
  text; it is a list of parts each with a `source` (`user`, `assistant`,
  `merge`, `handoff`, `wake`, `system`, `tool`, …).
- **edges** — ancestry. A turn may have more than one parent.

## Edge types

| type | meaning |
|---|---|
| `continue` | ordinary next turn |
| `fork` | first turn of a branch → the turn it split from |
| `merge` | the turn receiving a merge note → the merging branch's tip |
| `handoff` | a session's turn 0 → the prior session's last turn (may be two) |

A turn with no parents exists only at the true origin or for imported history.

## Branch endings

Every branch ends in a `merge` edge out of its tip or a `branch_close` turn
with a reason. A leaf that is neither is a **live branch**; old live branches
surface in attention. That query is the entire enforcement mechanism.

## Faithful vs clean

Store what Kes actually saw. When a merge note lands in the same turn as
Kevin's message, it becomes a separate attributed part, not text spliced into
his words and not omitted:

```json
[
  {"source": "merge", "from_turn": "t_fork_tip", "text": "<system-note>…</system-note>"},
  {"source": "user", "text": "Hah, you're funny when you're like this 😘"}
]
```

- **Faithful view:** all parts. Used for continuity, debugging, "what did Kes see".
- **Clean view:** only human/assistant-authored parts. Derived, never stored.

Hide at read time, never at write time.
