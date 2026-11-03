package db

import (
	"context"
	"fmt"
	"strings"
)

// splitCSV trims and returns the non-empty fields of a
// comma-separated string. Local helper to avoid a runtime dep.

// placeholders returns n comma-separated "?" tokens for a
// SQL IN (...) clause. Caller appends the matching args.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}

// LongestUserPrompt is one row of the "top-N user prompts by
// content_length" query: enough metadata to locate the message
// (session_id, ordinal) plus a preview for surfacing in tables.
type LongestUserPrompt struct {
	SessionID     string `json:"session_id"`
	Project       string `json:"project"`
	Ordinal       int    `json:"ordinal"`
	ContentLength int    `json:"content_length"`
	Preview       string `json:"preview"`
	Timestamp     string `json:"timestamp,omitempty"`
}

// GetLongestUserPrompts returns the top `limit` longest user-role
// messages by content_length, with a preview clipped to 200
// characters. is_system messages are excluded (they're internal
// scaffolding, not human prompts). Subagent sessions are excluded
// since their "user" messages are agent-spawned tool prompts,
// not actual user input.
//
// `project`, when non-empty, accepts a comma-separated list of
// project names to restrict to (e.g. "tomato,crashes"). Matches
// sessions.project — the display-derived name (e.g. "tomato"),
// not the path-derived sibling that subagents use ("tomat"),
// which is fine here since subagents are already filtered out.
//
// previewChars caps the substr; 200 is enough to identify the
// prompt without bloating the response.
func (db *DB) GetLongestUserPrompts(
	ctx context.Context, limit int, project string,
) ([]LongestUserPrompt, error) {
	if limit <= 0 {
		limit = 20
	}
	const previewChars = 200

	// Filter out auto-injected "user" messages that aren't actually
	// user-typed: <task-notification> (TaskCreate/Update output),
	// <system-reminder> (CLAUDE.md/skill scaffolding), <command-name>
	// (slash command echoes), and <local-command-stdout> (shell
	// command output captured for context).
	query := `
		SELECT
			m.session_id,
			COALESCE(s.project, '') AS project,
			m.ordinal,
			m.content_length,
			substr(m.content, 1, ?) AS preview,
			COALESCE(m.timestamp, '') AS timestamp
		FROM messages m
		JOIN sessions s ON s.id = m.session_id
		WHERE m.role = 'user'
		  AND m.is_system = 0
		  AND s.deleted_at IS NULL
		  AND COALESCE(s.relationship_type, '') != 'subagent'
		  AND m.content NOT LIKE '<task-notification>%'
		  AND m.content NOT LIKE '<system-reminder>%'
		  AND m.content NOT LIKE '<command-name>%'
		  AND m.content NOT LIKE '<local-command-stdout>%'`
	args := []any{previewChars}

	if project != "" {
		projects := splitCSV(project)
		if len(projects) == 1 {
			query += ` AND s.project = ?`
			args = append(args, projects[0])
		} else {
			query += ` AND s.project IN (` +
				placeholders(len(projects)) + `)`
			for _, p := range projects {
				args = append(args, p)
			}
		}
	}

	query += ` ORDER BY m.content_length DESC, m.session_id ASC, m.ordinal ASC LIMIT ?`
	args = append(args, limit)

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf(
			"querying longest user prompts: %w", err,
		)
	}
	defer rows.Close()

	var out []LongestUserPrompt
	for rows.Next() {
		var p LongestUserPrompt
		if err := rows.Scan(
			&p.SessionID, &p.Project, &p.Ordinal,
			&p.ContentLength, &p.Preview, &p.Timestamp,
		); err != nil {
			return nil, fmt.Errorf(
				"scanning longest prompt row: %w", err,
			)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf(
			"iterating longest prompt rows: %w", err,
		)
	}
	return out, nil
}
