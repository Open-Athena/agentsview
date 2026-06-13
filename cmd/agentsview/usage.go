package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/pricing"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/sync"
)

// quickSyncMargin pads the mtime cutoff backward from the
// last recorded sync start time to catch files modified
// during the prior sync. Smaller values are faster but risk
// missing recent writes; 10s is a safe default.
const quickSyncMargin = 10 * time.Second

// defaultUsageDays is the default lookback window for
// `agentsview usage daily` when neither --since nor --all is
// given. Matches ccusage's default and avoids scanning the
// full history when users usually want recent spend.
const defaultUsageDays = 30

// resolveDefaultSince returns the effective --since value,
// applying a 30-day lookback only when the caller gave no
// explicit range at all. If --until is set we leave --since
// empty so "everything up to --until" still works; otherwise
// a bare --until would produce From > To and empty results.
func resolveDefaultSince(
	since, until string, all bool, now time.Time, tz string,
) string {
	if since != "" || until != "" || all {
		return since
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.Local
	}
	return now.In(loc).
		AddDate(0, 0, -(defaultUsageDays - 1)).
		Format("2006-01-02")
}

type UsageDailyConfig struct {
	JSON      bool
	Since     string
	Until     string
	All       bool
	Agent     string
	Breakdown bool
	Offline   bool
	NoSync    bool
	Timezone  string
}

func runUsageDaily(cfg UsageDailyConfig) {
	database, appCfg := openUsageDB()
	defer database.Close()

	ensureFreshData(appCfg, database, cfg.NoSync)
	ensurePricing(database, cfg.Offline)

	tz := cfg.Timezone
	if tz == "" {
		tz = localTimezone()
	}

	effectiveSince := resolveDefaultSince(
		cfg.Since, cfg.Until, cfg.All, time.Now(), tz,
	)

	filter := db.UsageFilter{
		From:     effectiveSince,
		To:       cfg.Until,
		Agent:    cfg.Agent,
		Timezone: tz,
	}

	result, err := database.GetDailyUsage(
		context.Background(), filter,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if cfg.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(result); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	printDailyTable(result, cfg.Breakdown)
}

type UsageProjectsConfig struct {
	JSON     bool
	Since    string
	Until    string
	All      bool
	Agent    string
	Limit    int
	Offline  bool
	NoSync   bool
	Timezone string
}

type usageProjectTotal struct {
	Project             string  `json:"project"`
	InputTokens         int     `json:"inputTokens"`
	OutputTokens        int     `json:"outputTokens"`
	CacheCreationTokens int     `json:"cacheCreationTokens"`
	CacheReadTokens     int     `json:"cacheReadTokens"`
	Cost                float64 `json:"cost"`
}

type usageProjectsResult struct {
	From     string              `json:"from"`
	To       string              `json:"to"`
	Projects []usageProjectTotal `json:"projects"`
	Total    usageProjectTotal   `json:"total"`
}

func runUsageProjects(cfg UsageProjectsConfig) {
	database, appCfg := openUsageDB()
	defer database.Close()

	ensureFreshData(appCfg, database, cfg.NoSync)
	ensurePricing(database, cfg.Offline)

	tz := cfg.Timezone
	if tz == "" {
		tz = localTimezone()
	}

	effectiveSince := resolveDefaultSince(
		cfg.Since, cfg.Until, cfg.All, time.Now(), tz,
	)

	filter := db.UsageFilter{
		From:       effectiveSince,
		To:         cfg.Until,
		Agent:      cfg.Agent,
		Timezone:   tz,
		Breakdowns: true,
	}

	result, err := database.GetDailyUsage(
		context.Background(), filter,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	projects := foldProjectsFromDaily(result.Daily)

	if cfg.Limit > 0 && len(projects) > cfg.Limit {
		projects = projects[:cfg.Limit]
	}

	out := usageProjectsResult{
		From:     filter.From,
		To:       filter.To,
		Projects: projects,
		Total: usageProjectTotal{
			Project:             "TOTAL",
			InputTokens:         result.Totals.InputTokens,
			OutputTokens:        result.Totals.OutputTokens,
			CacheCreationTokens: result.Totals.CacheCreationTokens,
			CacheReadTokens:     result.Totals.CacheReadTokens,
			Cost:                result.Totals.TotalCost,
		},
	}

	if cfg.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	printProjectsTable(out)
}

// foldProjectsFromDaily aggregates per-day project breakdowns
// from GetDailyUsage into range-wide per-project totals,
// sorted by cost descending. Mirrors server.foldProjectTotals
// without depending on the server package.
func foldProjectsFromDaily(
	daily []db.DailyUsageEntry,
) []usageProjectTotal {
	m := make(map[string]*usageProjectTotal)
	for _, d := range daily {
		for _, pb := range d.ProjectBreakdowns {
			pt, ok := m[pb.Project]
			if !ok {
				pt = &usageProjectTotal{Project: pb.Project}
				m[pb.Project] = pt
			}
			pt.InputTokens += pb.InputTokens
			pt.OutputTokens += pb.OutputTokens
			pt.CacheCreationTokens += pb.CacheCreationTokens
			pt.CacheReadTokens += pb.CacheReadTokens
			pt.Cost += pb.Cost
		}
	}
	out := make([]usageProjectTotal, 0, len(m))
	for _, v := range m {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cost != out[j].Cost {
			return out[i].Cost > out[j].Cost
		}
		return out[i].Project < out[j].Project
	})
	return out
}

func printProjectsTable(r usageProjectsResult) {
	w := tabwriter.NewWriter(
		os.Stdout, 0, 4, 2, ' ', 0,
	)
	fmt.Fprintln(w,
		"PROJECT\tINPUT\tOUTPUT\tCACHE_CR\tCACHE_RD\tCOST")
	fmt.Fprintln(w,
		"-------\t-----\t------\t--------\t--------\t----")
	for _, p := range r.Projects {
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%s\n",
			p.Project,
			p.InputTokens,
			p.OutputTokens,
			p.CacheCreationTokens,
			p.CacheReadTokens,
			fmtCost(p.Cost),
		)
	}
	fmt.Fprintln(w,
		"-------\t-----\t------\t--------\t--------\t----")
	fmt.Fprintf(w, "TOTAL\t%d\t%d\t%d\t%d\t%s\n",
		r.Total.InputTokens,
		r.Total.OutputTokens,
		r.Total.CacheCreationTokens,
		r.Total.CacheReadTokens,
		fmtCost(r.Total.Cost),
	)
	w.Flush()
}

