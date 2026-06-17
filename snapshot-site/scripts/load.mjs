// Load a snapshot's NDJSON data into the running snapshot Worker's D1 via
// the bound-param /admin/load endpoint (large message content never goes
// through SQL statement text).
//
// Apply schema.sql first (with the Worker stopped), then start the Worker,
// then run this:
//
//   wrangler d1 execute agentsview-snapshot --local --file <dir>/schema.sql
//   wrangler dev --port 8799 &
//   node scripts/load.mjs <dir> [http://127.0.0.1:8799]
import { readFileSync } from "node:fs";
import { join } from "node:path";

const snapDir = process.argv[2];
const base = process.argv[3] ?? "http://127.0.0.1:8799";
if (!snapDir) {
  console.error("usage: node load.mjs <snapshot-dir> [worker-base-url]");
  process.exit(1);
}

for (const table of ["sessions", "messages"]) {
  const body = readFileSync(join(snapDir, `${table}.ndjson`), "utf8");
  const res = await fetch(`${base}/admin/load?table=${table}`, {
    method: "POST",
    body,
  });
  const out = await res.json();
  if (!res.ok) {
    console.error(`load ${table} failed (${res.status}):`, out);
    process.exit(1);
  }
  console.log(`loaded ${table}: ${out.inserted} rows`);
}
