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
	"go.kenn.io/agentsview/internal/money"
)

// seedSource builds a source DB with three sessions:
//   - s-main (project alpha)         -> included
//   - s-sub  (project beta, subagent of s-main) -> included via rollup
//   - s-other (project gamma)        -> excluded
func seedSource(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "source.db")

	d, err := db.Open(t.Context(), path)
	require.NoError(t, err, "open source db")

	require.NoError(t, d.UpsertSession(t.Context(), db.Session{
		ID: "s-main", Project: "alpha", Machine: "local", Agent: "claude",
	}), "upsert s-main")
	require.NoError(t, d.UpsertSession(t.Context(), db.Session{
		ID: "s-sub", Project: "beta", Machine: "local", Agent: "claude",
		ParentSessionID: new("s-main"), RelationshipType: "subagent",
	}), "upsert s-sub")
	require.NoError(t, d.UpsertSession(t.Context(), db.Session{
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
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "s-main", mainMsgs),
		"messages s-main")
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "s-sub", []db.Message{
		msg("s-sub", 0, "subagent message"),
	}), "messages s-sub")
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "s-other", []db.Message{
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

// routeBody returns the JSON body of one precomputed analytics_cache route.
func routeBody(t *testing.T, path, route string) string {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err, "open %s", path)
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for sc.Scan() {
		var obj map[string]any
		require.NoError(t, json.Unmarshal(sc.Bytes(), &obj), "parse cache line")
		if obj["route"] == route {
			body, _ := obj["body"].(string)
			return body
		}
	}
	t.Fatalf("route %q not found in %s", route, path)
	return ""
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
		FullProjects: []string{"alpha"},
		GeneratedAt:  "2026-06-17T00:00:00Z",
	})
	require.NoError(t, err, "Export")

	// Manifest: s-main + s-sub (rollup), 3 messages total, 1 tool call,
	// 13 precomputed routes, s-other excluded.
	assert.Equal(t, &Manifest{
		FullProjects:    []string{"alpha"},
		GeneratedAt:     "2026-06-17T00:00:00Z",
		SessionCount:    2,
		MessageCount:    3,
		ToolCallCount:   1,
		AnalyticsRoutes: 14,
		Tables: []string{
			"sessions", "messages", "tool_calls",
			"messages_fts", "analytics_cache",
		},
	}, m)

	// Precomputed routes, in emission order: aggregate routes first, then the
	// session-detail routes, then the view/meta self-description.
	assert.Equal(t, []string{
		"summary", "activity", "heatmap", "projects", "hour-of-week",
		"sessions", "tools", "velocity", "signals", "usage/summary",
		"top-sessions", "usage/top-sessions", "messages/longest", "view/meta",
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
	// triggers, and the snapshot-only analytics_cache + grants tables.
	assert.Equal(t, []string{
		"analytics_cache", "grants", "messages", "messages_ad", "messages_ai",
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
	assert.Empty(t, m.FullProjects, "no project scope recorded")

	assert.Equal(t, []string{"s-main", "s-other", "s-sub"},
		readNDJSONIDs(t, filepath.Join(out, "sessions.ndjson"), "id"))
}

func TestExportStatsTierRedacted(t *testing.T) {
	src := seedSource(t)
	out := filepath.Join(t.TempDir(), "snap")

	// alpha is full; gamma (s-other) is stats-only — its sessions/messages
	// must NOT appear in the exported data, but it still feeds analytics.
	m, err := Export(context.Background(), src, out, Options{
		FullProjects:  []string{"alpha"},
		StatsProjects: []string{"gamma"},
		GeneratedAt:   "2026-06-17T00:00:00Z",
	})
	require.NoError(t, err, "Export (tiered)")
	assert.Equal(t, []string{"alpha"}, m.FullProjects)
	assert.Equal(t, []string{"gamma"}, m.StatsProjects)

	// Data scope: only full-tier sessions (alpha + its subagent rollup).
	// The stats-only gamma session (s-other) is redacted from raw data.
	assert.Equal(t, []string{"s-main", "s-sub"},
		readNDJSONIDs(t, filepath.Join(out, "sessions.ndjson"), "id"))
	assert.NotContains(t,
		readNDJSONIDs(t, filepath.Join(out, "messages.ndjson"), "session_id"),
		"s-other", "redacted project's messages must be absent")
}

// readNDJSONObjs returns each NDJSON line of path parsed into a map.
func readNDJSONObjs(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err, "open %s", path)
	defer f.Close()
	var objs []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for sc.Scan() {
		var obj map[string]any
		require.NoError(t, json.Unmarshal(sc.Bytes(), &obj), "parse ndjson")
		objs = append(objs, obj)
	}
	require.NoError(t, sc.Err(), "scan %s", path)
	return objs
}

// longestPromptSessionIDs parses the messages/longest route body (an array of
// {session_id, ...}) and returns the session IDs whose prompts it surfaces.
// This route sorts by content length, so it is independent of session
// timestamps (unlike top-sessions, which the date-bounded filter would drop for
// timestamp-less fixtures).
func longestPromptSessionIDs(t *testing.T, cachePath string) []string {
	t.Helper()
	var entries []struct {
		SessionID string `json:"session_id"`
	}
	require.NoError(t, json.Unmarshal(
		[]byte(routeBody(t, cachePath, "messages/longest")), &entries),
		"parse messages/longest")
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.SessionID)
	}
	return ids
}

