# familiar-services

The singleton layer of Familiar: everything that must exist **exactly once**
regardless of how many Familiar Pi processes are running.

> Status: Milestone 0 provides a read-only continuity mirror and its NixOS import units.

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
  ├─ wakes        durable scheduled wakes
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
- **Every fork ends.** A branch closes by merging or by an explicit close
  record. A branch that simply stops is a bug, and continuity reports it.
- **Record faithfully, project cleanly.** Store what actually happened;
  derive clean views at read time.
- Go, stdlib-first, SQLite. Nix flake for build and devshell.

See `docs/ARCHITECTURE.md`, `docs/CONTINUITY.md`, and `docs/MIGRATION.md`.
