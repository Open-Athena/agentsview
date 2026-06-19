# Publishable, auth'd, multi-tier agentsview views

## Goal

Let the local agentsview server (where the data lives) publish **views** —
named, filtered, optionally-redacted, optionally-access-controlled slices of
agent session data — to Cloudflare, hosted under `claude.rbw.sh/<view>`.

Driving examples:
- A **main private view**: "all OA projects (plus a few in other GH orgs I
  primarily use at OA)", full drill-down, gated to me / chosen recipients.
- A **fully public stats view**: aggregate stats + cost for *all* projects but
  **no raw prompts or session content** — safe to share with anyone.

This builds directly on the snapshot work already on this branch
(`snapshot-site/` Worker + D1 + Static Assets, `agentsview export-snapshot`,
analytics/usage precompute, `VITE_SNAPSHOT` FE mode). It reuses the
capability-token auth model from `~/c/rac/mortgage-viz`.

## Decisions (locked)

- **Fork/home**: `runsascoded/agentsview` (public personal fork). Branch
  `cache-1h-and-projects` pushed to remote `r`.
- **Auth**: capability tokens (mortgage-viz model) — owner secret + grant
  tokens (`view`) → session cookies, instant revoke. No accounts. Public views
  skip auth entirely.
- **Hosting**: **one Worker + one D1 database per view**, path-routed
  (`claude.rbw.sh/oa`, `/public`, …). Scales to ~5,000 views (D1-bindings/Worker
  limit) vs 500 for one-Worker-per-view; keeps each view's data isolated in its
  own D1 (the redaction guarantee — see below). Adding a view = create D1 + add
  binding + redeploy the single Worker (publishing is already a deliberate admin
  step, so redeploy-per-view is fine).
- **Redaction**: enforced at **export time**, not query time (see P1).

### Cloudflare limits (verified 2026-06-18, Workers Paid)

| Resource | Limit |
| --- | --- |
| Worker scripts / account | 500 |
| D1 databases / account | 50,000 (raise on request) |
| D1 bindings / Worker | ~5,000 (1 MB script-metadata cap) |
| Routes / custom domains per zone | 1,000 / 100 |
| Static-asset files / Worker | 100,000 (25 MiB each) |

## Core concepts

- **View**: a named config (`views/<name>.json`) = project selection + per-project
  exposure tier + optional MD blurbs + theme + access policy. Compiles to one
  published D1 + a path on the Worker.
- **Exposure tier** (per project, within a view):
  - `full` — sessions/messages/tool_calls exported; drill-down works.
  - `stats-only` — included in analytics/usage **aggregates** but no raw
    sessions/messages in the published D1 (no drill-down).
  - `excluded` — absent entirely.
- **Grant**: a capability token (`view` perm) for a gated view; exchangeable for
  a session cookie; revocable. Public views need none.

## Redaction model (P1) — the security-critical piece

Enforce at **export time**: a `stats-only` (or `excluded`) project's
sessions/messages/tool_calls are **never written to the published D1**. This
maps onto the split the exporter already has:

- **Analytics/usage precompute** runs over the temp DB containing *all* of the
  view's `full`+`stats-only` projects → aggregate dashboards/cost include
  everything.
- **Data NDJSON** (sessions/messages/tool_calls) is written only for `full`
  projects → the published D1 physically lacks redacted prompts.

Because the prompts aren't in the published D1 at all, no auth bug or query can
leak them. A "fully public stats view" = every project at `stats-only`.

Per-view D1 isolation is what makes this hold: a project redacted in view A but
`full` in view B lives only in B's D1, never A's. (A shared D1 across views
would force query-time filtering and break this — hence one-D1-per-view.)

The exporter (`internal/snapshot`) needs:
- A view config input (projects + tiers) replacing the flat `--project` list.
- Two selections: analytics scope (full+stats) vs data scope (full only).
- Optional per-project MD blurbs surfaced in the UI (stats-only projects can
  carry a human summary in lieu of sessions).

## Hosting model (one Worker, per-view D1)

