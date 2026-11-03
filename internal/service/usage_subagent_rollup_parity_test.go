//go:build !(windows && arm64)

// The subagent rollup parity test proves that every backend bills a subagent
// session's usage to its parent session's project, and that project filters
// select the same rows the per-project breakdown shows. SQLite and the DuckDB
// mirror always run; PostgreSQL and ClickHouse join when the pgtest or chtest
// build tag registers them through usageParityReplicaStores.
package service_test

import (
	"fmt"
	"maps"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	duckdbstore "go.kenn.io/agentsview/internal/duckdb"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/storage"
)

// usageParityReplicaStores pushes local into every configured replica and
// returns their stores by backend name. Untagged builds have none; the
// pgtest/chtest file replaces it.
var usageParityReplicaStores = func(
	t *testing.T, local *db.DB, schema string, sessions int,
) map[string]db.Store {
	t.Helper()
	return nil
}

const (
	rollupParent = "claude:rollup-parent"
	rollupSub    = "claude:rollup-sub"
	rollupOther  = "claude:rollup-other"
)

// seedSubagentRollupParity writes a parent session in project "display", a
// subagent of it whose own project is the path-derived "path_sibling", and an
// unrelated root session in "path_sibling". The subagent also shares one
// Claude request with its parent: the subagent records it first (so snapshot
// attribution credits the subagent) while the parent's later copy carries the
// larger output snapshot (so the parent's row survives deduplication).
func seedSubagentRollupParity(t *testing.T) *db.DB {
	t.Helper()
	local, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "rollup.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, local.Close()) })
	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern: "claude-rollup-model", InputPerMTok: money.MustParseDollars("1"),
		OutputPerMTok: money.MustParseDollars("10"),
	}}))

	parentRef := rollupParent
	session := func(id, project string, parent *string, relationship string) db.Session {
		started := "2026-04-10T10:00:00Z"
		ended := "2026-04-10T10:30:00Z"
		return db.Session{
			ID: id, Project: project, Machine: "local", Agent: "claude",
			GitBranch: "main", StartedAt: &started, EndedAt: &ended,
			CreatedAt: started, LocalModifiedAt: &ended, MessageCount: 4,
			UserMessageCount: 2, ParentSessionID: parent,
			RelationshipType: relationship, DataVersion: 1,
		}
	}
	user := func(id string, ordinal int) db.Message {
		return db.Message{
			SessionID: id, Ordinal: ordinal, Role: "user", Content: "ask " + id,
			ContentLength: len("ask " + id), Timestamp: "2026-04-10T10:00:00Z",
		}
	}
	assistant := func(id string, ordinal int, clock, messageID string, in, out int) db.Message {
		return db.Message{
			SessionID: id, Ordinal: ordinal, Role: "assistant", Content: "work",
			ContentLength: len("work"), Timestamp: "2026-04-10T" + clock + "Z",
			Model: "claude-rollup-model", ClaudeMessageID: messageID,
			ClaudeRequestID: "req-" + messageID,
			TokenUsage: []byte(fmt.Sprintf(
				`{"input_tokens":%d,"output_tokens":%d}`, in, out)),
		}
	}
	_, err = local.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{
		{
			Session: session(rollupParent, "display", nil, ""),
			Messages: []db.Message{
				user(rollupParent, 0), user(rollupParent, 1),
				assistant(rollupParent, 2, "10:02:00", "m-parent", 10, 100),
				assistant(rollupParent, 3, "10:05:00", "m-shared", 7, 60),
			},
			ReplaceMessages: true, DataVersion: 1,
		},
		{
			Session: session(rollupSub, "path_sibling", &parentRef, "subagent"),
			Messages: []db.Message{
				user(rollupSub, 0), user(rollupSub, 1),
				assistant(rollupSub, 2, "10:01:00", "m-shared", 7, 40),
				assistant(rollupSub, 3, "10:03:00", "m-sub", 20, 200),
			},
			ReplaceMessages: true, DataVersion: 1,
		},
		{
			Session: session(rollupOther, "path_sibling", nil, ""),
			Messages: []db.Message{
				user(rollupOther, 0), user(rollupOther, 1),
				assistant(rollupOther, 2, "10:04:00", "m-other", 1000, 2),
			},
			ReplaceMessages: true, DataVersion: 1,
		},
	})
	require.NoError(t, err)
	return local
}

type rollupProjectTotal struct {
	Project                   string
	InputTokens, OutputTokens int
	CostMicrodollars          int64
}

type rollupTopSession struct {
	SessionID, Project string
	CostMicrodollars   int64
}

type rollupParitySummary struct {
	Projects          []rollupProjectTotal
	SessionsByProject map[string]int
	TopSessions       []rollupTopSession
	MatchingSessions  int
}

