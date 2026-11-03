package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/publish"
)

// PublishConfig holds flags for the publish command.
type PublishConfig struct {
	Preview     bool
	Out         string
	Sessions    []string
	Since       string
	Contributor string
	Visibility  string
	NoDirenv    bool
	DenyList    string
}

func newPublishCommand() *cobra.Command {
	var cfg PublishConfig
	cmd := &cobra.Command{
		Use:   "publish <project>... (--preview | --out <dir>)",
		Short: "Preview or write a redacted snapshot of your sessions",
		Long: "Select a project's sessions, scan them for secrets, and either " +
			"report what would be redacted (--preview, writes nothing) or " +
			"write a redacted export-snapshot directory (--out).\n\n" +
			"Detectors:\n" +
			"  - the secret scanner's rules (definite matches, JWTs, URLs with " +
			"credentials, private keys, and assignments whose variable name " +
			"looks secret are redacted; other candidates are flagged)\n" +
			"  - exact values from each session directory's environment: what " +
			"direnv resolves there, plus .env*, .dev.vars (and .envrc when " +
			"direnv is unavailable)\n" +
			"  - deny-list terms, one per line (flagged, not redacted)\n\n" +
			"Redacted text becomes [REDACTED:<kind>]. Secret values are never " +
			"printed. Sessions marked private keep only a scrubbed placeholder.",
		GroupID:      groupData,
		SilenceUsage: true,
		Args:         cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cfg.Preview == (cfg.Out != "") {
				return errors.New("pass exactly one of --preview or --out")
			}
			return runPublish(cmd.Context(), cmd.OutOrStdout(), args, cfg, outputFormat(cmd) == "json")
		},
	}
	cmd.Flags().StringVarP(&cfg.Contributor, "contributor", "c", "",
		"Contributor email stamped into manifest.json (default $AGENTSVIEW_CONTRIBUTOR)")
	cmd.Flags().StringVarP(&cfg.DenyList, "deny", "d", "",
		"Deny-list file of terms to flag (default <data dir>/publish-deny.txt)")
	cmd.Flags().BoolVarP(&cfg.NoDirenv, "no-direnv", "D", false,
		"Read .envrc files instead of resolving each directory through direnv")
	cmd.Flags().StringVarP(&cfg.Out, "out", "o", "", "Write the redacted snapshot to this (empty or new) directory")
	cmd.Flags().BoolVarP(&cfg.Preview, "preview", "n", false, "Scan and report only; write nothing")
	cmd.Flags().StringArrayVarP(&cfg.Sessions, "session", "S", nil,
		"Only this session ID and its subagents (repeatable)")
	cmd.Flags().StringVarP(&cfg.Since, "since", "s", "",
		"Only sessions that ended on or after YYYY-MM-DD")
	cmd.Flags().StringVarP(&cfg.Visibility, "visibility", "v", "draft",
		"Visibility stamped on each session: "+strings.Join(publish.Visibilities, ", "))
	registerFormatFlags(cmd.Flags())
	return cmd
}

func runPublish(ctx context.Context, w io.Writer, projects []string, cfg PublishConfig, asJSON bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	appCfg, err := config.LoadMinimal()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	deny := cfg.DenyList
	if deny == "" {
		deny = filepath.Join(appCfg.DataDir, "publish-deny.txt")
	}
	contributor := cfg.Contributor
	if contributor == "" {
		contributor = os.Getenv("AGENTSVIEW_CONTRIBUTOR")
	}
	rep, err := publish.Run(ctx, publish.Options{
		ArchivePath: appCfg.DBPath,
		Projects:    projects,
		Sessions:    cfg.Sessions,
		Since:       cfg.Since,
		OutDir:      cfg.Out,
		Contributor: contributor,
		Visibility:  cfg.Visibility,
		Env:         publish.EnvOptions{Direnv: !cfg.NoDirenv},
		DenyList:    deny,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Progress: func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", args...)
		},
	})
	if err != nil {
		return err
	}
	if asJSON {
		b, err := json.Marshal(rep, json.Deterministic(true), jsontext.WithIndent("  "))
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "%s\n", b)
		return err
	}
	writePublishReport(w, rep)
	return nil
}

// maxListedOrdinals bounds the ordinals printed per finding group.
const maxListedOrdinals = 10

func writePublishReport(w io.Writer, rep *publish.Report) {
	verb := "Previewed"
	if rep.Out != "" {
		verb = "Wrote"
	}
	fmt.Fprintf(w, "%s %d sessions (%d messages) from %s on %s\n",
		verb, rep.SessionCount, rep.MessageCount,
		strings.Join(rep.Projects, ", "), strings.Join(rep.Machines, ", "))
	e := rep.Env
	fmt.Fprintf(w, "Env: %d dirs (%d via direnv, %d from files, %d missing) -> %d secret values\n",
		e.Dirs, e.Direnv, e.FileOnly, e.Missing, e.Values)
	fmt.Fprintf(w, "Deny-list: %d terms\n", rep.DenyTerms)

	clean := 0
	for _, s := range rep.Sessions {
		if len(s.Findings) == 0 {
			clean++
			continue
		}
		title := s.Title
		if s.Private {
			title = "(private)"
		}
		fmt.Fprintf(w, "\n%s  %s  %q\n", s.ID, s.Project, title)
		writeGroups(w, s.Findings)
	}
	if len(rep.Aggregates) > 0 {
		fmt.Fprintf(w, "\nanalytics panels (repeat message text)\n")
		writeGroups(w, rep.Aggregates)
	}
	if clean > 0 {
		fmt.Fprintf(w, "\n%d sessions with no findings\n", clean)
	}

	fmt.Fprintf(w, "\nTotals: %d redactions, %d flags", rep.Redactions(), rep.Flags())
	if n := rep.Ignored["high-entropy-assignment"]; n > 0 {
		fmt.Fprintf(w, " (ignored %d high-entropy candidates without a secret-looking name)", n)
	}
	fmt.Fprintln(w)
	if rep.Out == "" {
		fmt.Fprintln(w, "Nothing was written. Re-run with --out <dir> to write the redacted snapshot.")
	} else {
		fmt.Fprintf(w, "Redacted snapshot: %s\n", rep.Out)
	}
}

func writeGroups(w io.Writer, groups []publish.Group) {
	for _, g := range groups {
		fmt.Fprintf(w, "  %-6s  %-32s  x%-4d", g.Action, g.Kind, g.Count)
		switch {
		case len(g.Ordinals) > 0:
			fmt.Fprintf(w, "  @ %s", listInts(g.Ordinals))
		case len(g.Routes) > 0:
			fmt.Fprintf(w, "  in %s", strings.Join(g.Routes, ", "))
		}
		fmt.Fprintln(w)
	}
}

func listInts(xs []int) string {
	n := min(len(xs), maxListedOrdinals)
	parts := make([]string, n)
	for i := range n {
		parts[i] = strconv.Itoa(xs[i])
	}
	s := strings.Join(parts, ", ")
	if len(xs) > n {
		s += fmt.Sprintf(", ... +%d", len(xs)-n)
	}
	return s
}
