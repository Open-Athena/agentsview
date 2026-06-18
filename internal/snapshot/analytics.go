package snapshot

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/server"
)

// analyticsTZ is the timezone used for day/hour bucketing in precomputed
// analytics. A published snapshot freezes one view; UTC keeps it stable.
const analyticsTZ = "UTC"

// analyticsRoute is one precomputed dashboard endpoint: the route key the
// Worker serves it under, and the JSON body (the Go handler's exact shape).
type analyticsRoute struct {
	Route string `json:"route"`
	Body  string `json:"body"`
}

// precomputeAnalytics builds a temporary database containing only the
// snapshot's rows and runs the real analytics queries against it, so the
// results are correctly scoped to the snapshot (no project-filter edge
// cases) and reuse the exact Go SQL. Returns one row per dashboard route.
//
// Only the homepage analytics/* panels are precomputed here; they need just
// sessions/messages/tool_calls. Usage panels (which also need usage_events
// and model_pricing) are a separate follow-up.
func precomputeAnalytics(
	ctx context.Context,
	srcPath string,
	analyticsIDs, dataIDs []string,
) ([]analyticsRoute, error) {
	// Aggregate routes run over the analytics scope (full + stats tier).
	aDB, aPath, aClean, err := openScopedTemp(ctx, srcPath, analyticsIDs)
	if err != nil {
		return nil, err
	}
	defer aClean()

	// Bound the filter to the snapshot's data span. The calendar heatmap
	// needs explicit From/To to build its grid (an empty range yields no
	// entries); other panels are unaffected since the span covers all data.
	from, to, err := snapshotDateRange(ctx, aPath)
	if err != nil {
		return nil, err
	}

	// Match the dashboard's defaults: exclude one-shot and automated
	// sessions (the page only includes them when toggled on).
	f := db.AnalyticsFilter{
		Timezone:         analyticsTZ,
		From:             from,
		To:               to,
		ExcludeOneShot:   true,
		ExcludeAutomated: true,
	}
	routes := make([]analyticsRoute, 0, 16)
	add := func(route string, v any, err error) error {
		if err != nil {
			return fmt.Errorf("snapshot: analytics %s: %w", route, err)
		}
		body, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("snapshot: marshal %s: %w", route, err)
		}
		routes = append(routes, analyticsRoute{Route: route, Body: string(body)})
		return nil
	}

	// --- Aggregate routes: no session-level identifying content, so they
	// safely span full + stats-tier projects. ---
	summary, err := aDB.GetAnalyticsSummary(ctx, f)
	if err := add("summary", summary, err); err != nil {
		return nil, err
	}
	activity, err := aDB.GetAnalyticsActivity(ctx, f, "day")
	if err := add("activity", activity, err); err != nil {
		return nil, err
	}
	heatmap, err := aDB.GetAnalyticsHeatmap(ctx, f, "messages")
	if err := add("heatmap", heatmap, err); err != nil {
		return nil, err
	}
	projects, err := aDB.GetAnalyticsProjects(ctx, f)
	if err := add("projects", projects, err); err != nil {
		return nil, err
	}
	how, err := aDB.GetAnalyticsHourOfWeek(ctx, f)
	if err := add("hour-of-week", how, err); err != nil {
		return nil, err
	}
	shape, err := aDB.GetAnalyticsSessionShape(ctx, f)
	if err := add("sessions", shape, err); err != nil {
		return nil, err
	}
	tools, err := aDB.GetAnalyticsTools(ctx, f)
	if err := add("tools", tools, err); err != nil {
		return nil, err
	}
	velocity, err := aDB.GetAnalyticsVelocity(ctx, f)
	if err := add("velocity", velocity, err); err != nil {
		return nil, err
	}
	signals, err := aDB.GetAnalyticsSignals(ctx, f)
	if err := add("signals", signals, err); err != nil {
		return nil, err
	}
	// Usage defaults differ: include one-shot, exclude automated, per-day
	// breakdowns. usage/summary is aggregate (totals + per-project/model/agent
	// breakdowns), no session names.
	uf := db.UsageFilter{
		Timezone:         analyticsTZ,
		From:             from,
		To:               to,
		ExcludeAutomated: true,
		Breakdowns:       true,
	}
	usageSummary, err := server.BuildUsageSummary(ctx, aDB, uf)
	if err := add("usage/summary", usageSummary, err); err != nil {
		return nil, err
	}

	// --- Session-detail routes expose session names/first-messages/prompts.
	// They must cover only the data scope (full tier); for a stats-only
	// project the underlying sessions are absent here, so nothing leaks. ---
	dDB := aDB
	if len(dataIDs) != len(analyticsIDs) { // dataIDs is always a subset
		var dClean func()
		dDB, _, dClean, err = openScopedTemp(ctx, srcPath, dataIDs)
		if err != nil {
			return nil, err
		}
		defer dClean()
	}

	top, err := dDB.GetAnalyticsTopSessions(ctx, f, "messages")
	if err := add("top-sessions", top, err); err != nil {
		return nil, err
	}
	ufTop := uf
	ufTop.Breakdowns = false
	topCost, err := dDB.GetTopSessionsByCost(ctx, ufTop, 20)
	if err := add("usage/top-sessions", topCost, err); err != nil {
		return nil, err
	}
	longest, err := dDB.GetLongestUserPrompts(ctx, 50, "")
	if err := add("messages/longest", longest, err); err != nil {
		return nil, err
	}
	return routes, nil
}

