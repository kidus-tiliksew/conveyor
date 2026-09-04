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

type genesisImporter interface {
	ImportGenesis(context.Context, postgresstore.GenesisOptions) (postgresstore.GenesisReport, error)
	Close()
}

type openGenesisImporter func(context.Context, string) (genesisImporter, error)

// migrateLogCmd builds the event log for a deployment that predates it.
// Log-core migration plan, phase 1, task 1.3.
func migrateLogCmd() *cobra.Command {
	return newMigrateLogCmd(func(ctx context.Context, databaseURL string) (genesisImporter, error) {
		return postgresstore.Open(ctx, databaseURL)
	})
}

func newMigrateLogCmd(openStore openGenesisImporter) *cobra.Command {
	var workspaces []string
	var batchSize int
	var asJSON bool
	command := &cobra.Command{
		Use:   "migrate-log",
		Short: "Import legacy history and entity snapshots into the event log (idempotent, resumable)",
		Long: `migrate-log builds the event log for a deployment whose data predates it.

For every workspace it appends legacy events not yet in the log, then writes
one snapshot per task, work order, document, decision, planning session,
worker, and workspace, plus one per user at the deployment level. Snapshots
are hashed: re-running with unchanged rows writes nothing. The run holds the
startup-migrations lock, so it never overlaps a daemon applying migrations.

Requires CONVEYOR_DATABASE_URL.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			databaseURL := strings.TrimSpace(os.Getenv("CONVEYOR_DATABASE_URL"))
			if databaseURL == "" {
				return errors.New("CONVEYOR_DATABASE_URL is required; set it to the deployment Postgres database and retry")
			}
			st, err := openStore(cmd.Context(), databaseURL)
			if err != nil {
				return fmt.Errorf("open deployment database: %w", err)
			}
			defer st.Close()
			progress := func(line string) {
				if !asJSON {
					fmt.Fprintln(cmd.ErrOrStderr(), line)
				}
			}
			report, err := st.ImportGenesis(cmd.Context(), postgresstore.GenesisOptions{
				Workspaces: workspaces, BatchSize: batchSize, Progress: progress,
			})
			if err != nil {
				return err
			}
			if asJSON {
				encoder := json.NewEncoder(cmd.OutOrStdout())
				encoder.SetIndent("", "  ")
				return encoder.Encode(report)
			}
			out := cmd.OutOrStdout()
			for _, wr := range report.Workspaces {
				fmt.Fprintf(out, "%s: history imported %d (already in log %d), snapshots written %s, unchanged %d, marker %t\n",
					wr.Workspace, wr.HistoryImported, wr.HistoryAlreadyInLog, formatSnapshotCounts(wr.SnapshotsWritten), wr.SnapshotsUnchanged, wr.MarkerWritten)
			}
			fmt.Fprintf(out, "deployment: snapshots written %s, unchanged %d\n", formatSnapshotCounts(report.Deployment.SnapshotsWritten), report.Deployment.SnapshotsUnchanged)
			fmt.Fprintf(out, "done in %s\n", report.Duration.Round(1e6))
			return nil
		},
	}
	command.Flags().StringArrayVar(&workspaces, "workspace-id", nil, "limit the import to a workspace (repeatable; default all)")
	command.Flags().IntVar(&batchSize, "batch", 0, "legacy history rows per transaction (default 2000)")
	command.Flags().BoolVar(&asJSON, "json", false, "print the report as JSON")
	return command
}

func formatSnapshotCounts(counts map[string]int) string {
	if len(counts) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(counts))
	for _, family := range []string{"task", "work_order", "requirement", "design", "decision", "reference_document", "planning_session", "planning_bundle", "worker", "workspace", "user"} {
		if n, ok := counts[family]; ok {
			parts = append(parts, fmt.Sprintf("%s=%d", family, n))
		}
	}
	return strings.Join(parts, " ")
}