type UsageLongestPromptsConfig struct {
	JSON   bool
	Limit  int
	NoSync bool
}

func runUsageLongestPrompts(cfg UsageLongestPromptsConfig) {
	database, appCfg := openUsageDB()
	defer database.Close()

	ensureFreshData(appCfg, database, cfg.NoSync)

	limit := cfg.Limit
	if limit <= 0 {
		limit = 20
	}

	rows, err := database.GetLongestUserPrompts(
		context.Background(), limit,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if cfg.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w,
		"CHARS\tPROJECT\tSESSION\tORDINAL\tPREVIEW")
	fmt.Fprintln(w,
		"-----\t-------\t-------\t-------\t-------")
	for _, p := range rows {
		preview := strings.ReplaceAll(p.Preview, "\n", " ")
		if len(preview) > 100 {
			preview = preview[:97] + "..."
		}
		short := p.SessionID
		if len(short) > 8 {
			short = short[:8]
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%s\n",
			p.ContentLength, p.Project, short, p.Ordinal, preview)
	}
	w.Flush()
}

type UsageStatuslineConfig struct {
	Agent   string
	Offline bool
	NoSync  bool
}

func runUsageStatusline(cfg UsageStatuslineConfig) {
	database, appCfg := openUsageDB()
	defer database.Close()

	ensureFreshData(appCfg, database, cfg.NoSync)
	ensurePricing(database, cfg.Offline)

	today := time.Now().Format("2006-01-02")
	filter := db.UsageFilter{
		From:     today,
		To:       today,
		Agent:    cfg.Agent,
		Timezone: localTimezone(),
	}

	result, err := database.GetDailyUsage(
		context.Background(), filter,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if cfg.Agent != "" {
		fmt.Printf("%s today (%s)\n",
			fmtCost(result.Totals.TotalCost), cfg.Agent)
	} else {
		fmt.Printf("%s today\n",
			fmtCost(result.Totals.TotalCost))
	}
}

func applyCustomPricing(database *db.DB, cfg config.Config) {
	if len(cfg.CustomModelPricing) == 0 {
		return
	}
	database.SetCustomPricing(cfg.CustomModelPricing)
}

func openUsageDB() (*db.DB, config.Config) {
	cfg, err := config.LoadMinimal()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	database, err := openDB(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"error opening database: %v\n", err)
		os.Exit(1)
	}
	return database, cfg
}

