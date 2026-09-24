# Migration plan

## Rules of engagement

- Ship vertically; every milestone leaves production usable and has one named authority per datum.
- Migrations are **freeze → import/checksum → switch all clients → archive old store**, never indefinite dual-write.
- Stable source IDs and idempotency keys make every import/call safely repeatable. A rollback switches binaries/endpoints, not data back into an already-retired writer.
- `familiar-services` never parents Pi. It asks systemd to operate allow-listed `familiar-pi@<escaped-id>` units. Gateway proxies/authenticates; it does not own singleton state.
- Do router and process cutovers independently. Do not combine a Pi restart, router restart and state-authority change in one deployment.

## Milestones

### 0 — A useful continuity mirror (S, a few evenings)

**Visible result:** `imp continuity status|sessions|leaves` shows imported Kestrel history, inferred links, stale leaves and import errors. This turns the proposal into a running binary without changing the live Familiar.

**Scope**

- Implement service bootstrap, state-dir permissions, health/readiness, Unix-socket framed JSON API and `continuity.sqlite` migrations.
- Harden the schema before data: actor/producer attribution, branch/import provenance, idempotency key, interrupted marker, immutable created timestamp, schema version; graph constraints and tests for continue/fork/merge/handoff/close.
- Read-only import Pi session JSONL, handoff Markdown and old Background branch JSONL. Store raw blocks; mark inferred ancestry explicitly. Record import file identity/checksum/checkpoint. Generate faithful/clean projections and orphan/live-leaf report.
- Add only continuity read commands to `imp`; keep its existing resident socket routing for other areas.

**Delete:** nothing live. No canon import except as clearly labeled legacy notes; identity stays a file.

**Cutover / rollback:** deploy `familiar-services` with continuity API disabled for writes. Rollback stops the new unit and removes/rebuilds only its derived DB. Sources remain untouched.

**Verify:** fixture + copy-of-production dry run; imported session/turn/part counts; deterministic rerun; sampled byte-for-byte parts; no source mtimes change; graph invariant query; DB backup/restore; status command is visibly useful.

**Retires:** no authority yet; retires uncertainty about historical shape and validates the service/API/deployment skeleton.

### 1 — Live continuity; retire handoff files (M)

**Scope**

- Add an acknowledged, idempotent Pi continuity adapter that emits the exact system prompt as turn 0 and exact user/assistant/tool/custom parts at durable boundaries. Gateway metadata may enrich attribution but never supply model-visible content.
- Add session/open/close and fork/merge/handoff operations. Record interrupted streams faithfully. Alert on old unexplained live leaves.
- Choose confidentiality before capture: recommended age-encrypted part bodies with externally supplied key; include WAL/temp/export/backup policy and private-span behavior.
- Replace `/clear` handoff file read/write with a handoff turn + edge. Provide derived Markdown export only.

**Delete:** handoff writer/reader and `FAMILIAR_HANDOFF_*`; `packages/continuity` canon/handoff runtime; canon terminology. Archive old files read-only after reconciliation. Subconscious file either becomes explicit attention/wakes later or is disabled by Kevin's decision.

**Cutover / rollback:** shadow-capture a bounded test session and compare against Pi JSONL, then freeze handoff writer, final import, enable DB writer, start a fresh Pi session. Rollback may restore old handoff code only before any DB-only handoff; afterward fix forward and export compatibility Markdown.

**Verify:** kill service/Pi at each append boundary; replay same IDs; exact turn-0 prompt; tool/merge attribution; no duplicate turn; clean view is derived; every created fork test ends merge/close.

**Retires:** Markdown handoffs and continuity/canon file store as authorities.

### 2 — Attention, DND and wakes become singletons (M)

**Scope**

