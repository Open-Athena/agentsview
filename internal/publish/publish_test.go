package publish

import (
	"bufio"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// Fixture values. None is a real credential.
const (
	r2Secret   = "Zq8vN3kL0pW7rT2xY5bC9mJ4hF6gD1sA"
	r2KeyID    = "4f9c2a7e1b8d3c6f0a5e9b2d7c4f1a8e"
	apiToken   = "tk9Hd3Lq8Zr2Vw6Ny1Pb5Mx7Cs4Gf0Jk"
	accountID  = "8a3f6c1e9b2d7a4f0c5e8b1d6a3f9c2e"
	bucketName = "team-artifacts-bucket-prod"
	fooToken   = "Xr7Kp2Qm9Lw4Zt8Nv3Bc6Hd1Jf5Gs0Ya"
)

const envrc = `# project secrets
export R2_SECRET_ACCESS_KEY=` + r2Secret + `
export R2_ACCESS_KEY_ID=` + r2KeyID + `
export CF_ACCOUNT_ID=` + accountID + `
export DATA_BUCKET=` + bucketName + `
export API_TOKEN="` + apiToken + `"
export DEBUG=true
`

// seedArchive builds an archive with one session in project "alpha" whose
// cwd holds envrc, a tool call that printed it, and prose mentioning other
// assignments. A second session in "beta" must not be selected.
func seedArchive(t *testing.T) (archive, cwd string) {
	t.Helper()
	cwd = t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(cwd, ".envrc"), []byte(envrc), 0o600))
	archive = filepath.Join(t.TempDir(), "sessions.db")

	d, err := db.Open(t.Context(), archive)
	require.NoError(t, err, "open archive")
	require.NoError(t, d.UpsertSession(t.Context(), db.Session{
		ID: "s-alpha", Project: "alpha", Machine: "laptop", Agent: "claude",
		Cwd: cwd, FirstMessage: new("print the env for " + r2Secret),
	}))
	require.NoError(t, d.UpsertSession(t.Context(), db.Session{
		ID: "s-beta", Project: "beta", Machine: "laptop", Agent: "claude",
	}))

	msg := func(ord int, content string) db.Message {
		return db.Message{
			SessionID: "s-alpha", Ordinal: ord, Role: "user",
			Content: content, ContentLength: len(content),
		}
	}
	msgs := []db.Message{
		msg(0, "print the env for "+r2Secret),
		msg(1, "running cat .envrc"),
		msg(2, "set FOO_TOKEN="+fooToken+" and PYTHON=/usr/bin/python3"),
	}
	msgs[1].Role = "assistant"
	msgs[1].HasToolUse = true
	msgs[1].ToolCalls = []db.ToolCall{{
		ToolName: "Bash", Category: "exec",
		ResultContent: envrc, ResultContentLength: len(envrc),
	}}
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "s-alpha", msgs))
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "s-beta", []db.Message{{
		SessionID: "s-beta", Ordinal: 0, Role: "user", Content: "unrelated", ContentLength: 9,
	}}))
	require.NoError(t, d.Close())
	return archive, cwd
}

func readRows(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	var rows []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var row map[string]any
		require.NoError(t, json.Unmarshal(sc.Bytes(), &row), "row parses")
		rows = append(rows, row)
	}
	require.NoError(t, sc.Err())
	return rows
}

func strField(rows []map[string]any, field string) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i], _ = r[field].(string)
	}
	return out
}

var wantTotals = []Group{
	{Kind: "env:API_TOKEN", Action: ActionRedact, Count: 1},
	{Kind: "env:R2_ACCESS_KEY_ID", Action: ActionRedact, Count: 1},
	{Kind: "env:R2_SECRET_ACCESS_KEY", Action: ActionRedact, Count: 4},
	{Kind: "high-entropy-assignment", Action: ActionRedact, Count: 2},
}