// ensureFreshData makes sure the database reflects recent
// session file changes before serving a usage query.
//
// Decision tree:
//  1. If the stored data version is stale (parser changes on
//     upgrade), run a full resync.
//  2. If a server process is active (via state file), trust
//     its file watcher and skip on-demand sync. This avoids
//     duplicate work and write contention.
//  3. Otherwise, run a quick incremental sync scoped to files
//     modified since the last recorded sync start time, with
//     a small safety margin.
//
// Callers that need stale data (e.g. offline benchmarks) can
// bypass via skip=true.
func ensureFreshData(
	appCfg config.Config, database *db.DB, skip bool,
) {
	if skip {
		return
	}

	ctx := context.Background()

	// Silence engine worker log.Printf lines (e.g. "db:
	// InsertMessages (N msgs)") for both branches so --json and
	// statusline output stay clean. Progress goes to stderr
	// below to stay out of stdout-bound payloads.
	origLog := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(origLog)

	if database.NeedsResync() {
		engine := sync.NewEngine(database, sync.EngineConfig{
			AgentDirs: appCfg.AgentDirs,
			Machine:   "local",
		})
		fmt.Fprintln(os.Stderr,
			"Data version changed, running full resync...")
		t := time.Now()
		stats := engine.ResyncAll(ctx, printSyncProgressStderr)
		printSyncSummaryStderr(stats, t)
		return
	}

	// Skip on-demand sync only when a writable local daemon is
	// already keeping the SQLite archive fresh. pg serve daemons
	// (read-only) do not sync the local DB, so we still want to
	// run our own sync when only one of those is present.
	if server.IsLocalServerActive(appCfg.DataDir) {
		return
	}

	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: appCfg.AgentDirs,
		Machine:   "local",
	})

	since := engine.LastSyncStartedAt()
	if !since.IsZero() {
		since = since.Add(-quickSyncMargin)
	}

	engine.SyncAllSince(ctx, since, func(sync.Progress) {})
}

// printSyncProgressStderr mirrors printSyncProgress but writes
// to stderr so it does not pollute stdout-bound JSON or
// statusline output from the usage commands.
func printSyncProgressStderr(p sync.Progress) {
	if p.SessionsTotal > 0 {
		fmt.Fprintf(os.Stderr,
			"\r  %d/%d sessions (%.0f%%) · %d messages",
			p.SessionsDone, p.SessionsTotal,
			p.Percent(), p.MessagesIndexed,
		)
	}
}

// printSyncSummaryStderr mirrors printSyncSummary but writes to
// stderr, for the same reason as printSyncProgressStderr.
func printSyncSummaryStderr(stats sync.SyncStats, t time.Time) {
	summary := fmt.Sprintf(
		"\nSync complete: %d sessions synced",
		stats.Synced,
	)
	if stats.OrphanedCopied > 0 {
		summary += fmt.Sprintf(
			", %d archived sessions preserved",
			stats.OrphanedCopied,
		)
	}
	if stats.Failed > 0 {
		summary += fmt.Sprintf(", %d failed", stats.Failed)
	}
	summary += fmt.Sprintf(
		" in %s\n", time.Since(t).Round(time.Millisecond),
	)
	fmt.Fprint(os.Stderr, summary)
	for _, w := range stats.Warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
}

