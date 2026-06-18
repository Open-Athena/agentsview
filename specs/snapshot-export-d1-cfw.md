# Publishable snapshots via D1 + Cloudflare Worker read-API

## Goal

Publish a static-ish snapshot of the agentsview web UI, **scoped to selected
projects**, that anyone can browse without running the local Go server. Think:
"export my `oa/*` projects' sessions + usage + analytics to a URL I can share."

The published artifact must be read-only and self-contained (frozen dataset);
no file watcher, no sync, no mutations, no live SSE.

## Why this is tractable (enabling facts)

The current FE/BE boundary is already clean, and the read side is already
*backend-pluggable*. We are adding a third backend behind an existing contract,
not re-architecting.

1. **`db.Store` is the single read/write contract** (`internal/db/store.go`).
   All HTTP handlers call it; they don't touch SQLite directly.
2. **There's already a remote, read-only backend**: `internal/postgres` declares
   `var _ db.Store = (*Store)(nil)` and `pg serve` serves the *identical*
   `/api/v1` surface from Postgres. Mutations are no-ops; `ReadOnly()` returns
   true; `HasFTS()` advertises search capability. So "a read-only remote backend
   behind the same API" is a paved road.
3. **The FE already supports a remote API base**: `client.ts` `getBase()` →
   `${getServerUrl()}/api/v1`, with `isRemoteConnection()`. Reads are a single
   chokepoint; SSE is isolated to `watchSession`/`watchEvents`; mutations are
   isolated POST/PUT/DELETE calls.
4. **Static single-session export already exists**: `generateExportHTML`
   (`internal/server/export.go`), `/sessions/{id}/md`, and `publishSession` →
   GitHub gist. The repo already embraces "freeze a view to something
   serverless" — we're generalizing it from one session to a project-scoped
   whole-UI snapshot.

The one thing that does NOT port directly: the Go `Store` implementations. A
Worker runs JS/TS, not CGO-SQLite, so the CFW must *reimplement the read-side
query logic* against D1. The surface is finite (the GET routes below) and the
response shapes are fixed Go structs that mostly already have TS mirrors in
`frontend/src/lib/api/types*`.

## Recommended architecture

**One Cloudflare Worker per snapshot**, serving two things from the same origin:

- **`/` (everything non-API)** → the built Svelte SPA via Workers Static Assets.
- **`/api/v1/*` (GET subset)** → a TS read-API backed by a **D1** database
  holding the frozen, project-filtered dataset.

Same-origin means `getBase()` returns `/api/v1` with zero FE config and no CORS.
The FE runs almost unchanged; it just needs a **snapshot mode** that hides
mutation/sync/SSE affordances (see "FE changes").

```
 publish:  agentsview export-snapshot --project oa/tomato --project oa/x --out snap/
              → snap/d1.sql       (schema + filtered rows + FTS rebuild + precomputed analytics)
              → snap/dist/        (built SPA, snapshot-mode flag baked in)
              → snap/wrangler.toml (Worker: assets=dist, d1 binding, fetch handler)

 serve:    wrangler deploy   →   https://<snap>.workers.dev
              GET /            → SPA (Static Assets)
              GET /api/v1/...  → Worker fetch handler → D1 (read-only)
```

### Why D1+CFW over the pure-static alternatives

- **Static JSON + client index**: simplest to host, but loses FTS5 exactness and
  forces precomputing every analytics param combo (date range / tz / exclude
  flags) — combinatorial. Interactive re-filtering breaks.
- **SQLite + sql.js-httpvfs**: full parity but ships a wasm runtime and a large
  DB file over range requests; query logic still must live client-side.
- **D1+CFW (chosen)**: reuses the FE's existing remote-backend assumptions almost
  verbatim, keeps interactive filtering (real queries run server-side), gives
  real FTS5 (pending spike), scales via D1, and plays to the CF stack we already
  use. Cost: a Worker must run (trivial/cheap for us) and the read queries get
  reimplemented in TS.

## Read-API surface the Worker must implement

From `internal/server/server.go`. Snapshot serves the **GET reads**; everything
else is disabled in snapshot mode.

