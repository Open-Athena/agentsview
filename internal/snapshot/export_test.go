package snapshot

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func ptr(s string) *string { return &s }

// seedSource builds a source DB with three sessions:
//   - s-main (project alpha)         -> included
//   - s-sub  (project beta, subagent of s-main) -> included via rollup
//   - s-other (project gamma)        -> excluded
func seedSource(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "source.db")

	d, err := db.Open(path)
	require.NoError(t, err, "open source db")

	require.NoError(t, d.UpsertSession(db.Session{
		ID: "s-main", Project: "alpha", Machine: "local", Agent: "claude",
	}), "upsert s-main")
	require.NoError(t, d.UpsertSession(db.Session{
		ID: "s-sub", Project: "beta", Machine: "local", Agent: "claude",
		ParentSessionID: ptr("s-main"), RelationshipType: "subagent",
	}), "upsert s-sub")
	require.NoError(t, d.UpsertSession(db.Session{
		ID: "s-other", Project: "gamma", Machine: "local", Agent: "claude",
	}), "upsert s-other")

	msg := func(sid string, ord int, content string) db.Message {
		return db.Message{
			SessionID: sid, Ordinal: ord, Role: "user", Content: content,
			ContentLength: len(content),
		}
	}
	mainMsgs := []db.Message{
		msg("s-main", 0, "hello from main"),
		msg("s-main", 1, "second main message"),
	}
	mainMsgs[1].HasToolUse = true
	mainMsgs[1].ToolCalls = []db.ToolCall{{
		ToolName: "Bash", Category: "exec",
		ResultContent: "tool result text", ResultContentLength: 16,
	}}
	require.NoError(t, d.ReplaceSessionMessages("s-main", mainMsgs),
		"messages s-main")
	require.NoError(t, d.ReplaceSessionMessages("s-sub", []db.Message{
		msg("s-sub", 0, "subagent message"),
	}), "messages s-sub")
	require.NoError(t, d.ReplaceSessionMessages("s-other", []db.Message{
		msg("s-other", 0, "other project message"),
	}), "messages s-other")

	require.NoError(t, d.Close(), "close source db")
	return path
}

func readNDJSONIDs(t *testing.T, path, idField string) []string {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err, "open %s", path)
	defer f.Close()

	var ids []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for sc.Scan() {
		var obj map[string]any
		require.NoError(t, json.Unmarshal(sc.Bytes(), &obj), "parse ndjson line")
		id, ok := obj[idField].(string)
		require.True(t, ok, "field %q present and string in %v", idField, obj)
		ids = append(ids, id)
	}
	require.NoError(t, sc.Err(), "scan %s", path)
	return ids
}

var createObjectRe = regexp.MustCompile(
	`CREATE (?:VIRTUAL TABLE|TABLE|TRIGGER)(?: IF NOT EXISTS)? "?([a-z_]+)"?`,
)

func schemaObjects(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err, "read schema.sql")
	var names []string
	for _, m := range createObjectRe.FindAllStringSubmatch(string(b), -1) {
		names = append(names, m[1])
	}
	sort.Strings(names)
	return names
}

func TestExportProjectScope(t *testing.T) {
	src := seedSource(t)
	out := filepath.Join(t.TempDir(), "snap")

	m, err := Export(context.Background(), src, out, Options{
		Projects:    []string{"alpha"},
		GeneratedAt: "2026-06-17T00:00:00Z",
	})
	require.NoError(t, err, "Export")

	// Manifest: s-main + s-sub (rollup), 3 messages total, 1 tool call,
	// 10 precomputed analytics routes, s-other excluded.
	assert.Equal(t, &Manifest{
		Projects:        []string{"alpha"},
		GeneratedAt:     "2026-06-17T00:00:00Z",
		SessionCount:    2,
		MessageCount:    3,
		ToolCallCount:   1,
		AnalyticsRoutes: 10,
		Tables: []string{
			"sessions", "messages", "tool_calls",
			"messages_fts", "analytics_cache",
		},
	}, m)

	// Precomputed dashboard routes, in emission order.
	assert.Equal(t, []string{
		"summary", "activity", "heatmap", "projects", "hour-of-week",
		"sessions", "tools", "velocity", "top-sessions", "signals",
	}, readNDJSONIDs(t, filepath.Join(out, "analytics_cache.ndjson"), "route"))

	// Sessions NDJSON: exactly the rolled-up set, deterministically ordered.
	assert.Equal(t, []string{"s-main", "s-sub"},
		readNDJSONIDs(t, filepath.Join(out, "sessions.ndjson"), "id"))

	// Messages NDJSON: every row belongs to an included session.
	msgSessions := readNDJSONIDs(t, filepath.Join(out, "messages.ndjson"), "session_id")
	assert.Equal(t, []string{"s-main", "s-main", "s-sub"}, msgSessions)

	// tool_calls NDJSON: the single tool call belongs to s-main.
	assert.Equal(t, []string{"s-main"},
		readNDJSONIDs(t, filepath.Join(out, "tool_calls.ndjson"), "session_id"))

	// Schema: base tables (incl. tool_calls), the FTS virtual table and its
	// triggers, and the snapshot-only analytics_cache.
	assert.Equal(t, []string{
		"analytics_cache", "messages", "messages_ad", "messages_ai",
		"messages_au", "messages_fts", "sessions", "tool_calls",
	}, schemaObjects(t, filepath.Join(out, "schema.sql")))
}

func TestExportAllProjects(t *testing.T) {
	src := seedSource(t)
	out := filepath.Join(t.TempDir(), "snap")

	// No projects => full, unfiltered export of every session.
	m, err := Export(context.Background(), src, out, Options{
		GeneratedAt: "2026-06-17T00:00:00Z",
	})
	require.NoError(t, err, "Export (all)")
	assert.Equal(t, 3, m.SessionCount, "all three sessions")
	assert.Equal(t, 4, m.MessageCount, "all four messages")
	assert.Empty(t, m.Projects, "no project scope recorded")

	assert.Equal(t, []string{"s-main", "s-other", "s-sub"},
		readNDJSONIDs(t, filepath.Join(out, "sessions.ndjson"), "id"))
}
