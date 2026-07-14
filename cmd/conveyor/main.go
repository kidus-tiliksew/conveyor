// conveyor is the CLI — the primary human surface alongside the review UI
// (spec §17.1). It manages tasks, the standalone local runner, human
// checkout/done, and secret input.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/secrets"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var version = "dev" // set via -ldflags at build time

func main() {
	configPath := "conveyor.yaml"
	root := &cobra.Command{
		Use:           "conveyor",
		Short:         "Conveyor: a software factory platform",
		Long:          "Conveyor orchestrates coding-agent pipelines against Git repositories in disposable sandboxes.",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&configPath, "config", configPath, "path to deployment config")

	root.AddCommand(
		taskCmd(),
		configCmd(),
		checkoutCmd(),
		doneCmd(),
		runnerCmd(&configPath),
		secretsCmd(&configPath),
	)

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

	var repo, base, body, level string
	newCmd := &cobra.Command{
		Use:   "new <title>",
		Short: "Create a task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := newClient().createTaskWithLevel(args[0], body, repo, base, core.EscalationLevel(level))
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
	newCmd.Flags().StringVar(&level, "level", string(core.L2), "escalation level: L0, L1, L2, or L3")

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
		Short: "Add the task branch as a worktree in your local clone (spec §8.4)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			task, err := newClient().getTask(args[0])
			if err != nil {
				return err
			}
			path, err := checkoutTask(cmd.Context(), task.Branch, task.Repo, task.ID, destination)
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
	var redispatch bool
	cmd := &cobra.Command{
		Use:   "done <task-id>",
		Short: "Remove the task worktree, optionally re-dispatching",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			task, err := client.getTask(args[0])
			if err != nil {
				return err
			}
			if err := removeTaskWorktree(cmd.Context(), task.Branch, redispatch); err != nil {
				return err
			}
			if redispatch {
				if _, err := client.redispatchTask(task.ID); err != nil {
					return err
				}
				fmt.Printf("removed checkout and re-dispatched task %s\n", task.ID)
			} else {
				fmt.Printf("removed checkout for task %s\n", task.ID)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&redispatch, "redispatch", false, "re-dispatch the task after removing the worktree")
	return cmd
}

func runnerCmd(configPath *string) *cobra.Command {
	cmd := &cobra.Command{Use: "runner", Short: "Manage runners"}
	var local bool
	start := &cobra.Command{
		Use:   "start",
		Short: "Start the standalone local Docker runner",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !local {
				return fmt.Errorf("only --local is implemented; K8sRunner is demand-triggered Phase 8")
			}
			binary, err := siblingBinary("conveyor-runner")
			if err != nil {
				return err
			}
			commandArgs := []string{"-config", *configPath}
			child := exec.CommandContext(cmd.Context(), binary, commandArgs...)
			child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
			return child.Run()
		},
	}
	start.Flags().BoolVar(&local, "local", false, "run the local Docker runner")
	cmd.AddCommand(start)
	return cmd
}

func secretsCmd(configPath *string) *cobra.Command {
	cmd := &cobra.Command{Use: "secrets", Short: "Manage workspace secrets"}
	var fromStdin bool
	set := &cobra.Command{
		Use:   "set <workspace>/<set>/<NAME>",
		Short: "Set a secret value (spec §17.1)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !fromStdin {
				return fmt.Errorf("--from-stdin is required so secret values never enter argv or shell history")
			}
			cfg, err := config.Load(*configPath)
			if err != nil {
				return err
			}
			ref, err := secrets.ParseRef("secretref://" + strings.TrimPrefix(args[0], "secretref://"))
			if err != nil {
				return err
			}
			if ref.Workspace != cfg.Workspace {
				return fmt.Errorf("secret workspace %q does not match configured workspace %q", ref.Workspace, cfg.Workspace)
			}
			if _, ok := cfg.Secrets.Sets[ref.Set]; !ok {
				return fmt.Errorf("secret set %q has no configured delivery policy", ref.Set)
			}
			value, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 1024*1024+1))
			if err != nil {
				return err
			}
			if len(value) > 1024*1024 {
				return fmt.Errorf("secret value exceeds 1 MiB")
			}
			value = bytesTrimOneNewline(value)
			if err := cfg.SecretResolver().Set(cmd.Context(), ref, string(value)); err != nil {
				return err
			}
			fmt.Printf("stored %s\n", ref.String())
			return nil
		},
	}
	set.Flags().BoolVar(&fromStdin, "from-stdin", false, "read the value from stdin")
	cmd.AddCommand(set)
	return cmd
}

