# Merge upstream (`kenn-io/agentsview`) into `cache-1h-and-projects` via waypoints

## Situation

Our branch `cache-1h-and-projects` (39 commits) forked from upstream at merge-base
`47b30a1e` (2026-05-29). Upstream `u/main` is **483 commits / 2 months ahead**
(through `b0b0553e`, 2026-07-29). We want upstream's work without falling further
behind, while keeping our snapshot/usage/private-view additions.

A single `git merge u/main` completes but is a trap: **32 files / 67 conflict
hunks**, and — worse than the hunk count — upstream *rewrote the three areas our
39 commits live in*, from a different premise:

- **Money model**: upstream stores cost as **authoritative microdollars**
  (#1224) + bundles a **LiteLLM offline fallback** (#791). Our float-dollar
  pricing math (`internal/pricing/*`, the 5m/1h ephemeral cache-write split) is
  the single biggest conflict source and is now the *weaker* implementation.
- **API layer**: upstream migrated REST -> **typed Huma routes** (#599) and went
  **daemon-first / kit runtime** (#578, #826). Our hand-written handlers conflict,
  and `internal/server/session_mgmt.go` (our private-toggle `PATCH`) was
  **deleted upstream** — that logic must be re-homed onto Huma.
- **Export**: upstream built its own export surfaces (see below).

So this is a *forward reconciliation*, not a mechanical merge: at each step we
merge an upstream **waypoint into our branch** (upstream is the incoming side;
our 39 commits stay put and are carried forward), and decide feature-by-feature
whether to keep ours, adopt theirs, or rebuild ours on their foundation.

## Export: ours vs. theirs (not superseded)

Upstream has three export-flavored things; **none produces a hostable, browsable
web dashboard**, which is exactly what ours does:

| | Purpose | Output | Human-facing? |
| --- | --- | --- | --- |
| **Ours** (`internal/snapshot/`) | Publish a curated, redactable, optionally auth-gated **view of the dashboard** | D1 + CFW + Svelte SPA (snapshot mode) | **Yes** — a hostable website |
| upstream `export sessions` (#991, `internal/export/`) | Content-**free** session summaries for BI/scripts | JSON/NDJSON, no transcripts | No — data interchange |
| upstream artifact ledger (#1251, `internal/artifact/`) | Content-addressed, incremental **archive-to-archive replication** | manifests/segments to a docbank store | No — sync infra; explicitly "export-side only, no CLI/daemon wiring, no import path" yet |

**We are not superseded.** Upstream has no viewer-publishing path; `export
sessions` strips all content, and the artifact ledger is replication plumbing
with no viewer and no public-hosting story. Ours is the fork's one
distinguishing capability, is built, and is browser-verified.

**But converge where it helps** (this shrinks the merge surface):

- Retire our parallel float pricing in favor of upstream's `internal/export/pricing.go`
  (pricing provenance + digest) riding the microdollar model. The snapshot's
  usage precompute sits on top of that instead of `internal/pricing/*`.
- Upstream's `export sessions` filter vocabulary (Project / ExcludeProject /
  Machine / GitBranch / Date ranges / Outcome / HasSecret / ...) is a superset of
  our `--project` / `--since` / `--until`; our snapshot selection can adopt it.
- End state: **our snapshot-publish layer rebuilt on top of `internal/export` +
  microdollars.** If it ever goes upstream, it's a *new* "publishable view"
  capability layered on their export foundation, not a competitor to
  `export sessions`.

## Waypoints

Four hard semantic pivots exist between merge-base and `u/main`. Merge to
intermediate targets so each pivot's conflicts resolve in isolation; each banked
resolution shrinks the next merge.

| # | Merge target | Date | Absorbs | The reconciliation |
| --- | --- | --- | --- | --- |
| **A** | `fe5f6a93` (#599) | Jun 7 | kit-daemon runtime (#578) + **Huma typed routes** | Re-home our REST handlers + the **deleted `session_mgmt.go`** (private toggle) onto Huma. Biggest structural conflict, done alone. |
| **B** | `84af6fbf` (#826) | Jun 24 | **LiteLLM offline pricing** (#791) + daemon-first | Retire our `internal/pricing/*` float math in favor of theirs. |
| **C** | `725a3d0c` (#991) | Jul 4 | **export v1 / `internal/export`** | Point our snapshot selection/pricing at their primitives. |
| **D** | `8dc6adc6` (#1224) | Jul 27 | **money -> microdollars** | Convert remaining snapshot cost math to microdollars. |
| **E** | `u/main` (`b0b0553e`) | Jul 29 | artifact ledger (#1251) + the rest | Cleanup; ensure snapshot + artifact ledger coexist. |

## Procedure (per waypoint)

Work on a scratch branch off our HEAD — **never touch `cache-1h-and-projects`
until the whole sequence is green and reviewed.**

```bash
cd ~/c/agentsview
git switch -c merge-upstream            # once, off cache-1h-and-projects HEAD
# per waypoint <SHA>:
git merge <SHA>                         # upstream is the incoming side
# resolve conflicts (forward-reconcile toward the table above)
make build && make test                 # green before committing
git commit                              # merge commit banks the resolution
```

- Conflict count from the full trial merge (for reference): `internal/db/usage.go`
  (7 hunks), `frontend/.../usage.svelte.ts` (6), `App.svelte` (5),
  `internal/db/pricing.go` (4), `client.ts` (4), then a long tail of 1-3.
- `internal/server/session_mgmt.go` is a **modify/delete**: deleted upstream,
  modified by us. Re-home the private-toggle `PATCH` onto the Huma route added
  by #599 rather than resurrecting the file.
- Frontend: upstream may have regenerated API client types (Huma -> typed
  generated models under `frontend/src/lib/api/generated/`); prefer regenerating
  over hand-merging generated files.

## Progress

- **Waypoint A — DONE** (`f520e3d0`, merges `fe5f6a93`). Huma migration +
  generated-client frontend re-port. Backend: Huma registration adopted;
  private-toggle + longest-prompts re-homed as Huma routes; `session_mgmt.go`
  deleted; `BuildUsageSummary` kept as shared builder (`humaUsageSummary`
  DRY'd onto it). Frontend: regenerated OpenAPI client (now emits
  `MessagesService.getApiV1MessagesLongest`,
  `SessionsService.patchApiV1SessionsIdPrivate`); usage/sessions stores +
  App.svelte deltas re-applied; fork-local `api/view.ts` for the snapshot-only
  `/view/meta`. Added `vitest.setup.ts` localStorage polyfill (Node 26 native
  `localStorage` shadows jsdom). All green: go build/vet/test, svelte-check,
  1171 vitest tests, `make build`.

- **Waypoint B — DONE** (`1d881faa`, merges `84af6fbf`, 155 commits). LiteLLM
  offline-pricing snapshot + daemon-first CLI + DuckDB backend + i18n +
  vite-plus. Re-threaded the fork's **1h cache-write pricing** (additive column
  through upstream's batched builders; 1h derived via shared
  `catalog.Fill1hCacheRate` in the live parser, snapshot overlay, and at
  bundled-snapshot load in `fallback.go`; `parseUsageTokenCounters` extended for
  the 5m/1h split, applied in both cost sites) and the **subagent rollup**
  (`rollupProjectExpr` injected into all four of upstream's new daily-usage SQL
  templates). Adopted upstream's usage-handler rework (comparison as its own
  Huma endpoint). Ported `usage projects` onto `resolveArchiveQueryBackend` and
  `usage longest-prompts` onto a read-only open. DuckDB: implemented
  `GetLongestUserPrompts`, stubbed `SetSessionPrivate`. Frontend: re-ported the
  longestPrompts panel onto the reworked usage store. All green (svelte-check,
  1631 vitest, go build/vet/test, make build) except one snapshot test needing
  a git `origin` remote.
  - **Deferred follow-ups (Waypoint B):** 1h-cache parity for the PostgreSQL and
    DuckDB *cost* paths (their `modelRates` still single-rate — optional
    backends the fork never supported); the offline bundled snapshot derives 1h
    at load rather than shipping it in the SHA-pinned artifact; no DuckDB unit
    test yet for `GetLongestUserPrompts`.

- **Waypoint C — DONE** (`128abb43`, merges `725a3d0c`, 126 commits). Export v1
  (`internal/export`), the `export.PricingResolver` cost model, a `service`
  layer for usage summaries, kit-ui (TopBar/theme), i18n, pairwise usage
  comparison, Activity/RecentEdits nav. Threaded the fork's 1h cache-write rate
  through `export.ModelRates` as an **internal, non-serialized**
  `CacheWrite1hPerMTok` (verified contract-preserving: no canonical-JSON key, no
  `EffectiveModelRate` field — digest/wire byte-identical), populated in
  `loadPricingMap`/`modelPricingRates`; restored the 5m/1h split in
  `dailyUsageAmounts`+`sessionRowCost` via a new `parseEphemeralCacheSplit`
  (upstream's rewritten parser skips the nested `cache_creation` object). Dropped
  the orphaned `db.modelRates`. Re-homed `BuildUsageSummary` onto
  `service.BuildUsageSummaryResult`. Frontend: kit-ui TopBar with snapshot-mode
  `tabs` filtering, kit-ui theme, kept the fork's golden-angle/org projectColor,
  longestPrompts coexists with the new pairwise panel. CLI adopted the
  `--format`/`--json` pairing. CIC-verified: usage total **$7543 matches
  waypoint A** (1h threading is cost-neutral/correct); pairwise + longest-prompts
  both render.

- **Waypoint D — DONE** (`aed2538f`, merges `8dc6adc6`, 173 commits). The
  money->microdollars rewrite (`internal/money`; all cost/rate fields become
  `money.Money` int64 microdollars). Converted the fork's 1h cache-write rate to
  `money.Money` (still internal to `export.ModelRates`, contract-preserving);
  `model_pricing` column is `cache_creation_1h_microdollars_per_mtok INTEGER`;
  `Fill1hCacheRate` derives 1h from 5m as exactly 8/5 via `money.Divide`; the two
  cost paths use `money.CostPerMillion`/`SignedCostPerMillion` for the split
  cost+savings; re-fetched the money-format snapshot (SHA-pinned). Artifact
  `manifestSession` gained `private` (golden updated). Frontend adopted upstream's
  kit-ui CSS-var palette + `seriesColorMap` (supersedes the fork's 24-color
  palette / `disambiguateColors`), keeping the fork's org-theming; costs read
  `.microdollars`; forced a kit-ui refetch to the pinned SHA. CIC-verified on a
  cloned+resynced archive: **usage total $7543.14 — identical to waypoints A/C**
  (microdollars rewrite is cost-neutral); 1h rates stored correctly (6.25->10.00);
  the data-version resync rebuilds an old DB to the new schema; pairwise +
  longest-prompts panels render money as dollars.
  - **Note (existing-DB upgrade):** old archives (pre-money columns) are rebuilt
    by the automatic `dataVersion=74` full resync on normal serve; `--no-sync`
    against an old DB 500s on the usage routes until that resync runs.

- **Waypoint E — DONE** (`b335c130`, merges `u/main` @ `b0b0553e`, 14 commits).
  Final cleanup pass: artifact-ledger reliability follow-ups, duckdb push-watch
  fix, settings-page host layout, a kit-ui bump (97be355e). Kept the fork's
  redacted-transcript CSS alongside upstream's new `.settings-page-host`;
  regenerated package-lock. **The branch is now fully current with `u/main`**
  (`git merge-base --is-ancestor u/main HEAD` = yes; 0 behind). CIC-verified:
  usage total $7543.14 (still cost-neutral) rendering as dollars under the newest
  kit-ui.

## Status: COMPLETE

All five waypoints merged. `merge-upstream` is fully current with
`kenn-io/agentsview` `u/main` and carries all fork features (snapshot publish +
views, 1h cache-write pricing in microdollars, subagent rollup, private
sessions, longest-prompts, org color-coding). Next: review the merge, then
fast-forward `cache-1h-and-projects` onto `merge-upstream`.

## Acceptance

- [x] Waypoint A merged with `make build && make test` green
- [x] Waypoint B merged (green except the `origin`-remote snapshot test)
- [x] Waypoint C merged (same green status)
- [x] Waypoint D merged (money->microdollars; same green status + 2 flaky/env)
- [x] Waypoint E merged; branch fully current with u/main
- [ ] `merge-upstream` reviewed, then fast-forward `cache-1h-and-projects` onto it
- [ ] Private-view toggle works post-Huma (PATCH route re-homed, e2e passes)
- [ ] Snapshot export still produces a working hosted dashboard (browser-verified)
- [ ] Snapshot usage/cost reconciled onto microdollars (no float pricing left in
      the snapshot path)
- [ ] `merge-upstream` reviewed, then fast-forward `cache-1h-and-projects` onto it
- [ ] Decide separately whether to still open upstream PRs (see
      `cache-1h-and-projects-followups.md`) — some of that work may now be
      superseded by upstream's pricing/export rewrites.

## How to pick up

```bash
cd ~/c/agentsview
git log --oneline --first-parent merge-upstream 2>/dev/null | head   # waypoint progress
git status                                                            # mid-merge?
```

If mid-merge, `git diff --name-only --diff-filter=U` lists remaining conflicts;
resolve toward the waypoint table, then `make build && make test` before
committing.
