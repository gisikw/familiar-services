# Architecture gap analysis

**Snapshot:** 2026-09-23. `familiar` `ee7f960`, `familiar-ui` `007e104` (local fallback), `familiar-fleet` `fb9141a`, `tiamat-router` `6d0573d`. This is a source/deployment audit, not a claim that every optional feature is active. The live Azula/Kestrel wiring in `fort-nix` and the observed `/var/lib/kestrel` layout are called out separately.

The GitHub clone of `familiar-ui` required credentials and failed. The readable fallback `/home/familiar/Projects/familiar-ui` was used; remote freshness could not be verified. The other requested repositories cloned successfully. No repository other than `familiar-services` was modified.

## Executive gap map

| Current authority / surface | Evidence | Target | Disposition |
|---|---|---|---|
| Worklist queue + DND live inside the resident Pi | `familiar/integrations/pi/extensions/worklist/{index.ts,store.ts,PROTOCOL.md}` | `internal/attention`, `imp`, gateway API | Move. Import once, then make the old extension a stateless delivery adapter until removed. |
| Attention is another resident-owned SQLite store | `familiar-ui/packages/extension/src/attention.ts`; `familiar-ui/ATTENTION.md` | `internal/attention` | Move and cut over atomically with worklist/DND. |
| Durable wakes are Pi timers over JSON files | `familiar/integrations/pi/extensions/wake/{index.ts,runtime.ts,store.ts}` | `internal/wakes` | Move scheduling/claims; Pi receives wake turns only. |
| Continuity is split among Pi JSONL, handoff Markdown, subconscious JSON, and an unused canon/handoff package | `familiar/packages/continuity/src/index.ts`; `integrations/pi/extensions/handoff/`; Pi sessions under the configured `PI_CODING_AGENT_DIR` | `internal/continuity` turn graph | Import, then record live. Retire handoff files and the canon abstraction. |
| Two durable dispatch systems plus the divergent in-process Background implementation | `familiar/integrations/pi/extensions/agents/`; `familiar/contrib/familiar/pi/agents/`; `familiar/packages/background/README.md` | `internal/fleet`; later peer Familiar forks | Consolidate dispatch in fleet; delete old Background rather than porting it. |
| Fleet enrollment is owned by the old gateway while dispatch is resident-owned | `familiar/services/gateway/src/fleet.ts`; Agents paths above | `internal/fleet`; gateway is auth/protocol only | Move registry, policy, ledger, Herdr reconciliation and dispatch. Keep the node-side `familiar-fleet` client. |
| Projects filesystem API is in the UI extension; a separate Projects service also runs | `familiar-ui/packages/extension/src/projects-fs.ts`; `projects/README.md`; `fort-nix/.../azula/manifest.nix:1841-1946` | `internal/projects` or fleet view; familiar-ui client | Move reads to service API; retire the duplicate standalone browser after annotation decision. |
| Tiamat Router is already a substantial Go/SQLite service | `tiamat-router/main.go`, `internal/{api,store,facade,...}`, `overlay.nix` | `internal/router`, separate drained `familiar-router` unit | Move mostly intact; preserve protocol and data. Add drain semantics before unit cutover. |
| Gateway currently owns transport **and** Presence lifecycle/fleet state, and has no application auth | `familiar/services/gateway/src/{main.ts,pty.ts,fleet.ts}`; `services/gateway/README.md` | gateway owns auth/client protocol and proxies Pi/services | Remove `presence.sh ensure` and singleton stores. Add authenticated client/session routing. |
| Presence/server supervise and respawn one privileged Pi | `familiar/services/presence/presence.sh`; `services/server/`; `fort-nix/apps/familiar-instance/default.nix` | `familiar-pi@<id>`, `familiar-services`, drained `familiar-router` | Replace, not layer underneath. No new service parents Pi. |
| Identity is ordinary Markdown loaded on each turn | `familiar/integrations/pi/extensions/identity/index.ts:41-48` | config-pointed identity file/dir | Keep as a file boundary. Do not create identity/canon services. |

The proposal has package names but no implementation yet (`README.md` explicitly says nothing runs). The biggest missing contracts are: an idempotent write API, authenticated caller/actor attribution, service-to-systemd spawn protocol, gateway-to-live-Pi routing, continuity ingestion/fork/merge rules, data migration checkpoints, backup/key policy, and router drain behavior.

