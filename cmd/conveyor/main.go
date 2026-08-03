// conveyor is the CLI — the primary human surface alongside the review UI
// (spec §17.1). It manages tasks, config, and safe local task worktrees.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/envfile"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var version = "dev" // set via -ldflags at build time
var workspaceFlag string

func main() {
	if err := envfile.LoadDefault(); err != nil {
		fmt.Fprintln(os.Stderr, "error: load local environment:", err)
		os.Exit(1)
	}
	root := &cobra.Command{
		Use:           "conveyor",
		Short:         "Conveyor: a software factory platform",
		Long:          "Conveyor orchestrates coding-agent pipelines and delegates implementation through MCP.",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(
		taskCmd(),
		configCmd(),
		monitorCmd(),
		workerCmd(),
		checkoutCmd(),
		lineageCmd(),
		doneCmd(),
	)
	root.PersistentFlags().StringVar(&workspaceFlag, "workspace", os.Getenv("CONVEYOR_WORKSPACE"), "workspace id (required when the server has multiple workspaces)")

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func lineageCmd() *cobra.Command {
	command := &cobra.Command{Use: "lineage", Short: "Operate the workspace lineage projection"}
	var reason, requestID string
	rebuild := &cobra.Command{Use: "rebuild", Short: "Atomically rebuild event-derived lineage", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(workspaceFlag) == "" {
				return fmt.Errorf("--workspace is required")
			}
			if strings.TrimSpace(reason) == "" || strings.TrimSpace(requestID) == "" {
				return fmt.Errorf("--reason and --request-id are required")
			}
			result, err := newClient().rebuildLineage(reason, requestID)
			if err != nil {
				return err
			}
			out, _ := json.Marshal(result)
			_, err = fmt.Fprintln(cmd.OutOrStdout(), string(out))
			return err
		}}
	rebuild.Flags().StringVar(&reason, "reason", "", "operator reason")
	rebuild.Flags().StringVar(&requestID, "request-id", "", "idempotency key")
	command.AddCommand(rebuild)
	return command
}

func monitorCmd() *cobra.Command {
	command := &cobra.Command{Use: "monitor", Short: "Inspect monitor health and repository drift"}
	status := &cobra.Command{
		Use: "status", Short: "Show workspace monitor health and unresolved drift",
		RunE: func(cmd *cobra.Command, _ []string) error {
			current, err := newClient().monitorStatus()
			if err != nil {
				return err
			}
			out, _ := json.MarshalIndent(current, "", "  ")
			_, err = fmt.Fprintln(cmd.OutOrStdout(), string(out))
			return err
		},
	}
	var outcome string
	resolve := &cobra.Command{
		Use: "resolve <drift-id>", Short: "Record an audited drift reconciliation outcome", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(outcome) == "" {
				return fmt.Errorf("--outcome is required")
			}
			drift, err := newClient().resolveMonitorDrift(args[0], outcome)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "resolved drift %s via %s (task %s)\n", drift.ID, drift.Outcome, drift.TaskID)
			return nil
		},
	}
	resolve.Flags().StringVar(&outcome, "outcome", "", "requirements_amended, conflict_resolved, or change_reverted")
	command.AddCommand(status, resolve)
	return command
}

func configCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Export or import database-backed workspace config"}
	export := &cobra.Command{
		Use: "export", Short: "Write the current workspace config as YAML",
		RunE: func(cmd *cobra.Command, _ []string) error {
			record, err := newClient().getWorkspaceConfig()
			if err != nil {
				return err
			}
			data, err := yaml.Marshal(record.Document)
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(data)
			return err
		},
	}
	importConfig := &cobra.Command{
		Use: "import <path|->", Short: "Replace workspace config from a YAML document", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var data []byte
			var err error
			if args[0] == "-" {
				data, err = io.ReadAll(cmd.InOrStdin())
			} else {
				data, err = os.ReadFile(args[0])
			}
			if err != nil {
				return err
			}
			var document config.WorkspaceDocument
			decoder := yaml.NewDecoder(strings.NewReader(string(data)))
			decoder.KnownFields(true)
			if err := decoder.Decode(&document); err != nil {
				return fmt.Errorf("parse workspace config: %w", err)
			}
			client := newClient()
			current, err := client.getWorkspaceConfig()
			if err != nil {
				return err
			}
			receipt, err := client.updateWorkspaceConfig(document, current.Version)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "updated workspace config v%d -> v%d (event %d)\n", current.Version, receipt.Version, receipt.EventID)
			return nil
		},
	}
	cmd.AddCommand(export, importConfig)
	return cmd
}

func taskCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "task", Short: "Create and inspect tasks"}

	var repo, base, body, mode, setup, specGate, mergeGate string
	var dependsOn []string
	var hold bool
	newCmd := &cobra.Command{
		Use:   "new",
		Short: "Create a task",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			specApproval, err := optionalGate(specGate)
			if err != nil {
				return err
			}
			mergeApproval, err := optionalGate(mergeGate)
			if err != nil {
				return err
			}
			t, err := newClient().createTaskWithDependencies(body, repo, base, hold, core.TaskMode(mode), specApproval, mergeApproval, setup, dependsOn)
			if err != nil {
				return err
			}
			fmt.Printf("created task %s (branch %s)\n", t.ID, t.Branch)
			return nil
		},
	}
	newCmd.Flags().StringVar(&repo, "repo", "", "repository the task targets")
	newCmd.Flags().StringVar(&base, "base", "main", "base branch")
	newCmd.Flags().StringVarP(&body, "message", "m", "", "task description (becomes part of the prompt)")
	newCmd.Flags().BoolVar(&hold, "hold", false, "reserve the task from the worker daemon; claim it yourself (spec §21.31)")
	newCmd.Flags().StringVar(&mode, "mode", "", "deprecated (spec §21.31): manual maps to --hold, auto is a no-op")
	_ = newCmd.Flags().MarkDeprecated("mode", "use --hold; execution modes were removed by spec §21.31")
	newCmd.Flags().StringVar(&setup, "setup", "", "named execution setup (defaults to workspace default)")
	newCmd.Flags().StringVar(&specGate, "spec-approval", "default", "spec approval override: default, on, or off")
	newCmd.Flags().StringVar(&mergeGate, "merge-approval", "default", "merge approval override: default, on, or off")
	newCmd.Flags().StringSliceVar(&dependsOn, "depends-on", nil, "open task ID that must merge first (repeatable)")

	listCmd := &cobra.Command{
		Use: "list", Short: "List tasks",
		RunE: func(*cobra.Command, []string) error {
			tasks, err := newClient().listTasks()
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tSTATE\tREPO\tSOURCE\tTITLE")
			for _, t := range tasks {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t.ID, t.State, t.Repo, t.Source, t.Title)
			}
			return tw.Flush()
		},
	}

	showCmd := &cobra.Command{
		Use: "show <id>", Short: "Show a task and its jobs", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient()
			t, err := c.getTask(args[0])
			if err != nil {
				return err
			}
			jobs, err := c.listJobs(args[0])
			if err != nil {
				return err
			}
			result := map[string]any{"task": t, "jobs": jobs}
			if spec, specErr := c.getLatestSpec(args[0]); specErr == nil {
				result["spec"] = spec
			}
			out, _ := json.MarshalIndent(result, "", "  ")
			fmt.Println(string(out))
			return nil
		},
	}

	cmd.AddCommand(newCmd, listCmd, showCmd,
		closeTaskCmd(),
		removeTaskDependencyCmd(),
		changeTaskSetupCmd(),
		reviewTaskCmd(core.InterventionApprove),
		reviewTaskCmd(core.InterventionReject),
		reviewTaskCmd(core.InterventionRedirect),
	)
	return cmd
}

func removeTaskDependencyCmd() *cobra.Command {
	var reason, requestID string
	command := &cobra.Command{
		Use: "unlink <task-id> <dependency-task-id>", Short: "Remove one blocking dependency edge", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(reason) == "" || strings.TrimSpace(requestID) == "" {
				return fmt.Errorf("--reason and --request-id are required")
			}
			result, err := newClient().removeTaskDependency(args[0], args[1], reason, requestID)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed dependency %s -> %s (request %s)\n", args[0], args[1], result.RequestID)
			return nil
		},
	}
	command.Flags().StringVarP(&reason, "reason", "r", "", "operator reason")
	command.Flags().StringVar(&requestID, "request-id", "", "idempotency key")
	return command
}

func closeTaskCmd() *cobra.Command {
	var reason string
	command := &cobra.Command{
		Use:   "close <id>",
		Short: "Cancel a non-terminal task",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			reason = strings.TrimSpace(reason)
			if reason == "" {
				return fmt.Errorf("--reason is required")
			}
			task, err := newClient().closeTask(args[0], reason)
			if err != nil {
				return err
			}
			fmt.Printf("cancelled task %s (state %s)\n", task.ID, task.State)
			return nil
		},
	}
	command.Flags().StringVar(&reason, "reason", "", "cancellation reason")
	return command
}

