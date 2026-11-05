package main

// conveyor task set-distinct-models changes a task's frozen
// review.distinct_models policy through POST /v1/tasks/{id}/policy with the
// resolved server, user credential, and workspace (req-review-gates-evidence
// AC-2.11; component-task-lifecycle). The server admits, refuses, and replays
// the change; a refusal is a command failure.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

func taskSetDistinctModelsCmd() *cobra.Command {
	var reason string
	var on, off bool
	cmd := &cobra.Command{
		Use:   "set-distinct-models <task-id> --off|--on --reason <text>",
		Short: "Change whether a task's review seats must report different model names",
		Long: "Sets the task's frozen review.distinct_models policy for later review seat claims, with a required operator reason. " +
			"The change opens no review round and changes no verdict. The server refuses it while a seat of the task is claimed or holds a submitted verdict.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if on == off {
				return fmt.Errorf("exactly one of --on or --off is required")
			}
			requestID, err := newReviewSeatRequestID()
			if err != nil {
				return err
			}
			return runTaskSetDistinctModels(newClient(), cmd.OutOrStdout(), args[0], on, reason, requestID)
		},
	}
	cmd.Flags().BoolVar(&on, "on", false, "require a different reported model name for each seat of a round")
	cmd.Flags().BoolVar(&off, "off", false, "stop comparing seat model names")
	cmd.Flags().StringVarP(&reason, "reason", "r", "", "operator reason recorded with the change (required)")
	return cmd
}

func runTaskSetDistinctModels(c *client, w io.Writer, taskID string, enabled bool, reason, requestID string) error {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return fmt.Errorf("task ID is required")
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("--reason is required")
	}
	body, err := json.Marshal(struct {
		ReviewDistinctModels bool   `json:"review_distinct_models"`
		Reason               string `json:"reason"`
		RequestID            string `json:"request_id"`
	}{enabled, reason, requestID})
	if err != nil {
		return err
	}
	var result struct {
		RequestID string `json:"request_id"`
	}
	if err = c.do(http.MethodPost, "/v1/tasks/"+url.PathEscape(taskID)+"/policy", body, &result); err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "set review.distinct_models to %t on task %s (request %s)\n", enabled, taskID, result.RequestID)
	return err
}
