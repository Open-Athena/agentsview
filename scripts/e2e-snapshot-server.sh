#!/usr/bin/env bash
# E2E web server for the snapshot/publishable-views stack: seed a fixture DB,
# export a view (gated or public), load it into a local D1, and serve it via
# the snapshot Worker.
#
#   e2e-snapshot-server.sh [view-name] [port] [mint]
#     view-name : views/<name>.json (default: e2e-gated)
#     port      : default 8799
#     mint      : "token" to mint the deterministic grant "e2e-token" (gated
#                 views); anything else mints nothing (public views).
#
# Used as Playwright's webServer. Schema + data + grant are loaded BEFORE the
# port opens, so tests never race the load (and no SQLITE_BUSY contention).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
VIEW="${1:-e2e-gated}"
PORT="${2:-8799}"
MINT="${3:-token}"
DB_NAME="agentsview-snapshot"
TOKEN="e2e-token"
VIEW_CONFIG="$ROOT/views/$VIEW.json"

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT
export AGENTSVIEW_DATA_DIR="$TMPDIR"
DB_PATH="$TMPDIR/sessions.db"
SNAP="$TMPDIR/snap"

echo "[e2e-snapshot:$VIEW] building fixture + binary..."
FIXTURE="$TMPDIR/testfixture"
AGENTSVIEW="$TMPDIR/agentsview"
CGO_ENABLED=1 go build -tags fts5 -o "$FIXTURE" "$ROOT/cmd/testfixture"
CGO_ENABLED=1 go build -tags fts5 -o "$AGENTSVIEW" "$ROOT/cmd/agentsview"
"$FIXTURE" -out "$DB_PATH"

echo "[e2e-snapshot:$VIEW] building snapshot frontend..."
( cd "$ROOT/frontend" && VITE_SNAPSHOT=1 npm run build >/dev/null )
rm -rf "$ROOT/snapshot-site/public"
cp -r "$ROOT/frontend/dist" "$ROOT/snapshot-site/public"

echo "[e2e-snapshot:$VIEW] exporting view..."
"$AGENTSVIEW" export-snapshot --view "$VIEW_CONFIG" -o "$SNAP"

cd "$ROOT/snapshot-site"
echo "[e2e-snapshot:$VIEW] resetting local D1..."
rm -rf .wrangler/state/v3/d1

echo "[e2e-snapshot:$VIEW] loading schema + data into D1..."
npx wrangler d1 execute "$DB_NAME" --local --file "$SNAP/schema.sql" >/dev/null
node scripts/ndjson-to-sql.mjs "$SNAP" > "$TMPDIR/data.sql"
npx wrangler d1 execute "$DB_NAME" --local --file "$TMPDIR/data.sql" >/dev/null

if [ "$MINT" = "token" ]; then
  HASH="$(node -e 'const c=require("crypto");process.stdout.write(c.createHash("sha256").update(process.argv[1]).digest("base64url"))' "$TOKEN")"
  npx wrangler d1 execute "$DB_NAME" --local --command \
    "INSERT INTO grants (id, token_hash, name, created_at) VALUES ('e2e', '$HASH', 'e2e login', '2026-01-01T00:00:00Z')" >/dev/null
  echo "[e2e-snapshot:$VIEW] minted token: $TOKEN"
fi

echo "[e2e-snapshot:$VIEW] ready; starting Worker on :$PORT..."
exec npx wrangler dev --port "$PORT"
