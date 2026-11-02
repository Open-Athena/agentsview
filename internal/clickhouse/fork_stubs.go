package clickhouse

import (
	"context"

	"go.kenn.io/agentsview/internal/db"
)

// GetLongestUserPrompts is not implemented for the ClickHouse replica.
func (s *Store) GetLongestUserPrompts(
	_ context.Context, _ int, _ string,
) ([]db.LongestUserPrompt, error) {
	return []db.LongestUserPrompt{}, nil
}

// SetSessionPrivate is a local redaction control; the replica is read-only.
func (s *Store) SetSessionPrivate(_ context.Context, _ string, _ bool) error {
	return db.ErrReadOnly
}
