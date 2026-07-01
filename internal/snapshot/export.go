// Package snapshot exports a project-scoped, read-only slice of the
// agentsview SQLite database into artifacts that load into a Cloudflare
// D1 database (see specs/snapshot-export-d1-cfw.md):
//
//	schema.sql        base table DDL + FTS5 virtual table/triggers
//	sessions.ndjson   one JSON object per selected session row
//	messages.ndjson   one JSON object per message of a selected session
//	manifest.json     projects, counts, and generation metadata
//
// Rows are read straight from the source DB (no intermediate SQLite) and
// emitted as NDJSON so the loader can insert them with bound parameters,
// keeping large message content out of SQL statement text.
package snapshot

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

// idChunkSize bounds the number of IDs per `IN (...)` query so a
// project with many sessions does not build an unbounded statement.
const idChunkSize = 400

// baseTables are dumped as data; their DDL plus the FTS objects make up
// schema.sql. Order matters for FK references: sessions before messages,
// and tool_calls last (it references both).
var baseTables = []string{"sessions", "messages", "tool_calls"}

// ftsObjects are emitted as DDL only (no data) — the messages_ai trigger
// rebuilds the index as the loader inserts message rows.
var ftsObjects = []string{
	"messages_fts",
	"messages_ai",
	"messages_ad",
	"messages_au",
}

// Options configures an export and its per-project exposure tiers.
//
// Tiers (see specs/publishable-views.md):
//   - full  (FullProjects): sessions/messages/tool_calls are exported AND the
//     project is counted in analytics/usage aggregates.
//   - stats (StatsProjects, or all non-full when AllStats): counted in
//     analytics/usage aggregates only — no raw sessions/messages reach the
//     published data, so redacted prompts cannot leak.
//   - excluded: any project named in neither tier (when any tier is set).
//
// With no tiers set at all, the export is a full, unfiltered dump (every
// project is full tier).
type Options struct {
	// FullProjects get both data and analytics. A session is selected if its
	// project is in the set, or it is a subagent whose parent's project is
	// (honoring the usage-rollup semantics).
	FullProjects []string
	// StatsProjects contribute to analytics aggregates only.
	StatsProjects []string
	// AllStats treats every non-full project as stats tier (the "public
	// stats" view: aggregates for everything, raw prompts for nothing).
	AllStats bool
	// PrivateSessions are session IDs to redact at session granularity: the
	// session stays in aggregate analytics (a ghost row with scrubbed title
	// and no messages), but its transcript is elided. Combined with any session
	// flagged private in the DB. See specs/per-session-redaction.md.
	PrivateSessions []string
	// Gated marks the view as token-required: the Worker serves only
	// view/meta without a valid grant token.
	Gated bool
	// Title is a human-readable heading for the published view.
	Title string
	// Blurbs maps a project to a Markdown summary surfaced in the UI.
	Blurbs map[string]string
	// Orgs groups projects for color-coding (see OrgConfig).
	Orgs []OrgConfig
	// GeneratedAt is an RFC3339 timestamp stamped into the manifest. Passed
	// in by the caller so the export stays deterministic/testable.
	GeneratedAt string
}

// viewMeta is the published view's self-description, served by the Worker at
// /api/v1/view/meta so the SPA knows its title, whether it's stats-only (no
// drill-down), and any per-project Markdown blurbs.
type viewMeta struct {
	Title       string            `json:"title,omitempty"`
	StatsOnly   bool              `json:"statsOnly"`
	Gated       bool              `json:"gated"`
	Blurbs      map[string]string `json:"blurbs,omitempty"`
	Orgs        []OrgConfig       `json:"orgs,omitempty"`
	GeneratedAt string            `json:"generatedAt"`
}

// Manifest summarizes an export; also written to manifest.json.
type Manifest struct {
	FullProjects    []string `json:"fullProjects,omitempty"`
	StatsProjects   []string `json:"statsProjects,omitempty"`
	AllStats        bool     `json:"allStats,omitempty"`
	GeneratedAt     string   `json:"generatedAt"`
	SessionCount    int      `json:"sessionCount"`
	PrivateCount    int      `json:"privateCount,omitempty"`
	MessageCount    int      `json:"messageCount"`
	ToolCallCount   int      `json:"toolCallCount"`
	AnalyticsRoutes int      `json:"analyticsRoutes"`
	Tables          []string `json:"tables"`
}

