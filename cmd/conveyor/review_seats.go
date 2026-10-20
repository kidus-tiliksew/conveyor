package main

// conveyor task add-review-seats adds operator review seats to a task's open
// review round through POST /v1/tasks/{id}/review-round/seats with the
// resolved server, user credential, and workspace (feature-review-seat-minimum
// RS-5; req-review-gates-evidence AC-2.5, AC-2.6). The server admits, refuses,
// and replays the addition; a refusal is a command failure.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

type reviewSeatAdditionResult struct {
	RequestID      string `json:"request_id"`
	TaskID         string `json:"task_id"`
	ReviewRound    int    `json:"review_round"`
	PriorSeatCount int    `json:"prior_seat_count"`
	SeatCount      int    `json:"seat_count"`
}

func taskAddReviewSeatsCmd() *cobra.Command {
	var reason string
	var count int
	cmd := &cobra.Command{
		Use:   "add-review-seats <task-id> --reason <text> [--count N]",
		Short: "Add reviewer seats to a task's open review round",
		Long: "Adds --count reviewer seats (default 1) to the task's latest review round while it is unsettled, with a required operator reason. " +
			"The round then waits for every added seat's verdict. The server refuses a task without an open round and a settled round. No command removes a seat.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			requestID, err := newReviewSeatRequestID()
			if err != nil {
				return err
			}
			return runTaskAddReviewSeats(newClient(), cmd.OutOrStdout(), args[0], reason, count, requestID)
		},
	}
	cmd.Flags().StringVarP(&reason, "reason", "r", "", "operator reason recorded with the addition (required)")
	cmd.Flags().IntVar(&count, "count", 1, "number of seats to add")
	return cmd
}

// newReviewSeatRequestID generates one idempotency key per invocation.
func newReviewSeatRequestID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(random[:]), nil
}

func runTaskAddReviewSeats(c *client, w io.Writer, taskID, reason string, count int, requestID string) error {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return fmt.Errorf("task ID is required")
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("--reason is required")
	}
	if count < 1 {
		return fmt.Errorf("--count must be at least 1")
	}
	if strings.TrimSpace(requestID) == "" {
		return fmt.Errorf("request ID is required")
	}
	body, err := json.Marshal(struct {
		RequestID string `json:"request_id"`
		Reason    string `json:"reason"`
		Add       int    `json:"add"`
	}{requestID, reason, count})
	if err != nil {
		return err
	}
	var result reviewSeatAdditionResult
	if err = c.do(http.MethodPost, "/v1/tasks/"+url.PathEscape(taskID)+"/review-round/seats", body, &result); err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "added %d review seat(s) to round %d of task %s: %d -> %d seats (request %s)\n",
		result.SeatCount-result.PriorSeatCount, result.ReviewRound, result.TaskID, result.PriorSeatCount, result.SeatCount, result.RequestID)
	return err
}