// seedPricing ensures fallback rates are present in
// model_pricing, then kicks off a background LiteLLM refresh.
//
// Fallback rates are only upserted when the stored seed
// version differs from pricing.FallbackVersion (or is
// absent). This avoids overwriting live LiteLLM rates on
// every restart while still propagating corrected fallback
// rates when the binary is upgraded.
func seedPricing(database *db.DB) {
	const metaKey = "_fallback_version"
	stored, err := database.GetPricingMeta(metaKey)
	if err != nil {
		log.Printf("pricing seed: %v", err)
	}
	if stored != pricing.FallbackVersion {
		if err := upsertPricing(
			database, pricing.FallbackPricing(),
		); err != nil {
			log.Printf("pricing seed: %v", err)
		} else if err := database.SetPricingMeta(
			metaKey, pricing.FallbackVersion,
		); err != nil {
			log.Printf("pricing seed: %v", err)
		}
	}
	go refreshPricingFromLiteLLM(database)
}

// refreshPricingFromLiteLLM fetches the upstream LiteLLM
// catalog and upserts it over whatever is in the table. Called
// from a goroutine after the synchronous fallback seed so a
// slow or failing fetch never blocks server startup.
func refreshPricingFromLiteLLM(database *db.DB) {
	prices, err := pricing.FetchLiteLLMPricing()
	if err != nil {
		log.Printf(
			"pricing refresh: litellm fetch failed: %v", err,
		)
		return
	}
	if err := upsertPricing(database, prices); err != nil {
		log.Printf("pricing refresh: upsert failed: %v", err)
	}
}

// pricingRefreshMetaKey marks the last time the CLI tried to
// refresh model_pricing from LiteLLM. Cooldown is enforced
// against this value, win or fail, so a repeatedly-failing
// fetch (offline, DNS broken) does not block every CLI call.
const pricingRefreshMetaKey = "_litellm_last_attempt"

// pricingRefreshCooldown is the minimum interval between
// CLI-triggered LiteLLM fetches. Short enough that a newly
// released model gets priced within hours of the user noticing,
// long enough that statusline-style repeated CLI invocations
// don't hammer LiteLLM when a session uses a truly unpriced
// model (e.g. a local Ollama model).
const pricingRefreshCooldown = time.Hour

// refreshPricingIfStale fetches the LiteLLM pricing catalog
// and upserts it when the last attempt is older than cooldown
// (or has never run). The fetcher is injectable for tests so
// the cooldown logic can be exercised without network. Returns
// true when an upsert succeeded; callers can re-query pricing
// after a true result. Errors from the fetch are returned so
// the caller can emit a warning; cooldown is recorded before
// the fetch so a persistent failure won't retry every call.
func refreshPricingIfStale(
	database *db.DB,
	fetch func() ([]pricing.ModelPricing, error),
	cooldown time.Duration,
	now time.Time,
) (bool, error) {
	stored, err := database.GetPricingMeta(pricingRefreshMetaKey)
	if err != nil {
		return false, fmt.Errorf(
			"reading pricing refresh meta: %w", err)
	}
	if stored != "" {
		last, perr := time.Parse(time.RFC3339, stored)
		if perr == nil && now.Sub(last) < cooldown {
			return false, nil
		}
	}
	if err := database.SetPricingMeta(
		pricingRefreshMetaKey, now.UTC().Format(time.RFC3339),
	); err != nil {
		return false, fmt.Errorf(
			"recording pricing refresh attempt: %w", err)
	}
	prices, err := fetch()
	if err != nil {
		return false, err
	}
	if err := upsertPricing(database, prices); err != nil {
		return false, err
	}
	return true, nil
}

func ensurePricing(database *db.DB, offline bool) {
	var prices []pricing.ModelPricing

	if offline {
		prices = pricing.FallbackPricing()
	} else {
		var err error
		prices, err = pricing.FetchLiteLLMPricing()
		if err != nil {
			fmt.Fprintf(os.Stderr,
				"warning: pricing fetch failed: %v"+
					"; using fallback\n", err)
			prices = pricing.FallbackPricing()
		}
	}

	if err := upsertPricing(database, prices); err != nil {
		fmt.Fprintf(os.Stderr,
			"warning: pricing upsert failed: %v\n", err)
	}
}

