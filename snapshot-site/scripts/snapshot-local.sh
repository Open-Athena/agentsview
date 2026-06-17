#!/usr/bin/env bash
# Serve a snapshot locally: reset the local D1, apply schema, start the
# Worker dev server, and load the snapshot's NDJSON over it.
#
# Sequencing matters — applying schema via `wrangler d1 execute --local`
# while `wrangler dev` holds the same local SQLite triggers SQLITE_BUSY.
# So: stop dev -> reset -> schema -> start dev -> load.
#
#   ./scripts/snapshot-local.sh <snapshot-dir> [port]
set -euo pipefail

SNAP="${1:?usage: snapshot-local.sh <snapshot-dir> [port]}"
PORT="${2:-8799}"
DB=agentsview-snapshot
cd "$(dirname "$0")/.."

if [ ! -f "$SNAP/schema.sql" ]; then
  echo "error: $SNAP/schema.sql not found (run 'agentsview export-snapshot' first)" >&2
  exit 1
fi

echo "Stopping any dev server on :$PORT..."
lsof -tiTCP:"$PORT" -sTCP:LISTEN 2>/dev/null | xargs kill 2>/dev/null || true
sleep 1

echo "Resetting local D1..."
rm -rf .wrangler/state/v3/d1

echo "Applying schema..."
npx wrangler d1 execute "$DB" --local --file "$SNAP/schema.sql" >/dev/null

echo "Starting dev server on :$PORT..."
npx wrangler dev --port "$PORT" >"/tmp/snapshot-dev-$PORT.log" 2>&1 &
DEV_PID=$!
disown || true

for _ in $(seq 1 40); do
  if curl -fsS "http://127.0.0.1:$PORT/api/v1/sessions?limit=1" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

echo "Loading data..."
node scripts/load.mjs "$SNAP" "http://127.0.0.1:$PORT"

echo ""
echo "Snapshot serving at http://127.0.0.1:$PORT (dev pid $DEV_PID; 'kill-port $PORT' to stop)"