- Implement `attention.sqlite` and `wakes.sqlite`, preserving Attention revisions/events, worklist priority/ack semantics, DND expiry, wake modes and fired history.
- Define wake delivery states (`scheduled`, `claimed`, `delivered/observed`, `cancelled`, `failed`) and idempotent target turn IDs; do not repeat the old claim-before-send ambiguity silently.
- Move the complete `imp attn` contract to the service socket; add `imp wake`. Gateway exposes the same API and projects service state to familiar-ui.
- One maintenance window: pause synthetic delivery, import Attention DB + worklist/DND + wakes, compare, switch `imp`, UI and delivery adapter, then resume.

**Delete:** resident Attention DB/store, worklist filesystem owner, process-global DND seam, Pi wake registry/timers, `imp plate`, migrated Plate files and deployment variables. Keep only thin Pi delivery/UI adapters until Milestone 3.

**Cutover / rollback:** retain immutable snapshots. Before first new write, endpoint rollback is safe. Afterward restore the snapshots into service and fix forward—never reactivate old writers with divergent mutations.

**Verify:** revision conflict tests; DND across multiple Pis; wake survives service/Pi reboot; crash at every delivery transition; old/new count and digest report; UI and `imp` observe the same revision; no writes under old roots.

**Retires:** three resident singleton owners plus stale Plate.

### 3 — Independent Pi units and real gateway (L)

**Scope**

- Ship `familiar-pi@.service`, stable escaped IDs, private runtime dirs and launch config pointing to the identity directory, services socket and router. Ship allow-listed systemd start/stop/status integration with quotas and reconciliation.
- Convert the conventional primary to `familiar-pi@primary`; add fork unit creation and startup handshake. All peers load the same tools/`imp`; only session/branch identity differs.
- Gateway gains authentication, client protocol, live-Pi registry/routing and direct thread selection. It proxies service API and Pi streams. It no longer calls `presence.sh ensure` or owns fleet state.
- Preserve existing UI/Hearth protocol through adapters. Keep terminal viewer as a client if useful.

**Delete:** `services/presence` tmux respawn ownership, Presence child in `services/server`, opportunistic gateway ensure/recovery, per-Pi public familiar-ui descriptor/broker where gateway replacement is proven. Remove old server's authority to restart Pi.

**Cutover / rollback:** deploy units/gateway dark; start `@primary` only after old Presence is stopped and its last turn is recorded. Reverse proxy flips to new gateway. Rollback stops `@primary`, restores old gateway/Presence against the same Pi session archive; do not run both primaries.

**Verify:** restart gateway/services without disturbing a streaming Pi; restart one Pi without affecting peers; auth rejection; attach to each fork; unit-name injection tests; reboot reconciliation; no Pi PID descends from gateway/services; Fort activation does not restart Pi.

**Retires:** old Presence/server supervision topology and in-process public bridge.

### 4 — Correct peer forks/merge, then Exo's briefing wake (L)

This is deliberately after continuity + attention + wakes + spawning.

**Scope**

- Implement fork from a recorded turn: new `familiar-pi@<id>`, same identity/tools/services, `fork` edge and client-visible ephemeral thread. Add quotas/timeouts without subordinating the peer.
- “Ready to merge” is an attention item. Merge injects an attributed merge part into the receiving live branch and records the second parent edge. Close writes `branch_close` with reason. A reconciler surfaces abandoned live branches but never invents closure.
- Rebuild background work as this mechanism, not the old scheduler. Add a scheduled wake named **Daily Briefing — Exo** (the current Hoard prompt says “You are Exo”) that forks a background Familiar to maintain a salient-event timeline.

**Delete:** `packages/background`, Background Pi/UI extension and patched owner-control assumptions; old workstream SQLite/branch runtime state after historical import; Kobold `daily-briefing` workflow and the already-no-op legacy `daily-briefing.service` shim. Keep old briefing artifacts as history/export inputs.

**Cutover / rollback:** first enable manual forks, then merge/close, then one harmless scheduled background fork, then replace daily timer. Rollback disables new fork creation/wake but leaves existing branches visible until explicitly merged/closed; never delete them.

**Verify:** concurrent primary/fork conversation; multi-parent graph; merge text exactly matches seen part; stale-parent merge is explicit; crash/reboot reconciliation; every test branch ends; attention clears only after merge/close; daily wake produces/updates timeline once.

