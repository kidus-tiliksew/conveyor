package main

// `conveyor plan` command group (feature-local-planning-drafts LP-2–LP-4;
// req-local-planning REQ-1, REQ-2, AC-4.1; DEC-69). check, approve, and status
// never construct the REST client, make a network request, serve a page, or
// open a listening port.

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func planCmd() *cobra.Command {
	command := &cobra.Command{
		Use:   "plan",
		Short: "Check, approve, and inspect a local planning draft",
		Long: "Work with a local planning draft: a git-ignored directory of requirement, decision, System Design, and task items.\n" +
			"check, approve, and status run offline. A local approval only permits a later push of unconfirmed proposals; it confirms nothing.",
	}
	command.AddCommand(planCheckCmd(), planApproveCmd(), planStatusCmd())
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
