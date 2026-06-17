// Parity harness: compare the snapshot Worker's /api/v1 responses against
// the live Go server's, route by route, on the salient fields (ignoring
// omitempty / key-order noise). Use while porting routes to D1.
//
//   node scripts/parity.mjs --session <id> [--project awair] \
//     [--go http://127.0.0.1:8090] [--worker http://127.0.0.1:8799] [--q awair]
//
// Exits non-zero if any route diverges.
import { parseArgs } from "node:util";

const { values } = parseArgs({
  options: {
    go: { type: "string", default: "http://127.0.0.1:8090" },
    worker: { type: "string", default: "http://127.0.0.1:8799" },
    session: { type: "string" },
    project: { type: "string", default: "awair" },
    q: { type: "string", default: "awair" },
  },
});

if (!values.session) {
  console.error("error: --session <id> is required");
  process.exit(2);
}

const { go, worker, project, q, session } = values;

// Each route declares the path and a `pick` that projects the response to
// the fields whose equality defines parity.
const routes = [
  {
    name: "sessions-list",
    path: `/api/v1/sessions?project=${project}&include_one_shot=true&include_automated=true&limit=5`,
    pick: (r) => ({
      total: r.total,
      ids: r.sessions.map((s) => s.id),
      automatedIsBool: r.sessions.every((s) => typeof s.is_automated === "boolean"),
    }),
  },
  {
    name: "session-detail",
    path: `/api/v1/sessions/${session}`,
    pick: (r) => ({ id: r.id, project: r.project, message_count: r.message_count }),
  },
  {
    name: "messages",
    path: `/api/v1/sessions/${session}/messages?from=0&limit=5&direction=asc`,
    pick: (r) => ({
      count: r.count,
      rows: r.messages.map((m) => [m.ordinal, m.role, m.content_length]),
      toolUseIsBool: r.messages.every((m) => typeof m.has_tool_use === "boolean"),
    }),
  },
  {
    name: "session-search",
    path: `/api/v1/sessions/${session}/search?q=${q}`,
    pick: (r) => ({ ordinals: r.ordinals }),
  },
  {
    name: "global-search",
    path: `/api/v1/search?q=${q}&limit=3&project=${project}`,
    pick: (r) => ({
      results: r.results.map((x) => [x.session_id, x.ordinal]),
      allMarked: r.results.every((x) => x.snippet.includes("<mark>")),
    }),
  },
];

async function fetchJSON(base, path) {
  const res = await fetch(base + path);
  if (!res.ok) return { __error: `${res.status} ${res.statusText}` };
  return res.json();
}

function eq(a, b) {
  return JSON.stringify(a) === JSON.stringify(b);
}

let failed = 0;
for (const route of routes) {
  const [gj, wj] = await Promise.all([
    fetchJSON(go, route.path),
    fetchJSON(worker, route.path),
  ]);
  if (gj.__error || wj.__error) {
    console.log(`DIFF  ${route.name}: fetch error go=${gj.__error ?? "ok"} worker=${wj.__error ?? "ok"}`);
    failed++;
    continue;
  }
  const gp = route.pick(gj);
  const wp = route.pick(wj);
  if (eq(gp, wp)) {
    console.log(`MATCH ${route.name}`);
  } else {
    failed++;
    console.log(`DIFF  ${route.name}`);
    console.log(`  go    : ${JSON.stringify(gp)}`);
    console.log(`  worker: ${JSON.stringify(wp)}`);
  }
}

console.log(`\n${routes.length - failed}/${routes.length} routes match`);
process.exit(failed > 0 ? 1 : 0);
