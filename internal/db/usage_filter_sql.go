package db

import "strings"

// BuildUsageSourceFilter renders model filters before session predicates.
func BuildUsageSourceFilter(f UsageFilter, b *QueryBuilder, modelCol string) []string {
	var preds []string
	for _, item := range []struct {
		raw     string
		include bool
	}{{f.Model, true}, {f.ExcludeModel, false}} {
		if pred := b.ValuesPredicate(modelCol, CSVFilterValues(item.raw), item.include); pred != "" {
			preds = append(preds, pred)
		}
	}
	return preds
}

// BuildUsageSessionFilter preserves label arrays and the session-stage argument order.
func BuildUsageSessionFilter(f UsageFilter, b *QueryBuilder, sessionID string) []string {
	return BuildUsageSessionFilterWithProject(f, b, sessionID, "s.project")
}

// UsageRollupProjectExpr renders the project a usage row bills to for the
// session aliased sessionAlias: a subagent's parent's project when the parent
// has one, otherwise the session's own. Every backend groups and filters
// usage by this attribution, so spend lands where the work was requested.
// SQLite, PostgreSQL, and DuckDB evaluate the correlated primary-key lookup
// below; ClickHouse and hosted PostgreSQL supply their own form through
// WithUsageRollupProject. Every lookup reads the tables visible to the query,
// so a hosted tenant's schema and row security bound it like the outer read.
func UsageRollupProjectExpr(d QueryDialect, sessionAlias string) string {
	a := sessionAlias
	if d.usageRollupProject != nil {
		return d.usageRollupProject(a)
	}
	return `COALESCE(
	NULLIF((SELECT ps.project FROM sessions ps
	        WHERE ps.id = ` + a + `.parent_session_id
	          AND ` + a + `.relationship_type = 'subagent'), ''),
	` + a + `.project
)`
}

// BuildUsageSessionFilterWithProject is BuildUsageSessionFilter with the
// expression project include, exclude, and branch filters match against. A
// backend that attributes usage to a project other than s.project passes the
// same expression it groups by, so a project filter selects exactly the usage
// the breakdown shows under that project.
func BuildUsageSessionFilterWithProject(f UsageFilter, b *QueryBuilder, sessionID, projectCol string) []string {
	var preds []string
	for _, item := range []struct {
		col    string
		values []string
	}{{"s.agent", CSVFilterValues(f.Agent)}, {projectCol, f.ProjectFilterLabels()}} {
		if pred := b.ValuesPredicate(item.col, item.values, true); pred != "" {
			preds = append(preds, pred)
		}
	}
	if values := CSVFilterValues(f.Machine); len(values) > 0 {
		preds = append(preds, b.MachinePredicate("s.id", "s.machine", values))
	}
	if f.GitBranch != "" {
		preds = append(preds, BranchPairPredicate(projectCol, "s.git_branch", f.GitBranch, func(v string) string { return b.Add(v) }))
	}
	for _, item := range []struct {
		col    string
		values []string
	}{{projectCol, f.ExcludedProjectFilterLabels()}, {"s.agent", CSVFilterValues(f.ExcludeAgent)}} {
		if pred := b.ValuesPredicate(item.col, item.values, false); pred != "" {
			preds = append(preds, pred)
		}
	}
	if sessionID != "" {
		preds = append(preds, "s.id = "+b.Add(sessionID))
	}
	if f.MinUserMessages > 0 {
		preds = append(preds, "s.user_message_count >= "+b.Add(f.MinUserMessages))
	}
	scope := NormalizeAutomatedScope(f.AutomatedScope, f.ExcludeAutomated)
	falseLiteral := b.dialect.falseLiteral
	automated := "COALESCE(s.is_automated, " + falseLiteral + ")"
	if f.ExcludeOneShot {
		pred := "s.user_message_count > 1"
		if scope != "human" {
			pred = "(" + pred + " OR " + automated + " = " + b.dialect.trueLiteral + ")"
		}
		preds = append(preds, pred)
	}
	if pred := b.dialect.AutomatedScopePredicate(scope, automated); pred != "" {
		preds = append(preds, pred)
	}
	if f.ActiveSince != "" {
		preds = append(preds, b.activityExpr("s.")+" >= "+b.dialect.activityParam(b.Add(f.ActiveSince)))
	}
	if pred := b.reportTerminationPredicate(f.Termination, "s."); pred != "" {
		preds = append(preds, pred)
	}
	return preds
}

// AppendUsagePredicates keeps the existing backend query indentation.
func AppendUsagePredicates(where string, preds []string, indent string) string {
	if len(preds) == 0 {
		return where
	}
	separator := "\n" + indent + "AND "
	return where + separator + strings.Join(preds, separator)
}
