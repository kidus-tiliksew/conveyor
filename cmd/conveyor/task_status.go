package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// conveyor task status prints the server's next-action projection for one
// task (req-operator-next-action REQ-5, AC-5.1, AC-1.4, AC-1.8; DEC-61). The
// CLI decodes the wire shape with local structs that copy the JSON tags of
// internal/nextaction and never derives an action, label, or choice itself.

type nextActionBlocker struct {
	TaskID string `json:"task_id"`
	Title  string `json:"title"`
	State  string `json:"state"`
}

type nextActionChoice struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Effect     string `json:"effect"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Capability string `json:"capability"`
	Allowed    bool   `json:"allowed"`
	Direction  string `json:"direction,omitempty"`
}

type nextActionOrder struct {
	ID                string     `json:"id"`
	Stage             string     `json:"stage"`
	State             string     `json:"state"`
	QueueDeadline     *time.Time `json:"queue_deadline"`
	ExecutionDeadline *time.Time `json:"execution_deadline"`
	LeaseExpiresAt    *time.Time `json:"lease_expires_at"`
	RecoveryRefusal   string     `json:"recovery_refusal,omitempty"`
}

type nextActionAttempt struct {
	Harness        string     `json:"harness,omitempty"`
	Model          string     `json:"model,omitempty"`
	User           string     `json:"user,omitempty"`
	ClaimedBy      string     `json:"claimed_by,omitempty"`
	SessionID      string     `json:"session_id,omitempty"`
	AttemptID      string     `json:"attempt_id,omitempty"`
	ClaimedAt      *time.Time `json:"claimed_at"`
	LastCheckIn    *time.Time `json:"last_check_in"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at"`
	Progress       string     `json:"progress,omitempty"`
	ElapsedSeconds int64      `json:"elapsed_seconds"`
}

type nextActionRefusal struct {
	Choice string `json:"choice"`
	Text   string `json:"text"`
}

type nextActionCitation struct {
	DocumentID           string `json:"document_id"`
	CitedVersion         int    `json:"cited_version"`
	StatementOrSectionID string `json:"statement_or_section_id,omitempty"`
}

type nextActionProposal struct {
	Tier    string `json:"tier"`
	ID      string `json:"id"`
	Version int    `json:"version"`
}

// nextActionDetails carries the agent-question checkpoint context the server
// states: the decision request, its class, citations, and the cited pending
// proposals (req-operator-next-action AC-1.6; feature-operator-next-action NA-8).
type nextActionDetails struct {
	GateKind         string               `json:"gate_kind,omitempty"`
	ReasonCode       string               `json:"reason_code,omitempty"`
	StallReason      string               `json:"stall_reason,omitempty"`
	ReleaseOutcome   string               `json:"release_outcome,omitempty"`
	Checkpoint       string               `json:"checkpoint,omitempty"`
	CheckpointClass  string               `json:"checkpoint_class,omitempty"`
	Citations        []nextActionCitation `json:"citations,omitempty"`
	PendingProposals []nextActionProposal `json:"pending_proposals,omitempty"`
	Refusals         []nextActionRefusal  `json:"refusals,omitempty"`
}

type nextActionView struct {
	Label        string              `json:"label"`
	Actor        string              `json:"actor"`
	Kind         string              `json:"kind"`
	Judgment     bool                `json:"judgment"`
	Summary      string              `json:"summary"`
	Question     string              `json:"question,omitempty"`
	Choices      []nextActionChoice  `json:"choices"`
	IfIgnored    string              `json:"if_ignored,omitempty"`
	Deadline     *time.Time          `json:"deadline"`
	BlockedBy    []nextActionBlocker `json:"blocked_by,omitempty"`
	CurrentOrder *nextActionOrder    `json:"current_order,omitempty"`
	Attempt      *nextActionAttempt  `json:"attempt,omitempty"`
	Details      nextActionDetails   `json:"details"`
}

