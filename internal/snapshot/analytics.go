package snapshot

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/agentsview/internal/db"
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
	ids []string,
) ([]analyticsRoute, error) {
	tmpDir, err := os.MkdirTemp("", "agentsview-snap-analytics")
	if err != nil {
		return nil, fmt.Errorf("snapshot: temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	tmpPath := filepath.Join(tmpDir, "analytics.db")

	if err := copySnapshotSubset(ctx, tmpPath, srcPath, ids); err != nil {
		return nil, err
	}

	tdb, err := db.Open(tmpPath)
	if err != nil {
		return nil, fmt.Errorf("snapshot: open analytics db: %w", err)
	}
	defer tdb.Close()

	// Bound the filter to the snapshot's data span. The calendar heatmap
	// needs explicit From/To to build its grid (an empty range yields no
	// entries); other panels are unaffected since the span covers all data.
	from, to, err := snapshotDateRange(ctx, tmpPath)
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

	summary, err := tdb.GetAnalyticsSummary(ctx, f)
	if err := add("summary", summary, err); err != nil {
		return nil, err
	}
	activity, err := tdb.GetAnalyticsActivity(ctx, f, "day")
	if err := add("activity", activity, err); err != nil {
		return nil, err
	}
	heatmap, err := tdb.GetAnalyticsHeatmap(ctx, f, "messages")
	if err := add("heatmap", heatmap, err); err != nil {
		return nil, err
	}
	projects, err := tdb.GetAnalyticsProjects(ctx, f)
	if err := add("projects", projects, err); err != nil {
		return nil, err
	}
	how, err := tdb.GetAnalyticsHourOfWeek(ctx, f)
	if err := add("hour-of-week", how, err); err != nil {
		return nil, err
	}
	shape, err := tdb.GetAnalyticsSessionShape(ctx, f)
	if err := add("sessions", shape, err); err != nil {
		return nil, err
	}
	tools, err := tdb.GetAnalyticsTools(ctx, f)
	if err := add("tools", tools, err); err != nil {
		return nil, err
	}
	velocity, err := tdb.GetAnalyticsVelocity(ctx, f)
	if err := add("velocity", velocity, err); err != nil {
		return nil, err
	}
	top, err := tdb.GetAnalyticsTopSessions(ctx, f, "messages")
	if err := add("top-sessions", top, err); err != nil {
		return nil, err
	}
	signals, err := tdb.GetAnalyticsSignals(ctx, f)
	if err := add("signals", signals, err); err != nil {
		return nil, err
	}
	return routes, nil
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

	copies := []struct{ table, key string }{
		{"sessions", "id"},
		{"messages", "session_id"},
		{"tool_calls", "session_id"},
	}
	for _, c := range copies {
		q := fmt.Sprintf(
			"INSERT INTO %s SELECT * FROM src.%s WHERE %s IN (SELECT id FROM _ids)",
			c.table, c.table, c.key,
		)
		if _, err := raw.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("snapshot: copy %s: %w", c.table, err)
		}
	}
	return nil
}
