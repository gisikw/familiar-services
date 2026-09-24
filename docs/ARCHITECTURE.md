# Architecture (proposal)

## Shape

One Go module, one binary (`familiar-services`), with each service as an
internal package and one SQLite database per service under a shared state dir.
Services are independent packages so they can be split into separate processes
later if needed; there is no reason to pay that cost now.

| Package | Owns | Consumers |
|---|---|---|
| `continuity` | turn graph: turns, parts, edges, branch lifecycle | imp, gateway, briefing wake |
| `attention` | attention items, DND, "ready to merge" flags | imp, gateway/UI |
| `wakes` | durable wake schedule and delivery | imp, gateway (delivers to the right Pi) |
| `fleet` | workbox registry, Herdr connections, dispatch | imp |
| `projects` | project listing/viewing | gateway/UI; likely folds into `fleet` |
| `router` | inference routing, credentials, usage | every Pi (model endpoint), gateway |
| `api` | local HTTP/Unix-socket API shared by imp and gateway | — |

## Interfaces

- **imp → familiar-services:** Unix socket, JSON. imp stays the thin,
  progressively-discoverable CLI it is today; this repo owns the state behind it.
- **gateway → familiar-services:** same API. The gateway adds auth and client
  protocol; it never owns singleton state.
- **Pi → router:** OpenAI/Anthropic-shaped HTTP, as tiamat-router does now.

## Forks

The gateway spawns Familiar Pi processes. Each is either the conventional
primary or a fork. All get the same system prompt, tools and imp. Clients may
talk to any live fork directly: forks are ephemeral threads, not channels.

A fork's life:

1. `fork` edge from the turn it branched at.
2. Works; may dispatch subagents via `fleet`.
3. Raises an attention item: "ready to merge".
4. Either writes a merge (a note injected into another live branch, recorded as
   an attributed part plus a `merge` edge) or a `branch_close` turn.

Which branch is "primary" after concurrent work (for example local model on a
plane versus server-side jobs) is decided at merge time, by Kevin and Kes.

## Process ownership (decided 2026-09-23)

No service is the parent of a Familiar Pi. Three kinds of systemd unit, each
restartable without disturbing the others:

- **familiar-services** — the singletons. familiar-services asks systemd to
  start/stop Familiars; it never parents them. Redeploys are invisible to a
  running Familiar; `imp` retries across a brief restart.
- **familiar-router** — same binary, `--router` mode, separate unit. Restarting
  it mid-stream truncates a turn, so it drains before deploy: stop accepting new
  streams, let in-flight ones finish, then exit.
- **familiar-pi@<id>** — templated unit per Familiar (conventional primary or
  fork). Gateway only proxies clients to them; it owns auth and client protocol.

## Open questions

- Is `projects` its own service or a view over `fleet`?
- Router cutover: rename in place or run both until clients move?

## Identity vs continuity (decided 2026-09-23)

Identity is not a service. It is whatever file or directory the Familiar
config points to; Pi reads it at launch as its system prompt. There is no
identity package.

Continuity is the record. System prompts are persisted as turns like
everything else, so historical identity is fully preserved in the registry:
for any conversation you can see exactly which version of Kes was present.
Handoffs likewise live only as turns (`handoff` parts + edges); no separate
handoff files. Readable exports are derived from the registry if wanted.
