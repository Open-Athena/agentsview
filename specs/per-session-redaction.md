# Per-session redaction ("private" sessions)

## Goal

Extend the publishable-views redaction model (currently per-project full /
stats-only / excluded, see `publishable-views.md`) down to **per-session**
granularity, so a mostly-shareable view can include individual sessions in
aggregate stats while eliding their transcripts on click-through.

Motivating case (real, from the archive): a `Downloads` scratch session titled
"convert rust.png to rust.jpg" is 727 messages of mostly-innocuous work
(slack-emoji generation, tmux config) with a private chunk woven through
(filling a TIAA custodial-account form — SSN, family names, medical/insurance
PDFs). Project-level tiers can't catch it: it lives in a generic project and its
title is innocuous. Whole-session elision is the right granularity — per-message
redaction would be fragile against scattered private content.

## Model

A session marked **private** is a *ghost row* in the export:

- Its `sessions` row IS exported (so it still appears in the list and is
  click-through-able), but `first_message` and `display_name` are scrubbed to
  `[redacted]` and `private = 1`.
- Its `messages` / `tool_calls` are NOT exported → click-through renders a
  "transcript redacted" placeholder (nothing to show).
- It still contributes to **aggregate** analytics (summary/activity/heatmap/
  usage totals) — those run over the analytics scope, which keeps private
  sessions.
- It is excluded from **session-detail** routes (top-sessions, usage top
  sessions, longest-prompts) so its scrubbed title / prompts never surface.

This reuses the exporter's existing two-scope split (analytics scope vs data
scope): `contentIDs = dataIDs − privateIDs` drives messages/tool_calls and the
detail routes; `analyticsIDs` is unchanged.

Redaction is by **omission at export time**, never FE-hiding: the private
transcript is physically absent from the published D1, so it cannot leak via
the API, devtools, or an FE bug.

## Marking sessions private

Persistent `sessions.private` boolean (migration, default 0). It is a
user/system-managed column, NOT written by the sync upsert (not in the INSERT
column list or `ON CONFLICT DO UPDATE SET`), so it **survives re-syncs** exactly
like `deleted_at`.

Sources of the private set for an export:

1. The `private` column (primary) — toggled per session.
2. `Options.PrivateSessions` — an explicit id list in the view config (one-offs
   without the flag).

## Suggestions (assist, never auto-trust)

Marking is human-approved, but seeded from two signals already in the DB:

- `secret_leak_count > 0` (10 sessions today) — API keys/tokens in transcripts.
- The private-topic content scan (medical/insurance, tax/PII, financial,
  legal) — see `tmp/scan_private2.py` for the prototype.

Keyword matching may only *suggest*; a false negative would leak, so nothing is
auto-redacted.

## Phases

- **P1 — schema + flag.** `private` column (schema.sql + migrateColumns);
  `SetSessionPrivate(id, bool)`; preserved across sync. DONE.
- **P2 — exporter elision.** `privateIDs` / `contentIDs`; ghost-row scrub in
  sessions.ndjson; detail routes over `contentIDs`; `Options.PrivateSessions`;
  `Manifest.PrivateCount`. DONE.
- **P3 — server API.** `POST /api/v1/sessions/:id/private` (local/admin only);
  `private` on session list/detail responses; a redaction-suggestions endpoint.
  TODO.
- **P4 — Worker.** Passes `private` through (SELECT * already carries it);
  session-detail for a ghost returns metadata + `private:true`, empty messages.
  Mostly free. TODO.
- **P5 — FE.** "Redacted" placeholder on a private session's detail view; a
  "mark private" toggle in local/admin mode; a suggested-redactions surface.
  TODO.
