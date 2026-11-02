package server

import (
	"context"
	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/agentsview/internal/db"
)

// registerMessagesRoutes wires fork-local message-level endpoints. Currently
// just "longest user prompts": the top-N longest user-role messages by
// content length, optionally scoped to a comma-separated project list.
func (s *Server) registerMessagesRoutes() {
	group := huma.NewGroup(s.api, "/api/v1/messages")
	configureRouteGroup(group, "Messages")
	s.get(group, "/longest", "Longest user prompts", s.humaLongestPrompts)
}

type longestPromptsInput struct {
	Limit   int    `query:"limit" minimum:"0" maximum:"200" default:"20" doc:"Maximum number of prompts (capped at 200)"`
	Project string `query:"project" doc:"Comma-separated project names to restrict to"`
}

func (s *Server) humaLongestPrompts(
	ctx context.Context,
	in *longestPromptsInput,
) (*jsonOutput[[]db.LongestUserPrompt], error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := s.db.GetLongestUserPrompts(ctx, limit, in.Project)
	if err != nil {
		if handled := handleHumaContextError(err); handled != nil {
			return nil, handled
		}
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("longest prompts error", err)
	}
	return &jsonOutput[[]db.LongestUserPrompt]{Body: rows}, nil
}
