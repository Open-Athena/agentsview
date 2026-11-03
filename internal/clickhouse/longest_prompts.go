package clickhouse

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

// GetLongestUserPrompts returns the top-N longest user-role messages by
// content length, mirroring the SQLite implementation: system messages,
// subagent sessions and injected scaffolding blocks are excluded.
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
			s.project,
			toInt64(m.ordinal),
			toInt64(m.content_length),
			substringUTF8(m.content, 1, ?) AS preview,
			m.timestamp
		FROM messages m
		INNER JOIN sessions s ON s.id = m.session_id
		WHERE m.role = 'user'
		  AND m.is_system = false
		  AND s.deleted_at IS NULL
		  AND s.relationship_type != 'subagent'
		  AND NOT startsWith(m.content, '<task-notification>')
		  AND NOT startsWith(m.content, '<system-reminder>')
		  AND NOT startsWith(m.content, '<command-name>')
		  AND NOT startsWith(m.content, '<local-command-stdout>')`
	args := []any{previewChars}

	var projects []string
	for p := range strings.SplitSeq(project, ",") {
		if p = strings.TrimSpace(p); p != "" {
			projects = append(projects, p)
		}
	}
	if len(projects) > 0 {
		query += ` AND s.project IN (` +
			strings.TrimSuffix(strings.Repeat("?,", len(projects)), ",") + `)`
		for _, p := range projects {
			args = append(args, p)
		}
	}

	query += ` ORDER BY m.content_length DESC, m.session_id ASC, m.ordinal ASC LIMIT ?`
	args = append(args, limit)

	rows, err := s.queryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying clickhouse longest user prompts: %w", err)
	}
	defer rows.Close()

	var out []db.LongestUserPrompt
	for rows.Next() {
		var p db.LongestUserPrompt
		var ts any
		var ordinal, length int64
		if err := rows.Scan(
			&p.SessionID, &p.Project, &ordinal, &length, &p.Preview, &ts,
		); err != nil {
			return nil, fmt.Errorf("scanning clickhouse longest user prompt: %w", err)
		}
		p.Ordinal = int(ordinal)
		p.ContentLength = int(length)
		p.Timestamp = formatDBTime(ts)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating clickhouse longest user prompts: %w", err)
	}
	return out, nil
}
