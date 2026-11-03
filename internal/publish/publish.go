package publish

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"go.kenn.io/agentsview/internal/snapshot"
	"go.kenn.io/agentsview/internal/stringutil"
)

// Visibilities a published session can carry. Draft is visible only to its
// contributor until they promote it.
var Visibilities = []string{"draft", "oa", "link"}

// Options configures a publish.
type Options struct {
	// ArchivePath is the SQLite archive, opened read-only.
	ArchivePath string
	// Projects select whole projects (plus subagent children, as in
	// export-snapshot).
	Projects []string
	// Sessions, when set, narrow the selection to these session IDs and
	// their subagent children.
	Sessions []string
	// Since, when set (YYYY-MM-DD or RFC3339), keeps sessions that ended (or,
	// lacking an end, started) on or after it.
	Since string
	// OutDir receives the redacted snapshot. Empty means preview: export to a
	// temporary directory, scan, report, and remove it.
	OutDir string
	// Contributor and Visibility are stamped into manifest.json (OutDir only).
	Contributor string
	Visibility  string
	// Env configures the environment-value detector.
	Env EnvOptions
	// DenyList is a path to deny-list terms (flag only). Missing is empty.
	DenyList string
	// GeneratedAt is stamped into the manifest (RFC3339).
	GeneratedAt string
	// Progress, when set, receives one line per completed stage.
	Progress func(format string, args ...any)
}

// Group is a count of one finding kind and action at a set of places.
type Group struct {
	Kind     string   `json:"kind"`
	Action   Action   `json:"action"`
	Count    int      `json:"count"`
	Ordinals []int    `json:"ordinals,omitempty"` // message ordinals (sessions only)
	Routes   []string `json:"routes,omitempty"`   // analytics routes (aggregates only)
}

// SessionReport lists one session's findings.
type SessionReport struct {
	ID       string  `json:"id"`
	Project  string  `json:"project"`
	Title    string  `json:"title"`
	Private  bool    `json:"private,omitzero"`
	Findings []Group `json:"findings,omitempty"`
}

// Report is the outcome of a preview or publish.
type Report struct {
	Projects     []string        `json:"projects"`
	SessionCount int             `json:"sessionCount"`
	MessageCount int             `json:"messageCount"`
	Machines     []string        `json:"machines"`
	Env          EnvReport       `json:"env"`
	DenyTerms    int             `json:"denyTerms"`
	Sessions     []SessionReport `json:"sessions"`
	// Aggregates are findings in precomputed analytics bodies (e.g. the
	// longest-prompts panel), which repeat message text.
	Aggregates []Group        `json:"aggregates,omitempty"`
	Totals     []Group        `json:"totals"`
	Ignored    map[string]int `json:"ignored,omitempty"`
	Out        string         `json:"out,omitempty"`
}

// Redactions totals redact-action findings.
func (r *Report) Redactions() int {
	n := 0
	for _, g := range r.Totals {
		if g.Action == ActionRedact {
			n += g.Count
		}
	}
	return n
}

// Flags totals flag-action findings.
func (r *Report) Flags() int {
	n := 0
	for _, g := range r.Totals {
		if g.Action == ActionFlag {
			n += g.Count
		}
	}
	return n
}

type sessionMeta struct {
	id, project, title, cwd, machine string
	private                          bool
}

