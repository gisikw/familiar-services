# Roadmap

The canonical sequence for moving Familiar to this architecture. Each milestone
ships on its own, leaves the system working, and **deletes more than it adds**.
If a milestone fails that test, it is making a nicer maze, not a smaller harness.

Tracked as cards on the Attention board `familiar-services`.

## Done

- **Deletion pass (2026-09-23).** Removed Background Exo runtime, Plate/`imp plate`,
  canon/continuity file store, private extension, old Familiar Agents + gateway fleet
  registry (familiar `5a5a709`), their familiar-ui surfaces (`4683356`), fort-nix Plate
  wiring, and the claude-cache-gateway repo. ~29k lines.
- **M0 — Read-only continuity mirror (2026-09-24).** `familiar-services continuity
  import|stats`; derived SQLite index of Pi session JSONL, rebuilt from source, never in
  Pi's path. Deployed on azula via fort.tracked + 1-minute timer
  (`/var/lib/familiar-continuity/continuity.db`).

## Next

### M1 — Complete record; retire handoff files
- Identity extension writes the exact system prompt into the session (custom entry) at
  session start and whenever it changes; importer maps it to a turn-0 `system` part.
- Handoff extension reads the latest handoff from session history (the `/clear`
  compaction entry), not from `FAMILIAR_HANDOFF_PATH`. Delete the file writer/reader and
  the config key. Existing files stay in the kestrel repo as history.
- Check first: whether `/clear` compacts in place or starts a new session file.
- Validate with a real `/clear` before pushing — this is the memory path.

### M2 — Attention, DND, wakes become singletons
- First time familiar-services runs as a long-lived service (systemd unit, Unix socket).
- Move Attention store, worklist/DND, and the wake registry out of the resident Pi;
  `imp attn` / wakes point at the service.
- One maintenance window: freeze, import, switch, resume. No dual-writing.

### M3 — Familiars as systemd units
- `familiar-pi@<id>` templated units; no service parents a Pi. Presence keeps the TTY.
- Gateway becomes auth + client protocol + proxy; drops `presence.sh ensure`.
- Only one path left that can restart a Familiar.

### M4 — Peer forks and merge; Exo's briefing wake
- Fork = new `familiar-pi@` unit branched from a turn; same prompt, tools, imp.
- Clients may talk to live forks directly. Every fork ends in a recorded merge
  (attributed part + `merge` edge); stale live leaves surface in Attention.
- Then retire the standalone daily-briefing (Kobold workflow in hoard) in favor of a
  scheduled wake that forks a Familiar to maintain the timeline, named for Exo.

### Later / independent
- **Projects:** fold the viewer into a service API or leave as-is; decide when touched.
- **Router:** stays its own project (general-purpose). Not moving into this repo.
- **Pre-Pi history:** importers for Claude Code / OpenAI-shaped archives
  (`sessions.source_format`); schema already accommodates them.

## Standing decisions
See ARCHITECTURE.md and CONTINUITY.md. In short: Pi session JSONL is the source of truth
and the DB is a derived index; identity is a config-pointed file; no encryption of the
record (not in version control); Golem stays; Presence stays.
