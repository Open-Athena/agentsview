package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/snapshot"
)

// ExportSnapshotConfig holds flags for the export-snapshot command.
type ExportSnapshotConfig struct {
	Out           string
	FullProjects  []string
	StatsProjects []string
	AllStats      bool
}

func runExportSnapshot(cfg ExportSnapshotConfig) {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	m, err := snapshot.Export(
		context.Background(),
		appCfg.DBPath,
		cfg.Out,
		snapshot.Options{
			FullProjects:  cfg.FullProjects,
			StatsProjects: cfg.StatsProjects,
			AllStats:      cfg.AllStats,
			GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		},
	)
	if err != nil {
		log.Fatalf("export-snapshot: %v", err)
	}

	fmt.Fprintf(
		os.Stderr,
		"Wrote snapshot to %s: %d sessions, %d messages, %d tool calls (%s)\n",
		cfg.Out, m.SessionCount, m.MessageCount, m.ToolCallCount,
		describeTiers(m),
	)
}

// describeTiers renders the export's exposure tiers for the CLI summary.
func describeTiers(m *snapshot.Manifest) string {
	switch {
	case m.AllStats && len(m.FullProjects) == 0:
		return "all projects stats-only (no raw prompts)"
	case m.AllStats:
		return fmt.Sprintf("full: %v; all others stats-only", m.FullProjects)
	case len(m.FullProjects) == 0 && len(m.StatsProjects) == 0:
		return "full unfiltered export"
	case len(m.StatsProjects) == 0:
		return fmt.Sprintf("full: %v", m.FullProjects)
	default:
		return fmt.Sprintf("full: %v; stats-only: %v", m.FullProjects, m.StatsProjects)
	}
}