Per-route plan — each route is `live D1 query`, `precompute at export`, or
`drop`:

| Route | Snapshot plan |
| --- | --- |
| `GET /api/v1/sessions` (+ sidebar index) | live (params: project/agent/date/min-msgs… vary a lot) |
| `GET /api/v1/sessions/{id}` / `{id}/full` / children / directory | live |
| `GET /api/v1/sessions/{id}/messages` | live (pagination params) |
| `GET /api/v1/sessions/{id}/search` | live (FTS5 — spike) |
| `GET /api/v1/sessions/{id}/timing` / activity | live |
| `GET /api/v1/search`, `/search/content` | live (FTS5 — spike) |
| `GET /api/v1/projects` / `machines` / `agents` / `stats` | live (cheap) |
| `GET /api/v1/analytics/*` (summary, activity, heatmap, projects, hour-of-week, sessions, velocity, tools, top-sessions, signals) | **decide**: port SQL (live, keeps interactive filters) vs precompute for the snapshot's fixed project set + a small set of default ranges |
| `GET /api/v1/trends/terms` | live or precompute (term inputs are user-supplied → lean live) |
| `GET /api/v1/usage/summary`, `/usage/top-sessions`, `/messages/longest` | live (this branch's new work — port the SQL incl. 1h-cache pricing + subagent rollup) |
| `GET /api/v1/insights*` | drop (or freeze a static list) |
| `GET /api/v1/version`, `/config/*`, `/settings`, `/update/check` | stub with snapshot-appropriate constants |
| SSE `/events`, `/sessions/{id}/watch` | drop (snapshot is frozen) |
| all mutations (star/pin/rename/delete/sync/resync/secrets-scan/insights-generate/worktree-mappings/settings PUT) | drop → 405 |

**Analytics decision is the main fork.** Two viable stances:
- **(a) Port the analytics/usage SQL to TS-on-D1.** Most faithful; preserves
  interactive date/tz/exclude filtering in the published view. Most porting work
  (and the pricing/rollup SQL is subtle — see `internal/db/usage.go`).
- **(b) Precompute at export.** The Go exporter already has every query; run them
  for the filtered project set and write results into D1 tables (or JSON blobs)
  keyed by a small, enumerated param space; CFW does thin `SELECT`. Cuts CFW
  complexity hugely but freezes the filter controls for those panels.

  Recommended: **(b) for the heavy analytics/usage panels with a fixed default
  param set, (a)/live for sessions+messages+search** where params are inherently
  open-ended. Revisit if interactive analytics filtering in snapshots is wanted.

## Exporter CLI (Go)

New subcommand, reusing existing query plumbing:

```
agentsview export-snapshot \
  --project oa/tomato --project oa/x \   # repeatable; project filter
  [--since DATE --until DATE] \
  [--include-precomputed] \              # run analytics/usage, write result tables
  --out snap/
```

Emits:
- `snap/schema.sql` — `CREATE TABLE` DDL (subset of `internal/db/schema.sql`) +
  the FTS5 virtual table/triggers + small reference/precomputed-analytics tables,
  loaded via `d1 execute --file`.
- `snap/messages.ndjson` (or similar) — filtered message rows, read straight from
  the source SQLite, loaded into D1 via a **bound-param loader** (D1 HTTP API /
  loader Worker) so large `content` never goes through SQL text. The `messages_ai`
  trigger builds FTS as rows land; a trailing `'rebuild'` is optional insurance.
  No intermediate local SQLite is built.
- `snap/dist/` — `make frontend` output with snapshot mode enabled.
- `snap/wrangler.toml` + `snap/worker/` — the Worker (assets binding + D1 binding
  + fetch handler).

D1 load: `wrangler d1 create <name>` then
`wrangler d1 execute <name> --file snap/d1.sql` (or `d1 import` from a SQLite
file — decide in spike).

Filtering must honor the **subagent rollup** semantics already in this branch
(a subagent session's rows belong to the parent's project) so a project snapshot
includes the right sessions.

## The Worker

`snap/worker/` — a small TS Worker. Router mirrors the GET routes above. A thin
query layer over the D1 binding returns the same JSON shapes the Go handlers do
(reuse/copy the FE's existing TS types as the contract). `HasFTS()` →
`/health`-style capability the FE can read, or just always-on if the FTS5 spike
passes.

## FE changes ("snapshot mode")

Minimal. Introduce a build-time/`window`-injected `SNAPSHOT` flag (baked into the
emitted `dist` by the exporter, e.g. a `<meta>` or `window.__AGENTSVIEW_SNAPSHOT__`).
When set:
- Hide/disable: sync + resync, star/pin writes, rename/delete/trash, publish,
  resume, insights-generate, settings writes, worktree-mapping edits, the auth
  overlay (snapshot is public or Worker-gated).
- Don't open SSE (`watchSession`/`watchEvents` become no-ops).
- Optionally show a "Snapshot — projects: …, captured <date>" banner.

This is a guard layer over the existing single `client.ts` seam + the isolated
SSE/mutation functions; it is *not* a `DataSource` rewrite (same-origin `/api/v1`
means reads are unchanged).

## Spikes / open questions

### Resolved (P0 spike, verified against remote D1, 2026-06-17)

1. **D1 + FTS5 — PASS (unblocks search).** Created a remote D1 and ran
   agentsview's *exact* FTS5 setup: external-content `messages_fts USING fts5(
   content, content='messages', content_rowid='id', tokenize='porter unicode61')`
   plus the `_ai`/`_ad`/`_au` triggers. All verified correct on `v3-prod` D1:
   - `MATCH` returns the right rows; `bm25()` ranking and `snippet()` highlight
     both work.
   - **Porter stemming works** (`MATCH 'run'` hit "Running").
   - Triggers maintain the external-content index on INSERT and on DELETE (the
     `('delete', …)` special insert correctly evicts rows).
   - `INSERT INTO messages_fts(messages_fts) VALUES('rebuild')` succeeds — this is
     the path the exporter will use to build the index after bulk-loading rows.
   So search needs **no fallback index**; the snapshot keeps real FTS5.
2. **D1 import path & limits — PASS.** `wrangler d1 execute --remote --file
   schema+data.sql` ingested the schema, virtual table, triggers, inserts, and
   deletes in one shot. Limits (Workers Paid): `10 GB`/DB, `5 GB` import file,
   `50,000` DBs/account — all comfortable for project-filtered snapshots.

   **Import direction & transport (settles two earlier over-flags):**
   - There is *no* binary-SQLite upload and no `d1 import` subcommand; wrangler's
     only bulk-load path is `d1 execute --file` (SQL *text*). `d1 export` goes the
     other way (D1 → SQL) and is **not in our flow at all** — so its inability to
     dump FTS5 DBs is a non-issue (footnote, not a constraint).
   - We do **not** build an intermediate local SQLite. The source agentsview
     SQLite already exists; the exporter reads filtered rows straight from it.
   - **Message rows load via bound parameters, never inline SQL literals.** The
     100 KB max-SQL-statement length only bites if you serialize a big `content`
     value into `INSERT … VALUES('…')` text — which we avoid by loading `messages`
     through the D1 HTTP API / a small loader Worker (`prepare().bind()` in
     `db.batch`, ≤100 params/query). Then the only ceiling is the 2 MB row limit,
     which transcript messages stay under. Schema + small reference tables can
     still go via `d1 execute --file`; the `messages_ai` trigger builds the FTS
     index as rows land (a trailing `'rebuild'` is belt-and-suspenders).

### Still open (design decisions, non-blocking)

3. **Analytics: live-port vs precompute** (the (a)/(b) fork above) — decide which
   panels keep interactive filters.
4. **Auth/visibility**: public snapshot vs Worker-gated (token, Cloudflare
   Access). FE remote-mode already handles a token; same-origin simplifies it.
5. **CFW vs CF Pages Functions** for the API handler; Static Assets binding
   shape; one Worker vs Worker(API)+Pages(SPA).
6. **Loader implementation** (settled in approach, TBD in detail): message rows
   load via a bound-param batch loader (D1 HTTP API / loader Worker), not SQL
   text. Detail to nail down: batch size (≤100 params/query), retry/idempotency,
   and whether the loader runs from the `export-snapshot` CLI or as a one-shot
   Worker fed the NDJSON.

## Phased plan

- **P0 — spikes**: D1 FTS5 ✅ + D1 import ✅ done (see Resolved above); still owe
  the analytics stance + a smallest end-to-end (one project, sessions+messages+
  search live on D1 behind a hand-written Worker, SPA pointed at it) and the
  large-content loader decision.
- **P1 — exporter + Worker read path** (core loop DONE):
  - `export-snapshot` (Go) emits schema.sql + sessions/messages/tool_calls
    NDJSON + manifest.
  - `snapshot-site/`: Worker (`/api/v1` reads over D1), bound-param loader,
    `parity.mjs` harness (Worker vs live Go server), and `snapshot-local.sh`
    (`npm run snapshot:local <dir>` — reset/schema/dev/load in one step,
    sequenced to avoid the SQLITE_BUSY lock from mixing `d1 execute --local`
    with a running `wrangler dev`).
  - Route parity vs Go (project `awair`, 57 sessions / 3072 msgs / 2628 tool
    calls): **5/5 match exactly** — sessions-list, session-detail, messages,
    session-search (LIKE over content + `tool_calls.result_content`), and global
    search (FTS branch deduped per session + name branch + rank/recency/id
    tiebreak). Minor: Worker bm25 magnitudes differ from Go's (~0 vs ~-8) while
    ordering is identical — cosmetic.
  - SPA now served by the Worker: a Static Assets binding (`public/` <-
    `frontend/dist`) serves the built Svelte SPA; the Worker is the SPA
    fallback (real assets served first; unmatched GET navigations -> index.html;
    `/api/*` and SSE -> Worker). On-load routes implemented: projects/agents/
    machines/stats/sidebar-index/children derived from snapshot data;
    version/settings(empty agent_dirs, no path leak)/starred/pins/sync-status/
    update-check/config.* and per-session timing/activity as static stubs. All
    on-load endpoints + assets return 200; deep-link client routes fall back to
    index.html. (Rendered-pixel check still pending — verify in browser.)
  - FE snapshot mode (`VITE_SNAPSHOT=1` baked into the snapshot's dist build;
    same `frontend/dist` source, Go-embedded build unaffected): lands on the
    most-recent session instead of the analytics page (which 404s in P1),
    suppresses the version-mismatch warning, and hides the Usage/More nav.
    Verified in-browser against the awair snapshot: clean render, full
    transcript, no console errors.
  - **Next**: wire the dist build (`VITE_SNAPSHOT=1`) + `public/` copy into the
    exporter/wrapper; then P3 analytics/usage routes (re-enable those tabs),
    then P4 deploy to CF.
- **P2 — FE snapshot mode**: flag + guards hiding mutations/SSE/auth.
- **P3 — analytics/usage**: per the (a)/(b) decision; wire the remaining panels.
- **P4 — DX**: one-command publish (`export-snapshot` + `wrangler deploy`),
  docs, og:image/title per snapshot.

## Acceptance criteria

- [ ] `agentsview export-snapshot --project X --out snap/` produces a deployable
  Worker that serves the SPA + `/api/v1` reads over D1, scoped to project X.
- [ ] Browsing the published URL: session list, message view, and full-text
  search work against the frozen data (FTS5 or documented fallback).
- [ ] Usage + analytics panels render for the snapshot's projects (interactive or
  frozen per the decided stance), honoring 1h-cache pricing + subagent rollup.
- [ ] No mutation/sync/SSE affordances are reachable; mutation routes 405.
- [ ] Snapshot is reproducible and self-contained (no dependency on the author's
  local DB or source files).

## Relationship to `cache-1h-and-projects-followups.md`

This supersedes follow-ups #4 (CLI≡UI total check) and #5 (longest-prompts UX
polish) in priority. #3 (rm `pnpm-lock.yaml`) is done; #2 (scroll precision) is
largely a no-op — `MessageList.scrollToOrdinalInternal` already awaits
`messages.ensureOrdinalLoaded`. The upstream-PR item (#1) is unaffected and
remains deferred.
