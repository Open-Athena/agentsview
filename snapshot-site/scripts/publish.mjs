// Build a self-contained, deployable bundle for a published view (P4).
//
//   node scripts/publish.mjs <view-config.json>
//
// DRY-RUN by design: it exports the view, builds the SPA, and writes a
// publish bundle (dist/publish/<name>/) containing the snapshot data, the
// Worker + assets, a generated wrangler.jsonc, and a deploy.sh with the exact
// Cloudflare commands — then prints the plan. It provisions NOTHING. Review
// the bundle, then run its deploy.sh when ready (that's what touches your CF
// account: wrangler d1 create / execute / deploy).
import { execFileSync } from "node:child_process";
import {
  readFileSync, writeFileSync, mkdirSync, rmSync, cpSync, chmodSync,
} from "node:fs";
import { join, resolve, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const SITE = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const ROOT = resolve(SITE, "..");

const viewArg = process.argv[2];
if (!viewArg) {
  console.error("usage: node scripts/publish.mjs <view-config.json>");
  process.exit(1);
}
const viewConfigPath = resolve(viewArg);
const view = JSON.parse(readFileSync(viewConfigPath, "utf8"));
const name = view.name;
if (!name) {
  console.error("view config has no name");
  process.exit(1);
}
const dbName = `agentsview-${name}`;
const gated = view.access === "token";
const PUB = join(SITE, "dist", "publish", name);

console.log(`\n=== publish bundle: ${name} (DRY-RUN — provisions nothing) ===`);
rmSync(PUB, { recursive: true, force: true });
mkdirSync(PUB, { recursive: true });

// 1. Build the CLI and export the view's snapshot.
console.log("• building agentsview + exporting view...");
const bin = join(SITE, "dist", "agentsview");
execFileSync("go", ["build", "-tags", "fts5", "-o", bin, "./cmd/agentsview"], {
  cwd: ROOT,
  env: { ...process.env, CGO_ENABLED: "1" },
  stdio: "inherit",
});
execFileSync(bin, ["export-snapshot", "--view", viewConfigPath, "-o",
  join(PUB, "snap")], { stdio: "inherit" });

// 2. Build the snapshot SPA.
console.log("• building SPA (VITE_SNAPSHOT=1)...");
execFileSync("npm", ["run", "build"], {
  cwd: join(ROOT, "frontend"),
  env: { ...process.env, VITE_SNAPSHOT: "1" },
  stdio: "inherit",
});
cpSync(join(ROOT, "frontend", "dist"), join(PUB, "public"), { recursive: true });

// 3. Worker + data.sql (small-data path; see note in deploy.sh).
cpSync(join(SITE, "src"), join(PUB, "src"), { recursive: true });
const dataSql = execFileSync("node",
  [join(SITE, "scripts", "ndjson-to-sql.mjs"), join(PUB, "snap")],
  { encoding: "utf8", maxBuffer: 1 << 30 });
writeFileSync(join(PUB, "data.sql"), dataSql);

// 4. wrangler.jsonc — NOTE: no ALLOW_LOAD var (the deployed Worker must have
// no ingestion surface). database_id is filled in after `d1 create`.
const wrangler = {
  $schema: "../../node_modules/wrangler/config-schema.json",
  name: `agentsview-${name}`,
  main: "src/index.ts",
  compatibility_date: "2026-06-01",
  assets: { directory: "public", binding: "ASSETS" },
  d1_databases: [{
    binding: "DB",
    database_name: dbName,
    database_id: "<FILL_FROM_d1_create>",
  }],
};
writeFileSync(join(PUB, "wrangler.jsonc"),
  JSON.stringify(wrangler, null, 2) + "\n");

// 5. deploy.sh — the exact commands; run it when ready.
const deploy = `#!/usr/bin/env bash
# Deploy the "${name}" view to Cloudflare. Run from this directory.
# This is the step that provisions resources in your CF account.
set -euo pipefail
cd "$(dirname "$0")"

# 1. Create the D1 (once). Copy the printed database_id into
#    wrangler.jsonc -> d1_databases[0].database_id.
wrangler d1 create ${dbName}

# 2. Load schema + data into the REMOTE D1.
#    NOTE: data.sql uses inline INSERTs (fine for small/medium views). A view
#    with messages over ~100 KB needs a bound-param loader instead (see
#    specs/snapshot-export-d1-cfw.md, loader open question).
wrangler d1 execute ${dbName} --remote --file snap/schema.sql
wrangler d1 execute ${dbName} --remote --file data.sql
${gated ? `
# 3. Gated view — mint an access token into the remote grants table:
node ../../scripts/mint-token.mjs "${name} access" ${dbName} --remote
` : `
# 3. Public view — no token needed.
`}
# 4. Deploy the Worker (serves the SPA + /api/v1 over the D1).
wrangler deploy

# 5. (optional) Route it under claude.rbw.sh in the Cloudflare dashboard or:
#    wrangler deploy --route 'claude.rbw.sh/${name}/*'
`;
writeFileSync(join(PUB, "deploy.sh"), deploy);
chmodSync(join(PUB, "deploy.sh"), 0o755);

console.log(`
✔ Bundle written to: snapshot-site/dist/publish/${name}/
    wrangler.jsonc   worker config (fill database_id after d1 create)
    src/             Worker source
    public/          built SPA (snapshot mode)
    snap/schema.sql  D1 schema
    data.sql         inline INSERTs for the snapshot data
    deploy.sh        the Cloudflare commands (review, then run)

Access: ${gated ? "gated (token required — deploy.sh mints one)" : "public"}

This dry-run provisioned nothing. To deploy:
    cd snapshot-site/dist/publish/${name} && ./deploy.sh
`);
