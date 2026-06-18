/**
 * Read-only agentsview snapshot Worker.
 *
 * Reimplements the `/api/v1` GET subset the SPA needs (sessions, messages,
 * search) over a D1 database loaded from an `export-snapshot` artifact set.
 * Mutations and SSE are intentionally absent — a snapshot is frozen.
 *
 * `/admin/load` ingests NDJSON rows via bound parameters (so large message
 * content never goes through SQL statement text). It is gated on the
 * ALLOW_LOAD var, which a deployed read-only snapshot must not set.
 */

interface Env {
  DB: D1Database;
  ASSETS: Fetcher;
  ALLOW_LOAD?: string;
}

// Integer columns that represent booleans in the Go JSON contract. D1
// returns them as 0/1; the FE expects true/false.
const SESSION_BOOLS = new Set([
  "has_total_output_tokens",
  "has_peak_context_tokens",
  "is_automated",
  "has_tool_calls",
  "has_context_data",
  "is_truncated",
]);
const MESSAGE_BOOLS = new Set([
  "has_thinking",
  "has_tool_use",
  "is_system",
  "has_context_tokens",
  "has_output_tokens",
  "is_sidechain",
  "is_compact_boundary",
]);

type Row = Record<string, unknown>;

function coerceBools(row: Row, bools: Set<string>): Row {
  const out: Row = { ...row };
  for (const k of bools) {
    if (k in out && out[k] !== null) out[k] = !!out[k];
  }
  return out;
}

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json; charset=utf-8" },
  });
}

function errorJSON(status: number, message: string): Response {
  return json({ error: message }, status);
}

/** Build an FTS5 MATCH expression: AND of quoted terms (phrase-safe). */
function ftsQuery(q: string): string {
  return q
    .trim()
    .split(/\s+/)
    .filter(Boolean)
    .map((t) => `"${t.replace(/"/g, '""')}"`)
    .join(" ");
}

async function handleSessions(env: Env, url: URL): Promise<Response> {
  const project = url.searchParams.get("project");
  const includeOneShot = url.searchParams.get("include_one_shot") === "true";
  const includeAutomated =
    url.searchParams.get("include_automated") === "true";
  const includeChildren =
    url.searchParams.get("include_children") === "true";
  const limit = clampInt(url.searchParams.get("limit"), 50, 1, 500);

  const where: string[] = ["deleted_at IS NULL"];
  const args: unknown[] = [];
  if (project) {
    where.push("project = ?");
    args.push(project);
  }
  // Children (subagents, forks) are hidden from the top-level list unless
  // explicitly requested, matching the Go ListSessions filter.
  if (!includeChildren) {
    where.push("relationship_type NOT IN ('subagent', 'fork')");
  }
  if (!includeAutomated) where.push("is_automated = 0");
  if (!includeOneShot) where.push("user_message_count > 1");
  const whereSQL = where.join(" AND ");

  const total = await env.DB.prepare(
    `SELECT count(*) AS n FROM sessions WHERE ${whereSQL}`,
  )
    .bind(...args)
    .first<{ n: number }>();

  const rows = await env.DB.prepare(
    `SELECT * FROM sessions WHERE ${whereSQL}
       ORDER BY ended_at DESC LIMIT ?`,
  )
    .bind(...args, limit)
    .all<Row>();

  return json({
    sessions: rows.results.map((r) => coerceBools(r, SESSION_BOOLS)),
    total: total?.n ?? 0,
  });
}

async function handleSession(env: Env, id: string): Promise<Response> {
  const row = await env.DB.prepare("SELECT * FROM sessions WHERE id = ?")
    .bind(id)
    .first<Row>();
  if (!row) return errorJSON(404, "session not found");
  const out = coerceBools(row, SESSION_BOOLS);
  // health_score_basis is computed server-side in the Go app; snapshots
  // surface an empty basis (the stored health_score still renders).
  out.health_score_basis = [];
  return json(out);
}