func TestExportPrivateSessionElided(t *testing.T) {
	src := seedSource(t)
	opts := Options{FullProjects: []string{"alpha"}, GeneratedAt: "2026-06-17T00:00:00Z"}

	// Baseline: export with nothing marked private.
	base := filepath.Join(t.TempDir(), "base")
	_, err := Export(context.Background(), src, base, opts)
	require.NoError(t, err, "baseline export")
	baseCache := filepath.Join(base, "analytics_cache.ndjson")
	// s-main's prompts appear in messages/longest here, making the "absent
	// after redaction" assertion below non-vacuous.
	require.Contains(t, longestPromptSessionIDs(t, baseCache), "s-main",
		"baseline longest-prompts includes s-main")

	// Now flag s-main private via the DB and re-export.
	d, err := db.Open(t.Context(), src)
	require.NoError(t, err, "reopen source")
	require.NoError(t, d.SetSessionPrivate(context.Background(), "s-main", true), "flag private")
	require.NoError(t, d.Close(), "close source")

	out := filepath.Join(t.TempDir(), "snap")
	m, err := Export(context.Background(), src, out, opts)
	require.NoError(t, err, "Export")
	assert.Equal(t, 1, m.PrivateCount, "one private session")
	// The ghost row is still counted as an exported session.
	assert.Equal(t, 2, m.SessionCount, "s-main ghost + s-sub")
	// Only s-sub's single message survives; s-main's two are elided.
	assert.Equal(t, 1, m.MessageCount, "private session's messages elided")

	// sessions.ndjson still lists both, but s-main is scrubbed + marked.
	byID := map[string]map[string]any{}
	for _, o := range readNDJSONObjs(t, filepath.Join(out, "sessions.ndjson")) {
		byID[o["id"].(string)] = o
	}
	require.Contains(t, byID, "s-main", "private ghost row present")
	assert.Equal(t, "[redacted]", byID["s-main"]["first_message"],
		"private title scrubbed")
	assert.Equal(t, "[redacted]", byID["s-main"]["display_name"],
		"private display_name scrubbed")
	assert.Equal(t, float64(1), byID["s-main"]["private"], "marked private")
	// The non-private session keeps its real content.
	assert.NotEqual(t, "[redacted]", byID["s-sub"]["first_message"],
		"non-private session unscrubbed")

	// messages.ndjson: nothing from the private session.
	assert.Equal(t, []string{"s-sub"},
		readNDJSONIDs(t, filepath.Join(out, "messages.ndjson"), "session_id"),
		"private session's messages absent")

	// Detail routes: the private session must not appear (its prompts/title
	// would otherwise surface there).
	cache := filepath.Join(out, "analytics_cache.ndjson")
	assert.NotContains(t, longestPromptSessionIDs(t, cache), "s-main",
		"private session's prompts absent from messages/longest")

	// But it still feeds aggregate analytics: the aggregate summary route is
	// byte-identical to the baseline (privacy only affects data + detail scope).
	assert.Equal(t, routeBody(t, baseCache, "summary"), routeBody(t, cache, "summary"),
		"aggregate summary unchanged by redaction")
	assert.Equal(t, routeBody(t, baseCache, "usage/summary"), routeBody(t, cache, "usage/summary"),
		"aggregate usage summary unchanged by redaction")
}