## Appendix A — repository inventory and landing map

### `familiar`

The repository is currently a monorepo/runtime, not just Pi integration. Its Pi directory entrypoints are `agents`, `background`, `footer`, `handoff`, `identity`, `imp`, `private`, `stuff`, `subscriber`, `tiamat`, `timegap`, `wake`, `web`, `worklist`, and `zip` (`integrations/pi/extensions/*/index.ts`). It also builds `imp`, desktop, gateway, server, Presence, LLM/STT/TTS proxies and the native viewer (`flake.nix`, `apps/`, `services/`).

| Current component | Actual behavior and state | Target / recommendation |
|---|---|---|
| `services/presence` | Private tmux owns one resident Pi; `presence.sh` runs a respawn loop and Pi uses `--continue` (`services/presence/README.md`). | **Nuke after cutover.** Replaced by `familiar-pi@<id>`. Its viewer attachment concerns move to the gateway/client adapter, not familiar-services. |
| `services/server` | Starts/restarts gateway, Presence, LLM/STT/TTS; exposes child restart/stop (`services/server/README.md`, `supervisor.go`). | **Nuke process ownership.** systemd owns the three target unit classes. LLM/STT/TTS are not covered by this proposal; Kevin decides whether they remain separate units (default: yes). |
| `services/gateway` + `subscriber` extension | SSE/submit/cancel/voice/PTYS; extension relays Pi events. Gateway calls `presence.sh ensure`; optional fleet registry persists there (`services/gateway/README.md`, `src/pty.ts`, `src/fleet.ts`). | Gateway/client. Keep protocol features, move fleet state to `internal/fleet`, remove Pi lifecycle, add auth and fork selection. Retire `subscriber` when gateway talks to each Pi unit through the chosen internal protocol. |
| Identity extension | Rebuilds system prompt from sorted `*.md` in `FAMILIAR_IDENTITY_PATH` (`integrations/pi/extensions/identity/index.ts`). | Identity-file. Keep a thin launch/prompt adapter; continuity records the exact resulting prompt at turn 0. |
| Worklist + DND | Atomic per-item JSON queue/dropbox and `dnd.json`; injects synthetic turns and offers `/remind`, `/snooze`, `ack_worklist`, DND tool (`worklist/PROTOCOL.md`, `store.ts`, `index.ts`). | `internal/attention` plus delivery. Preserve policy and idempotence, not filesystem layout. |
| Wake | Pending/fired/quarantine JSON, in-process timers, at-most-once send attempt (`wake/store.ts`, `wake/runtime.ts`, `README.md`). | `internal/wakes`; delivery targets a branch/session via gateway/systemd routing. The existing claim-before-send gap must become an explicit delivery state machine. |
| Handoff + subconscious | `/clear` writes Markdown handoffs and atomically replaces `state/subconscious/reminders.json` (`README.md`, `integrations/pi/extensions/handoff/`). | Handoffs become continuity turns/edges. **Nuke files and “canon”.** Kevin decides subconscious reminders: default preserve as attributed scheduled/attention records, not a hidden file. |
| `packages/continuity` | File API for `canon/*.json`, `handoffs/*.json`, preferences; not the Pi transcript (`packages/continuity/README.md`, `src/index.ts`). | **Nuke implementation.** One-time importer only. Preferences are not covered; default home is gateway/client profile storage, not continuity. |
| `imp` | Go CLI with `plate`, `agent`, `attn`; Unix socket currently terminates inside the owning Pi (`packages/imp/README.md`, `internal/cli`). | Keep CLI and repoint it to familiar-services. Remove `plate`; add continuity/wake/fork operations progressively. |
| Familiar Agents | Resident owns `agents.sqlite3`, policy JSON, remote admission/reconcile/settlement and Herdr transport (`integrations/pi/extensions/agents/index.ts`, `ledger.mjs`, `owner.mjs`). | `internal/fleet`; `imp agent` remains contract. Remove process-global symbol/owner election. |
| Golem plugin/relay | A second agent API and settlement relay to worklist; optional fallback starts golemd (`contrib/familiar/README.md`, `contrib/familiar/plugin.toml`). | Fleet adapter during transition, then **nuke duplicate Pi tools/relay** once fleet speaks Herdr. Golemd itself may remain for non-Familiar consumers. |
| Background Exo | SQLite scheduler and independent SDK sessions *inside one Pi*, canonical-owner transaction, branch JSONL, Golem child adapter (`packages/background/README.md`, `integrations/pi/extensions/background/`). | **Nuke. Do not migrate its design or state machine.** Evidence: it deliberately creates divergent reduced-tool sessions in one owner process, excludes ambient extensions/worklist/UI, and depends on patched Pi owner APIs—the opposite of peer `familiar-pi@` units. Import branch transcripts as historical continuity only. |
| Tiamat extension | Router catalogue/provider adapter for Pi (`integrations/pi/extensions/tiamat/`). | Pi-side router client remains; server implementation moves to `internal/router`. |
| `private` | Encrypted private conversation compartment/sealing (`integrations/pi/extensions/private/PRIVATE.md`, `seal.ts`). | **Not covered — Kevin decides.** Default: keep Pi-local and explicitly mark private spans in continuity with encrypted/withheld body policy; never silently copy decrypted content. |
| `stuff` | Thin Pi command/UI integration to external Stuff CLI (`integrations/pi/extensions/stuff/`). | Client integration; not a singleton here. Keep unless attention replaces its use. |
| `footer`, `timegap`, `web`, `zip` | Pi-local presentation/context/search/compaction affordances (`integrations/pi/extensions/*`). | Pi extensions, not services. `zip`/compaction events must still be recorded faithfully. |
| LLM/STT/TTS/viewer/desktop | Local proxies, native terminal viewer and clients (`services/{llm,stt,tts,viewer}`, `apps/desktop`). | **Not covered — needs a home.** Default: separate systemd units and clients; do not fold into singleton packages. |

