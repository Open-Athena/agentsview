package db

import (
	"context"
	"fmt"
)

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
// previewChars caps the substr; 200 is enough to identify the
// prompt without bloating the response.
func (db *DB) GetLongestUserPrompts(
	ctx context.Context, limit int,
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
	rows, err := db.getReader().QueryContext(ctx, `
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
		  AND m.content NOT LIKE '<local-command-stdout>%'
		ORDER BY m.content_length DESC, m.id DESC
		LIMIT ?`,
		previewChars, limit,
	)
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