func TestExportAllStatsNoData(t *testing.T) {
	src := seedSource(t)
	out := filepath.Join(t.TempDir(), "snap")

	// Public stats view: every project stats-only — analytics over all, but
	// zero raw sessions/messages exported.
	m, err := Export(context.Background(), src, out, Options{
		AllStats:    true,
		GeneratedAt: "2026-06-17T00:00:00Z",
	})
	require.NoError(t, err, "Export (all-stats)")
	assert.True(t, m.AllStats)
	assert.Equal(t, 0, m.SessionCount, "no raw sessions exported")
	assert.Equal(t, 0, m.MessageCount, "no raw messages exported")
	assert.Equal(t, 14, m.AnalyticsRoutes, "analytics still precomputed")
	assert.Empty(t,
		readNDJSONIDs(t, filepath.Join(out, "sessions.ndjson"), "id"),
		"sessions.ndjson must be empty")

	// Session-detail routes must carry no session-level content (names,
	// first-messages, prompts) in a stats-only export.
	cache := filepath.Join(out, "analytics_cache.ndjson")
	for _, route := range []string{"messages/longest", "usage/top-sessions"} {
		var entries []any
		require.NoError(t,
			json.Unmarshal([]byte(routeBody(t, cache, route)), &entries),
			"parse %s", route)
		assert.Empty(t, entries, "%s must have no session-level entries", route)
	}
	// Aggregate cost is still present.
	assert.Contains(t, routeBody(t, cache, "usage/summary"), "totalCost",
		"aggregate cost is preserved")
}

func TestViewConfigExport(t *testing.T) {
	src := seedSource(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "public.json")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`{
		"name": "public",
		"title": "All projects (public)",
		"allStats": true,
		"blurbs": {"alpha": "Alpha **summary**."}
	}`), 0o644), "write view config")

	vc, err := LoadViewConfig(cfgPath)
	require.NoError(t, err, "LoadViewConfig")
	assert.Equal(t, "public", vc.Name)

	out := filepath.Join(dir, "snap")
	m, err := Export(context.Background(), src, out,
		vc.Options("2026-06-18T00:00:00Z"))
	require.NoError(t, err, "Export (view)")
	assert.True(t, m.AllStats)
	assert.Equal(t, 0, m.SessionCount, "all-stats: no raw sessions")

	// view/meta carries the title, statsOnly flag, and per-project blurbs.
	var meta map[string]any
	require.NoError(t, json.Unmarshal(
		[]byte(routeBody(t, filepath.Join(out, "analytics_cache.ndjson"), "view/meta")),
		&meta), "parse view/meta")
	assert.Equal(t, "All projects (public)", meta["title"])
	assert.Equal(t, true, meta["statsOnly"])
	blurbs, _ := meta["blurbs"].(map[string]any)
	assert.Equal(t, "Alpha **summary**.", blurbs["alpha"])
}

func TestLoadViewConfigErrors(t *testing.T) {
	dir := t.TempDir()
	missingName := filepath.Join(dir, "a.json")
	require.NoError(t, os.WriteFile(missingName, []byte(`{"allStats": true}`), 0o644))
	_, err := LoadViewConfig(missingName)
	assert.ErrorContains(t, err, "name is required")

	unknownField := filepath.Join(dir, "b.json")
	require.NoError(t, os.WriteFile(unknownField, []byte(`{"name":"x","bogus":1}`), 0o644))
	_, err = LoadViewConfig(unknownField)
	require.Error(t, err, "unknown field should error")
}