async function handleMessages(
  env: Env,
  id: string,
  url: URL,
): Promise<Response> {
  const from = clampInt(url.searchParams.get("from"), 0, 0, Number.MAX_SAFE_INTEGER);
  const limit = clampInt(url.searchParams.get("limit"), 1000, 1, 5000);
  const desc = url.searchParams.get("direction") === "desc";

  const cmp = desc ? "<=" : ">=";
  const order = desc ? "DESC" : "ASC";
  const rows = await env.DB.prepare(
    `SELECT * FROM messages
       WHERE session_id = ? AND ordinal ${cmp} ?
       ORDER BY ordinal ${order} LIMIT ?`,
  )
    .bind(id, from, limit)
    .all<Row>();

  return json({
    messages: rows.results.map((r) => coerceBools(r, MESSAGE_BOOLS)),
    count: rows.results.length,
  });
}

/** Escape LIKE wildcards for substring search with ESCAPE '\'. */
function escapeLike(s: string): string {
  return s.replace(/[\\%_]/g, (c) => `\\${c}`);
}

async function handleSessionSearch(
  env: Env,
  id: string,
  url: URL,
): Promise<Response> {
  const q = url.searchParams.get("q") ?? "";
  if (!q.trim()) return json({ ordinals: [] });

  // In-session search uses LIKE substring semantics (browser find-bar UX),
  // matching the Go SearchSession: message content OR tool_calls result.
  const like = `%${escapeLike(q)}%`;
  const rows = await env.DB.prepare(
    `SELECT DISTINCT m.ordinal AS ordinal
       FROM messages m
       LEFT JOIN tool_calls tc ON tc.message_id = m.id
      WHERE m.session_id = ? AND m.is_system = 0
        AND (m.content LIKE ? ESCAPE '\\'
             OR tc.result_content LIKE ? ESCAPE '\\')
      ORDER BY m.ordinal ASC`,
  )
    .bind(id, like, like)
    .all<{ ordinal: number }>();

  return json({ ordinals: rows.results.map((r) => r.ordinal) });
}

/** Wrap case-insensitive occurrences of q in <mark> for name-branch snippets. */
function markName(name: string, q: string): string {
  const i = name.toLowerCase().indexOf(q.toLowerCase());
  if (i < 0) return name;
  return `${name.slice(0, i)}<mark>${name.slice(i, i + q.length)}</mark>${name.slice(i + q.length)}`;
}

