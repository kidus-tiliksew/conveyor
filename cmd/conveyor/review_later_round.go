package main

// conveyor task set-later-round-review changes a task's frozen
// later_round_review policy through POST /v1/tasks/{id}/policy with the
// resolved server, user credential, and workspace (feature-delta-review-rounds
// DR-6; req-review-gates-evidence AC-12.8). The server admits, refuses, and
// replays the change; a refusal is a command failure.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

func taskSetLaterRoundReviewCmd() *cobra.Command {
	var reason string
	var full, delta bool
	cmd := &cobra.Command{
		Use:   "set-later-round-review <task-id> --full|--delta --reason <text>",
		Short: "Change what a task's later review rounds compare",
		Long: "Sets the task's frozen later_round_review policy, with a required operator reason. " +
			"The new value applies to the next implementation submission. " +
			"The change opens no review round and changes no verdict or recorded comparison. " +
			"The server refuses it only for a merged or closed task.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if full == delta {
				return fmt.Errorf("exactly one of --full or --delta is required")
			}
			scope := "full"
			if delta {
				scope = "delta"
			}
			requestID, err := newReviewSeatRequestID()
			if err != nil {
				return err
			}
			return runTaskSetLaterRoundReview(newClient(), cmd.OutOrStdout(), args[0], scope, reason, requestID)
		},
	}
	cmd.Flags().BoolVar(&full, "full", false, "review the whole change in a round after a bounce")
	cmd.Flags().BoolVar(&delta, "delta", false, "review only the changes since the last complete round")
	cmd.Flags().StringVarP(&reason, "reason", "r", "", "operator reason recorded with the change (required)")
	return cmd
}

func runTaskSetLaterRoundReview(c *client, w io.Writer, taskID, scope, reason, requestID string) error {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return fmt.Errorf("task ID is required")
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("--reason is required")
	}
	body, err := json.Marshal(struct {
		LaterRoundReview string `json:"later_round_review"`
		Reason           string `json:"reason"`
		RequestID        string `json:"request_id"`
	}{scope, reason, requestID})
	if err != nil {
		return err
	}
	var result struct {
		RequestID string `json:"request_id"`
	}
	if err = c.do(http.MethodPost, "/v1/tasks/"+url.PathEscape(taskID)+"/policy", body, &result); err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "set later_round_review to %s on task %s (request %s)\n", scope, taskID, result.RequestID)
	return err
}