// scope identifies a set of projects to select sessions for: all of them,
// or a specific (possibly empty) list.
type scope struct {
	all      bool
	projects []string
}

// resolveScopes derives the data scope (sessions/messages exported) and the
// analytics scope (sessions counted in aggregates) from the tier options.
func resolveScopes(opts Options) (data, analytics scope) {
	if !opts.AllStats && len(opts.FullProjects) == 0 && len(opts.StatsProjects) == 0 {
		return scope{all: true}, scope{all: true} // full unfiltered dump
	}
	if opts.AllStats {
		// Everything in analytics; data only for explicit full projects.
		return scope{projects: opts.FullProjects}, scope{all: true}
	}
	return scope{projects: opts.FullProjects},
		scope{projects: unionProjects(opts.FullProjects, opts.StatsProjects)}
}

// unionProjects returns the de-duplicated, sorted union of two project lists.
func unionProjects(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	var out []string
	for _, p := range append(append([]string(nil), a...), b...) {
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Export reads a project-scoped slice of srcPath and writes the snapshot
// artifacts into outDir (created if absent). It returns the manifest.
func Export(
	ctx context.Context,
	srcPath, outDir string,
	opts Options,
) (*Manifest, error) {
	// No projects means a full, unfiltered export (all sessions).

	dsn := fmt.Sprintf("file:%s?mode=ro&_busy_timeout=5000", srcPath)
	src, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("snapshot: open source db: %w", err)
	}
	defer src.Close()
	if err := src.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("snapshot: source db unreachable: %w", err)
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("snapshot: create out dir: %w", err)
	}

	dataScope, analyticsScope := resolveScopes(opts)

	// Data scope: only full-tier sessions get raw content exported.
	dataIDs, err := selectSessionIDs(ctx, src, dataScope)
	if err != nil {
		return nil, err
	}
	// Analytics scope: full + stats tier sessions feed the aggregates.
	analyticsIDs, err := selectSessionIDs(ctx, src, analyticsScope)
	if err != nil {
		return nil, err
	}

	// Private sessions (DB-flagged or explicitly listed) within the data scope
	// are elided: their row is still exported (a ghost with scrubbed title) so
	// they show in aggregates and click-through, but their messages/tool_calls
	// and detail-route entries are omitted. contentIDs = data scope − private.
	privateIDs, err := selectPrivateIDs(ctx, src, dataIDs, opts.PrivateSessions)
	if err != nil {
		return nil, err
	}
	privateSet := make(map[string]struct{}, len(privateIDs))
	for _, id := range privateIDs {
		privateSet[id] = struct{}{}
	}
	contentIDs := subtractIDs(dataIDs, privateSet)

	if err := writeSchema(ctx, src, filepath.Join(outDir, "schema.sql")); err != nil {
		return nil, err
	}

	// sessions.ndjson: every data-scope session (including private ghosts). For
	// a private row, scrub the human-readable fields and mark it redacted.
	scrubPrivate := func(obj map[string]any) {
		id, _ := obj["id"].(string)
		if _, ok := privateSet[id]; !ok {
			return
		}
		obj["first_message"] = "[redacted]"
		obj["display_name"] = "[redacted]"
		obj["private"] = int64(1)
	}
	sessionCount, err := dumpToFile(
		ctx, src, filepath.Join(outDir, "sessions.ndjson"),
		"SELECT * FROM sessions WHERE id IN (%s) ORDER BY id",
		dataIDs, scrubPrivate,
	)
	if err != nil {
		return nil, fmt.Errorf("snapshot: dump sessions: %w", err)
	}

	messageCount, err := dumpToFile(
		ctx, src, filepath.Join(outDir, "messages.ndjson"),
		"SELECT * FROM messages WHERE session_id IN (%s) ORDER BY session_id, ordinal",
		contentIDs, nil,
	)
	if err != nil {
		return nil, fmt.Errorf("snapshot: dump messages: %w", err)
	}

	toolCallCount, err := dumpToFile(
		ctx, src, filepath.Join(outDir, "tool_calls.ndjson"),
		"SELECT * FROM tool_calls WHERE session_id IN (%s) ORDER BY session_id, id",
		contentIDs, nil,
	)
	if err != nil {
		return nil, fmt.Errorf("snapshot: dump tool_calls: %w", err)
	}

	// Detail routes (top-sessions, longest-prompts) run over contentIDs so a
	// private session's scrubbed title / prompts never surface; aggregates
	// still span analyticsIDs (private sessions included).
	analyticsRoutes, err := precomputeAnalytics(ctx, srcPath, analyticsIDs, contentIDs)
	if err != nil {
		return nil, err
	}

	// view/meta: served by the Worker so the SPA knows the view's title,
	// whether it has any drillable sessions, and per-project blurbs.
	meta := viewMeta{
		Title:       opts.Title,
		StatsOnly:   sessionCount == 0,
		Gated:       opts.Gated,
		Blurbs:      opts.Blurbs,
		Orgs:        opts.Orgs,
		GeneratedAt: opts.GeneratedAt,
	}
	metaBody, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("snapshot: marshal view meta: %w", err)
	}
	analyticsRoutes = append(analyticsRoutes,
		analyticsRoute{Route: "view/meta", Body: string(metaBody)})

	if err := writeAnalyticsCache(
		filepath.Join(outDir, "analytics_cache.ndjson"),
		analyticsRoutes,
	); err != nil {
		return nil, err
	}

	tables := append(append([]string(nil), baseTables...), "messages_fts", "analytics_cache")
	m := &Manifest{
		FullProjects:    append([]string(nil), opts.FullProjects...),
		StatsProjects:   append([]string(nil), opts.StatsProjects...),
		AllStats:        opts.AllStats,
		GeneratedAt:     opts.GeneratedAt,
		SessionCount:    sessionCount,
		PrivateCount:    len(privateIDs),
		MessageCount:    messageCount,
		ToolCallCount:   toolCallCount,
		AnalyticsRoutes: len(analyticsRoutes),
		Tables:          tables,
	}
	if err := writeManifest(filepath.Join(outDir, "manifest.json"), m); err != nil {
		return nil, err
	}
	return m, nil
}

