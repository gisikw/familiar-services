# Attention v2 — contract (Oct 11, 2026)

Kev approved. Wireframe: ~/Projects/wireframes/attention-v2. This file is the
cross-repo contract for three slices: familiar-services (store), familiar (imp
CLI), familiar-ui (protocol/extension/web). Names here are binding.

## 1. Needs Attention becomes "asks": Kes's inbox to Kev

An **ask** is a bounded decision Kes puts in front of Kev about one card.
At most **3 surfaced** at a time. Kes may **stage** more; staged asks are
invisible until Kev clicks "Give me more". Nothing ever auto-refills.

### Store (familiar-services, SQLite, `internal/attention`)

New table (created by the existing `schema` string; `create table if not exists`):

```
asks (
  id integer primary key,
  card_id text not null references cards(id) on delete cascade,
  question text not null,            -- one line, <= 300 code units (ReasonSchema-like bounds: use 300)
  steps text,                        -- optional prose, <= 4096; newline-separated steps; markdown-ish, rendered as an ordered list, one step per non-empty line
  link text,                         -- optional; http(s) URL or absolute filesystem path (reader doc), <= 4096
  link_label text,                   -- optional, <= 200; default in UI: "open ↗"
  actions text not null,             -- JSON array, see below; 1..4 actions
  state text not null check(state in ('staged','surfaced','resolved','dropped')),
  position integer not null,         -- surfacing order (ascending); new asks append
  created_at text not null,
  surfaced_at text,
  resolved_at text,
  resolution text                    -- action key, or 'not-now', or 'dropped'
)
```

Action JSON: `{"key": "close", "label": "Close it", "primary": true, "ops": [ {"op":"card.move","id":"<uuid>","lane":"archived"} ]}`
- `key`: `[a-z0-9-]{1,32}`, unique within the ask; `not-now` and `talk` are reserved (rejected).
- `label`: 1..120 code units, one line.
- `primary`: optional bool (at most one true).
- `ops`: optional array (0..8) of card mutations executed when Kev picks it. Allowed op names ONLY:
  `card.move {id,lane}`, `card.set {id, owner?, title?, summary?}`, `card.done {id, done?}`,
  `card.block {id, reason}`, `card.unblock {id}`, `note.add {card, text, detail?}`.
  `id`/`card` may be any card (e.g. archive two sibling cards). Validate shape at `ask.add` time
  (op name allowed, required fields present, ids resolve to existing cards).

Operations (added to `HandleAs` switch; same JSON-over-socket transport):
- `ask.add {card, question, steps?, link?, link_label?, actions:[...], stage?: bool}` → full ask.
  `card` accepts full uuid or 8+ hex prefix (use `resolveCardID`).
  If `stage` is false/absent and fewer than 3 are surfaced → state `surfaced`; otherwise `staged`.
  Writes a card event kind `note`? NO — write a card note by `kes`: `Asked Kev: <question>`.
- `ask.list {all?: bool}` → `{surfaced:[ask...], staged:[ask...]}` (ordered by position);
  with `all:true` also `resolved:[...]` (last 50, newest first).
- `ask.resolve {id, choice}` where `choice` is an action key or `"not-now"`.
  - action key: run each op in order through the normal mutation path **as the calling actor**
    (kevin from the browser), then mark resolved (resolution=key), then add a note on the ask's
    card by the actor: `Kev chose “<label>” on: <question>`.
  - `not-now`: mark resolved (resolution='not-now'); note: `Kev: not now on “<question>” (wrong moment — not a judgment on the ask)`.
  - Unknown id or already resolved → `not_found` / `conflict`.
  - Returns `{resolved: ask, card: fullCard}`.
