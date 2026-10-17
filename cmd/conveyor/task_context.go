package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// conveyor task context confirm|dismiss decides one pending task-context
// proposal through the route the dashboard posts, so a terminal operator
// records the same audited act (req-operator-next-action AC-2.2, AC-5.5;
// DEC-25; DEC-45; feature-operator-next-action NA-8). The server checks
// operate_gates and records the decision event; the CLI derives nothing.

func taskContextCmd() *cobra.Command {
	command := &cobra.Command{
		Use:   "context",
		Short: "Confirm or dismiss a task's proposed context",
	}
	command.AddCommand(taskContextDecisionCmd("confirm"), taskContextDecisionCmd("dismiss"))
	return command
}

func taskContextDecisionCmd(action string) *cobra.Command {
	short := "Attach a proposed document to a task's context"
	if action == "dismiss" {
		short = "Dismiss a document proposed for a task's context"
	}
	return &cobra.Command{
		Use:   action + " <task-id> <kind> <target-id>",
		Short: short,
		Long: short + ". <kind> is requirement or system_design; <target-id> is the proposed document ID.\n" +
			"`conveyor task status <task-id>` prints this command for each pending proposal.",
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTaskContextDecision(newClient(), cmd.OutOrStdout(), action, args[0], args[1], args[2])
		},
	}
}

func runTaskContextDecision(c *client, out io.Writer, action, taskID, kind, targetID string) error {
	if strings.TrimSpace(taskID) == "" || strings.TrimSpace(kind) == "" || strings.TrimSpace(targetID) == "" {
		return fmt.Errorf("task ID, kind, and target ID are required")
	}
	proposal, err := c.decideTaskContextProposal(taskID, kind, targetID, action)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s context %s %s on task %s (state %s)\n",
		pastTense(action), proposal.TargetKind, proposal.TargetID, proposal.TaskID, proposal.State)
	return err
}

func pastTense(action string) string {
	if action == "dismiss" {
		return "dismissed"
	}
	return "confirmed"
}

// taskContextProposalPath escapes each segment once, so a target ID with a
// slash, space, or percent sign reaches the route unchanged.
func taskContextProposalPath(taskID, kind, targetID, action string) string {
	return "/v1/tasks/" + url.PathEscape(taskID) + "/context/proposals/" +
		url.PathEscape(kind) + "/" + url.PathEscape(targetID) + "/" + action
}

// decideTaskContextProposal posts the body-less decision. A refusal returns
// the server's status and its error code and message unchanged.
func (c *client) decideTaskContextProposal(taskID, kind, targetID, action string) (core.TaskContextProposal, error) {
	if c.token == "" {
		return core.TaskContextProposal{}, fmt.Errorf("a credential is required to %s task context; run `conveyor auth login`", action)
	}
	var proposal core.TaskContextProposal
	err := c.do(http.MethodPost, taskContextProposalPath(taskID, kind, targetID, action), nil, &proposal)
	return proposal, err
}

// taskContextChoiceCommand maps a confirm:task_context:<kind>:<target-id> or
// dismiss:task_context:<kind>:<target-id> choice to its CLI equivalent only
// when the offered route states exactly that act: a POST on this task's
// task-context route, a supported kind, and the id's action, kind, and target.
// The kind and target are compared after unescaping the route, so a target ID
// containing a colon keeps it. Any disagreement prints no command, so the CLI
// never offers an act other than the one the server stated
// (req-operator-next-action AC-1.7, AC-5.5).
func taskContextChoiceCommand(taskID string, choice nextActionChoice) (string, bool) {
	action, _, found := strings.Cut(choice.ID, ":task_context:")
	if !found || (action != "confirm" && action != "dismiss") || choice.Method != http.MethodPost {
		return "", false
	}
	parts := strings.Split(strings.Trim(choice.Path, "/"), "/")
	// v1 tasks <id> context proposals <kind> <target> <action>
	if len(parts) != 8 || parts[0] != "v1" || parts[1] != "tasks" || parts[3] != "context" ||
		parts[4] != "proposals" || parts[7] != action {
		return "", false
	}
	routeTask, err := url.PathUnescape(parts[2])
	if err != nil || routeTask != taskID {
		return "", false
	}
	kind, err := url.PathUnescape(parts[5])
	if err != nil || (kind != "requirement" && kind != "system_design") {
		return "", false
	}
	target, err := url.PathUnescape(parts[6])
	if err != nil || target == "" || choice.ID != action+":task_context:"+kind+":"+target {
		return "", false
	}
	return "conveyor task context " + action + " " + shellWord(taskID) + " " + kind + " " + shellWord(target), true
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9._/:=@%+,-]+$`)

// shellWord leaves a plain word as it is and single-quotes anything else, so
// the printed command reproduces the argument exactly in a POSIX shell.
func shellWord(value string) string {
	if shellSafe.MatchString(value) {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
