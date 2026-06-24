// Convert a snapshot's NDJSON data files to SQL statements on stdout, for
// loading a published view's data via `wrangler d1 execute --remote --file`.
//
// Rows whose inline INSERT would exceed D1's 100 KB per-statement limit are
// emitted as a small INSERT plus chunked `col = col || '...'` appends, each
// statement staying under the limit. This keeps deploys pure-`wrangler` (no API
// token, no Worker ingestion surface) even for large analytics_cache bodies or
// message content — see scripts/load.mjs for the local /admin/load path.
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

// Primary key per table, used to target chunked appends at a single row.
const keyCol = {
  sessions: "id",
  messages: "id",
  tool_calls: "id",
  analytics_cache: "route",
};

// Keep every emitted statement comfortably under D1's 100 KB limit.
const MAX_STMT = 90_000;
const CHUNK = 80_000;

function sqlVal(v) {
  if (v === null || v === undefined) return "NULL";
  if (typeof v === "number") return String(v);
  if (typeof v === "boolean") return v ? "1" : "0";
  return "'" + String(v).replace(/'/g, "''") + "'";
}

// Split an already-escaped SQL string literal body (without surrounding quotes)
// into <=size pieces, never cutting through an escaped '' quote pair.
function splitEscaped(escaped, size) {
  const parts = [];
  let i = 0;
  while (i < escaped.length) {
    let end = Math.min(i + size, escaped.length);
    while (
      end < escaped.length &&
      end > i + 1 &&
      (escaped[end - 1] === "'" || escaped[end] === "'")
    ) {
      end--;
    }
    parts.push(escaped.slice(i, end));
    i = end;
  }
  return parts;
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
    const vals = cols.map((c) => sqlVal(row[c]));
    const stmt = `INSERT INTO "${t}" (${colList}) VALUES (${vals.join(", ")});`;
    if (stmt.length <= MAX_STMT) {
      out.push(stmt);
      continue;
    }
    // Oversized row: chunk its largest string column via `||` appends.
    const key = keyCol[t];
    if (!key || !(key in row)) {
      throw new Error(
        `row in "${t}" exceeds ${MAX_STMT} bytes but has no known key ` +
          `column to chunk against (cols: ${cols.join(", ")})`,
      );
    }
    let bigCol = null;
    let bigLen = -1;
    for (const c of cols) {
      if (c === key) continue;
      if (typeof row[c] === "string" && row[c].length > bigLen) {
        bigCol = c;
        bigLen = row[c].length;
      }
    }
    if (bigCol === null) {
      throw new Error(`row in "${t}" is oversized but has no string column`);
    }
    const escaped = String(row[bigCol]).replace(/'/g, "''");
    const pieces = splitEscaped(escaped, CHUNK);
    const where = `"${key}" = ${sqlVal(row[key])}`;
    // First piece goes in via INSERT (all other columns set normally).
    const firstVals = cols.map((c) =>
      c === bigCol ? `'${pieces[0]}'` : sqlVal(row[c]),
    );
    out.push(
      `INSERT INTO "${t}" (${colList}) VALUES (${firstVals.join(", ")});`,
    );
    for (let p = 1; p < pieces.length; p++) {
      out.push(
        `UPDATE "${t}" SET "${bigCol}" = "${bigCol}" || '${pieces[p]}' ` +
          `WHERE ${where};`,
      );
    }
  }
}
process.stdout.write(out.join("\n") + "\n");
