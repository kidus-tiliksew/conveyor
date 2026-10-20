package main

// `conveyor plan` command group (feature-local-planning-drafts LP-2–LP-5;
// req-local-planning REQ-1–REQ-3, AC-4.1; DEC-69). check, approve, and status
// never construct the REST client, make a network request, serve a page, or
// open a listening port. Only push reaches the factory.

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func planCmd() *cobra.Command {
	command := &cobra.Command{
		Use:   "plan",
		Short: "Check, approve, inspect, and push a local planning draft",
		Long: "Work with a local planning draft: a git-ignored directory of requirement, decision, System Design, and task items.\n" +
			"check, approve, and status run offline. A local approval only permits a later push of unconfirmed proposals; it confirms nothing.\n" +
			"push posts one approved layer as unconfirmed proposals and tasks; an operator confirms them.",
	}
	command.AddCommand(planCheckCmd(), planApproveCmd(), planStatusCmd(), planPushCmd())
	return command
}

func planCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check <draft-dir>",
		Short: "Check a planning draft offline and print one line per failure",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			draft, diagnostics, err := ReadPlanDraft(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			checked, err := CheckPlanDraft(cmd.Context(), draft)
			if err != nil {
				return err
			}
			diagnostics = append(diagnostics, checked...)
			out := cmd.OutOrStdout()
			failures, warnings := 0, 0
			for _, diagnostic := range diagnostics {
				if diagnostic.Warning {
					warnings++
				} else {
					failures++
				}
				if _, err := fmt.Fprintln(out, diagnostic.String()); err != nil {
					return err
				}
			}
			if failures > 0 {
				return fmt.Errorf("plan check: %d failure(s) in %d item(s)", failures, len(draft.Items))
			}
			_, err = fmt.Fprintf(out, "plan check: %d item(s) passed with %d warning(s)\n", len(draft.Items), warnings)
			return err
		},
	}
}

func planApproveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "approve <draft-dir> <item-id>@<digest>...",
		Short: "Record approval of draft items at their current digests, all or none",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			draft, _, err := ReadPlanDraft(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			approvals, failures, err := approvePlanItems(draft, args[1:])
			if err != nil {
				return fmt.Errorf("plan approve: no approval recorded: %w", err)
			}
			out := cmd.OutOrStdout()
			if len(failures) > 0 {
				for _, failure := range failures {
					if _, err := fmt.Fprintln(out, failure.String()); err != nil {
						return err
					}
				}
				return fmt.Errorf("plan approve: %d item(s) failed; no approval recorded", len(failures))
			}
			for _, approval := range approvals {
				if _, err := fmt.Fprintf(out, "approved %s@%s\n", approval.ItemID, approval.Digest); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func planStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status <draft-dir>",
		Short: "Print each draft item's layer, current digest, approval state, and push record",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			draft, _, err := ReadPlanDraft(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			rows := PlanStatus(draft)
			table := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(table, "ITEM\tLAYER\tDIGEST\tSTATE\tPUSHED")
			invalid := 0
			for _, row := range rows {
				digest, state := row.Digest, row.State
				if row.Err != nil {
					invalid++
					digest, state = "-", "invalid"
				}
				fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", row.ItemID, row.Layer, digest, state, row.pushed())
			}
			if err := table.Flush(); err != nil {
				return err
			}
			if invalid > 0 {
				for _, row := range rows {
					if row.Err != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", row.ItemID, row.Err)
					}
				}
				return fmt.Errorf("plan status: %d item(s) have no digest; run conveyor plan check", invalid)
			}
			return nil
		},
	}
}

func planPushCmd() *cobra.Command {
	var options planPushOptions
	command := &cobra.Command{
		Use:   "push <draft-dir> --layer requirements|decisions|system-designs|tasks [--dry-run]",
		Short: "Post one approved draft layer as unconfirmed proposals and tasks",
		Long: "Run the draft check and every read-only pre-check for the layer, then post its items in dependency order.\n" +
			"Any refused item stops the whole layer before its first post. A rerun recovers an interrupted post from factory state.\n" +
			"Push calls no confirm or dismiss route. --dry-run applies every check, prints each request, and posts nothing.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPlanPush(cmd.Context(), args[0], options, newPlanAPI(), cmd.OutOrStdout())
		},
	}
	command.Flags().StringVar(&options.Layer, "layer", "", "layer to push: "+strings.Join(PlanLayers, ", "))
	command.Flags().BoolVar(&options.DryRun, "dry-run", false, "apply every check and print the requests without posting")
	_ = command.MarkFlagRequired("layer")
	return command
}
