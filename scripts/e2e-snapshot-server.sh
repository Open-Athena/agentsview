#!/usr/bin/env bash
# E2E web server for the snapshot/publishable-views stack: seed a fixture DB,
# export a *gated* view, load it into a local D1, and serve it via the
# snapshot Worker on :8799 with a known grant token minted in.
#
# Used as Playwright's webServer (see frontend/playwright.snapshot.config.ts).
# Playwright waits for /api/v1/view/meta to return 200, which only happens
# after the data load completes — so tests never race the load.
#
# The minted token is deterministic ("e2e-token") so specs can use it.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PORT="${E2E_SNAPSHOT_PORT:-8799}"
DB_NAME="agentsview-snapshot"
TOKEN="e2e-token"

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT
export AGENTSVIEW_DATA_DIR="$TMPDIR"
DB_PATH="$TMPDIR/sessions.db"
SNAP="$TMPDIR/snap"

echo "[e2e-snapshot] building fixture + binary..."
FIXTURE="$TMPDIR/testfixture"
AGENTSVIEW="$TMPDIR/agentsview"
CGO_ENABLED=1 go build -tags fts5 -o "$FIXTURE" "$ROOT/cmd/testfixture"
CGO_ENABLED=1 go build -tags fts5 -o "$AGENTSVIEW" "$ROOT/cmd/agentsview"
"$FIXTURE" -out "$DB_PATH"

echo "[e2e-snapshot] building snapshot frontend..."
( cd "$ROOT/frontend" && VITE_SNAPSHOT=1 npm run build >/dev/null )
rm -rf "$ROOT/snapshot-site/public"
cp -r "$ROOT/frontend/dist" "$ROOT/snapshot-site/public"

echo "[e2e-snapshot] exporting gated view..."
"$AGENTSVIEW" export-snapshot --view "$ROOT/views/e2e-gated.json" -o "$SNAP"

cd "$ROOT/snapshot-site"
echo "[e2e-snapshot] resetting local D1..."
rm -rf .wrangler/state/v3/d1

# Load schema + data + grant while the dev server is stopped, so the view is
# fully populated BEFORE the port opens — Playwright never races the load, and
# there's no SQLITE_BUSY contention.
echo "[e2e-snapshot] loading schema + data into D1..."
npx wrangler d1 execute "$DB_NAME" --local --file "$SNAP/schema.sql" >/dev/null
node scripts/ndjson-to-sql.mjs "$SNAP" > "$TMPDIR/data.sql"
npx wrangler d1 execute "$DB_NAME" --local --file "$TMPDIR/data.sql" >/dev/null

HASH="$(node -e 'const c=require("crypto");process.stdout.write(c.createHash("sha256").update(process.argv[1]).digest("base64url"))' "$TOKEN")"
npx wrangler d1 execute "$DB_NAME" --local --command \
  "INSERT INTO grants (id, token_hash, name, created_at) VALUES ('e2e', '$HASH', 'e2e login', '2026-01-01T00:00:00Z')" >/dev/null

echo "[e2e-snapshot] ready (token: $TOKEN); starting Worker on :$PORT..."
exec npx wrangler dev --port "$PORT"
