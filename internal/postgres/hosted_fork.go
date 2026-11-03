package postgres

import (
	"context"
	"slices"

	"go.kenn.io/agentsview/internal/db"
)

// GetLongestUserPrompts reads the tenant's own schema (row-level security on
// tenant_id) like the other analytics forwards, then maps physical session IDs
// to their public aliases.
func (h *HostedStore) GetLongestUserPrompts(
	ctx context.Context, limit int, project string,
) ([]db.LongestUserPrompt, error) {
	return hostedMapped(ctx, h, func(h *HostedStore) ([]db.LongestUserPrompt, error) {
		return h.physical.GetLongestUserPrompts(ctx, limit, project)
	}, func(p *[]db.LongestUserPrompt, r *hostedRefs) {
		*p = slices.Clone(*p)
		for i := range *p {
			r.add(&(*p)[i].SessionID)
		}
	})
}

// SetSessionPrivate is a local redaction control; hosted stores are
// read-only for it.
func (h *HostedStore) SetSessionPrivate(_ context.Context, _ string, _ bool) error {
	return db.ErrReadOnly
}
