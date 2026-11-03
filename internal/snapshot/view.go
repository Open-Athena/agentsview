package snapshot

import (
	"encoding/json/v2"
	"fmt"
	"os"
)

// ViewConfig is a declarative description of a published view (see
// specs/publishable-views.md). It is loaded from a JSON file and compiled
// into export Options. Fields beyond the exposure tiers (theme, access) are
// added in later phases; unknown fields are tolerated.
type ViewConfig struct {
	// Name identifies the view (used for the D1 database / URL path).
	Name string `json:"name"`
	// Title is a human-readable heading shown in the published UI.
	Title string `json:"title,omitempty"`
	// Full projects get both data and analytics (drill-down works).
	Full []string `json:"full,omitempty"`
	// Stats projects contribute to analytics aggregates only.
	Stats []string `json:"stats,omitempty"`
	// AllStats treats every non-full project as stats tier (public view).
	AllStats bool `json:"allStats,omitzero"`
	// Access is "public" (default) or "token" (gated: a grant token is
	// required to read anything beyond the view's metadata).
	Access string `json:"access,omitempty"`
	// Blurbs maps a project name to a Markdown summary, surfaced in the UI
	// in lieu of (redacted) session content for stats-only projects.
	Blurbs map[string]string `json:"blurbs,omitempty"`
	// Orgs groups projects by GitHub org / tag for color-coding: each org has
	// a base color and its projects render as shades of it.
	Orgs []OrgConfig `json:"orgs,omitempty"`
}

// OrgConfig colors a group of projects by a shared base color.
type OrgConfig struct {
	Name     string   `json:"name"`
	Color    string   `json:"color"` // named (e.g. "orange") or "hN" hue
	Projects []string `json:"projects"`
}

// LoadViewConfig reads and validates a view config JSON file.
func LoadViewConfig(path string) (*ViewConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("view config: %w", err)
	}
	var v ViewConfig
	if err := json.Unmarshal(b, &v, json.RejectUnknownMembers(true), json.MatchCaseInsensitiveNames(true)); err != nil {
		return nil, fmt.Errorf("view config %s: %w", path, err)
	}
	if v.Name == "" {
		return nil, fmt.Errorf("view config %s: name is required", path)
	}
	if v.AllStats && len(v.Stats) > 0 {
		return nil, fmt.Errorf(
			"view config %s: allStats and stats are mutually exclusive", path)
	}
	switch v.Access {
	case "", "public", "token":
	default:
		return nil, fmt.Errorf(
			"view config %s: access must be \"public\" or \"token\"", path)
	}
	return &v, nil
}

// Options compiles the view config into export Options, stamping the given
// generation timestamp.
func (v *ViewConfig) Options(generatedAt string) Options {
	return Options{
		FullProjects:  v.Full,
		StatsProjects: v.Stats,
		AllStats:      v.AllStats,
		Gated:         v.Access == "token",
		Title:         v.Title,
		Blurbs:        v.Blurbs,
		Orgs:          v.Orgs,
		GeneratedAt:   generatedAt,
	}
}
