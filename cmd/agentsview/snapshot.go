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
	Out      string
	Projects []string
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
			Projects:    cfg.Projects,
			GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		},
	)
	if err != nil {
		log.Fatalf("export-snapshot: %v", err)
	}

	fmt.Fprintf(
		os.Stderr,
		"Wrote snapshot to %s: %d sessions, %d messages (projects: %v)\n",
		cfg.Out, m.SessionCount, m.MessageCount, m.Projects,
	)
}