// upsertPricing copies pricing rows into the db.ModelPricing
// shape and upserts them. Shared by ensurePricing (CLI),
// seedPricing (startup fallback), and
// refreshPricingFromLiteLLM (async refresh).
func upsertPricing(
	database *db.DB, prices []pricing.ModelPricing,
) error {
	dbPrices := make([]db.ModelPricing, len(prices))
	for i, p := range prices {
		dbPrices[i] = db.ModelPricing{
			ModelPattern:           p.ModelPattern,
			InputPerMTok:           p.InputPerMTok,
			OutputPerMTok:          p.OutputPerMTok,
			CacheCreationPerMTok:   p.CacheCreationPerMTok,
			CacheCreation1hPerMTok: p.CacheCreation1hPerMTok,
			CacheReadPerMTok:       p.CacheReadPerMTok,
		}
	}
	return database.UpsertModelPricing(dbPrices)
}

// insertMissingPricing inserts fallback rows for models not already
// priced, without overwriting existing rows. Used by the direct
// usage path so a CLI-only data dir still prices fallback-catalog
// models, while never clobbering richer LiteLLM rows.
func insertMissingPricing(
	database *db.DB, prices []pricing.ModelPricing,
) error {
	dbPrices := make([]db.ModelPricing, len(prices))
	for i, p := range prices {
		dbPrices[i] = db.ModelPricing{
			ModelPattern:           p.ModelPattern,
			InputPerMTok:           p.InputPerMTok,
			OutputPerMTok:          p.OutputPerMTok,
			CacheCreationPerMTok:   p.CacheCreationPerMTok,
			CacheCreation1hPerMTok: p.CacheCreation1hPerMTok,
			CacheReadPerMTok:       p.CacheReadPerMTok,
		}
	}
	return database.InsertMissingModelPricing(dbPrices)
}

func printDailyTable(
	result db.DailyUsageResult, breakdown bool,
) {
	w := tabwriter.NewWriter(
		os.Stdout, 0, 4, 2, ' ', 0,
	)

	fmt.Fprintln(w,
		"DATE\tINPUT\tOUTPUT\tCACHE_CR\tCACHE_RD\tCOST\tMODELS")
	fmt.Fprintln(w,
		"----\t-----\t------\t--------\t--------\t----\t------")

	for _, day := range result.Daily {
		models := joinModels(day.ModelsUsed)
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%s\t%s\n",
			day.Date,
			day.InputTokens,
			day.OutputTokens,
			day.CacheCreationTokens,
			day.CacheReadTokens,
			fmtCost(day.TotalCost),
			models,
		)

		if breakdown {
			for _, mb := range day.ModelBreakdowns {
				fmt.Fprintf(w,
					"  %s\t%d\t%d\t%d\t%d\t%s\t\n",
					mb.ModelName,
					mb.InputTokens,
					mb.OutputTokens,
					mb.CacheCreationTokens,
					mb.CacheReadTokens,
					fmtCost(mb.Cost),
				)
			}
		}
	}

	fmt.Fprintln(w,
		"----\t-----\t------\t--------\t--------\t----\t------")
	fmt.Fprintf(w, "TOTAL\t%d\t%d\t%d\t%d\t%s\t\n",
		result.Totals.InputTokens,
		result.Totals.OutputTokens,
		result.Totals.CacheCreationTokens,
		result.Totals.CacheReadTokens,
		fmtCost(result.Totals.TotalCost),
	)

	w.Flush()
}

// localTimezone returns the IANA name of the system's local timezone.
func localTimezone() string {
	return time.Now().Location().String()
}

// fmtCost formats a dollar amount with two decimal places,
// matching conventional currency display. Non-zero values
// under half a cent would otherwise round to "$0.00" and
// read as "free", so they render as "<$0.01" instead.
func fmtCost(v float64) string {
	if v > 0 && v < 0.005 {
		return "<$0.01"
	}
	return fmt.Sprintf("$%.2f", v)
}

func joinModels(models []string) string {
	if len(models) == 0 {
		return ""
	}
	var s strings.Builder
	s.WriteString(models[0])
	for _, m := range models[1:] {
		s.WriteString(", " + m)
	}
	return s.String()
}