async function handleSearch(env: Env, url: URL): Promise<Response> {
  const q = url.searchParams.get("q") ?? "";
  const project = url.searchParams.get("project");
  const limit = clampInt(url.searchParams.get("limit"), 20, 1, 200);
  const offset = clampInt(url.searchParams.get("offset"), 0, 0, Number.MAX_SAFE_INTEGER);
  if (!q.trim()) return json({ query: q, results: [], count: 0 });

  const projClause = project ? "AND s.project = ?" : "";
  const projArgs = project ? [project] : [];

  // FTS branch: snippet()/bm25() are only usable in a flat query against the
  // FTS table (not inside a wrapping subquery/window), so rank in SQL and
  // dedup to the best-ranked hit per session in JS — mirroring the Go global
  // search's one-result-per-session behavior.
  const cap = Math.min(2000, (offset + limit) * 25 + 50);
  const fts = await env.DB.prepare(
    `SELECT m.session_id AS session_id, s.project AS project, s.agent AS agent,
            COALESCE(NULLIF(s.display_name, ''), s.first_message, '') AS name,
            m.ordinal AS ordinal, s.ended_at AS session_ended_at,
            snippet(messages_fts, 0, '<mark>', '</mark>', '...', 12) AS snippet,
            bm25(messages_fts) AS rank
       FROM messages_fts
       JOIN messages m ON m.id = messages_fts.rowid
       JOIN sessions s ON s.id = m.session_id
      WHERE messages_fts MATCH ? AND m.is_system = 0
        AND s.deleted_at IS NULL ${projClause}
      ORDER BY bm25(messages_fts), m.ordinal LIMIT ?`,
  )
    .bind(ftsQuery(q), ...projArgs, cap)
    .all<Row>();

  const seen = new Set<unknown>();
  const results: Row[] = [];
  for (const r of fts.results) {
    if (seen.has(r.session_id)) continue;
    seen.add(r.session_id);
    results.push(r);
  }

  // Name branch: sessions whose title matches but had no content hit (Go
  // appends these with ordinal -1). Ordered after FTS hits by recency.
  const like = `%${escapeLike(q)}%`;
  const names = await env.DB.prepare(
    `SELECT s.id AS session_id, s.project AS project, s.agent AS agent,
            COALESCE(NULLIF(s.display_name, ''), s.first_message, '') AS name,
            s.ended_at AS session_ended_at
       FROM sessions s
      WHERE s.deleted_at IS NULL
        AND (s.display_name LIKE ? ESCAPE '\\'
             OR s.first_message LIKE ? ESCAPE '\\') ${projClause}
      ORDER BY julianday(s.ended_at) DESC`,
  )
    .bind(like, like, ...projArgs)
    .all<Row>();
  for (const r of names.results) {
    if (seen.has(r.session_id)) continue;
    seen.add(r.session_id);
    results.push({
      ...r,
      ordinal: -1,
      snippet: markName(String(r.name ?? ""), q),
      rank: 0,
    });
  }

  // Match Go's outer ordering: rank ASC, then recency, then id. (Go also
  // tiebreaks on match position within the message, which we don't track —
  // so deep-tie orderings can still differ slightly.)
  results.sort((a, b) => {
    const ra = Number(a.rank ?? 0);
    const rb = Number(b.rank ?? 0);
    if (ra !== rb) return ra - rb;
    const ea = String(a.session_ended_at ?? "");
    const eb = String(b.session_ended_at ?? "");
    if (ea !== eb) return ea < eb ? 1 : -1;
    return String(a.session_id) < String(b.session_id) ? -1 : 1;
  });

  const page = results.slice(offset, offset + limit);
  const body: Record<string, unknown> = {
    query: q,
    results: page,
    count: page.length,
  };
  if (results.length > offset + limit) body.next = offset + limit;
  return json(body);
}

async function handleAdminLoad(
  req: Request,
  env: Env,
  url: URL,
): Promise<Response> {
  if (env.ALLOW_LOAD !== "1") return errorJSON(403, "load disabled");
  const table = url.searchParams.get("table");
  const LOADABLE = new Set([
    "sessions",
    "messages",
    "tool_calls",
    "analytics_cache",
  ]);
  if (!table || !LOADABLE.has(table)) {
    return errorJSON(400, `table must be one of ${[...LOADABLE].join(", ")}`);
  }

  const text = await req.text();
  const lines = text.split("\n").filter((l) => l.trim().length > 0);
  if (lines.length === 0) return json({ table, inserted: 0 });

  const rows = lines.map((l) => JSON.parse(l) as Row);
  const cols = Object.keys(rows[0]);
  const colList = cols.map((c) => `"${c}"`).join(", ");
  const placeholders = cols.map(() => "?").join(", ");
  const stmt = env.DB.prepare(
    `INSERT INTO ${table} (${colList}) VALUES (${placeholders})`,
  );

  let inserted = 0;
  const BATCH = 100;
  for (let i = 0; i < rows.length; i += BATCH) {
    const slice = rows.slice(i, i + BATCH);
    const batch = slice.map((r) =>
      stmt.bind(...cols.map((c) => (r[c] === undefined ? null : r[c]))),
    );
    await env.DB.batch(batch);
    inserted += slice.length;
  }
  return json({ table, inserted });
}

// Default top-level list filter, matching the Go session list's default
// (no children, no automated, no one-shot sessions).
const LIST_FILTER =
  "deleted_at IS NULL AND relationship_type NOT IN ('subagent', 'fork')" +
  " AND is_automated = 0 AND user_message_count > 1";