### State-store ledger

| Owner | Default/configured format and path |
|---|---|
| Pi/Familiar | Session JSONL under `$PI_CODING_AGENT_DIR/sessions` (live Kestrel: `/var/lib/kestrel/state/pi/sessions`); handoff Markdown at `FAMILIAR_HANDOFF_PATH`; identity Markdown at `FAMILIAR_IDENTITY_PATH`; JSONL sidecar logs at `FAMILIAR_LOG_PATH` (`familiar.toml.example`, `familiar.sh`). |
| Worklist/DND/wakes | Per-item JSON under `state/worklist/{items,incoming,acknowledgements}`, `dnd.json`, and wake JSON under `state/wakes/{pending,fired,quarantine}` (`worklist/store.ts`, `wake/store.ts`). |
| Attention/UI | `$FAMILIAR_ATTENTION_DB` or `$XDG_STATE_HOME/familiar-ui/attention.sqlite`; attachments under `$XDG_STATE_HOME/familiar-ui/attachments`; `.familiar-ui/bridge.json` descriptor; in-memory bridge journal (`familiar-ui/packages/extension/src/{attention,attachments,descriptor}.ts`, `bridge/src/journal.ts`). |
| Agents/Background | `$FAMILIAR_AGENTS_STATE_DIR/agents.sqlite3` + `agent-policy.json`; `$FAMILIAR_BACKGROUND_STATE_DIR/workstreams.sqlite` + per-branch `branch.jsonl` (live: `/var/lib/kestrel/state/{agents,background}`; `agents/index.ts`, `packages/background/store.mjs`). |
| Old continuity | `canon/*.json`, `handoffs/*.json`, `preferences/*.json` under a caller-supplied root; live handoffs are instead Markdown (`packages/continuity/README.md`). |
| Gateway fleet | Configured `FAMILIAR_FLEET_STATE_DIR`, registry plus generated `authorized_keys`, `known_hosts`, `ssh_config`, `herdr-machines.json` (`services/gateway/src/fleet.ts`, README). |
| Server/Presence | Child logs under configured server `state_dir`; tmux socket/config under `FAMILIAR_PRESENCE_STATE_DIR` (live `/var/lib/kestrel/state/presence`) (`services/{server,presence}/README.md`). |

### `familiar-ui`

This is an in-process Pi extension plus loopback bearer bridge, descriptor broker, web app and protocols (`README.md`, `packages/{extension,bridge,broker,web}`). The bridge journal is bounded and in-memory (`packages/bridge/src/journal.ts`), while Pi JSONL supplies historical projection.

