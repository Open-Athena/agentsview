package server

import (
	"context"
	"net/http"

	"go.kenn.io/agentsview/internal/db"
)

// registerSessionPrivateRoute wires the private-toggle mutation. It lives in
// its own file (rather than in huma_routes_sessions.go) because the private
// flag is a fork-local addition: a private session stays in aggregate
// analytics but its transcript is elided from published snapshot exports (see
// specs/per-session-redaction.md). Local/admin only; the deployed snapshot
// Worker exposes no such mutation.
func (s *Server) registerSessionPrivateRoute(group routeGroup) {
	s.patch(group, "/sessions/{id}/private", "Set session private", s.humaSetSessionPrivate)
}

type setPrivateInput struct {
	ID   string `path:"id" required:"true" doc:"Session ID"`
	Body struct {
		Private bool `json:"private"`
	}
}

func (s *Server) humaSetSessionPrivate(
	ctx context.Context,
	in *setPrivateInput,
) (*jsonOutput[*db.Session], error) {
	session, err := s.db.GetSession(ctx, in.ID)
	if err != nil {
		return nil, internalError("set-private lookup", err)
	}
	if session == nil {
		return nil, apiError(http.StatusNotFound, "session not found")
	}
	if err := s.db.SetSessionPrivate(in.ID, in.Body.Private); err != nil {
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("set session private", err)
	}
	updated, err := s.db.GetSession(ctx, in.ID)
	if err != nil {
		return nil, internalError("set-private readback", err)
	}
	if updated == nil {
		return nil, apiError(http.StatusNotFound, "session not found")
	}
	return &jsonOutput[*db.Session]{Body: updated}, nil
}