func TestPublishPreviewReportsWithoutWriting(t *testing.T) {
	archive, _ := seedArchive(t)
	rep, err := Run(t.Context(), Options{
		ArchivePath: archive,
		Projects:    []string{"alpha"},
		GeneratedAt: "2026-11-03T00:00:00Z",
	})
	require.NoError(t, err)

	assert.Equal(t, "", rep.Out)
	assert.Equal(t, 1, rep.SessionCount)
	assert.Equal(t, EnvReport{Dirs: 1, FileOnly: 1, Values: 3}, rep.Env)
	assert.Equal(t, []string{"laptop"}, rep.Machines)
	assert.Equal(t, []SessionReport{{
		ID: "s-alpha", Project: "alpha",
		Title: "print the env for [REDACTED:env:R2_SECRET_ACCESS_KEY]",
		Findings: []Group{
			{Kind: "env:API_TOKEN", Action: ActionRedact, Count: 1, Ordinals: []int{1}},
			{Kind: "env:R2_ACCESS_KEY_ID", Action: ActionRedact, Count: 1, Ordinals: []int{1}},
			{Kind: "env:R2_SECRET_ACCESS_KEY", Action: ActionRedact, Count: 3, Ordinals: []int{0, 1}},
			{Kind: "high-entropy-assignment", Action: ActionRedact, Count: 1, Ordinals: []int{2}},
		},
	}}, rep.Sessions)
	// The longest-prompts panel repeats the two user prompts.
	assert.Equal(t, []Group{
		{Kind: "env:R2_SECRET_ACCESS_KEY", Action: ActionRedact, Count: 1, Routes: []string{"messages/longest"}},
		{Kind: "high-entropy-assignment", Action: ActionRedact, Count: 1, Routes: []string{"messages/longest"}},
	}, rep.Aggregates)
	assert.Equal(t, wantTotals, rep.Totals)
	assert.Equal(t, 8, rep.Redactions())
	assert.Equal(t, 0, rep.Flags())
}

func TestPublishOutRedactsAndRoundTrips(t *testing.T) {
	archive, _ := seedArchive(t)
	out := filepath.Join(t.TempDir(), "snap")
	rep, err := Run(t.Context(), Options{
		ArchivePath: archive,
		Projects:    []string{"alpha"},
		OutDir:      out,
		Contributor: "dev@example.com",
		GeneratedAt: "2026-11-03T00:00:00Z",
	})
	require.NoError(t, err)
	assert.Equal(t, out, rep.Out)
	assert.Equal(t, wantTotals, rep.Totals)

	// Messages: secret-named env values and the secret-named assignment are
	// redacted; the python path is left alone.
	assert.Equal(t, []string{
		"print the env for [REDACTED:env:R2_SECRET_ACCESS_KEY]",
		"running cat .envrc",
		"set FOO_TOKEN=[REDACTED:high-entropy-assignment] and PYTHON=/usr/bin/python3",
	}, strField(readRows(t, filepath.Join(out, "messages.ndjson")), "content"))

	// The printed .envrc: credentials redacted, the account ID and bucket
	// name (identifiers) kept.
	assert.Equal(t, []string{`# project secrets
export R2_SECRET_ACCESS_KEY=[REDACTED:env:R2_SECRET_ACCESS_KEY]
export R2_ACCESS_KEY_ID=[REDACTED:env:R2_ACCESS_KEY_ID]
export CF_ACCOUNT_ID=` + accountID + `
export DATA_BUCKET=` + bucketName + `
export API_TOKEN="[REDACTED:env:API_TOKEN]"
export DEBUG=true
`}, strField(readRows(t, filepath.Join(out, "tool_calls.ndjson")), "result_content"))

	// No collected value survives in any output file.
	entries, err := os.ReadDir(out)
	require.NoError(t, err)
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(out, e.Name()))
		require.NoError(t, err)
		for _, v := range []string{r2Secret, r2KeyID, apiToken, fooToken} {
			assert.False(t, strings.Contains(string(b), v), "%s leaks a value", e.Name())
		}
	}

	var manifest struct {
		SessionCount int         `json:"sessionCount"`
		Publish      publishInfo `json:"publish"`
	}
	b, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &manifest))
	assert.Equal(t, 1, manifest.SessionCount)
	assert.Equal(t, publishInfo{
		Contributor: "dev@example.com",
		Machines:    []string{"laptop"},
		Visibility:  map[string]string{"s-alpha": "draft"},
		Redactions:  wantTotals,
	}, manifest.Publish)
}