```
claude.rbw.sh/oa/...      ─┐
claude.rbw.sh/public/...  ─┤→ one Worker → route by first path segment → env.DB_<view>
claude.rbw.sh/<view>/...  ─┘                                   (one D1 per view)
                                + Static Assets (the SPA, shared)
                                + grants/sessions tables (per-view D1) for auth
```

- One Worker script; wrangler config binds `DB_oa`, `DB_public`, … (one D1 each).
- A static map `{ oa: env.DB_oa, public: env.DB_public }` selects the D1 by the
  leading path segment; the SPA is served from shared Static Assets with a
  `<base href>`/config so `getBase()` targets `/<view>/api/v1`.
- The SPA already supports a base path and `VITE_SNAPSHOT` mode.

## Auth (P3) — capability tokens, per mortgage-viz

Reuse the mortgage-viz model (`functions/_lib/doc-auth.ts`), simplified to
read-only `view` grants:

- Per published view: an `owner_secret` (admin). `ADMIN_SECRET`-gated publish.
- `grants(id, token_hash, perms='view', label, created_at, expires_at,
  revoked_at)` and `sessions(id, grant_id, created_at, expires_at)` tables in the
  **view's D1**.
- Recipient flow: `claude.rbw.sh/oa?t=<token>` → `POST /oa/api/v1/session {token}`
  → validate (not revoked/expired) → httpOnly session cookie → reads allowed.
- Public views: a `public: true` flag in config → Worker serves without a grant.
- Instant revoke: check `revoked_at IS NULL` per request.

No Durable Object / Yjs (those are mortgage-viz's *live-collab* half; snapshots
are read-only).

## Infra orchestration (P4) — local/admin publish

Admin mode = the localhost agentsview server. Publish flow (CLI-first, an
`mz.mjs` analog; candidate: `agentsview publish <view>`):

1. Build the redacted snapshot for the view (P1 exporter).
2. Provision: `wrangler d1 create agentsview-<view>`, load schema + data +
   precomputed analytics (bound-param loader), build the SPA, upload assets.
3. Wire the D1 binding into the Worker config + redeploy.
4. Mint/list/revoke grants for gated views.

Reuse existing pieces: `export-snapshot`, the bound-param loader, the
`snapshot-local.sh` orchestration (generalize from local dev to remote deploy).

## Theming (P5) — org/tag color-coding

- A project→org/tag map (config; e.g. `oa: [tomato, ...]`, `hccs: [...]`).
- Org → base hue (OA orange, HCCS green, …); projects within an org → shades of
  the org hue. Extends the existing golden-angle palette in
  `frontend/src/lib/utils/projectColor.ts` (added earlier on this branch) to be
  org-aware. Applies to the treemap, charts, and session list.

## Testing

- **Go unit tests** (`internal/snapshot`): exposure tiers + the redaction
  guarantee (stats/all-stats sessions+messages absent from data; session-detail
  routes empty), view configs.
