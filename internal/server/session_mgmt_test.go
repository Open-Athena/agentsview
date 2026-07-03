package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestHandleSetSessionPrivate(t *testing.T) {
	srv := testServer(t, time.Second)
	require.NoError(t, srv.db.UpsertSession(db.Session{
		ID: "s1", Project: "p", Machine: "local", Agent: "claude", MessageCount: 1,
	}), "seed session")

	patchPrivate := func(id, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(
			http.MethodPatch, "/api/v1/sessions/"+id+"/private",
			strings.NewReader(body))
		srv.mux.ServeHTTP(w, r)
		return w
	}

	// Set private: 200 with the updated session reflecting private=true.
	w := patchPrivate("s1", `{"private":true}`)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var got db.Session
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got), "decode response")
	assert.True(t, got.Private, "response reports private")

	// Persisted: a fresh GetSession sees it.
	stored, err := srv.db.GetSession(context.Background(), "s1")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.True(t, stored.Private, "flag persisted")

	// Clear it again.
	w = patchPrivate("s1", `{"private":false}`)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got), "decode response")
	assert.False(t, got.Private, "response reports not-private")

	// Unknown session: 404 (looked up before any mutation).
	assert.Equal(t, http.StatusNotFound,
		patchPrivate("nope", `{"private":true}`).Code)

	// Malformed body: 400.
	assert.Equal(t, http.StatusBadRequest,
		patchPrivate("s1", `not json`).Code)
}
