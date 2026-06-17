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
  // matching the Go SearchSession. NOTE: Go also searches
  // tool_calls.result_content; until the snapshot includes the tool_calls
  // table, hits that occur only inside tool results are not surfaced here.
  const rows = await env.DB.prepare(
    `SELECT DISTINCT m.ordinal AS ordinal
       FROM messages m
      WHERE m.session_id = ? AND m.is_system = 0
        AND m.content LIKE ? ESCAPE '\\'
      ORDER BY m.ordinal ASC`,
  )
    .bind(id, `%${escapeLike(q)}%`)
    .all<{ ordinal: number }>();

  return json({ ordinals: rows.results.map((r) => r.ordinal) });
}

async function handleSearch(env: Env, url: URL): Promise<Response> {
  const q = url.searchParams.get("q") ?? "";
  const limit = clampInt(url.searchParams.get("limit"), 20, 1, 200);
  const offset = clampInt(url.searchParams.get("offset"), 0, 0, Number.MAX_SAFE_INTEGER);
  if (!q.trim()) return json({ query: q, results: [], count: 0 });

  // FTS5 snippet()/bm25() are only usable in a flat query against the FTS
  // table (not inside a wrapping subquery/window). So rank in SQL, then
  // dedup to the best-ranked hit per session in JS — mirroring the Go
  // global search's one-result-per-session behavior.
  const cap = Math.min(2000, (offset + limit) * 25 + 50);
  const rows = await env.DB.prepare(
    `SELECT m.session_id AS session_id, s.project AS project, s.agent AS agent,
            COALESCE(NULLIF(s.display_name, ''), s.first_message, '') AS name,
            m.ordinal AS ordinal, s.ended_at AS session_ended_at,
            snippet(messages_fts, 0, '<mark>', '</mark>', '...', 12) AS snippet,
            bm25(messages_fts) AS rank
       FROM messages_fts
       JOIN messages m ON m.id = messages_fts.rowid
       JOIN sessions s ON s.id = m.session_id
      WHERE messages_fts MATCH ? AND m.is_system = 0
        AND s.deleted_at IS NULL
      ORDER BY bm25(messages_fts), m.ordinal LIMIT ?`,
  )
    .bind(ftsQuery(q), cap)
    .all<Row>();

  const seen = new Set<unknown>();
  const deduped: Row[] = [];
  for (const r of rows.results) {
    if (seen.has(r.session_id)) continue;
    seen.add(r.session_id);
    deduped.push(r);
  }
  const page = deduped.slice(offset, offset + limit);

  const body: Record<string, unknown> = {
    query: q,
    results: page,
    count: page.length,
  };
  if (deduped.length > offset + limit) body.next = offset + limit;
  return json(body);
}

async function handleAdminLoad(
  req: Request,
  env: Env,
  url: URL,
): Promise<Response> {
  if (env.ALLOW_LOAD !== "1") return errorJSON(403, "load disabled");
  const table = url.searchParams.get("table");
  if (table !== "sessions" && table !== "messages") {
    return errorJSON(400, "table must be sessions or messages");
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

    if (path === "/api/v1/sessions") return handleSessions(env, url);
    if (path === "/api/v1/search") return handleSearch(env, url);

    const sess = path.match(/^\/api\/v1\/sessions\/([^/]+)$/);
    if (sess) return handleSession(env, decodeURIComponent(sess[1]));

    const msgs = path.match(/^\/api\/v1\/sessions\/([^/]+)\/messages$/);
    if (msgs) return handleMessages(env, decodeURIComponent(msgs[1]), url);

    const ssearch = path.match(/^\/api\/v1\/sessions\/([^/]+)\/search$/);
    if (ssearch) {
      return handleSessionSearch(env, decodeURIComponent(ssearch[1]), url);
    }

    return errorJSON(404, "not found");
  },
};
