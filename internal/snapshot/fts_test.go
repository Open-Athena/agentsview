package snapshot

import (
	"bufio"
	"database/sql"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loadSnapshot builds a fresh SQLite DB from an export directory the way a
// snapshot loader does: run schema.sql, then insert each NDJSON row. The
// FTS triggers in schema.sql rebuild the index as messages are inserted.
func loadSnapshot(t *testing.T, outDir string) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "loaded.db"))
	require.NoError(t, err, "open loaded db")
	t.Cleanup(func() { _ = conn.Close() })

	schema, err := os.ReadFile(filepath.Join(outDir, "schema.sql"))
	require.NoError(t, err, "read schema.sql")
	_, err = conn.ExecContext(t.Context(), string(schema))
	require.NoError(t, err, "apply schema.sql")

	for _, table := range baseTables {
		f, err := os.Open(filepath.Join(outDir, table+".ndjson"))
		require.NoError(t, err, "open %s.ndjson", table)
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		for sc.Scan() {
			var row map[string]any
			require.NoError(t, json.Unmarshal(sc.Bytes(), &row), "decode %s row", table)
			cols := make([]string, 0, len(row))
			for c := range row {
				cols = append(cols, c)
			}
			slices.Sort(cols)
			args := make([]any, len(cols))
			for i, c := range cols {
				args[i] = row[c]
			}
			stmt := "INSERT INTO " + table + " (" + strings.Join(cols, ", ") +
				") VALUES (" + strings.TrimSuffix(strings.Repeat("?, ", len(cols)), ", ") + ")"
			_, err := conn.ExecContext(t.Context(), stmt, args...)
			require.NoError(t, err, "insert %s row", table)
		}
		require.NoError(t, sc.Err(), "scan %s.ndjson", table)
		require.NoError(t, f.Close())
	}
	return conn
}

type ftsHit struct {
	SessionID string
	Ordinal   int
	Snippet   string
}

// TestExportedSchemaSupportsSnippetSearch loads an export into a fresh DB and
// runs the snapshot server's FTS query. snippet() reads the FTS content
// table, so the export must carry every object messages_fts depends on.
func TestExportedSchemaSupportsSnippetSearch(t *testing.T) {
	src := seedSource(t)
	out := filepath.Join(t.TempDir(), "snap")
	_, err := Export(t.Context(), src, out, Options{
		FullProjects: []string{"alpha"},
		GeneratedAt:  "2026-06-17T00:00:00Z",
	})
	require.NoError(t, err, "Export")

	conn := loadSnapshot(t, out)
	rows, err := conn.QueryContext(t.Context(), `
		SELECT m.session_id, m.ordinal,
			snippet(messages_fts, 0, '<mark>', '</mark>', '...', 12)
		FROM messages_fts
		JOIN messages m ON m.id = messages_fts.rowid
		WHERE messages_fts MATCH ?
		ORDER BY m.session_id, m.ordinal`, `"main"`)
	require.NoError(t, err, "FTS query")
	defer rows.Close()

	var hits []ftsHit
	for rows.Next() {
		var h ftsHit
		require.NoError(t, rows.Scan(&h.SessionID, &h.Ordinal, &h.Snippet))
		hits = append(hits, h)
	}
	require.NoError(t, rows.Err(), "iterate FTS rows")

	assert.Equal(t, []ftsHit{
		{SessionID: "s-main", Ordinal: 0, Snippet: "hello from <mark>main</mark>"},
		{SessionID: "s-main", Ordinal: 1, Snippet: "second <mark>main</mark> message"},
	}, hits)
}
