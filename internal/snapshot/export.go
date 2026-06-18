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

// Options configures an export.
type Options struct {
	// Projects to include. A session is selected if its project is in
	// this set, or if it is a subagent whose parent session's project
	// is in this set (honoring the usage-rollup semantics).
	Projects []string
	// GeneratedAt is an RFC3339 timestamp stamped into the manifest.
	// Passed in by the caller so the export stays deterministic/testable.
	GeneratedAt string
}

// Manifest summarizes an export; also written to manifest.json.
type Manifest struct {
	Projects        []string `json:"projects"`
	GeneratedAt     string   `json:"generatedAt"`
	SessionCount    int      `json:"sessionCount"`
	MessageCount    int      `json:"messageCount"`
	ToolCallCount   int      `json:"toolCallCount"`
	AnalyticsRoutes int      `json:"analyticsRoutes"`
	Tables          []string `json:"tables"`
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

	ids, err := selectSessionIDs(ctx, src, opts.Projects)
	if err != nil {
		return nil, err
	}

	if err := writeSchema(ctx, src, filepath.Join(outDir, "schema.sql")); err != nil {
		return nil, err
	}

	sessionCount, err := dumpToFile(
		ctx, src, filepath.Join(outDir, "sessions.ndjson"),
		"SELECT * FROM sessions WHERE id IN (%s) ORDER BY id",
		ids,
	)
	if err != nil {
		return nil, fmt.Errorf("snapshot: dump sessions: %w", err)
	}

	messageCount, err := dumpToFile(
		ctx, src, filepath.Join(outDir, "messages.ndjson"),
		"SELECT * FROM messages WHERE session_id IN (%s) ORDER BY session_id, ordinal",
		ids,
	)
	if err != nil {
		return nil, fmt.Errorf("snapshot: dump messages: %w", err)
	}

	toolCallCount, err := dumpToFile(
		ctx, src, filepath.Join(outDir, "tool_calls.ndjson"),
		"SELECT * FROM tool_calls WHERE session_id IN (%s) ORDER BY session_id, id",
		ids,
	)
	if err != nil {
		return nil, fmt.Errorf("snapshot: dump tool_calls: %w", err)
	}

	analyticsRoutes, err := precomputeAnalytics(ctx, srcPath, ids)
	if err != nil {
		return nil, err
	}
	if err := writeAnalyticsCache(
		filepath.Join(outDir, "analytics_cache.ndjson"),
		analyticsRoutes,
	); err != nil {
		return nil, err
	}

	tables := append(append([]string(nil), baseTables...), "messages_fts", "analytics_cache")
	m := &Manifest{
		Projects:        append([]string(nil), opts.Projects...),
		GeneratedAt:     opts.GeneratedAt,
		SessionCount:    sessionCount,
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
	projects []string,
) ([]string, error) {
	query := "SELECT id FROM sessions WHERE deleted_at IS NULL"
	var args []any
	if len(projects) > 0 {
		ph := placeholders(len(projects))
		for _, p := range projects {
			args = append(args, p)
		}
		for _, p := range projects {
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
// IDs in chunks, writing each result row as one NDJSON line. Returns the
// total row count.
func dumpToFile(
	ctx context.Context,
	src *sql.DB,
	path, queryTmpl string,
	ids []string,
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
		n, err := dumpRows(ctx, src, query, args, f)
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

// dumpRows writes each result row of query as a single-line JSON object.
// BLOB/TEXT values arrive as []byte and are emitted as strings.
func dumpRows(
	ctx context.Context,
	src *sql.DB,
	query string,
	args []any,
	w io.Writer,
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
