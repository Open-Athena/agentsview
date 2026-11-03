//go:build pgtest

package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawderive"
)

type hostedRollupProject struct {
	Project       string
	Input, Output int
}

type hostedRollupTopSession struct {
	SessionID, Project string
}

func hostedRollupProjects(t *testing.T, h *HostedStore, project string) []hostedRollupProject {
	t.Helper()
	result, err := h.GetDailyUsage(t.Context(), db.UsageFilter{
		From: "2026-01-01", To: "2026-01-01", Timezone: "UTC",
		Breakdowns: true, Project: project,
	})
	require.NoError(t, err)
	var out []hostedRollupProject
	for _, day := range result.Daily {
		for _, pb := range day.ProjectBreakdowns {
			out = append(out, hostedRollupProject{pb.Project, pb.InputTokens, pb.OutputTokens})
		}
	}
	return out
}

func hostedRollupTopSessions(t *testing.T, h *HostedStore) []hostedRollupTopSession {
	t.Helper()
	top, err := h.GetTopSessionsByCost(t.Context(), db.UsageFilter{
		From: "2026-01-01", To: "2026-01-01", Timezone: "UTC",
		TopSessionsSort: "tokens",
	}, 10)
	require.NoError(t, err)
	var out []hostedRollupTopSession
	for _, s := range top {
		out = append(out, hostedRollupTopSession{s.SessionID, s.Project})
	}
	return out
}

// hostedRollupOutcome is the shared projection transcript with a subagent
// child whose own project differs from its parent's.
func hostedRollupOutcome(parentText string) rawderive.ParsedManifest {
	p := projectionOutcome(parentText)
	child := projectionOutcome("rollup child").Outcome.Results[0]
	child.Result.Session.ID = "codex:child"
	child.Result.Session.SourceSessionID = "child"
	child.Result.Session.Project = "path_sibling"
	child.Result.Session.ParentSessionID = "codex:portable"
	child.Result.Session.RelationshipType = "subagent"
	child.Result.Session.EndedAt = child.Result.Session.StartedAt.Add(7 * time.Second)
	child.Result.UsageEvents = []parser.ParsedUsageEvent{{
		Source: "turn", Model: "synthetic-model", InputTokens: 100, OutputTokens: 50,
	}}
	p.Outcome.Results[0].Result.Messages[1].ToolCalls[0].SubagentSessionID = "codex:child"
	p.Outcome.Results[0].Result.Messages[1].HasToolUse = true
	p.Outcome.Results = append(p.Outcome.Results, child)
	return p
}

// A hosted raw projection names its parent by alias, not physical ID, so the
// rollup resolves the parent through the tenant's projection links: the
// subagent's usage bills to the parent's project and project filters follow.
func TestHostedUsageRollsSubagentIntoParentProject(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "capture-a", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, hostedRollupOutcome("parent")))
	h, err := NewHostedStore(f.dsn, f.schema, f.tenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })

	assert.Equal(t, []hostedRollupProject{{"synthetic", 111, 57}}, hostedRollupProjects(t, h, ""))
	assert.Equal(t, []hostedRollupProject{{"synthetic", 111, 57}}, hostedRollupProjects(t, h, "synthetic"))
	assert.Empty(t, hostedRollupProjects(t, h, "path_sibling"))
	assert.Equal(t, []hostedRollupTopSession{
		{"codex:child", "synthetic"},
		{"codex:portable", "synthetic"},
	}, hostedRollupTopSessions(t, h))
}

// When the parent alias resolves to two divergent sessions, no single parent
// owns the subagent, so its usage keeps the subagent's own project.
func TestHostedUsageRollupKeepsOwnProjectForAmbiguousParent(t *testing.T) {
	f := newProjectionFixture(t)
	a, _ := f.accept(t, "device-a", "capture-a", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, hostedRollupOutcome("parent one")))
	b, _ := f.accept(t, "device-b", "capture-b", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, hostedRollupOutcome("parent two")))
	h, err := NewHostedStore(f.dsn, f.schema, f.tenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })

	assert.Equal(t, []hostedRollupProject{
		{"path_sibling", 100, 50},
		{"synthetic", 22, 14},
	}, hostedRollupProjects(t, h, ""))
	assert.Equal(t, []hostedRollupProject{{"path_sibling", 100, 50}}, hostedRollupProjects(t, h, "path_sibling"))
}