// seedDatedSource builds a source DB with one long-running session whose
// messages straddle a date boundary, each carrying priced token_usage.
// This mirrors the real shape that motivated --since/--until: a session
// started months before the window it is being reported on.
func seedDatedSource(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source.db")
	d, err := db.Open(t.Context(), path)
	require.NoError(t, err, "open source db")

	require.NoError(t, d.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "claude-test",
		InputPerMTok:  money.MustParseDollars("1000"),
		OutputPerMTok: money.MustParseDollars("1000"),
	}}), "seed pricing")

	started := "2026-01-01T00:00:00Z"
	ended := "2026-03-31T00:00:00Z"
	// MessageCount/UserMessageCount are session-row counters maintained by the
	// sync engine, not by ReplaceSessionMessages — the analytics panels read
	// them, so seed them to match the messages written below.
	require.NoError(t, d.UpsertSession(t.Context(), db.Session{
		ID: "s-long", Project: "alpha", Machine: "local", Agent: "claude",
		StartedAt: &started, EndedAt: &ended,
		MessageCount: 8, UserMessageCount: 4,
	}), "upsert s-long")

	// Each date gets a user turn (so the session registers in the analytics
	// panels) plus a priced assistant turn: 1M input + 1M output tokens =>
	// $2000/day at the seeded rate, so each in-window day is worth $2000.
	var msgs []db.Message
	for _, ts := range []string{
		"2026-01-05T12:00:00Z", // before window
		"2026-02-10T12:00:00Z", // in window
		"2026-02-11T12:00:00Z", // in window
		"2026-03-20T12:00:00Z", // after window
	} {
		msgs = append(msgs, db.Message{
			SessionID: "s-long", Ordinal: len(msgs), Role: "user",
			Content: "ask " + ts, ContentLength: 8, Timestamp: ts,
		}, db.Message{
			SessionID: "s-long", Ordinal: len(msgs) + 1, Role: "assistant",
			Content: "ans " + ts, ContentLength: 8, Timestamp: ts,
			Model: "claude-test",
			TokenUsage: json.RawMessage(
				`{"input_tokens":1000000,"output_tokens":1000000}`),
		})
	}
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "s-long", msgs),
		"messages s-long")

	require.NoError(t, d.Close(), "close source db")
	return path
}

// usageTotalCost reads the precomputed usage/summary total out of an
// export directory.
func usageTotalCost(t *testing.T, outDir string) int64 {
	t.Helper()
	var body struct {
		Totals struct {
			TotalCost struct {
				Microdollars int64 `json:"microdollars"`
			} `json:"totalCost"`
		} `json:"totals"`
	}
	cachePath := filepath.Join(outDir, "analytics_cache.ndjson")
	require.NoError(t, json.Unmarshal(
		[]byte(routeBody(t, cachePath, "usage/summary")), &body),
		"parse usage/summary")
	return body.Totals.TotalCost.Microdollars
}

// analyticsSessionCount reads the precomputed analytics summary's session
// count out of an export directory.
func analyticsSessionCount(t *testing.T, outDir string) int {
	t.Helper()
	var body struct {
		TotalSessions int `json:"total_sessions"`
	}
	cachePath := filepath.Join(outDir, "analytics_cache.ndjson")
	require.NoError(t, json.Unmarshal(
		[]byte(routeBody(t, cachePath, "summary")), &body), "parse summary")
	return body.TotalSessions
}

func TestExportDateWindowScopesUsage(t *testing.T) {
	src := seedDatedSource(t)

	fullOut := t.TempDir()
	_, err := Export(context.Background(), src, fullOut, Options{
		GeneratedAt: "2026-04-01T00:00:00Z",
	})
	require.NoError(t, err, "export full span")

	winOut := t.TempDir()
	m, err := Export(context.Background(), src, winOut, Options{
		From: "2026-02-01", To: "2026-02-28",
		GeneratedAt: "2026-04-01T00:00:00Z",
	})
	require.NoError(t, err, "export windowed")

	// All four messages are priced when the span is unbounded; only the
	// two February ones fall inside the window. Cost is microdollars:
	// $8000 = 8e9, $4000 = 4e9.
	assert.Equal(t, int64(8_000_000_000), usageTotalCost(t, fullOut))
	assert.Equal(t, int64(4_000_000_000), usageTotalCost(t, winOut))

	// The window must not reach the analytics panels. Their From/To select
	// sessions by started_at, and s-long started 2026-01-01 — before the
	// window — so passing the window through would report zero sessions
	// next to a non-zero usage cost.
	assert.Equal(t, 1, analyticsSessionCount(t, winOut),
		"analytics keep the full span under a usage window")
	assert.Equal(t, 1, analyticsSessionCount(t, fullOut))

	// The window bounds the aggregate, not the data: the session started
	// 2026-01-01, well before the window, and must still be exported whole
	// so drill-down keeps working.
	assert.Equal(t, "2026-02-01", m.From)
	assert.Equal(t, "2026-02-28", m.To)
	assert.Equal(t, 1, m.SessionCount)
	assert.Equal(t, []string{"s-long"},
		readNDJSONIDs(t, filepath.Join(winOut, "sessions.ndjson"), "id"))
	assert.Equal(t, 8, m.MessageCount)
}