- `ask.talk {id}` → records a card note `Kev opened a conversation about: <question>`; does NOT resolve; returns the ask.
- `ask.more {}` → surfaces staged asks (by position) until 3 are surfaced; returns `{surfaced: n_added, staged_left: m}`.
- `ask.drop {id}` (Kes withdraws) → state dropped, resolution 'dropped'; note by actor `Withdrew ask: <question>`.
- `ask.surface {id}` (Kes) → force a staged ask to surfaced if <3 surfaced, else conflict.
Kes may re-ask a not-now item later simply with `ask.add` again.

Wire shape of one ask (store → clients):
```
{ "id": 7, "card": {"id": "<uuid>", "project": "familiar-services", "title": "M4 — …"},
  "question": "…", "steps": "line1\nline2" | null, "link": "…" | null, "link_label": "…" | null,
  "actions": [{"key":"close","label":"Close it","primary":true}],   // ops NOT sent to clients
  "state": "surfaced", "created_at": "RFC3339", "surfaced_at": "RFC3339"|null,
  "resolved_at": null, "resolution": null }
```
`status` gains `asks: {surfaced: n, staged: m}`; `needs_attention` in status = surfaced count.

## 2. Jots: Today vs Maybe today

Two new nullable columns on `cards` (add via `alter table` migration when missing,
guarded by `pragma table_info`): `due text` (YYYY-MM-DD) and `maybe_on text` (YYYY-MM-DD).
Only meaningful for `_today` jots.

- `jot.add {title, owner?, due?}`: `due` = `YYYY-MM-DD`. With `due` → a **Today** task.
  Without → a **Maybe today** intention with `maybe_on` = today's local date.
- `card.set` accepts `due` (YYYY-MM-DD or null) and `maybe_on` (YYYY-MM-DD or null) on `_today`
  cards only (else invalid_request). Setting `due` non-null clears `maybe_on` ("promote").
  Event `edited` records the fields.
- "Local date" uses `FAMILIAR_TZ` env (default `America/Chicago`); import `time/tzdata` so it
  works in a minimal Nix closure. Store gets a `tz *time.Location` (test seam alongside `now`).
