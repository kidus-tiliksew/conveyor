package main

// Plan-state filing flags and the two operator plan acts of the CLI
// (component-runtime invariants 40 and 41; req-task-plan-state AC-1.4,
// AC-2.3, AC-3.1, AC-3.2). The server admits, refuses, and replays each act;
// a refusal is a command failure.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// taskIntakePlan carries the plan-state fields `conveyor task new` sends.
// Each field is sent only when its flag was given, so the workspace default
// applies to an omitted plan state.
type taskIntakePlan struct {
	PlanState   string
	Plan        *string
	PlanBase    string
	ApprovePlan bool
}

// cliPlanState maps the CLI spelling needed or not-required to the wire
// value needed or not_required.
func cliPlanState(value string) (core.PlanState, error) {
	switch strings.TrimSpace(value) {
	case "needed":
		return core.PlanNeeded, nil
	case "not-required":
		return core.PlanNotRequired, nil
	}
	return "", fmt.Errorf("plan state must be needed or not-required, got %q", value)
}

// readPlanFile reads the plan from path, or from standard input for "-".
func readPlanFile(path string, stdin io.Reader) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("--plan-file requires a path or -")
	}
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return "", fmt.Errorf("read plan file: %w", err)
	}
	return string(data), nil
}

// resolveTaskIntakePlan applies the client-side refusals of
// component-runtime invariant 40 before any request: --plan-base or
// --approve-plan without --plan-file, and --plan-state with it.
func resolveTaskIntakePlan(flags interface{ Changed(string) bool }, planState, planFile, planBase string, approvePlan bool, stdin io.Reader) (taskIntakePlan, error) {
	var intake taskIntakePlan
	hasPlanFile := flags.Changed("plan-file")
	if !hasPlanFile && flags.Changed("plan-base") {
		return intake, fmt.Errorf("--plan-base requires --plan-file")
	}
	if !hasPlanFile && flags.Changed("approve-plan") {
		return intake, fmt.Errorf("--approve-plan requires --plan-file")
	}
	if hasPlanFile && flags.Changed("plan-state") {
		return intake, fmt.Errorf("--plan-state cannot be combined with --plan-file; a filed plan sets the plan state")
	}
	if flags.Changed("plan-state") {
		state, err := cliPlanState(planState)
		if err != nil {
			return intake, err
		}
		intake.PlanState = string(state)
	}
	if hasPlanFile {
		plan, err := readPlanFile(planFile, stdin)
		if err != nil {
			return intake, err
		}
		intake.Plan = &plan
		intake.PlanBase = strings.TrimSpace(planBase)
		intake.ApprovePlan = approvePlan
	}
	return intake, nil
}

// apply adds the given plan-state fields to a task intake payload.
func (p taskIntakePlan) apply(payload map[string]any) {
	if p.PlanState != "" {
		payload["plan_state"] = p.PlanState
	}
	if p.Plan != nil {
		payload["plan"] = *p.Plan
	}
	if p.PlanBase != "" {
		payload["plan_base_commit"] = p.PlanBase
	}
	if p.ApprovePlan {
		payload["plan_approved"] = true
	}
}

func taskPlanStateCmd() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "plan-state <task-id> needed|not-required --reason <text>",
		Short: "Set whether a task requires a plan",
		Long: "Sets the task's plan state to needed or not-required with a required operator reason. " +
			"The server opens or cancels the task's plan work order to match, keeps every earlier plan version, " +
			"and refuses the change while an implementation work order of the task is claimed.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			requestID, err := newReviewSeatRequestID()
			if err != nil {
				return err
			}
			return runTaskSetPlanState(newClient(), cmd.OutOrStdout(), args[0], args[1], reason, requestID)
		},
	}
	cmd.Flags().StringVarP(&reason, "reason", "r", "", "operator reason recorded with the change (required)")
	return cmd
}

func runTaskSetPlanState(c *client, w io.Writer, taskID, value, reason, requestID string) error {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return fmt.Errorf("task ID is required")
	}
	state, err := cliPlanState(value)
	if err != nil {
		return err
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("--reason is required")
	}
	body, err := json.Marshal(struct {
		PlanState core.PlanState `json:"plan_state"`
		Reason    string         `json:"reason"`
		RequestID string         `json:"request_id"`
	}{state, reason, requestID})
	if err != nil {
		return err
	}
	var result struct {
		RequestID string `json:"request_id"`
	}
	if err = c.do(http.MethodPost, "/v1/tasks/"+url.PathEscape(taskID)+"/policy", body, &result); err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "set plan_state to %s on task %s (request %s)\n", state, taskID, result.RequestID)
	return err
}

func taskAttachPlanCmd() *cobra.Command {
	var reason, planFile, planBase string
	cmd := &cobra.Command{
		Use:   "attach-plan <task-id> --plan-file <path|-> [--plan-base <commit>] --reason <text>",
		Short: "Attach an execution plan to a task for approval",
		Long: "Records the plan as the task's next plan version and sets its plan state to plan awaiting approval. " +
			"Approval comes only through the plan approval gate. The server refuses the act unless the task's plan state is " +
			"needed or not-required and no implementation work order of the task is claimed.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("plan-file") {
				return fmt.Errorf("--plan-file is required")
			}
			if strings.TrimSpace(reason) == "" {
				return fmt.Errorf("--reason is required")
			}
			plan, err := readPlanFile(planFile, cmd.InOrStdin())
			if err != nil {
				return err
			}
			requestID, err := newReviewSeatRequestID()
			if err != nil {
				return err
			}
			return runTaskAttachPlan(newClient(), cmd.OutOrStdout(), args[0], plan, planBase, reason, requestID)
		},
	}
	cmd.Flags().StringVar(&planFile, "plan-file", "", "file holding the execution plan, or - for standard input (required)")
	cmd.Flags().StringVar(&planBase, "plan-base", "", "full commit ID the plan was read against")
	cmd.Flags().StringVarP(&reason, "reason", "r", "", "operator reason recorded with the act (required)")
	return cmd
}

func runTaskAttachPlan(c *client, w io.Writer, taskID, plan, planBase, reason, requestID string) error {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return fmt.Errorf("task ID is required")
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("--reason is required")
	}
	payload := map[string]any{"plan": plan, "reason": reason, "request_id": requestID}
	if base := strings.TrimSpace(planBase); base != "" {
		payload["base_commit"] = base
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var result struct {
		RequestID   string `json:"request_id"`
		PlanVersion int    `json:"plan_version"`
	}
	if err = c.do(http.MethodPost, "/v1/tasks/"+url.PathEscape(taskID)+"/plan", body, &result); err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "attached plan version %d to task %s for approval (request %s)\n", result.PlanVersion, taskID, result.RequestID)
	return err
}
