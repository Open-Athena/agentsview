# `agentsview publish --preview`: scan, review, redact locally, then publish

Phase 2 of the transcripts hub (`~/c/oa/transcripts/specs/hub.md`, "Flow" and "Scanner"). Today a hub project is published by hand from `~/c/oa/transcripts`:

1. `scripts/slice.py` cuts the project out of the archive into a scratch DB.
2. `agentsview secrets scan` runs on the scratch DB.
3. `agentsview export-snapshot` exports it.
4. `scripts/redact.py` scrubs the NDJSON.
5. `scripts/load-data.sh` loads it into D1.

This spec moves steps 1–4 into agentsview, so any teammate can preview and publish their own sessions. The hub's `scripts/redact.py` and `scripts/slice.py` are the reference behavior; port them, don't call them.

## What exists already

- **Upstream's scanner**: `internal/secrets` (rules; `rules_version`), and the `secret_findings` table (`location_kind` message/tool_input/tool_result/tool_result_event, byte offsets `match_start`/`match_end` into that text).
  - `agentsview secrets scan` runs the full ruleset (definite + candidate); `agentsview secrets list` lists findings.
- **What the disky publish (2026-11-03) showed about the scanner**:
  - 4 definite findings: 2 `github-pat`, 2 `slack-token`. Plus 19 `basic-auth-url` and 7 `jwt` candidates.
  - About 43k `high-entropy-assignment` candidates, nearly all noise (`python3` paths, hashes, IDs). Redacting them blindly would wreck the transcripts. `redact.py` keeps one only when its variable name looks secret (`KEY|TOKEN|SECRET|PASS|…`, and not `_ID|_URL|_BUCKET|…` unless it contains `KEY`).
  - The scanner missed most real leaks: dozens of R2, Cloudflare, CoreWeave and GCS credentials, a Pulumi passphrase, a Discord webhook, and a Slack signing secret, all printed by `env`/`cat .envrc`-style tool calls. They were caught only by **exact values from the environment `direnv` resolves for each session's cwd** (`direnv exec <dir> env`, minus the baseline `direnv exec / env`, plus `.dev.vars` / `.env*`). This is the single most valuable detector, and it never needs to print a value.
- **Fork features in place**: `export-snapshot` (views, tiers, per-session `private`), `snapshot-site/`, and the `VITE_SNAPSHOT` SPA.

## Wanted

### `agentsview publish <project> --preview`

A read-only command that sends nothing anywhere.

- **Session selection**: by project (repeatable), with optional `--session` / `--since`. Sessions marked `private` are skipped.
- **Scan**:
  - upstream's full ruleset over the selected sessions;
  - the env-value detector: the distinct `cwd`s of the selected sessions, each resolved through `direnv` if it's installed, otherwise by reading `.envrc` / `.env*` / `.dev.vars` files. Values are held in memory and compared exactly (including JSON-escaped forms), never logged.
  - **Deny-list**: `~/.config/agentsview/publish-deny.txt`, flag only.
- **Report**:
  - per session: finding counts by kind and default action (redact or flag), with locations given as message ordinals.
  - JSON output (`--json`) for scripting.
- **Review UI**: served by the local daemon, e.g. `/publish/<project>`. It reuses the session view with findings highlighted (masked) and per-session toggles:
  - exclude this time;
  - mark `private` (persisted with the existing `private` flag);
  - keep a specific finding unredacted.

  Decisions persist locally, in a small table keyed by session plus finding fingerprint, so a re-preview remembers them.

### `agentsview publish <project> --out <dir>`

The same selection and scan, then:
- an `export-snapshot`-format directory with redactions applied: `[REDACTED:<kind>]`, the same token the hub uses;
- `manifest.json` with contributor (from config or `--contributor`), machine, per-session visibility (default `draft`), and the scan summary.

The hub's `load-data.sh` / `variants.py` can already consume that directory. Phase 3 then swaps `--out` for an upload to `POST https://transcripts.oa.dev/<project>/api/ingest`.

