# Migration sources (proposal)

| Service | Comes from |
|---|---|
| continuity | handoffs from `familiar/packages/continuity` (retire the file store); Pi session JSONL for backfill. Canon/identity does **not** migrate here: it stays a config-pointed file/dir |
| attention | `familiar/integrations/pi/extensions/worklist` (policy, store, DND); `imp attn` |
| wakes | the wake tool's durable registry in the Pi integration |
| fleet | `familiar-fleet`, golem capabilities/dispatch, `imp agent` |
| projects | projects viewer in familiar-ui |
| router | stays its own project (tiamat-router); not migrating |

## Daily briefing

The standalone daily-briefing agent retires. Her work becomes a scheduled wake
that forks a background Familiar to read recent history and maintain the
timeline of salient events. Her name stays on that wake's description so her
contribution remains visible in the record.

## Order

See ROADMAP.md.
