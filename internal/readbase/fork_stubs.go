package readbase

import (
	"context"

	"go.kenn.io/agentsview/internal/db"
)

// SetSessionPrivate is a local redaction control; mirrors are read-only.
func (MirrorStubs) SetSessionPrivate(_ context.Context, _ string, _ bool) error {
	return db.ErrReadOnly
}