func taskStatusCmd() *cobra.Command {
	asJSON := false
	cmd := &cobra.Command{
		Use:   "status <task-id>",
		Short: "Show what the task is waiting on and who acts next",
		Long: `Show the server's next-action statement for a task: what it is waiting on,
who acts next, the offered choices with their CLI equivalents, the deadline,
blockers, the current work order and attempt, and refused choices. Machine
identifiers print last. --json prints the server body verbatim.

Reads GET /v1/tasks/{id}/next-action.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTaskStatus(newClient(), cmd.OutOrStdout(), args[0], asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the server body verbatim")
	return cmd
}

func runTaskStatus(c *client, w io.Writer, taskID string, asJSON bool) error {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return fmt.Errorf("task ID is required")
	}
	var body json.RawMessage
	if err := c.doHeaders(http.MethodGet, "/v1/tasks/"+url.PathEscape(taskID)+"/next-action", nil, &body, nil); err != nil {
		return err
	}
	if asJSON {
		_, err := fmt.Fprintf(w, "%s\n", body)
		return err
	}
	var action nextActionView
	if err := json.Unmarshal(body, &action); err != nil {
		return fmt.Errorf("decode next action: %w", err)
	}
	return writeTaskStatus(w, taskID, action)
}

func writeTaskStatus(w io.Writer, taskID string, action nextActionView) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n%s\n\n", action.Label, action.Summary)
	actor := action.Actor
	if action.Judgment {
		actor += " (judgment required)"
	}
	fmt.Fprintf(&b, "actor:         %s\n", actor)
	if action.Question != "" {
		fmt.Fprintf(&b, "question:      %s\n", action.Question)
	}
	if action.Details.Checkpoint != "" {
		fmt.Fprintf(&b, "request:       %s\n", action.Details.Checkpoint)
	}
	for _, citation := range action.Details.Citations {
		line := fmt.Sprintf("%s v%d", citation.DocumentID, citation.CitedVersion)
		if citation.StatementOrSectionID != "" {
			line += " (" + citation.StatementOrSectionID + ")"
		}
		fmt.Fprintf(&b, "cites:         %s\n", line)
	}
	for _, proposal := range action.Details.PendingProposals {
		fmt.Fprintf(&b, "proposal:      %s v%d (%s, pending)\n", proposal.ID, proposal.Version, proposal.Tier)
	}
	if len(action.Choices) > 0 {
		b.WriteString("choices:\n")
		for _, choice := range action.Choices {
			fmt.Fprintf(&b, "  - %s: %s\n", choice.Label, choice.Effect)
			if command, ok := choiceCommand(taskID, action, choice); ok {
				fmt.Fprintf(&b, "      cli: %s\n", command)
			} else {
				fmt.Fprintf(&b, "      no CLI command; use the dashboard: %s %s\n", choice.Method, choice.Path)
			}
			if !choice.Allowed {
				b.WriteString("      not permitted for this credential\n")
			}
		}
	}
	if action.IfIgnored != "" {
		fmt.Fprintf(&b, "if ignored:    %s\n", action.IfIgnored)
	}
	deadline := "none"
	if action.Deadline != nil {
		deadline = action.Deadline.UTC().Format(time.RFC3339)
	}
	fmt.Fprintf(&b, "deadline:      %s\n", deadline)
	for _, blocker := range action.BlockedBy {
		fmt.Fprintf(&b, "blocked by:    %s %s (%s)\n", blocker.TaskID, blocker.Title, blocker.State)
	}
	if order := action.CurrentOrder; order != nil {
		line := fmt.Sprintf("%s (%s, %s)", order.ID, order.Stage, order.State)
		if order.RecoveryRefusal != "" {
			line += "; recovery refused: " + order.RecoveryRefusal
		}
		fmt.Fprintf(&b, "current order: %s\n", line)
	}
	if attempt := action.Attempt; attempt != nil {
		var parts []string
		for _, field := range [][2]string{
			{"harness", attempt.Harness}, {"model", attempt.Model}, {"user", attempt.User},
			{"claimed_by", attempt.ClaimedBy}, {"session", attempt.SessionID}, {"attempt", attempt.AttemptID},
			{"progress", attempt.Progress},
		} {
			if field[1] != "" {
				parts = append(parts, field[0]+"="+field[1])
			}
		}
		if attempt.LastCheckIn != nil {
			parts = append(parts, "last_check_in="+attempt.LastCheckIn.UTC().Format(time.RFC3339))
		}
		parts = append(parts, fmt.Sprintf("elapsed=%ds", attempt.ElapsedSeconds))
		fmt.Fprintf(&b, "attempt:       %s\n", strings.Join(parts, " "))
	}
	for _, refusal := range action.Details.Refusals {
		fmt.Fprintf(&b, "refused:       %s: %s\n", refusal.Choice, refusal.Text)
	}
	fmt.Fprintf(&b, "kind:          %s\n", action.Kind)
	for _, field := range [][2]string{
		{"gate kind:     ", action.Details.GateKind},
		{"reason code:   ", action.Details.ReasonCode},
		{"stall reason:  ", action.Details.StallReason},
		{"release:       ", action.Details.ReleaseOutcome},
		{"checkpoint:    ", action.Details.CheckpointClass},
	} {
		if field[1] != "" {
			fmt.Fprintf(&b, "%s%s\n", field[0], field[1])
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// choiceCommand maps one offered choice to its CLI equivalent, keyed on the
// choice id plus its rendered route. It reports false when no command exists.
func choiceCommand(taskID string, action nextActionView, choice nextActionChoice) (string, bool) {
	onTask := strings.HasPrefix(choice.Path, "/v1/tasks/")
	onOrder := strings.HasPrefix(choice.Path, "/v1/work-orders/")
	if command, ok := taskContextChoiceCommand(taskID, choice); ok {
		return command, true
	}
	switch {
	case onTask && strings.HasSuffix(choice.Path, "/review") &&
		(choice.ID == "approve" || choice.ID == "redirect" || choice.ID == "approve_revision" || choice.ID == "decline"):
		return "conveyor run " + taskID, true
	case onTask && choice.ID == "merge":
		return "conveyor task merge " + taskID, true
	case onTask && choice.ID == "request_changes":
		return "conveyor task request-changes " + taskID, true
	case onTask && choice.ID == "start_over":
		return "conveyor task restart " + taskID, true
	case onTask && choice.ID == "close":
		return "conveyor task close " + taskID, true
	case onTask && choice.ID == "remove_dependency":
		dependency := pathSegmentAfter(choice.Path, "dependencies")
		if dependency == "" {
			return "", false
		}
		return "conveyor task unlink " + taskID + " " + dependency, true
	case onOrder && choice.ID == "recover":
		if order := pathSegmentAfter(choice.Path, "work-orders"); order != "" {
			return "conveyor order recover " + order, true
		}
	case onOrder && choice.ID == "answer":
		// A choice that suggests a direction prints it shell-quoted, so the
		// command recovers with the confirmed versions as stated
		// (feature-operator-next-action NA-4, NA-8).
		if order := pathSegmentAfter(choice.Path, "work-orders"); order != "" && choice.Direction != "" {
			return "conveyor order recover " + order + " --direction " + shellWord(choice.Direction), true
		} else if order != "" {
			return "conveyor order recover " + order + ` --direction "<answer>"`, true
		}
	case onTask && strings.HasSuffix(choice.Path, "/redispatch") && (choice.ID == "recover" || choice.ID == "redispatch"):
		if action.CurrentOrder != nil && action.CurrentOrder.ID != "" {
			return "conveyor order redispatch " + action.CurrentOrder.ID, true
		}
	}
	return "", false
}

func pathSegmentAfter(path, segment string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == segment {
			value, err := url.PathUnescape(parts[i+1])
			if err != nil {
				return parts[i+1]
			}
			return value
		}
	}
	return ""
}
