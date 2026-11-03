//go:build chtest

package clickhouse

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// normalizePromptTimes renders timestamps as RFC3339Nano UTC: SQLite keeps the
// archive's millisecond text, ClickHouse returns a DateTime64.
func normalizePromptTimes(t *testing.T, ps []db.LongestUserPrompt) []db.LongestUserPrompt {
	t.Helper()
	for i := range ps {
		if ps[i].Timestamp == "" {
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, ps[i].Timestamp)
		require.NoError(t, err)
		ps[i].Timestamp = ts.UTC().Format(time.RFC3339Nano)
	}
	return ps
}

// The ClickHouse replica serves the same longest-prompts rows as the SQLite
// archive it was pushed from.
func TestClickHouseLongestUserPromptsParity(t *testing.T) {
	store, _, local := newPushedStore(t)
	for _, project := range []string{"", "nonexistent-project"} {
		want, err := local.GetLongestUserPrompts(t.Context(), 20, project)
		require.NoError(t, err)
		got, err := store.GetLongestUserPrompts(t.Context(), 20, project)
		require.NoError(t, err)
		assert.Equal(t, normalizePromptTimes(t, want), normalizePromptTimes(t, got), "project=%q", project)
	}
	all, err := local.GetLongestUserPrompts(t.Context(), 20, "")
	require.NoError(t, err)
	require.NotEmpty(t, all, "fixture must contain user prompts for the parity check to mean anything")
	filtered, err := store.GetLongestUserPrompts(t.Context(), 20, all[0].Project)
	require.NoError(t, err)
	want, err := local.GetLongestUserPrompts(t.Context(), 20, all[0].Project)
	require.NoError(t, err)
	assert.Equal(t, normalizePromptTimes(t, want), normalizePromptTimes(t, filtered))
}