| Surface/state | Target |
|---|---|
| Transcript, model/thinking/tool rendering, submit/cancel, attachments, command dispatch | Familiar UI remains a client; move bridge endpoints behind the authenticated gateway. Attachments need an explicit home: default gateway-managed blob store with continuity parts referencing immutable IDs. |
| Attention board and DND controls | Client of the gateway/service API. Remove `attention.sqlite` ownership and same-process DND symbol (`extension/src/{attention,dnd}.ts`). |
| Projects drawer/filesystem reads and project-file “handoff” annotation | Client of `internal/projects`; rename the annotation to project reference to avoid confusion with continuity handoff (`extension/src/projects-fs.ts`, `protocol/src/handoff.ts`). |
| Background workstream UI | **Nuke with current Background.** Rebuild later against live peer branches/attention; do not translate old workstream states (`extension/src/background*.ts`). |
| `.familiar-ui/bridge.json`, per-session token, Unix broker | Transitional only. Gateway auth/client protocol replaces the per-Pi public bridge. Keep only an internal Pi endpoint descriptor if needed. |
| Legacy Plate | Already migrated/retired by Attention (`ATTENTION.md:211-229`, `extension/src/attention.ts:225-400`) but `imp plate` remains. **Nuke CLI and stale deployment path.** |

### `familiar-fleet`

This is the **node-side** client, not the central fleet authority. It enrolls once via OIDC, runs loopback sshd + reverse tunnel, maintains a named Herdr server, activates an immutable Familiar worker runtime, and never persists OAuth tokens (`README.md`, `cmd/familiar-fleet/main.go`). State defaults to root `/var/lib/familiar-fleet`, else `$XDG_STATE_HOME/familiar-fleet`/`~/.local/state/familiar-fleet`; exact files are in `internal/client/state.go:32-95`. systemd user and launchd examples live in `packaging/`.

**Landing:** keep as a fleet-node client/deploy unit. Change its control/enrollment endpoint from the old gateway registry to gateway → `internal/fleet`; retain local Herdr/session/runtime state. The central service must not absorb node private keys or shell adapters.

### `tiamat-router`

A mature router already exists: Anthropic/OpenAI/speech routes, provider/client CRUD, OAuth refresh, derived models, availability/true-up, usage ledger, Claude CLI facade, bounded transcript capture and image cache (`README.md`, `docs/SPEC.md`, `internal/`). Persistent state is `<data-dir>/tiamat.db`, WAL, `<data-dir>/credential.key`, `<data-dir>/capture/`, facade work/temp; image cache defaults to `/tmp/tiamat-router-images` (`internal/store/sqlite.go`, `credentials.go`, `internal/capture`, `internal/images/cache.go`). Deployment is a Fort overlay (`overlay.nix`, `.forgejo/workflows/deploy.yml`).

**Landing:** port packages intact into `internal/router`; run the same binary as `familiar-router`. Preserve API paths and migrate/copy one database only. Current provider/OAuth credential blobs are age-encrypted and plaintext API keys are migrated (`internal/store/credentials.go`, `providers.go:277-315`), despite stale README text claiming all are plaintext. **Client tokens remain plaintext** in `clients.token` (`internal/store/sqlite.go:54`, `clients.go`), and the age identity is adjacent to the DB, so this protects copied media/backups only when key custody is separated. Add drain/readiness before replacing the overlay.

## Deployment reality and restart hazards

Azula runs a private Kestrel instance through `fort-nix/apps/familiar-instance/default.nix`: `familiar-instance-presence.service` owns tmux/Pi and `familiar-instance.service` owns the old supervisor. Tracked Familiar updates restart only the outer unit; familiar-ui updates restart only broker/stager (`fort-nix/clusters/bedlam/hosts/azula/manifest.nix:1564-1657`). Golemd and Stuff are separate tracked services; Projects is a separate hardened service.

**Anything that restarts a Familiar today:**

1. Presence's worker loop relaunches `familiar.sh pi --continue` after Pi exits (`services/presence/README.md`).
2. `familiar-instance-presence.service` is `Restart=on-failure`; its monitor exits if tmux/pane disappears and `ExecStop` stops Presence (`fort-nix/apps/familiar-instance/default.nix:136-188`).
3. The old server can restart a failed/unready detached Presence child and exposes `POST /children/presence/restart` (`services/server/README.md`).
4. Gateway boot/PTY recovery calls `presence.sh ensure` (`services/gateway/src/pty.ts`, README).
5. `/reload`, `/new`, `/clear` change/reload the Pi session from UI/TUI; Background deployment specifically requires stop/new birth (`docs/BACKGROUND-REMEDIATION-V3.md`).
6. Explicit `presence.sh stop`, `familiar.sh kill`, or systemd restart tears it down. Fort deliberately prevents ordinary familiar-ui/tracked activation from doing so; preserve that property during migration.

