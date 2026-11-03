package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetLongestUserPrompts(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s-b", "alpha")
	insertSession(t, d, "s-a", "beta")
	insertSession(t, d, "s-sub", "alpha", func(s *Session) {
		s.RelationshipType = "subagent"
		s.ParentSessionID = new("s-b")
	})
	insertMessages(t, d,
		userMsg("s-b", 0, "tie-tie-tie"),
		userMsg("s-b", 1, "longest prompt of all"),
		asstMsg("s-b", 2, "assistant text is never a prompt, however long it gets"),
		userMsg("s-a", 0, "tie-tie-tie"),
		userMsg("s-a", 1, "<system-reminder>injected scaffolding that is long</system-reminder>"),
		userMsg("s-sub", 0, "subagent prompt that is the longest of everything"),
	)

	type row struct {
		Session string
		Project string
		Ordinal int
		Length  int
	}
	rows := func(ps []LongestUserPrompt) []row {
		out := make([]row, len(ps))
		for i, p := range ps {
			out[i] = row{p.SessionID, p.Project, p.Ordinal, p.ContentLength}
		}
		return out
	}

	tests := []struct {
		name    string
		limit   int
		project string
		want    []row
	}{
		{"all, ties by session then ordinal", 10, "", []row{
			{"s-b", "alpha", 1, 21},
			{"s-a", "beta", 0, 11},
			{"s-b", "alpha", 0, 11},
		}},
		{"limit", 2, "", []row{
			{"s-b", "alpha", 1, 21},
			{"s-a", "beta", 0, 11},
		}},
		{"project filter", 10, "beta", []row{
			{"s-a", "beta", 0, 11},
		}},
		{"project list", 10, "beta, alpha", []row{
			{"s-b", "alpha", 1, 21},
			{"s-a", "beta", 0, 11},
			{"s-b", "alpha", 0, 11},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := d.GetLongestUserPrompts(t.Context(), tt.limit, tt.project)
			require.NoError(t, err)
			assert.Equal(t, tt.want, rows(got))
		})
	}
}
