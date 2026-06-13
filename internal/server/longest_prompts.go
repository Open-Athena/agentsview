package server

import (
	"log"
	"net/http"
	"strconv"
)

// handleLongestPrompts serves GET /api/v1/messages/longest, the
// top-N longest user-role messages by content_length. `limit`
// defaults to 20 and is capped at 200.
func (s *Server) handleLongestPrompts(
	w http.ResponseWriter, r *http.Request,
) {
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeError(w, http.StatusBadRequest,
				"invalid limit parameter")
			return
		}
		limit = n
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}

	rows, err := s.db.GetLongestUserPrompts(r.Context(), limit)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		if handleReadOnly(w, err) {
			return
		}
		log.Printf("longest prompts error: %v", err)
		writeError(w, http.StatusInternalServerError,
			"internal server error")
		return
	}

	writeJSON(w, http.StatusOK, rows)
}