The target removes 1–4: systemd alone restarts the named `familiar-pi@id`; gateway and familiar-services request unit operations but neither parents or opportunistically “ensures” Pi.

## Drift and confidentiality traps

### Duplicate state that will drift

- Worklist items/DND, Attention cards/agents, legacy Plate remnants, Stuff Items and Golem settlements can describe the same work with no shared revision.
- Old gateway fleet `nodes.json`/generated SSH/Herdr files, Familiar Agents `agents.sqlite3` + `agent-policy.json`, and golemd's registry each model machines/jobs independently.
- Pi session JSONL, handoff Markdown, subconscious reminders, Background branch JSONL/SQLite, gateway in-memory replay and router captures each retain overlapping conversation fragments.
- Projects exists both as familiar-ui host filesystem reads and the standalone `projects` service/annotation DB.
- Tiamat bootstrap JSON, SQLite provider state and deployment SQL mutate the same router catalogue (`fort-nix/.../azula/manifest.nix:36-158`, `1010-1129`).

Do not dual-write these. Each cutover uses: freeze old writer → import with stable source IDs/checksum → switch all readers/writers → archive old store read-only. A mirror is acceptable only when explicitly read-only and continuously compared.

### Identity and continuity at rest

Identity files are **not encrypted at rest in the live Kestrel instance**. `/var/lib/kestrel/familiar.toml` points to `/var/lib/kestrel/identity`; `identity/identity.md` is ordinary Markdown (observed mode `0644`) inside a `0700` instance root. The identity extension calls Node `readFile(..., "utf-8")` directly and has no age path (`identity/index.ts:41-48`). `age_key` is configured, but source uses age for voice packs and private sealing, not identity prompt loading (`services/tts`, `integrations/pi/extensions/private`). Repository docs also call identity “ordinary markdown” (`packages/continuity/README.md`, `docs/CONFIG.md`).

Continuity will contain **more sensitive material than identity**: exact system prompts, user/assistant/tool content, wakes and merges. A mode-0600 SQLite file is consistent with today's effective protection but is not encryption. Before live capture, choose and document one of:

- default recommendation: application-level age encryption for `parts.body_json` with a key supplied outside the state directory, leaving graph metadata queryable; encrypt exports/backups and account for SQLite WAL/temp pages; or
- explicit accepted exception: owner-only local DB on encrypted storage, with backup policy and threat model.

Never claim encryption merely because the DB and key are both mode-restricted. Private spans need a separate policy: encrypted body, redacted placeholder, or exclusion with a faithful marker.

## Not covered: decisions

| Item | Verdict | Recommendation / crisp question |
|---|---|---|
| LLM/STT/TTS local proxies and native viewer | **Needs a home** | Keep separate units and gateway dependencies. Do they remain in `familiar`, or move to dedicated repos? Default: leave until singleton migration is done. |
| Attachments/audio blobs | **Needs a home** | Gateway blob package, immutable IDs referenced by continuity; do not put multi-MiB bodies in continuity SQLite. |
| Client/device preferences and room state | **Needs a home** | Gateway/client-profile DB. Continuity records actions but is not preference authority. |
| Private compartment | **Kevin decides** | May decrypted private turns enter continuity? Default: encrypted parts with stricter authorization; otherwise faithful withheld markers. |
| Subconscious reminders | **Kevin decides** | Preserve behavior? Default: migrate useful reminders to attention/wakes, delete hidden file semantics. |
| Stuff overlap with Attention | **Kevin decides** | Is Stuff a general notebook or an old task surface? Default: keep inert Stuff, forbid it as attention/dispatch authority. |
| Standalone Projects annotations | **Kevin decides** | Must line comments survive? Default: import into `internal/projects` before retiring `/var/lib/projects-browser/annotations.sqlite3`; otherwise nuke service. |
| Golemd after fleet cutover | **Kevin decides** | Any non-Familiar consumers? Default: keep daemon externally, delete Familiar plugin/relay and duplicate tools. |
| `services/server`, Presence/tmux adapter, Plate, canon/handoff file API, old Background | **Nuke** | Superseded, already retired, or contradicts decided process/peer architecture; evidence above. |
| `claude-cache-gateway` | **Nuke for Familiar** | Router facade already implements continuation/cache rewrites (`tiamat-router/internal/facade/gateway.go`); standalone repo is redundant. |