**Retires:** known-wrong Background design and standalone daily-briefing agent.

### 5 — Fleet authority consolidation (L)

**Scope:** move gateway enrollment registry, Familiar Agents ledger/policy/reconciliation and Herdr dispatch into `internal/fleet`; preserve `imp agent`. Migrate generated SSH/Herdr manifests and jobs with stable IDs. Point node-side `familiar-fleet` enrollment at gateway → service. Use golemd only as a transitional adapter.

**Delete:** Pi Agents owner election/SQLite, gateway fleet state, Familiar Golem tools/settlement relay/fallback. Keep node `familiar-fleet`; keep golemd only for proven non-Familiar consumers.

**Cutover / rollback:** stop dispatch admission, let/settle active jobs or mark unresolved, import, compare machine/job/policy sets, switch `imp`, reopen admission. Roll back API adapter only while service remains job authority.

**Verify:** lost-reply idempotency, blocked/steer/cancel/settlement, host-key fencing, node reconnect, service/Pi reboot, no worklist relay duplicate, one job ID in one ledger.

**Retires:** three-way fleet/dispatch ownership.

### 6 — Router package and drained unit (M)

**Scope:** port Tiamat packages with minimal refactoring; preserve routes, encrypted provider/OAuth blobs, client/usage data, captures and image policy. Add drain endpoint/state and `familiar-router.service`. Migrate `tiamat.db` and `credential.key` with separate key custody; rotate/hash plaintext client tokens if protocol permits.

**Delete:** standalone overlay/binary after all clients move; redundant `claude-cache-gateway` in Familiar paths.

**Cutover / rollback:** test against copied DB; run new router on a second private port for catalog/harmless inference comparison (not dual state mutation), stop writes, backup, start authoritative unit, switch clients. Drain old/new before every switch. Rollback restores binary against untouched compatible DB backup and old endpoint.

**Verify:** route/catalog parity, OAuth refresh, ledger/cost, local-only refusal, captures, credential migration, streaming drain under deploy, fleet/Pi clients.

**Retires:** tiamat-router deployable repository in the Familiar stack (repository may remain archived).

### 7 — Projects and cleanup (M)

**Scope:** implement bounded project roots/list/stat/read in `internal/projects` or as fleet views; gateway API feeds familiar-ui. Decide/import standalone annotation DB. Rename UI “file handoff” to project reference. Remove compatibility code, old state variables/units and migration-only importers; publish backup/restore/runbooks.

**Delete:** Pi-hosted `projects-fs`, standalone Projects browser if annotations migrated/not wanted, obsolete familiar-ui bridge/broker remnants, old state archives after retention approval.

**Cutover / rollback:** read-only API comparison first; short annotation freeze/import if retained; switch UI. Old browser can remain read-only during validation, never writable.

**Verify:** traversal/symlink/revision bounds, large/binary files, UI parity, annotation counts, clean host unit graph, disaster restore of each per-service DB.

**Retires:** final project authority duplication and migration scaffolding.

## Kill list

Delete outright, rather than redesigning:

1. Current in-process Background Exo scheduler/runtime and its UI workstream protocol.
2. Canon as identity/continuity concept and the `packages/continuity` file store.
3. Handoff Markdown as live state (retain immutable import/archive only).
4. Legacy Plate and `imp plate`.
5. Presence tmux respawn/supervisor ownership once templated units work.
6. Gateway fleet registry and Pi-local Agent owner after fleet cutover.
7. Familiar's duplicate Golem tools/settlement relay after fleet cutover.
8. Standalone `claude-cache-gateway` in Familiar routing.
9. Standalone daily-briefing agent/service after the named wake succeeds.
10. Any compatibility dual-writer left after its milestone's verification window.

## Critical path

`M0 service/API + history` → `M1 authoritative continuity` → `M2 attention/wakes` → `M3 spawning/gateway` → `M4 peer background forks`. Fleet, router and projects then migrate independently, in that risk order. Do not start a “better Background” before M4's prerequisites exist.
