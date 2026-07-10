// conveyor is the CLI — the primary human surface alongside the review
// UI (spec §17.1). Phase 1 commands: task new/list/show, runner start,
// checkout/done (worktree escape hatch), secrets set.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var version = "dev" // set via -ldflags at build time

func main() {
	root := &cobra.Command{
		Use:           "conveyor",
		Short:         "Conveyor: a software factory platform",
		Long:          "Conveyor orchestrates coding-agent pipelines against Git repositories in disposable sandboxes.",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(
		taskCmd(),
		checkoutCmd(),
		doneCmd(),
		runnerCmd(),
		secretsCmd(),
	)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func taskCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "task", Short: "Create and inspect tasks"}

	var repo, base, level string
	newCmd := &cobra.Command{
		Use:   "new <title>",
		Short: "Create a task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// TODO(phase1): POST /v1/tasks on the control plane.
			return fmt.Errorf("not implemented: task new %q --repo %s --base %s --level %s", args[0], repo, base, level)
		},
	}
	newCmd.Flags().StringVar(&repo, "repo", "", "repository the task targets")
	newCmd.Flags().StringVar(&base, "base", "main", "base branch")
	newCmd.Flags().StringVar(&level, "level", "L2", "escalation level (L0–L3)")

	cmd.AddCommand(newCmd,
		&cobra.Command{
			Use: "list", Short: "List tasks",
			RunE: func(*cobra.Command, []string) error { return fmt.Errorf("not implemented") },
		},
		&cobra.Command{
			Use: "show <id>", Short: "Show a task", Args: cobra.ExactArgs(1),
			RunE: func(*cobra.Command, []string) error { return fmt.Errorf("not implemented") },
		},
	)
	return cmd
}

func checkoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "checkout <task-id>",
		Short: "Add the task branch as a worktree in your local clone (spec §8.4)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("not implemented: checkout %s", args[0])
		},
	}
}

func doneCmd() *cobra.Command {
	var redispatch bool
	cmd := &cobra.Command{
		Use:   "done <task-id>",
		Short: "Remove the task worktree, optionally re-dispatching",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("not implemented: done %s (redispatch=%v)", args[0], redispatch)
		},
	}
	cmd.Flags().BoolVar(&redispatch, "redispatch", false, "re-dispatch the task after removing the worktree")
	return cmd
}

func runnerCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "runner", Short: "Manage runners"}
	var local bool
	start := &cobra.Command{
		Use:   "start",
		Short: "Start a runner daemon that polls the control plane for jobs",
		RunE: func(cmd *cobra.Command, args []string) error {
			// TODO(phase1): LocalDockerRunner poll loop against the
			// control plane (spec §3.2).
			return fmt.Errorf("not implemented: runner start (local=%v)", local)
		},
	}
	start.Flags().BoolVar(&local, "local", false, "run the local Docker runner")
	cmd.AddCommand(start)
	return cmd
}

func secretsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "secrets", Short: "Manage workspace secrets"}
	var fromStdin bool
	set := &cobra.Command{
		Use:   "set <workspace>/<set>/<NAME>",
		Short: "Set a secret value (spec §17.1)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("not implemented: secrets set %s (from-stdin=%v)", args[0], fromStdin)
		},
	}
	set.Flags().BoolVar(&fromStdin, "from-stdin", false, "read the value from stdin")
	cmd.AddCommand(set)
	return cmd
}