func TestPublishRejectsUnknownVisibility(t *testing.T) {
	_, err := Run(t.Context(), Options{
		ArchivePath: "unused.db", Projects: []string{"alpha"}, Visibility: "public",
	})
	require.EqualError(t, err, `publish: visibility "public" is not one of draft, oa, link`)
}

func TestKeepEnvValue(t *testing.T) {
	cases := []struct {
		name, value string
		want        bool
	}{
		{"R2_SECRET_ACCESS_KEY", r2Secret, true},
		{"R2_ACCESS_KEY_ID", r2KeyID, true}, // an access key's ID names KEY
		{"CF_ACCOUNT_ID", accountID, false},
		{"DATA_BUCKET", bucketName, false},
		{"SOME_SETTING", "a1b2c3d4e5f6g7h8i9j0k1", true}, // credential-shaped value
		{"SOME_SETTING", "/usr/local/bin/python3.13", false},
		{"API_TOKEN", "short", false},
		{"API_TOKEN", "$OTHER_VAR_REFERENCE", false},
		{"DB_PASSWORD", "plain-words.here", false},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, keepEnvValue(c.name, c.value), "%s=%s", c.name, c.value)
	}
}

func TestRedactorHighEntropyAndDeny(t *testing.T) {
	red := NewRedactor(nil, []string{"Project Falcon"})
	loc := location{file: "messages.ndjson", sessionID: "s", ordinal: 4}
	got := red.apply(
		"FOO_TOKEN="+fooToken+"; COMMIT_SHA=3f4753055a1b2c3d4e5f60718293a4b5c6d7e8f9; about project falcon",
		loc, true)
	assert.Equal(t,
		"FOO_TOKEN=[REDACTED:high-entropy-assignment]; COMMIT_SHA=3f4753055a1b2c3d4e5f60718293a4b5c6d7e8f9; about project falcon",
		got)
	assert.Equal(t, []hit{
		{loc: loc, kind: "high-entropy-assignment", action: ActionRedact},
		{loc: loc, kind: KindDeny, action: ActionFlag},
	}, red.hits)
	assert.Equal(t, map[string]int{"high-entropy-assignment": 1}, red.ignored)
}

// TestPublishOlderArchiveSchema publishes from an archive whose
// model_pricing still has an older column layout (as written by an older
// agentsview). The scratch copy is migrated; the archive is only read.
func TestPublishOlderArchiveSchema(t *testing.T) {
	archive, _ := seedArchive(t)
	conn, err := sql.Open("sqlite3", archive)
	require.NoError(t, err)
	for _, stmt := range []string{
		"ALTER TABLE model_pricing DROP COLUMN cache_creation_1h_microdollars_per_mtok",
		"ALTER TABLE model_pricing ADD COLUMN cache_creation_1h_per_mtok REAL NOT NULL DEFAULT 0",
		`INSERT OR REPLACE INTO model_pricing (model_pattern, input_microdollars_per_mtok,
			output_microdollars_per_mtok, cache_creation_microdollars_per_mtok,
			cache_read_microdollars_per_mtok, cache_creation_1h_per_mtok)
			VALUES ('test-model', 3000000, 15000000, 3750000, 300000, 6.0)`,
	} {
		_, err := conn.ExecContext(t.Context(), stmt)
		require.NoError(t, err, stmt)
	}
	require.NoError(t, conn.Close())
	before := fileSHA(t, archive)

	rep, err := Run(t.Context(), Options{
		ArchivePath: archive,
		Projects:    []string{"alpha"},
		GeneratedAt: "2026-11-03T00:00:00Z",
	})
	require.NoError(t, err)
	assert.Equal(t, wantTotals, rep.Totals)
	assert.Equal(t, before, fileSHA(t, archive), "archive unchanged")
}

func fileSHA(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