func changeTaskSetupCmd() *cobra.Command {
	var setup, reason, requestID string
	var applyLatest bool
	command := &cobra.Command{
		Use: "setup <id>", Short: "Change a task's frozen setup for future work only", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(requestID) == "" {
				return fmt.Errorf("--request-id is required")
			}
			if applyLatest == (strings.TrimSpace(setup) != "") {
				return fmt.Errorf("exactly one of --setup or --apply-latest is required")
			}
			result, err := newClient().changeTaskSetup(args[0], setup, reason, requestID, applyLatest)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "task %s now freezes setup %s; affects future work only (%s)\n", result.Task.ID, result.Task.SetupName, result.ReviewTransition)
			return nil
		},
	}
	command.Flags().StringVar(&setup, "setup", "", "currently defined named workspace setup")
	command.Flags().BoolVar(&applyLatest, "apply-latest", false, "re-freeze the latest definition of the task's current setup")
	command.Flags().StringVarP(&reason, "reason", "r", "", "optional operator reason")
	command.Flags().StringVar(&requestID, "request-id", "", "idempotency key")
	return command
}

func optionalGate(value string) (*bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "default":
		return nil, nil
	case "on", "true":
		result := true
		return &result, nil
	case "off", "false":
		result := false
		return &result, nil
	default:
		return nil, fmt.Errorf("gate override must be default, on, or off")
	}
}

func reviewTaskCmd(action core.InterventionAction) *cobra.Command {
	var reason, comment string
	command := &cobra.Command{
		Use:   string(action) + " <id>",
		Short: strings.ToUpper(string(action[:1])) + string(action[1:]) + " a task at the human gate",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if reason == "" {
				if action == core.InterventionApprove {
					reason = "approved"
				} else {
					return fmt.Errorf("--reason is required")
				}
			}
			if action == core.InterventionRedirect && strings.TrimSpace(comment) == "" {
				return fmt.Errorf("--message is required for redirect")
			}
			task, err := newClient().reviewTask(args[0], action, reason, comment)
			if err != nil {
				return err
			}
			fmt.Printf("%s task %s (state %s)\n", action, task.ID, task.State)
			return nil
		},
	}
	command.Flags().StringVar(&reason, "reason", "", "structured reason code")
	command.Flags().StringVarP(&comment, "message", "m", "", "review comment")
	return command
}

func checkoutCmd() *cobra.Command {
	var destination string
	cmd := &cobra.Command{
		Use:   "checkout <task-id>",
		Short: "Create or reuse the task's dedicated local worktree (spec §21.8)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			branch, base, repo, repoURL, ok := assignedCheckoutFromEnvironment(args[0])
			if !ok {
				client := newClient()
				task, err := client.getTask(args[0])
				if err != nil {
					return err
				}
				record, err := client.getWorkspaceConfig()
				if err != nil {
					return fmt.Errorf("load configured identity for repository %q: %w", task.Repo, err)
				}
				for _, configured := range record.Document.Repos {
					if configured.Name == task.Repo {
						repoURL = configured.URL
						break
					}
				}
				if strings.TrimSpace(repoURL) == "" {
					return fmt.Errorf("assigned repository %q is missing from workspace configuration", task.Repo)
				}
				branch, base, repo = task.Branch, task.BaseBranch, task.Repo
			}
			path, err := checkoutTask(cmd.Context(), branch, base, repo, repoURL, args[0], destination)
			if err != nil {
				return err
			}
			fmt.Println(path)
			return nil
		},
	}
	cmd.Flags().StringVar(&destination, "path", "", "destination path (default: sibling conveyor-worktrees/<repo>-task-<id>)")
	return cmd
}

// assignedCheckoutFromEnvironment resolves the branch assignment a worker
// dispatch injects as CONVEYOR_TASK_* (spec §21.8). Worker credentials never
// authorize workspace REST reads, so a worker-spawned agent cannot call
// getTask; the assignment is only honored for the exact task it was issued
// for, and every other invocation falls back to the authenticated lookup.
func assignedCheckoutFromEnvironment(taskID string) (branch, base, repo, repoURL string, ok bool) {
	if strings.TrimSpace(os.Getenv("CONVEYOR_TASK_ID")) != taskID {
		return "", "", "", "", false
	}
	branch = strings.TrimSpace(os.Getenv("CONVEYOR_TASK_BRANCH"))
	base = strings.TrimSpace(os.Getenv("CONVEYOR_TASK_BASE_BRANCH"))
	repo = strings.TrimSpace(os.Getenv("CONVEYOR_TASK_REPO"))
	repoURL = strings.TrimSpace(os.Getenv("CONVEYOR_TASK_REPO_URL"))
	if branch == "" || base == "" || repo == "" || repoURL == "" {
		return "", "", "", "", false
	}
	return branch, base, repo, repoURL, true
}

func doneCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "done <task-id>",
		Short: "Remove a clean task worktree after merge or close (spec §21.8)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			task, err := newClient().getTask(args[0])
			if err != nil {
				return err
			}
			result, err := removeTaskWorktree(cmd.Context(), task.Branch, task.State)
			if err != nil {
				return err
			}
			for _, warning := range result.ProcessWarnings {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", warning)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "worktree=%s branch=%s path=%s\n", result.Worktree, result.Branch, result.Path)
			return nil
		},
	}
	return cmd
}
