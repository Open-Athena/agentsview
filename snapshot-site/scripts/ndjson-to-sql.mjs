// Convert a snapshot's NDJSON data files to SQL INSERT statements on stdout,
// for loading small datasets (e2e fixtures) via `wrangler d1 execute --file`
// before the dev server starts — avoiding the load-after-start race.
//
// NOTE: inline SQL literals; only suitable for small data (no rows whose
// content would blow D1's 100 KB statement limit). Production loads use the
// bound-param loader (scripts/load.mjs).
//
//   node scripts/ndjson-to-sql.mjs <snapshot-dir> > data.sql
import { readFileSync, existsSync } from "node:fs";
import { join } from "node:path";

const dir = process.argv[2];
if (!dir) {
  console.error("usage: node ndjson-to-sql.mjs <snapshot-dir>");
  process.exit(1);
}

const tables = ["sessions", "messages", "tool_calls", "analytics_cache"];

function sqlVal(v) {
  if (v === null || v === undefined) return "NULL";
  if (typeof v === "number") return String(v);
  if (typeof v === "boolean") return v ? "1" : "0";
  return "'" + String(v).replace(/'/g, "''") + "'";
}

const out = [];
for (const t of tables) {
  const f = join(dir, `${t}.ndjson`);
  if (!existsSync(f)) continue;
  for (const line of readFileSync(f, "utf8").split("\n")) {
    if (!line.trim()) continue;
    const row = JSON.parse(line);
    const cols = Object.keys(row);
    const colList = cols.map((c) => `"${c}"`).join(", ");
    const vals = cols.map((c) => sqlVal(row[c])).join(", ");
    out.push(`INSERT INTO "${t}" (${colList}) VALUES (${vals});`);
  }
}
process.stdout.write(out.join("\n") + "\n");