- **Snapshot e2e** (`make e2e-snapshot`, separate from the main app's `make
  e2e`): `scripts/e2e-snapshot-server.sh` seeds a fixture, exports a *gated*
  view, loads it into a local D1 (schema+data via SQL before the port opens, so
  Playwright never races the load), mints a deterministic grant, and serves it
  on :8799. `frontend/e2e-snapshot/auth.spec.ts` drives the browser: tokenless ->
  private prompt; `?t=` -> loads + scrubs the URL; reload -> LS persistence;
  bad token -> rejected. This run caught a **fail-open bug**: the Worker cached
  `gated=false` on a transient missing-meta read — now fails closed (deny +
  don't cache) when view/meta is unavailable.

## Phases

- **P1 — Redaction tiers** in the exporter (full/stats-only/excluded). **Exporter
  half DONE**: `export-snapshot` takes `--project` (full), `--stats-project`
  (stats), `--all-stats` (everything stats); data NDJSON is written only for the
  full-tier data scope while analytics precompute runs over full+stats. Unit
  tests assert the redaction guarantee (stats/all-stats projects' sessions and
  messages are absent from the exported data). **Also split the analytics
  precompute**: aggregate routes (summary, cost totals, treemap, heatmaps) run
  over full+stats scope, but the *session-detail* routes that carry session
  names/first-messages/prompts (`top-sessions`, `usage/top-sessions`,
  `messages/longest`) run over the *data* scope only — verified in-browser that a
  public all-stats view shows "No sessions/prompts in range" while all cost
  aggregates render. Still TODO in P1: per-project MD blurbs; FE handling of
  stats-only projects (no drill-down).
- **P2 — View configs** (`views/<name>.json`) + `agentsview publish <view>`
  scaffolding (local/build half). **DONE (config + build half)**:
  `ViewConfig` (name/title/full/stats/allStats/blurbs) loads via
  `export-snapshot --view <file>` (overrides the tier flags). The export emits a
  `view/meta` route (title, statsOnly flag, per-project blurbs) into
  analytics_cache, served by the Worker at `/api/v1/view/meta` so a deployed view
  is self-describing. Examples in `views/public.json` and `views/oa.example.json`.
  TODO: the `agentsview publish <view>` deploy command (folds into P4); FE
  consumption of `view/meta` (title banner, blurbs, disable drill-down when
  statsOnly).
- **P3 — Auth** (capability tokens). **Core (P3a) DONE**: no admin secret —
  admin is localhost-only and the deployed Worker has no mutation/admin surface.
  A view config `"access": "token"` marks it gated (-> view/meta.gated). The
  Worker gates every API read except view/meta behind a valid grant token
  (`x-view-token` header or `?t=`), validated (hash, not revoked, not expired)
  against a `grants` table in the view's D1. `scripts/mint-token.mjs` mints
  tokens locally (hashes + inserts; plaintext shown once). Verified by curl:
  meta public, data 401 without / with a bad token, 200 with a valid token.
  **P3c (FE) core DONE**: `resolveViewToken()` runs in main.ts before the app
  mounts — moves `?t=` into the auth-token store (sent as `Authorization:
  Bearer`, which the Worker now also accepts), scrubs it from the URL. The
  existing auth overlay, re-skinned for snapshots ("This view is private — enter
  your access token"), handles the tokenless case. Verified in-browser: no token
  -> private overlay; `?t=<token>` -> full dashboard, URL scrubbed, no errors.
  **TODO**: a "visible views" drawer of unlocked views (with monikers) to
  browse/switch — deferred until multi-view hosting (P4).
- **P4 — Infra orchestration**: provision D1 + upload assets + manage grants;
  one-command publish.
- **P5 — Org/tag theming.**

A sensible first milestone: **P1 + a single public stats view deployed to
`claude.rbw.sh/public`** (no auth) — proves redaction + remote hosting end to
end before layering auth and multi-view.

## Open questions / spikes

1. **Assets per view**: one shared SPA bundle for all views (path-aware base) vs
   per-view bundles (per-view theme baked in). Lean shared + runtime theme/config.
2. **Worker config churn**: adding a D1 binding needs a redeploy. Acceptable for
   curated views; if dynamic view creation is ever wanted, revisit (Workers for
   Platforms / shared-D1-with-care).
3. **MD blurbs**: source (per-project `.md` files in the repo? a config field?)
   and where they render (a project page / tooltip / stats-only placeholder).
4. **OG/share cards** per view (mortgage-viz rewrites OG via middleware) — nice
   for shared links; later.
5. **Custom domain + cert** for `claude.rbw.sh` (one custom domain, path-based).

## Relationship to existing work

This extends, doesn't replace, the snapshot pipeline already built:
`export-snapshot` → bound-param loader → D1 → Worker (`snapshot-site/`) → SPA in
`VITE_SNAPSHOT` mode, with analytics + usage precompute. P1 generalizes the
project filter into view configs + tiers; P3/P4 add auth + remote provisioning;
P5 adds theming. See `snapshot-export-d1-cfw.md` for the foundation.