// Run selects sessions, exports them, scans the export, and either reports
// (preview) or writes the redacted snapshot to opts.OutDir.
func Run(ctx context.Context, opts Options) (*Report, error) {
	if len(opts.Projects) == 0 {
		return nil, errors.New("publish: at least one project is required")
	}
	if opts.Visibility == "" {
		opts.Visibility = "draft"
	}
	if !slices.Contains(Visibilities, opts.Visibility) {
		return nil, fmt.Errorf("publish: visibility %q is not one of %s",
			opts.Visibility, strings.Join(Visibilities, ", "))
	}

	progress := opts.Progress
	if progress == nil {
		progress = func(string, ...any) {}
	}
	start := time.Now()
	stage := func(format string, args ...any) {
		progress("%s (%s)", fmt.Sprintf(format, args...), time.Since(start).Round(100*time.Millisecond))
		start = time.Now()
	}

	metas, err := selectSessions(ctx, opts)
	if err != nil {
		return nil, err
	}
	if len(metas) == 0 {
		return nil, errors.New("publish: no sessions match")
	}
	ids := make([]string, len(metas))
	var dirs, machines []string
	for i, m := range metas {
		ids[i] = m.id
		if !m.private {
			dirs = append(dirs, m.cwd)
		}
		machines = append(machines, m.machine)
	}
	slices.Sort(dirs)
	dirs = slices.Compact(dirs)
	slices.Sort(machines)
	machines = slices.Compact(machines)

	if opts.OutDir != "" {
		if entries, err := os.ReadDir(opts.OutDir); err == nil && len(entries) > 0 {
			return nil, fmt.Errorf("publish: %s is not empty", opts.OutDir)
		}
	}
	work, err := os.MkdirTemp("", "agentsview-publish-")
	if err != nil {
		return nil, fmt.Errorf("publish: temp dir: %w", err)
	}
	defer os.RemoveAll(work)

	// Export from a migrated copy of just these sessions, so an archive
	// written by an older (or newer) agentsview still exports correctly and
	// the archive itself is only read.
	scratch := filepath.Join(work, "sessions.db")
	if err := sliceArchive(ctx, opts.ArchivePath, scratch, ids); err != nil {
		return nil, err
	}
	stage("copied %d sessions to a scratch database", len(ids))
	outDir := opts.OutDir
	if outDir == "" {
		outDir = filepath.Join(work, "snapshot")
	}

	m, err := snapshot.Export(ctx, scratch, outDir, snapshot.Options{
		FullProjects: opts.Projects,
		OnlySessions: ids,
		GeneratedAt:  opts.GeneratedAt,
	})
	if err != nil {
		return nil, err
	}
	stage("exported %d messages", m.MessageCount)

	values, envRep := CollectEnv(ctx, dirs, opts.Env)
	stage("collected %d secret values from %d session directories", envRep.Values, envRep.Dirs)
	var deny []string
	if opts.DenyList != "" {
		if deny, err = LoadDenyList(opts.DenyList); err != nil {
			return nil, fmt.Errorf("publish: deny list: %w", err)
		}
	}
	red := NewRedactor(values, deny)
	write := opts.OutDir != ""
	if err := red.ProcessDir(outDir, write); err != nil {
		return nil, err
	}
	stage("scanned the export")

	rep := buildReport(red, metas, opts.Projects)
	rep.SessionCount = m.SessionCount
	rep.MessageCount = m.MessageCount
	rep.Machines = machines
	rep.Env = envRep
	rep.DenyTerms = len(deny)

	if write {
		if err := red.VerifyDir(outDir); err != nil {
			return nil, err
		}
		if err := writePublishManifest(outDir, opts, metas, rep); err != nil {
			return nil, err
		}
		rep.Out = outDir
	}
	return rep, nil
}