- `jot.list {all?: bool}` default view returns, ordered done asc, due asc nulls last, captured_at asc:
  - dated jots with `due <= today` (not archived) — done ones still shown until clear-done;
  - undated jots with `maybe_on = today` (or `maybe_on` null and captured on today's local date).
  Hidden (not deleted): dated jots in the future; maybes from earlier days ("faded").
  `all:true` returns every non-archived jot.
- Concise card gains `due: string|null` and `maybe_on: string|null` (always present from the new store).
- One-time migration (guard with a `meta` key `jots_v2_migrated`): every open (done=0, non-archived)
  `_today` jot gets `maybe_on` = today's local date, EXCEPT titles matching (case-insensitive)
  `septic`, `thinking summaries`, `trash` which get `due` = today instead. (So nothing Kev
  currently sees vanishes on deploy; intentions fade tonight.)
- `jot.clear-done` unchanged.

## 3. imp CLI (familiar, `packages/imp/internal/cli/attn.go`)

- Noun `ask`: verbs `add`, `list`, `resolve`, `more`, `drop`, `surface`, `talk`.
  - `imp attn ask add` reads the JSON ask body from stdin (`--json-in`, required) e.g.
    `echo '{"card":"6c5e0e39","question":"…","actions":[…]}' | imp attn ask add --json-in [--stage]`.
  - `imp attn ask list [--all] [--json]` human: `#7 [surfaced] familiar-services · Close M4…? — close|keep`.
  - `imp attn ask resolve ID CHOICE`, `ask more`, `ask drop ID`, `ask surface ID`, `ask talk ID`.
- `imp attn jot add … [--due DATE]` and `imp attn card set ID [--due DATE|none] [--maybe-on DATE|none]`,
  where DATE is `YYYY-MM-DD`, `today`, `tomorrow`, or a weekday name/abbrev (next occurrence,
  today counts if it IS that weekday) — resolved client-side in America/Chicago to YYYY-MM-DD.
- `imp attn jot list [--all]`; human rows show `due Fri` / `maybe`.
- Status line shows `asks: N surfaced, M staged` instead of `needs attention`/`in flight` counts.
- Help text updated; tests updated.

## 4. familiar-ui

Protocol (`packages/protocol/src/attention.ts`):
- `AskSchema` per wire shape (actions without ops). Concise card: `due`, `maybe_on` as
  `z.string().regex(/^\d{4}-\d{2}-\d{2}$/).nullable().optional()` (older stores omit them).
- Snapshot: add `asks: Ask[] (max 3)` and `asks_staged: number`. Keep `needs_attention` and
  `inflight` arrays in the snapshot for compatibility, but the UI no longer renders them.
  Status: `asks` optional object.
- Browser mutations add: `ask.resolve {id:int, choice}`, `ask.talk {id}`, `ask.more {}`,
  `card.set` gains optional `due` / `maybe_on` (nullable date strings),
  `jot.add` gains optional `due`.
- Imp ingress operation list adds the ask.* ops and they pass through.

Extension snapshot: call `attn.ask.list` in `refresh()`; tolerate older stores (error → `asks: []`,
`asks_staged: 0`). Jots come from `jot.list` as now (store filters).

Web (`packages/web/src/ui/Attention.tsx` + CSS):
- **Remove the In Flight section entirely.**
- **Needs attention** section renders `asks` (count = asks.length). Each ask, left verdigris rule like today:
  - question (prose, wraps);
  - context line: `project` · link (if `link`: label or "open ↗"; http(s) opens new tab;
    a filesystem path opens in the Reader using the existing open-document path the transcript
    chips/Open list use);
  - if `steps`: `<details>` **collapsed by default**, summary "What I need from you", ordered list;
  - actions row: one button per action (primary styled like `.go`), then two **icon buttons**:
    Not now (icon, `aria-label="Not now"`, title tooltip) and Talk about this (speech-bubble icon,
    `aria-label="Talk about this"`). Real `<button>`s, first-class, same height as the others.
  - The actions row is `flex-wrap: wrap`; buttons use `white-space: normal` so a long label
    (e.g. "No, drop the requirement") can take its own line; icon buttons stay together at row end.
  - Clicking an action → mutation `ask.resolve {id, choice:key}`; Not now → `ask.resolve {id, choice:"not-now"}`;
    Talk → `ask.talk {id}` then put a prefilled message in the composer (not sent) like
    `About card <first 8 of card id> “<card title>”: <question>` and focus it. Reuse whatever
    composer-prefill mechanism exists (Composer.tsx); if none, add a minimal one.
  - Optimistic: the ask disappears on click; on failure it comes back with the existing error affordance.
  - Footer: when asks.length <= 2 show a small "Give me more" link. If `asks_staged > 0` it calls
    `ask.more`; if 0 it prefills+SENDS? NO — it prefills the composer with "Give me more for Needs Attention." and focuses (Kev sends).
  - Empty: italic "Nothing from me right now." then the link.
- **Today / Maybe today** (replaces the single Today list; keep the both/you/kes filter and jot input):
  - "Today": jots with `due` (checkbox as now; trailing stone text `since Fri` when due < today;
    nothing when due == today). Done ones strike through; "clear done" stays.
  - "Maybe today": jots without `due` — a dot bullet, NO checkbox. On hover/focus (and always
    visible on touch as a small icon) a "promote" calendar-icon button; clicking shows an inline
    date chooser: chips `Today`, `Tomorrow`, next weekday names for the coming week, and a native
    `<input type="date">`. Choosing → `card.set {id, due}`. Promotion ALWAYS requires a date.
  - Old stores (no `due` field at all): render all jots as before under "Today" (graceful).
  - Plain jot input lands in Maybe today (store default). No Lists section.
- Mock (`protocol/mock/attention.ts`, stories) gets asks + dated/maybe jots so the e2e/stories work.
- Update unit/e2e tests that referenced In Flight / needs_attention rendering.