func checkoutTask(ctx context.Context, branch, repo, taskID, destination string) (string, error) {
	root, err := gitOutput(ctx, "", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("checkout must run inside the target repository: %w", err)
	}
	root = strings.TrimSpace(root)
	if destination == "" {
		destination = filepath.Join(filepath.Dir(root), repo+"-task-"+taskID)
	} else if !filepath.IsAbs(destination) {
		destination, err = filepath.Abs(destination)
		if err != nil {
			return "", err
		}
	}
	localRef := "refs/heads/" + branch
	remoteRef := "refs/remotes/origin/" + branch
	remoteListing, err := gitOutput(ctx, root, "ls-remote", "--heads", "origin", localRef)
	if err != nil {
		return "", err
	}
	remoteExists := strings.TrimSpace(remoteListing) != ""
	if remoteExists {
		if _, err := gitOutput(ctx, root, "fetch", "origin", "+"+localRef+":"+remoteRef); err != nil {
			return "", err
		}
	}
	localExists := gitRefExists(ctx, root, localRef)
	if localExists && remoteExists {
		switch {
		case gitIsAncestor(ctx, root, localRef, remoteRef):
			remoteCommit, err := gitOutput(ctx, root, "rev-parse", "--verify", remoteRef)
			if err != nil {
				return "", err
			}
			if _, err := gitOutput(ctx, root, "update-ref", localRef, strings.TrimSpace(remoteCommit)); err != nil {
				return "", err
			}
		case gitIsAncestor(ctx, root, remoteRef, localRef):
			// The human branch is ahead of origin; preserve it for checkout.
		default:
			return "", fmt.Errorf("task branch %s diverged between the local clone and origin", branch)
		}
	}
	if localExists {
		if _, err := gitOutput(ctx, root, "worktree", "add", destination, branch); err != nil {
			return "", err
		}
	} else if remoteExists {
		if _, err := gitOutput(ctx, root, "worktree", "add", "-b", branch, destination, remoteRef); err != nil {
			return "", err
		}
	} else {
		return "", fmt.Errorf("task branch %s was not found locally or on origin", branch)
	}
	return destination, nil
}

func removeTaskWorktree(ctx context.Context, branch string, push bool) error {
	root, err := gitOutput(ctx, "", "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("done must run inside the repository's primary checkout: %w", err)
	}
	root = strings.TrimSpace(root)
	listing, err := gitOutput(ctx, root, "worktree", "list", "--porcelain")
	if err != nil {
		return err
	}
	var path, candidate string
	for _, line := range strings.Split(listing, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			candidate = strings.TrimPrefix(line, "worktree ")
		case line == "branch refs/heads/"+branch:
			path = candidate
		}
	}
	if path == "" {
		return fmt.Errorf("no local worktree found for branch %s", branch)
	}
	if filepath.Clean(path) == filepath.Clean(root) {
		return fmt.Errorf("refusing to remove the primary checkout")
	}
	if push {
		status, err := gitOutput(ctx, path, "status", "--porcelain")
		if err != nil {
			return err
		}
		if strings.TrimSpace(status) != "" {
			return fmt.Errorf("task worktree has uncommitted changes; commit them before --redispatch")
		}
		if _, err := gitOutput(ctx, path, "push", "--set-upstream", "origin", branch); err != nil {
			return err
		}
	}
	_, err = gitOutput(ctx, root, "worktree", "remove", path)
	return err
}

func siblingBinary(name string) (string, error) {
	if executable, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(executable), name)
		if info, statErr := os.Stat(sibling); statErr == nil && !info.IsDir() {
			return sibling, nil
		}
	}
	if binary, err := exec.LookPath(name); err == nil {
		return binary, nil
	}
	return "", fmt.Errorf("%s not found beside conveyor or on PATH; run make build", name)
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	out, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func gitRefExists(ctx context.Context, dir, ref string) bool {
	_, err := gitOutput(ctx, dir, "rev-parse", "--verify", "--quiet", ref)
	return err == nil
}

func gitIsAncestor(ctx context.Context, dir, older, newer string) bool {
	command := exec.CommandContext(ctx, "git", "merge-base", "--is-ancestor", older, newer)
	command.Dir = dir
	return command.Run() == nil
}

func bytesTrimOneNewline(value []byte) []byte {
	value = []byte(strings.TrimSuffix(string(value), "\n"))
	return []byte(strings.TrimSuffix(string(value), "\r"))
}
