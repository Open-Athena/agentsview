package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/money"
)

// seedRollupParentChild inserts a parent session in project "display" and a
// subagent child carrying a path-derived sibling project, each with one
// assistant message of 10 input / 100 output tokens.
func seedRollupParentChild(t *testing.T, d *DB) {
	t.Helper()
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:         "claude-opus-4-6",
		InputPerMTok:         money.MustParseDollars("5"),
		OutputPerMTok:        money.MustParseDollars("25"),
		CacheCreationPerMTok: money.MustParseDollars("6.25"),
		CacheReadPerMTok:     money.MustParseDollars("0.50"),
	}}))
	exec := func(q string, args ...any) {
		t.Helper()
		_, err := d.getWriter().Exec(t.Context(), q, args...)
		require.NoError(t, err, "exec %q", q)
	}
	exec(`INSERT INTO sessions (id, project, machine, agent, started_at, ended_at)
	      VALUES ('s-parent', 'display', 'local', 'claude', ?, ?)`,
		"2026-04-10T10:00:00Z", "2026-04-10T10:05:00Z")
	exec(`INSERT INTO sessions (id, project, machine, agent, started_at, ended_at,
	      parent_session_id, relationship_type)
	      VALUES ('s-sub', 'path_sibling', 'local', 'claude', ?, ?, 's-parent', 'subagent')`,
		"2026-04-10T10:01:00Z", "2026-04-10T10:04:00Z")
	usage := `{"input_tokens":10,"output_tokens":100,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}`
	for _, row := range []struct{ sid, mid string }{{"s-parent", "msg_p"}, {"s-sub", "msg_s"}} {
		exec(`INSERT INTO messages
			(session_id, ordinal, role, content, timestamp, model, token_usage,
			 claude_message_id, claude_request_id, has_output_tokens, has_context_tokens)
			VALUES (?, 0, 'assistant', '', '2026-04-10T10:02:00Z', 'claude-opus-4-6', ?, ?, ?, 1, 1)`,
			row.sid, usage, row.mid, "req_"+row.mid)
	}
}

type projectTokens struct {
	Project       string
	Input, Output int
}

func dailyProjectTokens(t *testing.T, d *DB, project string) []projectTokens {
	t.Helper()
	result, err := d.GetDailyUsage(t.Context(), UsageFilter{
		From: "2026-04-10", To: "2026-04-10", Timezone: "UTC",
		Breakdowns: true, Project: project,
	})
	require.NoError(t, err)
	var out []projectTokens
	for _, day := range result.Daily {
		for _, pb := range day.ProjectBreakdowns {
			out = append(out, projectTokens{pb.Project, pb.InputTokens, pb.OutputTokens})
		}
	}
	return out
}

// A parent's project change after the usage cache is warm re-attributes its
// subagents' usage on the next read; nothing project-shaped is cached.
func TestUsageRollupFollowsParentProjectChange(t *testing.T) {
	d := testDB(t)
	seedRollupParentChild(t, d)
	assert.Equal(t, []projectTokens{{"display", 20, 200}}, dailyProjectTokens(t, d, ""))

	_, err := d.getWriter().Exec(t.Context(),
		`UPDATE sessions SET project = 'renamed' WHERE id = 's-parent'`)
	require.NoError(t, err)
	assert.Equal(t, []projectTokens{{"renamed", 20, 200}}, dailyProjectTokens(t, d, ""))
}

// Filtering usage by a project selects the same sessions that the breakdown
// attributes to it: subagents are filtered by their parent's project.
func TestUsageProjectFilterMatchesRollupAttribution(t *testing.T) {
	d := testDB(t)
	seedRollupParentChild(t, d)
	assert.Equal(t, []projectTokens{{"display", 20, 200}}, dailyProjectTokens(t, d, "display"))
	assert.Empty(t, dailyProjectTokens(t, d, "path_sibling"))
}

// The SQLite templates embed rollupProjectExpr as a constant; the other
// backends render the same attribution through UsageRollupProjectExpr.
func TestUsageRollupProjectExprMatchesSQLiteConstant(t *testing.T) {
	assert.Equal(t, rollupProjectExpr, UsageRollupProjectExpr(SQLiteQueryDialect(), "s"))
}
