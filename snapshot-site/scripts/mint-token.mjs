// Mint a grant token for a gated view by inserting a hashed row into the
// view's D1 `grants` table. The plaintext token is printed once and never
// stored. Local admin only — the deployed Worker never mints.
//
//   node scripts/mint-token.mjs "<moniker>" [db-name] [--remote]
import { execFileSync } from "node:child_process";
import { randomBytes, createHash } from "node:crypto";

const b64url = (buf) =>
  buf.toString("base64").replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");

const name = process.argv[2] ?? "";
const db = process.argv.find((a, i) => i >= 3 && !a.startsWith("--")) ?? "agentsview-snapshot";
const remote = process.argv.includes("--remote");

const token = b64url(randomBytes(24));
const hash = b64url(createHash("sha256").update(token).digest());
const id = b64url(randomBytes(8));
const now = new Date().toISOString();
const nameSql = name ? `'${name.replace(/'/g, "''")}'` : "NULL";

const sql =
  `INSERT INTO grants (id, token_hash, name, created_at) VALUES ` +
  `('${id}', '${hash}', ${nameSql}, '${now}')`;

execFileSync(
  "npx",
  ["wrangler", "d1", "execute", db, remote ? "--remote" : "--local", "--command", sql],
  { stdio: "inherit" },
);

console.log(`\nMinted grant "${name}" (id ${id})`);
console.log(`Token (shown once): ${token}`);
console.log(`Link: http://127.0.0.1:8799/?t=${token}`);