// selectSessions resolves the sessions a publish covers: the projects'
// sessions plus subagent children (matching export-snapshot), narrowed by
// Sessions and Since.
func selectSessions(ctx context.Context, opts Options) ([]sessionMeta, error) {
	src, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?mode=ro&_busy_timeout=5000", opts.ArchivePath))
	if err != nil {
		return nil, fmt.Errorf("publish: open archive: %w", err)
	}
	defer src.Close()

	ph := strings.TrimSuffix(strings.Repeat("?,", len(opts.Projects)), ",")
	var args []any
	for range 2 {
		for _, p := range opts.Projects {
			args = append(args, p)
		}
	}
	query := fmt.Sprintf(`
		SELECT s.id, s.project, s.cwd, s.machine, s.private,
			COALESCE(NULLIF(s.display_name, ''), NULLIF(s.session_name, ''), s.first_message, ''),
			COALESCE(s.ended_at, s.started_at, ''),
			COALESCE(s.parent_session_id, '')
		FROM sessions s
		WHERE s.deleted_at IS NULL AND (s.project IN (%s) OR (
			s.relationship_type = 'subagent' AND s.parent_session_id IN (
				SELECT id FROM sessions WHERE project IN (%s))))
		ORDER BY s.id`, ph, ph)
	rows, err := src.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("publish: select sessions: %w", err)
	}
	defer rows.Close()

	wanted := map[string]bool{}
	for _, id := range opts.Sessions {
		wanted[id] = true
	}
	var out []sessionMeta
	for rows.Next() {
		var m sessionMeta
		var private int
		var lastAt, parent string
		if err := rows.Scan(&m.id, &m.project, &m.cwd, &m.machine, &private, &m.title, &lastAt, &parent); err != nil {
			return nil, fmt.Errorf("publish: scan session: %w", err)
		}
		m.private = private != 0
		if len(wanted) > 0 && !wanted[m.id] && !wanted[parent] {
			continue
		}
		if opts.Since != "" && lastAt < opts.Since {
			continue
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func buildReport(red *Redactor, metas []sessionMeta, projects []string) *Report {
	type key struct {
		kind   string
		action Action
	}
	perSession := map[string]map[key]*Group{}
	aggregates := map[key]*Group{}
	totals := map[key]*Group{}
	bump := func(m map[key]*Group, k key) *Group {
		g, ok := m[k]
		if !ok {
			g = &Group{Kind: k.kind, Action: k.action}
			m[k] = g
		}
		g.Count++
		return g
	}
	for _, h := range red.hits {
		k := key{h.kind, h.action}
		bump(totals, k)
		if h.loc.file == "analytics_cache.ndjson" || h.loc.sessionID == "" {
			g := bump(aggregates, k)
			if h.loc.route != "" && !slices.Contains(g.Routes, h.loc.route) {
				g.Routes = append(g.Routes, h.loc.route)
			}
			continue
		}
		if perSession[h.loc.sessionID] == nil {
			perSession[h.loc.sessionID] = map[key]*Group{}
		}
		g := bump(perSession[h.loc.sessionID], k)
		if h.loc.ordinal >= 0 && !slices.Contains(g.Ordinals, h.loc.ordinal) {
			g.Ordinals = append(g.Ordinals, h.loc.ordinal)
		}
	}
	rep := &Report{Projects: projects, Ignored: red.ignored}
	for _, m := range metas {
		sr := SessionReport{
			ID: m.id, Project: m.project, Private: m.private,
			Title: stringutil.TruncateRunes(red.Scrub(firstLine(m.title)), 80, "..."),
		}
		if m.private {
			sr.Title = ""
		}
		sr.Findings = sortedGroups(perSession[m.id])
		rep.Sessions = append(rep.Sessions, sr)
	}
	rep.Aggregates = sortedGroups(aggregates)
	rep.Totals = sortedGroups(totals)
	if len(rep.Ignored) == 0 {
		rep.Ignored = nil
	}
	return rep
}

func sortedGroups[K comparable](m map[K]*Group) []Group {
	out := make([]Group, 0, len(m))
	for _, g := range m {
		slices.Sort(g.Ordinals)
		slices.Sort(g.Routes)
		out = append(out, *g)
	}
	slices.SortFunc(out, func(a, b Group) int {
		if c := strings.Compare(string(a.Action), string(b.Action)); c != 0 {
			return -c // redact before flag
		}
		return strings.Compare(a.Kind, b.Kind)
	})
	if len(out) == 0 {
		return nil
	}
	return out
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// publishInfo is added to manifest.json under "publish".
type publishInfo struct {
	Contributor string            `json:"contributor,omitempty"`
	Machines    []string          `json:"machines"`
	Visibility  map[string]string `json:"visibility"`
	Redactions  []Group           `json:"redactions"`
	Flags       []Group           `json:"flags,omitempty"`
}

func writePublishManifest(dir string, opts Options, metas []sessionMeta, rep *Report) error {
	path := filepath.Join(dir, "manifest.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var manifest map[string]jsontext.Value
	if err := json.Unmarshal(b, &manifest); err != nil {
		return fmt.Errorf("publish: read manifest: %w", err)
	}
	info := publishInfo{
		Contributor: opts.Contributor,
		Machines:    rep.Machines,
		Visibility:  map[string]string{},
	}
	for _, m := range metas {
		info.Visibility[m.id] = opts.Visibility
	}
	for _, g := range rep.Totals {
		g.Ordinals, g.Routes = nil, nil
		if g.Action == ActionRedact {
			info.Redactions = append(info.Redactions, g)
		} else {
			info.Flags = append(info.Flags, g)
		}
	}
	if info.Redactions == nil {
		info.Redactions = []Group{}
	}
	raw, err := json.Marshal(info, json.Deterministic(true))
	if err != nil {
		return err
	}
	manifest["publish"] = raw
	out, err := json.Marshal(manifest, json.Deterministic(true), jsontext.WithIndent("  "))
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}
