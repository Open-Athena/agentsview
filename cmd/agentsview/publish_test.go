package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/agentsview/internal/publish"
)

func TestWritePublishReport(t *testing.T) {
	ords := make([]int, 12)
	for i := range ords {
		ords[i] = i * 3
	}
	rep := &publish.Report{
		Projects: []string{"alpha"}, SessionCount: 3, MessageCount: 40,
		Machines: []string{"laptop"},
		Env:      publish.EnvReport{Dirs: 2, Direnv: 1, FileOnly: 0, Missing: 1, Values: 4},
		Sessions: []publish.SessionReport{
			{ID: "s1", Project: "alpha", Title: "deploy the worker", Findings: []publish.Group{
				{Kind: "env:API_TOKEN", Action: publish.ActionRedact, Count: 2, Ordinals: []int{4, 9}},
				{Kind: "aws-access-key", Action: publish.ActionRedact, Count: 12, Ordinals: ords},
				{Kind: "deny", Action: publish.ActionFlag, Count: 1, Ordinals: []int{2}},
			}},
			{ID: "s2", Project: "alpha", Title: "clean"},
			{ID: "s3", Project: "alpha", Private: true},
		},
		Aggregates: []publish.Group{
			{Kind: "env:API_TOKEN", Action: publish.ActionRedact, Count: 1, Routes: []string{"messages/longest"}},
		},
		Totals: []publish.Group{
			{Kind: "aws-access-key", Action: publish.ActionRedact, Count: 12},
			{Kind: "env:API_TOKEN", Action: publish.ActionRedact, Count: 3},
			{Kind: "deny", Action: publish.ActionFlag, Count: 1},
		},
		Ignored: map[string]int{"high-entropy-assignment": 7},
	}
	var b strings.Builder
	writePublishReport(&b, rep)
	assert.Equal(t, []string{
		"Previewed 3 sessions (40 messages) from alpha on laptop",
		"Env: 2 dirs (1 via direnv, 0 from files, 1 missing) -> 4 secret values",
		"Deny-list: 0 terms",
		"",
		`s1  alpha  "deploy the worker"`,
		"  redact  env:API_TOKEN                     x2     @ 4, 9",
		"  redact  aws-access-key                    x12    @ 0, 3, 6, 9, 12, 15, 18, 21, 24, 27, ... +2",
		"  flag    deny                              x1     @ 2",
		"",
		"analytics panels (repeat message text)",
		"  redact  env:API_TOKEN                     x1     in messages/longest",
		"",
		"2 sessions with no findings",
		"",
		"Totals: 15 redactions, 1 flags (ignored 7 high-entropy candidates without a secret-looking name)",
		"Nothing was written. Re-run with --out <dir> to write the redacted snapshot.",
	}, strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n"))
}
