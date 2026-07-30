package duckdb

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

// GetLongestUserPrompts returns the top-N longest user-role messages by
// content length, mirroring the SQLite implementation. is_system messages
// and subagent sessions are excluded, along with the auto-injected
// scaffolding blocks the parser flags as role=user.
func (s *Store) GetLongestUserPrompts(
	ctx context.Context, limit int, project string,
) ([]db.LongestUserPrompt, error) {
	if limit <= 0 {
		limit = 20
	}
	const previewChars = 200

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
		  AND m.is_system = FALSE
		  AND s.deleted_at IS NULL
		  AND COALESCE(s.relationship_type, '') != 'subagent'
		  AND m.content NOT LIKE '<task-notification>%'
		  AND m.content NOT LIKE '<system-reminder>%'
		  AND m.content NOT LIKE '<command-name>%'
		  AND m.content NOT LIKE '<local-command-stdout>%'`
	args := []any{previewChars}

	if project != "" {
		var projects []string
		for _, p := range strings.Split(project, ",") {
			if p = strings.TrimSpace(p); p != "" {
				projects = append(projects, p)
			}
		}
		if len(projects) == 1 {
			query += ` AND s.project = ?`
			args = append(args, projects[0])
		} else if len(projects) > 1 {
			query += ` AND s.project IN (` +
				strings.TrimSuffix(strings.Repeat("?,", len(projects)), ",") + `)`
			for _, p := range projects {
				args = append(args, p)
			}
		}
	}

	query += ` ORDER BY m.content_length DESC, m.id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.duck.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying longest user prompts: %w", err)
	}
	defer rows.Close()

	var out []db.LongestUserPrompt
	for rows.Next() {
		var p db.LongestUserPrompt
		if err := rows.Scan(
			&p.SessionID,
			&p.Project,
			&p.Ordinal,
			&p.ContentLength,
			&p.Preview,
			&p.Timestamp,
		); err != nil {
			return nil, fmt.Errorf("scanning longest user prompt: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating longest user prompts: %w", err)
	}
	return out, nil
}
