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
		workerCmd(),
		checkoutCmd(),
		doneCmd(),
	)
	root.PersistentFlags().StringVar(&workspaceFlag, "workspace", os.Getenv("CONVEYOR_WORKSPACE"), "workspace id (required when the server has multiple workspaces)")

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
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

	var repo, base, body, mode, specGate, mergeGate string
	newCmd := &cobra.Command{
		Use:   "new [title]",
		Short: "Create a task",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			specApproval, err := optionalGate(specGate)
			if err != nil {
				return err
			}
			mergeApproval, err := optionalGate(mergeGate)
			if err != nil {
				return err
			}
			title := ""
			if len(args) == 1 {
				title = args[0]
			}
			t, err := newClient().createTaskWithMode(title, body, repo, base, core.TaskMode(mode), specApproval, mergeApproval)
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
	newCmd.Flags().StringVar(&mode, "mode", "", "execution mode: auto or manual (defaults to workspace policy)")
	newCmd.Flags().StringVar(&specGate, "spec-approval", "default", "spec approval override: default, on, or off")
	newCmd.Flags().StringVar(&mergeGate, "merge-approval", "default", "merge approval override: default, on, or off")

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
		reviewTaskCmd(core.InterventionApprove),
		reviewTaskCmd(core.InterventionReject),
		reviewTaskCmd(core.InterventionRedirect),
	)
	return cmd
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
			task, err := newClient().getTask(args[0])
			if err != nil {
				return err
			}
			path, err := checkoutTask(cmd.Context(), task.Branch, task.BaseBranch, task.Repo, task.ID, destination)
			if err != nil {
				return err
			}
			fmt.Println(path)
			return nil
		},
	}
	cmd.Flags().StringVar(&destination, "path", "", "destination path (default: sibling <repo>-task-<id>)")
	return cmd
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
			fmt.Printf("worktree=%s branch=%s path=%s\n", result.Worktree, result.Branch, result.Path)
			return nil
		},
	}
	return cmd
}