// openScopedTemp builds a temporary DB containing only the given sessions'
// rows (via copySnapshotSubset) and opens it. The returned cleanup closes the
// DB and removes the temp directory.
func openScopedTemp(
	ctx context.Context, srcPath string, ids []string,
) (tdb *db.DB, tmpPath string, cleanup func(), err error) {
	tmpDir, err := os.MkdirTemp("", "agentsview-snap-analytics")
	if err != nil {
		return nil, "", nil, fmt.Errorf("snapshot: temp dir: %w", err)
	}
	cleanup = func() {
		if tdb != nil {
			tdb.Close()
		}
		os.RemoveAll(tmpDir)
	}
	tmpPath = filepath.Join(tmpDir, "analytics.db")
	if err = copySnapshotSubset(ctx, tmpPath, srcPath, ids); err != nil {
		cleanup()
		return nil, "", nil, err
	}
	if tdb, err = db.Open(tmpPath); err != nil {
		cleanup()
		return nil, "", nil, fmt.Errorf("snapshot: open analytics db: %w", err)
	}
	return tdb, tmpPath, cleanup, nil
}

// snapshotDateRange returns the snapshot's data span as inclusive
// YYYY-MM-DD bounds (empty strings if there are no sessions).
func snapshotDateRange(ctx context.Context, tmpPath string) (string, string, error) {
	raw, err := sql.Open(
		"sqlite3",
		fmt.Sprintf("file:%s?mode=ro", tmpPath),
	)
	if err != nil {
		return "", "", fmt.Errorf("snapshot: open for date range: %w", err)
	}
	defer raw.Close()

	var from, to sql.NullString
	err = raw.QueryRowContext(ctx, `
		SELECT min(substr(started_at, 1, 10)),
		       max(substr(COALESCE(ended_at, started_at), 1, 10))
		  FROM sessions WHERE started_at IS NOT NULL`,
	).Scan(&from, &to)
	if err != nil {
		return "", "", fmt.Errorf("snapshot: date range: %w", err)
	}
	return from.String, to.String, nil
}

// copySnapshotSubset creates a fresh DB at tmpPath (full schema via db.Open)
// and copies the selected sessions and their messages/tool_calls from the
// source via ATTACH, so analytics run against snapshot-scoped data.
func copySnapshotSubset(
	ctx context.Context,
	tmpPath, srcPath string,
	ids []string,
) error {
	// db.Open creates the schema (tables, FTS, triggers); close it so the
	// raw connection below owns the file for the bulk ATTACH copy.
	seed, err := db.Open(tmpPath)
	if err != nil {
		return fmt.Errorf("snapshot: seed analytics schema: %w", err)
	}
	seed.Close()

	raw, err := sql.Open(
		"sqlite3",
		fmt.Sprintf("file:%s?_busy_timeout=5000", tmpPath),
	)
	if err != nil {
		return fmt.Errorf("snapshot: open analytics raw: %w", err)
	}
	defer raw.Close()

	// Attach the source read-only so the export never write-locks the
	// user's live database.
	srcURI := fmt.Sprintf("file:%s?mode=ro", srcPath)
	if _, err := raw.ExecContext(ctx, "ATTACH DATABASE ? AS src", srcURI); err != nil {
		return fmt.Errorf("snapshot: attach source: %w", err)
	}

	if _, err := raw.ExecContext(ctx, "CREATE TEMP TABLE _ids(id TEXT PRIMARY KEY)"); err != nil {
		return fmt.Errorf("snapshot: create id table: %w", err)
	}
	insID, err := raw.PrepareContext(ctx, "INSERT OR IGNORE INTO _ids(id) VALUES (?)")
	if err != nil {
		return err
	}
	defer insID.Close()
	for _, id := range ids {
		if _, err := insID.ExecContext(ctx, id); err != nil {
			return fmt.Errorf("snapshot: seed id %s: %w", id, err)
		}
	}

	// Copy by explicit, name-matched column lists (not SELECT *): the
	// source's column order can differ from db.Open's current schema (e.g.
	// migration-appended columns), which would misalign a positional copy.
	copies := []struct{ table, key string }{
		{"sessions", "id"},
		{"messages", "session_id"},
		{"tool_calls", "session_id"},
		{"usage_events", "session_id"},
	}
	for _, c := range copies {
		cols, err := tableColumns(ctx, raw, c.table)
		if err != nil {
			return err
		}
		q := fmt.Sprintf(
			"INSERT INTO %s (%s) SELECT %s FROM src.%s WHERE %s IN (SELECT id FROM _ids)",
			c.table, cols, cols, c.table, c.key,
		)
		if _, err := raw.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("snapshot: copy %s: %w", c.table, err)
		}
	}

	// model_pricing is a small global table (no session scope); copy it in
	// full so usage cost computations use the source's synced rates,
	// replacing any defaults db.Open may have seeded.
	pricingCols, err := tableColumns(ctx, raw, "model_pricing")
	if err != nil {
		return err
	}
	if _, err := raw.ExecContext(ctx, "DELETE FROM model_pricing"); err != nil {
		return fmt.Errorf("snapshot: clear model_pricing: %w", err)
	}
	if _, err := raw.ExecContext(ctx, fmt.Sprintf(
		"INSERT INTO model_pricing (%s) SELECT %s FROM src.model_pricing",
		pricingCols, pricingCols,
	)); err != nil {
		return fmt.Errorf("snapshot: copy model_pricing: %w", err)
	}
	return nil
}

// tableColumns returns table's column names as a quoted, comma-separated
// list (e.g. `"a", "b"`) for use in INSERT/SELECT, read from the local DB
// so the copy is matched by name rather than position.
func tableColumns(ctx context.Context, raw *sql.DB, table string) (string, error) {
	rows, err := raw.QueryContext(
		ctx, fmt.Sprintf("PRAGMA table_info(%s)", table),
	)
	if err != nil {
		return "", fmt.Errorf("snapshot: columns for %s: %w", table, err)
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return "", err
		}
		cols = append(cols, `"`+name+`"`)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return strings.Join(cols, ", "), nil
}