async function handleProjects(env: Env): Promise<Response> {
  const rows = await env.DB.prepare(
    `SELECT project AS name, count(*) AS session_count
       FROM sessions WHERE ${LIST_FILTER}
      GROUP BY project ORDER BY project`,
  ).all<Row>();
  return json({ projects: rows.results });
}

async function handleAgents(env: Env): Promise<Response> {
  const rows = await env.DB.prepare(
    `SELECT agent AS name, count(*) AS session_count
       FROM sessions WHERE ${LIST_FILTER}
      GROUP BY agent ORDER BY agent`,
  ).all<Row>();
  return json({ agents: rows.results });
}

async function handleMachines(env: Env): Promise<Response> {
  const rows = await env.DB.prepare(
    `SELECT DISTINCT machine FROM sessions WHERE ${LIST_FILTER}
      ORDER BY machine`,
  ).all<{ machine: string }>();
  return json({ machines: rows.results.map((r) => r.machine) });
}

async function handleStats(env: Env): Promise<Response> {
  const row = await env.DB.prepare(
    `SELECT count(*) AS session_count,
            count(DISTINCT project) AS project_count,
            count(DISTINCT machine) AS machine_count,
            min(started_at) AS earliest_session
       FROM sessions WHERE ${LIST_FILTER}`,
  ).first<Row>();
  const msgs = await env.DB.prepare(
    "SELECT count(*) AS n FROM messages",
  ).first<{ n: number }>();
  return json({
    session_count: row?.session_count ?? 0,
    message_count: msgs?.n ?? 0,
    project_count: row?.project_count ?? 0,
    machine_count: row?.machine_count ?? 0,
    earliest_session: row?.earliest_session ?? null,
  });
}

// Serve a precomputed dashboard response verbatim. Snapshots freeze the
// all-time view, so query params (date range, metric/granularity toggles)
// are ignored — the precomputed default is returned.
async function handleAnalytics(env: Env, routeKey: string): Promise<Response> {
  const row = await env.DB.prepare(
    "SELECT body FROM analytics_cache WHERE route = ?",
  )
    .bind(routeKey)
    .first<{ body: string }>();
  if (!row) return errorJSON(404, "analytics route not precomputed");
  return new Response(row.body, {
    status: 200,
    headers: { "content-type": "application/json; charset=utf-8" },
  });
}

async function handleSidebarIndex(env: Env, url: URL): Promise<Response> {
  const project = url.searchParams.get("project");
  const where = [LIST_FILTER];
  const args: unknown[] = [];
  if (project) {
    where.push("project = ?");
    args.push(project);
  }
  const rows = await env.DB.prepare(
    `SELECT id, project, machine, agent, started_at, ended_at, created_at,
            termination_status, message_count, user_message_count,
            is_automated
       FROM sessions WHERE ${where.join(" AND ")}
      ORDER BY ended_at DESC`,
  )
    .bind(...args)
    .all<Row>();
  return json({
    sessions: rows.results.map((r) => ({
      ...coerceBools(r, SESSION_BOOLS),
      is_teammate: false,
    })),
  });
}

async function handleChildren(env: Env, id: string): Promise<Response> {
  const rows = await env.DB.prepare(
    `SELECT * FROM sessions
      WHERE parent_session_id = ? AND deleted_at IS NULL
      ORDER BY started_at ASC`,
  )
    .bind(id)
    .all<Row>();
  return json(rows.results.map((r) => coerceBools(r, SESSION_BOOLS)));
}

