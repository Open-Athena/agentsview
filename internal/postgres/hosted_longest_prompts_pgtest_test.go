//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

// Hosted stores read through the tenant's schema (RLS-forced tenant_id), so
// the longest-prompts panel must return the tenant's own prompts under their
// public session IDs rather than nothing.
func TestHostedLongestUserPrompts(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "capture-a", "")
	prompt := "a fairly long hosted prompt about the needle in the haystack"
	outcome := hostedMachineOutcome(prompt)
	outcome.Outcome.Results[0].Result.Messages[0].ContentLength = len(prompt)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, outcome))
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)

	got, err := h.GetLongestUserPrompts(t.Context(), 10, "")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.NotEmpty(t, got[0].Timestamp)
	got[0].Timestamp = ""
	assert.Equal(t, []db.LongestUserPrompt{{
		SessionID:     "codex:portable",
		Project:       "synthetic",
		Ordinal:       0,
		ContentLength: len(prompt),
		Preview:       prompt,
	}}, got)

	filtered, err := h.GetLongestUserPrompts(t.Context(), 10, "other-project")
	require.NoError(t, err)
	assert.Empty(t, filtered)
}