func subagentRollupSummary(
	t *testing.T, store db.Store, req service.UsageRequest,
) rollupParitySummary {
	t.Helper()
	ctx := t.Context()
	res, err := service.NewReadOnlyBackend(store).UsageSummary(ctx, req)
	require.NoError(t, err)
	out := rollupParitySummary{SessionsByProject: map[string]int{}}
	labels := map[string]string{}
	for _, p := range res.ProjectTotals {
		labels[p.ProjectKey] = p.Project
		out.Projects = append(out.Projects, rollupProjectTotal{
			Project: p.Project, InputTokens: p.InputTokens,
			OutputTokens: p.OutputTokens, CostMicrodollars: p.Cost.Microdollars,
		})
	}
	// Session counts arrive keyed by opaque project key; the totals carry
	// the label for each key.
	for key, n := range res.SessionCounts.ByProject {
		label, ok := labels[key]
		require.True(t, ok, "session count for project key %q without a total", key)
		out.SessionsByProject[label] = n
	}
	f, err := service.BuildUsageFilter(req, service.ModelGroups{})
	require.NoError(t, err)
	top, err := store.GetTopSessionsByCost(ctx, f, 10)
	require.NoError(t, err)
	for _, s := range top {
		out.TopSessions = append(out.TopSessions, rollupTopSession{
			SessionID: s.SessionID, Project: s.Project,
			CostMicrodollars: s.Cost.Microdollars,
		})
	}
	out.MatchingSessions, err = store.GetUsageMatchingSessionCount(ctx, f)
	require.NoError(t, err)
	return out
}

func TestUsageSubagentRollupParity(t *testing.T) {
	ctx := t.Context()
	local := seedSubagentRollupParity(t)
	const sessions = 3
	stores := map[string]db.Store{}

	duckTarget := filepath.Join(t.TempDir(), "rollup.duckdb")
	duckRes, err := duckdbstore.Push(ctx, duckTarget, local, "parity-machine",
		storage.MirrorPushOptions{}, true, nil)
	require.NoError(t, err)
	require.Equal(t, sessions, duckRes.SessionsPushed)
	duck, err := duckdbstore.NewStore(ctx, duckTarget)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, duck.Close()) })
	stores["duckdb"] = duck
	maps.Copy(stores, usageParityReplicaStores(
		t, local, "agentsview_subagent_rollup_parity_test", sessions,
	))

	// Costs in microdollars. Parent: 10 in / 100 out = 1010. Subagent:
	// 20 in / 200 out = 2020, plus the shared request (7 in / 60 out = 607),
	// which survives from the parent's copy but is attributed to the
	// subagent that recorded it first. Other: 1000 in / 2 out = 1020.
	display := rollupProjectTotal{"display", 37, 360, 3637}
	pathSibling := rollupProjectTotal{"path_sibling", 1000, 2, 1020}
	parentTop := rollupTopSession{rollupParent, "display", 1010}
	subTop := rollupTopSession{rollupSub, "display", 2627}
	otherTop := rollupTopSession{rollupOther, "path_sibling", 1020}
	all := rollupParitySummary{
		Projects:          []rollupProjectTotal{display, pathSibling},
		SessionsByProject: map[string]int{"display": 2, "path_sibling": 1},
		TopSessions:       []rollupTopSession{subTop, otherTop, parentTop},
		MatchingSessions:  3,
	}
	displayOnly := rollupParitySummary{
		Projects:          []rollupProjectTotal{display},
		SessionsByProject: map[string]int{"display": 2},
		TopSessions:       []rollupTopSession{subTop, parentTop},
		MatchingSessions:  2,
	}
	siblingOnly := rollupParitySummary{
		Projects:          []rollupProjectTotal{pathSibling},
		SessionsByProject: map[string]int{"path_sibling": 1},
		TopSessions:       []rollupTopSession{otherTop},
		MatchingSessions:  1,
	}
	base := service.UsageRequest{From: "2026-04-10", To: "2026-04-10", Timezone: "UTC"}
	with := func(edit func(*service.UsageRequest)) service.UsageRequest {
		req := base
		edit(&req)
		return req
	}
	for _, tc := range []struct {
		name string
		req  service.UsageRequest
		want rollupParitySummary
	}{
		{name: "unfiltered", req: base, want: all},
		{name: "parent project", req: with(func(r *service.UsageRequest) { r.Project = "display" }), want: displayOnly},
		{name: "subagent's own project", req: with(func(r *service.UsageRequest) { r.Project = "path_sibling" }), want: siblingOnly},
		{name: "exclude parent project", req: with(func(r *service.UsageRequest) { r.ExcludeProject = "display" }), want: siblingOnly},
		{name: "exclude subagent's own project", req: with(func(r *service.UsageRequest) { r.ExcludeProject = "path_sibling" }), want: displayOnly},
		{
			name: "parent project branch",
			req: with(func(r *service.UsageRequest) {
				r.GitBranch = db.EncodeBranchFilterToken("display", "main")
			}),
			want: displayOnly,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, subagentRollupSummary(t, local, tc.req), "sqlite")
			for name, store := range stores {
				assert.Equal(t, tc.want, subagentRollupSummary(t, store, tc.req), name)
			}
		})
	}
}