// selectSessionIDs resolves the set of session IDs to include: direct
// project members plus subagent children whose parent is a direct member.
// Returns a sorted slice for deterministic output.
func selectSessionIDs(
	ctx context.Context,
	src *sql.DB,
	sc scope,
) ([]string, error) {
	if !sc.all && len(sc.projects) == 0 {
		return nil, nil // empty scope selects no sessions
	}

	query := "SELECT id FROM sessions WHERE deleted_at IS NULL"
	var args []any
	if !sc.all {
		ph := placeholders(len(sc.projects))
		for _, p := range sc.projects {
			args = append(args, p)
		}
		for _, p := range sc.projects {
			args = append(args, p)
		}
		// Direct project members, plus subagent children whose parent is a
		// member (honoring the usage-rollup semantics).
		query = fmt.Sprintf(`
			SELECT id FROM sessions
			  WHERE deleted_at IS NULL AND project IN (%s)
			UNION
			SELECT s.id FROM sessions s
			  JOIN sessions p ON s.parent_session_id = p.id
			  WHERE s.deleted_at IS NULL
			    AND s.relationship_type = 'subagent'
			    AND p.project IN (%s)`, ph, ph)
	}

	rows, err := src.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("snapshot: select session ids: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("snapshot: scan session id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(ids)
	return ids, nil
}

// selectPrivateIDs returns the sorted set of data-scope session IDs to elide:
// those flagged private in the DB plus any explicitly listed, intersected with
// dataIDs (private only matters where content would otherwise be exported).
func selectPrivateIDs(
	ctx context.Context,
	src *sql.DB,
	dataIDs, explicit []string,
) ([]string, error) {
	dataSet := make(map[string]struct{}, len(dataIDs))
	for _, id := range dataIDs {
		dataSet[id] = struct{}{}
	}
	out := make(map[string]struct{})

	rows, err := src.QueryContext(ctx, "SELECT id FROM sessions WHERE private = 1")
	if err != nil {
		return nil, fmt.Errorf("snapshot: select private ids: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("snapshot: scan private id: %w", err)
		}
		if _, ok := dataSet[id]; ok {
			out[id] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range explicit {
		if _, ok := dataSet[id]; ok {
			out[id] = struct{}{}
		}
	}

	ids := make([]string, 0, len(out))
	for id := range out {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// subtractIDs returns the elements of ids not present in remove, order
// preserved.
func subtractIDs(ids []string, remove map[string]struct{}) []string {
	if len(remove) == 0 {
		return ids
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := remove[id]; !ok {
			out = append(out, id)
		}
	}
	return out
}

// writeSchema emits base-table DDL followed by the FTS virtual table and
// its triggers, read verbatim from the source's sqlite_master.
func writeSchema(ctx context.Context, src *sql.DB, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("snapshot: create schema file: %w", err)
	}
	defer f.Close()

	names := append(append([]string(nil), baseTables...), ftsObjects...)
	for _, name := range names {
		var ddl sql.NullString
		err := src.QueryRowContext(
			ctx,
			"SELECT sql FROM sqlite_master WHERE name = ?",
			name,
		).Scan(&ddl)
		if err == sql.ErrNoRows || !ddl.Valid {
			if isBaseTable(name) {
				return fmt.Errorf("snapshot: missing required table %q", name)
			}
			continue // FTS may be absent if the source lacks it
		}
		if err != nil {
			return fmt.Errorf("snapshot: read DDL for %q: %w", name, err)
		}
		if _, err := fmt.Fprintf(f, "%s;\n", strings.TrimSpace(ddl.String)); err != nil {
			return err
		}
	}

	// analytics_cache is snapshot-only (precomputed dashboard responses),
	// so its DDL is emitted directly rather than read from sqlite_master.
	if _, err := fmt.Fprintln(f,
		"CREATE TABLE IF NOT EXISTS analytics_cache (route TEXT PRIMARY KEY, body TEXT NOT NULL);",
	); err != nil {
		return err
	}
	// grants gates a token-required view. Empty for public views; the local
	// admin mints rows (hashed tokens) for gated ones. Never populated by the
	// exporter — access control is minted separately, post-deploy.
	if _, err := fmt.Fprintln(f,
		"CREATE TABLE IF NOT EXISTS grants ("+
			"id TEXT PRIMARY KEY, token_hash TEXT NOT NULL, name TEXT, "+
			"created_at TEXT NOT NULL, expires_at TEXT, revoked_at TEXT);",
	); err != nil {
		return err
	}
	return nil
}

// writeAnalyticsCache writes one NDJSON line per precomputed dashboard route.
func writeAnalyticsCache(path string, routes []analyticsRoute) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, r := range routes {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}

// dumpToFile runs queryTmpl (a single %s for the ID placeholders) over the
// IDs in chunks, writing each result row as one NDJSON line. hook, if non-nil,
// is applied to each row object before it is written (used to scrub private
// ghost rows). Returns the total row count.
func dumpToFile(
	ctx context.Context,
	src *sql.DB,
	path, queryTmpl string,
	ids []string,
	hook func(map[string]any),
) (int, error) {
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	total := 0
	for chunk := range chunks(ids, idChunkSize) {
		query := fmt.Sprintf(queryTmpl, placeholders(len(chunk)))
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		n, err := dumpRows(ctx, src, query, args, f, hook)
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

// dumpRows writes each result row of query as a single-line JSON object.
// BLOB/TEXT values arrive as []byte and are emitted as strings. hook, if
// non-nil, may mutate each row object before it is encoded.
func dumpRows(
	ctx context.Context,
	src *sql.DB,
	query string,
	args []any,
	w io.Writer,
	hook func(map[string]any),
) (int, error) {
	rows, err := src.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return 0, err
	}
	enc := json.NewEncoder(w)
	n := 0
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return 0, err
		}
		obj := make(map[string]any, len(cols))
		for i, c := range cols {
			if b, ok := vals[i].([]byte); ok {
				obj[c] = string(b)
			} else {
				obj[c] = vals[i]
			}
		}
		if hook != nil {
			hook(obj)
		}
		if err := enc.Encode(obj); err != nil {
			return 0, err
		}
		n++
	}
	return n, rows.Err()
}

func writeManifest(path string, m *Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}

func isBaseTable(name string) bool {
	for _, t := range baseTables {
		if t == name {
			return true
		}
	}
	return false
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// chunks yields successive slices of s of at most size n.
func chunks[T any](s []T, n int) func(func([]T) bool) {
	return func(yield func([]T) bool) {
		for i := 0; i < len(s); i += n {
			end := i + n
			if end > len(s) {
				end = len(s)
			}
			if !yield(s[i:end]) {
				return
			}
		}
	}
}
