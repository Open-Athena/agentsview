package postgres

import (
	"context"

	"go.kenn.io/agentsview/internal/db"
)

// GetLongestUserPrompts returns no rows for hosted stores: the physical query
// is not tenant-scoped, so delegating it would read across tenants.
func (h *HostedStore) GetLongestUserPrompts(
	_ context.Context, _ int, _ string,
) ([]db.LongestUserPrompt, error) {
	return []db.LongestUserPrompt{}, nil
}

// SetSessionPrivate is a local redaction control; hosted stores are
// read-only for it.
func (h *HostedStore) SetSessionPrivate(_ context.Context, _ string, _ bool) error {
	return db.ErrReadOnly
}