## Appendix B — adjacent live-stack repositories (cheap audit)

- **`fort-nix`** — actual Azula units, tracked deploys, router overlay, Golem, Stuff, Projects and familiar-ui auth proxy (`clusters/bedlam/hosts/azula/manifest.nix`, `apps/familiar-instance/default.nix`). It must implement the final templated units and remove old restart edges.
- **`golem`** — live delegated-agent daemon/CLI with a SQLite durable registry, artifacts and optional private Herdr backend; deployed as `golemd` by Fort (`service/store.go`, `README.md`, Azula manifest). Transitional fleet backend, not target singleton authority.
- **Herdr / Drover** — Herdr is pinned as a flake dependency and machine substrate; Drover supplies enrollment/remote UI. No standalone `herdr` repo was requested/read. Keep as external substrate.
- **`stuff`** — live CouchDB-backed Item/Note service and CLI; Familiar loads a thin capture extension (`stuff/README.md`, Azula manifest `1751-1840`). Not a replacement for attention.
- **`projects`** — live read-only project browser plus SQLite annotations (`README.md`, Azula manifest `1841-1946`). Overlaps `internal/projects`.
- **`hearth`** — intended gateway client, but current main says all surfaces use stub providers and are unwired (`hearth/README.md`). Treat its current wire contract as input, not running authority.
- **`familiar-ios`** — webview shell for familiar-ui (`README.md`); remains a client.
- **`hoard` daily briefing** — the active agent is now Kobold workflow `daily-briefing`, an unattended Muse whose prompt says “You are Exo”; the same-minute legacy `daily-briefing.timer` invokes a no-op shim pending decommission (`hoard/scripts/daily-briefing/run.sh`, `workflows/daily-briefing.{cue,prompt.md}`, `fort-nix/aspects/dev-sandbox/default.nix:594-620`). Replace the workflow with a wake named **Daily Briefing — Exo**; decide whether Hoard remains an export sink.
- **`claude-cache-gateway`** — diagnostic/repair proxy now duplicated by Router; not seen in Azula Familiar unit wiring.

## Risks and unknowns

1. **Continuity schema is not implementation-ready.** `parts.source` is not sufficient actor attribution; add actor/client/model/tool-call IDs and import provenance. Define idempotency keys, timestamps/order under concurrency, edge validation, branch identity, deletion prohibition, and interrupted-stream representation. Test `live_branches` against fork/merge/close cases.
2. **Capture point is undecided.** Gateway sees client traffic, Pi sees exact model context/tool/system parts. Faithful storage requires a Pi protocol hook or append adapter with acknowledgements; scraping JSONL is acceptable for import, not the permanent write path.
3. **Exactly-once is impossible without contracts.** Existing wakes admit at-most-once send attempts. Define claimed/delivered/observed states and idempotent turn IDs across service/Pi crashes.
4. **Systemd control is security-sensitive.** Specify allowed unit names/IDs, authorization, fork quotas, startup handshake, stale-unit reconciliation and branch-close behavior; never accept arbitrary unit strings over the API.
5. **Router cutover can truncate streams.** The current overlay has no drain protocol. Add reject-new + in-flight count + deadline and make deployment wait before stop.
6. **Historical import is lossy.** Handoff Markdown and gateway events cannot reconstruct exact multi-parent ancestry. Mark inferred edges/provenance; do not invent certainty.
7. **State permissions are inconsistent.** Live identity and many historical handoffs/Pi JSONL are `0644` under a `0700` parent. Migration must preserve confidentiality after files move to service-owned directories/backups.
8. **Gateway authentication is deployment-proxy-dependent today.** The old gateway itself says it has no auth; familiar-ui has a per-session bearer plus nginx identity wall. Define user/client identity and fork authorization before exposing the unified API.
9. **Naming collision:** central `fleet` and node client `familiar-fleet` are different products. Document this or rename the node client later; do not block migration on naming.
10. **Remote freshness:** familiar-ui was read from a local fallback because GitHub clone auth failed. Re-run this audit against remote main before deleting its stores.
