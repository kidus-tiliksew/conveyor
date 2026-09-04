package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	postgresstore "github.com/kidus-tiliksew/conveyor/internal/store/postgres"
)

type parityChecker interface {
	LogParity(context.Context, string, postgresstore.ParityOptions) (postgresstore.ParityReport, error)
	Close()
}

type openParityChecker func(context.Context, string) (parityChecker, error)

// logParityCmd compares the event log's catalog with the legacy rows.
// Log-core migration plan, phase 2, task 2.2.
func logParityCmd() *cobra.Command {
	return newLogParityCmd(func(ctx context.Context, databaseURL string) (parityChecker, error) {
		return postgresstore.Open(ctx, databaseURL)
	})
}

func newLogParityCmd(openStore openParityChecker) *cobra.Command {
	var workspaces []string
	var asJSON bool
	var maxDrifts int
	command := &cobra.Command{
		Use:   "log-parity",
		Short: "Compare the event log's read model with the legacy projection rows",
		Long: `log-parity replays a workspace's event log into an in-process catalog and
compares every entity's last snapshot with its live rows, hashed the same way
migrate-log hashes them.

  match    rows unchanged since the snapshot
  drift    rows changed; the kinds listed are what a projector must fold
  missing  rows exist but the log has no snapshot (run migrate-log)
  orphans  the log has the stream but the rows are gone

It never writes. Exit status is 1 when any entity drifted or is missing.
Requires CONVEYOR_DATABASE_URL.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			databaseURL := strings.TrimSpace(os.Getenv("CONVEYOR_DATABASE_URL"))
			if databaseURL == "" {
				return errors.New("CONVEYOR_DATABASE_URL is required; set it to the deployment Postgres database and retry")
			}
			if len(workspaces) == 0 {
				return errors.New("at least one --workspace-id is required")
			}
			st, err := openStore(cmd.Context(), databaseURL)
			if err != nil {
				return fmt.Errorf("open deployment database: %w", err)
			}
			defer st.Close()
			clean := true
			var reports []postgresstore.ParityReport
			for _, workspace := range workspaces {
				report, err := st.LogParity(cmd.Context(), workspace, postgresstore.ParityOptions{MaxDrifts: maxDrifts})
				if err != nil {
					return fmt.Errorf("workspace %s: %w", workspace, err)
				}
				reports = append(reports, report)
				if !report.Clean() {
					clean = false
				}
			}
			if asJSON {
				encoder := json.NewEncoder(cmd.OutOrStdout())
				encoder.SetIndent("", "  ")
				if err := encoder.Encode(reports); err != nil {
					return err
				}
			} else {
				printParity(cmd, reports)
			}
			if !clean {
				cmd.SilenceUsage = true
				return errors.New("parity: drift or missing entities found")
			}
			return nil
		},
	}
	command.Flags().StringArrayVar(&workspaces, "workspace-id", nil, "workspace to check (repeatable, required)")
	command.Flags().BoolVar(&asJSON, "json", false, "print the reports as JSON")
	command.Flags().IntVar(&maxDrifts, "max-drifts", 20, "drift details to keep per family (0 = all)")
	return command
}

func printParity(cmd *cobra.Command, reports []postgresstore.ParityReport) {
	out := cmd.OutOrStdout()
	for _, report := range reports {
		fmt.Fprintf(out, "%s: %d streams at position %d, replay %dms\n", report.Workspace, report.Streams, report.Position, report.ReplayMillis)
		for _, family := range report.Families {
			fmt.Fprintf(out, "  %-20s match %-5d drift %-5d missing %-5d orphans %d\n", family.Family, family.Match, family.Drift, family.Missing, family.Orphans)
			for _, kind := range family.KindsByWeight() {
				fmt.Fprintf(out, "    unfolded %-45s %d\n", kind, family.UnfoldedKinds[kind])
			}
			for _, drift := range family.Drifts {
				fmt.Fprintf(out, "    drift %s since: %s\n", drift.Stream, strings.Join(drift.KindsSince, ", "))
			}
		}
	}
}
