package publish

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

// sliceMetadataTables are copied whole: small tables the schema migrations
// and the export's pricing read, keyed by neither a session nor a session_id.
var sliceMetadataTables = []string{"archive_metadata", "background_migrations", "model_pricing"}

// sliceArchive copies the given sessions out of archive into a new SQLite
// database at out, then opens it with db.Open so its schema is migrated to
// this binary's version. The archive is only read. The schema is copied
// whole; rows only for the sessions (every table with a session_id column)
// and the metadata tables.
func sliceArchive(ctx context.Context, archive, out string, ids []string) error {
	if err := copySessions(ctx, archive, out, ids); err != nil {
		return err
	}
	d, err := db.Open(ctx, out)
	if err != nil {
		return fmt.Errorf("publish: migrate scratch db: %w", err)
	}
	return d.Close()
}

type schemaObject struct{ kind, name, sql string }

func copySessions(ctx context.Context, archive, out string, ids []string) error {
	conn, err := sql.Open("sqlite3", out)
	if err != nil {
		return fmt.Errorf("publish: open scratch db: %w", err)
	}
	defer conn.Close()
	// One connection: ATTACH and the temp table are per-connection state.
	conn.SetMaxOpenConns(1)

	srcURI := "file:" + archive + "?mode=ro"
	if _, err := conn.ExecContext(ctx, "ATTACH DATABASE ? AS src", srcURI); err != nil {
		return fmt.Errorf("publish: attach archive: %w", err)
	}

	rows, err := conn.QueryContext(ctx, `
		SELECT type, name, sql FROM src.sqlite_master
		WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return fmt.Errorf("publish: read archive schema: %w", err)
	}
	var objs []schemaObject
	for rows.Next() {
		var o schemaObject
		if err := rows.Scan(&o.kind, &o.name, &o.sql); err != nil {
			rows.Close()
			return err
		}
		objs = append(objs, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	var virtual []string
	for _, o := range objs {
		if o.kind == "table" && strings.HasPrefix(strings.ToUpper(o.sql), "CREATE VIRTUAL") {
			virtual = append(virtual, o.name)
		}
	}
	shadow := func(name string) bool {
		for _, v := range virtual {
			if strings.HasPrefix(name, v+"_") {
				return true
			}
		}
		return false
	}
	order := map[string]int{"table": 0, "index": 1, "view": 2, "trigger": 3}
	slices.SortStableFunc(objs, func(a, b schemaObject) int { return order[a.kind] - order[b.kind] })
	for _, o := range objs {
		if o.kind == "table" && shadow(o.name) {
			continue
		}
		if _, err := conn.ExecContext(ctx, o.sql); err != nil {
			return fmt.Errorf("publish: create %s %s: %w", o.kind, o.name, err)
		}
	}

	if _, err := conn.ExecContext(ctx, "CREATE TEMP TABLE keep (id TEXT PRIMARY KEY)"); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := conn.ExecContext(ctx, "INSERT OR IGNORE INTO keep VALUES (?)", id); err != nil {
			return err
		}
	}

	for _, o := range objs {
		if o.kind != "table" || shadow(o.name) || slices.Contains(virtual, o.name) {
			continue
		}
		cols, err := tableColumns(ctx, conn, o.name)
		if err != nil {
			return err
		}
		var where string
		switch {
		case o.name == "sessions":
			where = "WHERE id IN (SELECT id FROM keep)"
		case slices.Contains(cols, "session_id"):
			where = "WHERE session_id IN (SELECT id FROM keep)"
		case slices.Contains(sliceMetadataTables, o.name):
			where = ""
		default:
			continue
		}
		q := fmt.Sprintf(`INSERT OR REPLACE INTO main.%q SELECT * FROM src.%q %s`, o.name, o.name, where)
		if _, err := conn.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("publish: copy %s: %w", o.name, err)
		}
	}
	if _, err := conn.ExecContext(ctx, "DETACH DATABASE src"); err != nil {
		return fmt.Errorf("publish: detach archive: %w", err)
	}
	return nil
}

func tableColumns(ctx context.Context, conn *sql.DB, table string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, fmt.Sprintf("PRAGMA src.table_info(%q)", table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var (
			cid     int
			name    string
			typ     string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols = append(cols, name)
	}
	return cols, rows.Err()
}
