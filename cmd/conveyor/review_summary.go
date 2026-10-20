package main

// conveyor task review-summary records an informative summary of a settled
// review round through POST /v1/tasks/{id}/review-rounds/{round}/summaries
// with the resolved user credential and workspace (req-review-gates-evidence
// AC-10.3, AC-10.4; DEC-79; feature-review-round-summary SU-4). The server
// admits, validates, and versions the summary; the CLI grants nothing and a
// server refusal is a command failure.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

func taskReviewSummaryCmd() *cobra.Command {
	var round int
	var file, requestID, authorModel string
	cmd := &cobra.Command{
		Use:   "review-summary <task-id> --round <n> --file <path|-> --request-id <id> [--author-model <model>]",
		Short: "Record an informative summary of a settled review round",
		Long: "Reads {\"summary\": ..., \"findings\": [{\"severity\", \"problem\", \"location\", \"fix\", \"seats\"}]} JSON from --file, or from stdin when --file is -, " +
			"and posts it with --request-id and the optional --author-model to POST /v1/tasks/{id}/review-rounds/{round}/summaries. " +
			"The summary changes no verdict, bounce, gate, or work order. The round must have settled, and every seat that requested changes must be cited by a finding. " +
			"Repeating a request ID with identical content returns the recorded version.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTaskReviewSummary(newClient(), cmd.InOrStdin(), cmd.OutOrStdout(), args[0], round, file, requestID, authorModel)
		},
	}
	cmd.Flags().IntVar(&round, "round", 0, "settled review round number (required)")
	cmd.Flags().StringVar(&file, "file", "", "path to the {summary, findings} JSON, or - for stdin (required)")
	cmd.Flags().StringVar(&requestID, "request-id", "", "idempotency key for this summary (required)")
	cmd.Flags().StringVar(&authorModel, "author-model", "", "self-reported model that wrote the summary")
	for _, name := range []string{"round", "file", "request-id"} {
		_ = cmd.MarkFlagRequired(name)
	}
	return cmd
}

// reviewSummaryFileInput is exactly the {summary, findings} input document.
type reviewSummaryFileInput struct {
	Summary  string          `json:"summary"`
	Findings json.RawMessage `json:"findings"`
}

func readReviewSummaryInput(stdin io.Reader, file string) (reviewSummaryFileInput, error) {
	var raw []byte
	var err error
	if file == "-" {
		raw, err = io.ReadAll(stdin)
	} else {
		raw, err = os.ReadFile(file)
	}
	if err != nil {
		return reviewSummaryFileInput{}, fmt.Errorf("read review summary input: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var input reviewSummaryFileInput
	if err = decoder.Decode(&input); err != nil {
		return reviewSummaryFileInput{}, fmt.Errorf("review summary input must be one {summary, findings} JSON object: %w", err)
	}
	if _, err = decoder.Token(); !errors.Is(err, io.EOF) {
		return reviewSummaryFileInput{}, fmt.Errorf("review summary input must be one {summary, findings} JSON object")
	}
	if len(bytes.TrimSpace(input.Findings)) == 0 || bytes.Equal(bytes.TrimSpace(input.Findings), []byte("null")) {
		input.Findings = json.RawMessage(`[]`)
	}
	return input, nil
}

func runTaskReviewSummary(c *client, stdin io.Reader, w io.Writer, taskID string, round int, file, requestID, authorModel string) error {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return fmt.Errorf("task ID is required")
	}
	if strings.TrimSpace(requestID) == "" {
		return fmt.Errorf("--request-id is required")
	}
	if file == "" {
		return fmt.Errorf("--file is required (use - for stdin)")
	}
	input, err := readReviewSummaryInput(stdin, file)
	if err != nil {
		return err
	}
	body, err := json.Marshal(struct {
		RequestID   string          `json:"request_id"`
		Summary     string          `json:"summary"`
		Findings    json.RawMessage `json:"findings"`
		AuthorModel string          `json:"author_model,omitempty"`
	}{requestID, input.Summary, input.Findings, authorModel})
	if err != nil {
		return err
	}
	var result json.RawMessage
	path := "/v1/tasks/" + url.PathEscape(taskID) + "/review-rounds/" + strconv.Itoa(round) + "/summaries"
	if err = c.do(http.MethodPost, path, body, &result); err != nil {
		return err
	}
	var pretty bytes.Buffer
	if err = json.Indent(&pretty, result, "", "  "); err != nil {
		return fmt.Errorf("decode review summary result: %w", err)
	}
	_, err = fmt.Fprintln(w, pretty.String())
	return err
}