## Upstreamability

Keep the hub-specific parts out of core. The `publish` command, the env-value detector, and the review page are generic: "share a redacted snapshot of my sessions". They suit an upstream PR after the snapshot pipeline itself (see `~/c/oa/transcripts/specs/hub.md` "Upstream"). Contributor/visibility metadata in the manifest is generic too; only the ingest URL is hub-specific.

## Tests

- **Env detector**: a fixture session that `cat`s a temp `.envrc` gets every secret-named value redacted. A non-secret ID (`*_ACCOUNT_ID`) and a bucket name (`*_BUCKET`) are left alone.
- **High-entropy filter**: `FOO_TOKEN=<random>` is redacted. `PYTHON=/usr/bin/python3` and a bare hex hash are not.
- **Round trip**: the redacted NDJSON parses line by line, and no collected value survives anywhere in the output dir. Assert exact counts per kind.

## Status

### Phase 1: done (CLI preview and `--out`)

`agentsview publish <project>... (--preview | --out <dir>)`, in `internal/publish` and `cmd/agentsview/publish.go`. Flags: `-S/--session` (repeatable; includes subagents), `-s/--since`, `-c/--contributor` (default `$AGENTSVIEW_CONTRIBUTOR`), `-v/--visibility` (`draft`|`oa`|`link`, default `draft`), `-D/--no-direnv`, `-d/--deny`, `-j/--json`.

How it works:
1. **Select**: the projects' sessions plus subagent children, narrowed by `--session` / `--since`.
2. **Slice**: copy just those sessions into a scratch DB (a Go port of `slice.py`, reading the archive `mode=ro`) and open it with `db.Open`, which migrates it. An archive written by an older agentsview therefore still exports. Before this, a real archive from a June build failed on a renamed `model_pricing` column. `TestPublishOlderArchiveSchema` covers that case.
3. **Export** with `export-snapshot`'s code (`snapshot.Options.OnlySessions` is new), into `--out` or a temporary directory for `--preview`.
4. **Scan** every string value of every NDJSON row, token by token, so numbers and names pass through verbatim. Detectors, in order:
   - collected env values (raw and JSON-escaped, longest first) → redact;
   - the full scanner ruleset. Definite matches, `jwt`, `basic-auth-url` and `private-key-block` → redact. `high-entropy-assignment` → redact only when the variable name looks secret (the `redact.py` name rules), otherwise counted as ignored. Other candidates → flag;
   - deny-list terms (case-insensitive) → flag.
5. **Report**: per session, the kinds, actions, counts and message ordinals. Findings in analytics bodies (the longest-prompts panel repeats prompt text) are reported separately. Titles in the report are scrubbed.
6. **`--out` only**: rewrite the redacted files, then **fail closed** if any collected value survives anywhere in the directory. `manifest.json` gains a `publish` key holding `contributor`, `machines`, per-session `visibility`, and redaction/flag totals.

Deviations from the spec above:
- The deny-list lives at `<data dir>/publish-deny.txt` (default `~/.agentsview/publish-deny.txt`), next to the archive, instead of `~/.config/agentsview/`.
- The scan runs over the export, not over `secret_findings`. That covers exactly what would be published, and nothing needs `agentsview secrets scan` first. Content the export omits (for example `tool_result_events`) is never published, so it isn't scanned.

On disky (587 sessions, 69k messages), from a scratch copy, the preview takes about a minute: slice 10s, export 20s, env 11s, scan 23s. It reports 1,445 redactions, made up of 1,387 secret-named assignments, 19 `basic-auth-url`, 7 `jwt`, and 32 env values (R2, Cloudflare, Pulumi, Discord, Slack, GCS). It ignores 56,622 high-entropy candidates.

### Phase 2: not started (review UI and decisions)

- `/publish/<project>` in the local daemon: the session view with findings highlighted, plus per-session toggles (exclude, mark `private`, keep a finding).
- A local decisions table keyed by session plus finding fingerprint, applied by both `--preview` and `--out`.
