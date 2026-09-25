# familiar-services

The singleton layer of Familiar: everything that must exist **exactly once**
regardless of how many Familiar Pi processes are running.

> Status: Milestone 2 adds the long-lived local service for Attention and the unified event scheduler; the M0 continuity mirror remains available.

## Why this exists

Background Familiars drifted because singleton state (attention, wakes,
continuity, fleet access) lived inside individual Pi processes. Moving it here
makes a Familiar Pi forkable without tool or environment changes: every fork
sees the same world through the same interface.

```
 Familiar UI   Hearth / other clients
        \        /
      familiar-gateway ── spawns/tracks Familiar Pi processes (primary + forks)
        |                          |
  familiar-services  <──── imp (agentic CLI; the only way a Pi touches singletons)
  ├─ continuity   append-only turn graph (the record of us)
  ├─ attention    what wants Kevin's or Kes's attention, incl. "fork ready to merge"
  ├─ scheduler    durable notifications, future wakes, delivery, and DND
  ├─ fleet        workbox hosts via Herdr (Pi / Claude / Codex)
  ├─ projects     project viewer, probably folds into fleet
  └─ router       inference routing (née tiamat-router)
```

## Principles

- **Singletons live here; Pi processes are disposable.** Anything a fork could
  get wrong by having its own copy belongs in this repo.
- **imp is the agent-facing contract.** Pi never talks to these services except
  through `imp`. The gateway exposes the same capabilities over HTTP to clients.
- **No privileged primary.** "Primary" is a convention, not a role. Forks are
  peers; precedence is decided at merge time.
- **Every fork comes home.** A branch ends only by merging. A fork that simply
  stops is a bug, and continuity reports it; historical close records remain importable.
- **Record faithfully, project cleanly.** Store what actually happened;
  derive clean views at read time.
- Go, stdlib-first, SQLite. Nix flake for build and devshell.

See `docs/ROADMAP.md` for the milestone sequence, plus `docs/ARCHITECTURE.md`, `docs/CONTINUITY.md`, and `docs/MIGRATION.md`.