// Static stubs for endpoints that have no meaning in a frozen snapshot.
// agent_dirs is intentionally empty so a published snapshot leaks no local
// filesystem paths.
function snapshotStub(path: string): Response | null {
  switch (path) {
    case "/api/v1/version":
      return json({ version: "snapshot", commit: "snapshot", build_date: "" });
    case "/api/v1/settings":
      return json({ agent_dirs: {} });
    case "/api/v1/starred":
      return json({ session_ids: [] });
    case "/api/v1/pins":
      return json({ pins: [] });
    case "/api/v1/sync/status":
      return json({
        last_sync: null,
        stats: { total_sessions: 0, synced: 0, skipped: 0, failed: 0 },
      });
    case "/api/v1/update/check":
      return json({
        update_available: false,
        current_version: "snapshot",
        latest_version: "snapshot",
        is_dev_build: false,
      });
    case "/api/v1/config/github":
      return json({ configured: false });
    case "/api/v1/config/terminal":
      return json({ mode: "auto" });
    default:
      return null;
  }
}

function clampInt(
  raw: string | null,
  dflt: number,
  min: number,
  max: number,
): number {
  if (raw === null || raw === "") return dflt;
  const n = parseInt(raw, 10);
  if (!Number.isFinite(n)) return dflt;
  return Math.max(min, Math.min(max, n));
}

export default {
  async fetch(req: Request, env: Env): Promise<Response> {
    const url = new URL(req.url);
    const path = url.pathname;

    if (req.method === "POST" && path === "/admin/load") {
      return handleAdminLoad(req, env, url);
    }

    if (req.method !== "GET") {
      return errorJSON(405, "method not allowed (snapshot is read-only)");
    }

    // Non-API GET that reached the Worker means no static asset matched —
    // serve the SPA shell for client-side routing.
    if (!path.startsWith("/api/")) {
      return env.ASSETS.fetch(new URL("/index.html", url.origin));
    }

    const stub = snapshotStub(path);
    if (stub) return stub;

    if (path === "/api/v1/sessions") return handleSessions(env, url);
    if (path === "/api/v1/sessions/sidebar-index") {
      return handleSidebarIndex(env, url);
    }
    if (path === "/api/v1/search") return handleSearch(env, url);
    if (path === "/api/v1/projects") return handleProjects(env);
    if (path === "/api/v1/agents") return handleAgents(env);
    if (path === "/api/v1/machines") return handleMachines(env);
    if (path === "/api/v1/stats") return handleStats(env);

    const analytics = path.match(/^\/api\/v1\/analytics\/([a-z-]+)$/);
    if (analytics) return handleAnalytics(env, analytics[1]);

    const sess = path.match(/^\/api\/v1\/sessions\/([^/]+)$/);
    if (sess) return handleSession(env, decodeURIComponent(sess[1]));

    const msgs = path.match(/^\/api\/v1\/sessions\/([^/]+)\/messages$/);
    if (msgs) return handleMessages(env, decodeURIComponent(msgs[1]), url);

    const ssearch = path.match(/^\/api\/v1\/sessions\/([^/]+)\/search$/);
    if (ssearch) {
      return handleSessionSearch(env, decodeURIComponent(ssearch[1]), url);
    }

    const children = path.match(/^\/api\/v1\/sessions\/([^/]+)\/children$/);
    if (children) return handleChildren(env, decodeURIComponent(children[1]));

    // Per-session endpoints with no snapshot data: empty-but-valid shapes.
    const spins = path.match(/^\/api\/v1\/sessions\/([^/]+)\/pins$/);
    if (spins) return json({ pins: [] });

    const timing = path.match(/^\/api\/v1\/sessions\/([^/]+)\/timing$/);
    if (timing) {
      return json({
        session_id: decodeURIComponent(timing[1]),
        total_duration_ms: 0,
        tool_duration_ms: 0,
        turn_count: 0,
        tool_call_count: 0,
        subagent_count: 0,
        slowest_call: null,
      });
    }

    const activity = path.match(/^\/api\/v1\/sessions\/([^/]+)\/activity$/);
    if (activity) {
      return json({ buckets: [], interval_seconds: 60, total_messages: 0 });
    }

    return errorJSON(404, "not found");
  },
};
