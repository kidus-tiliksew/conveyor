package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/spf13/cobra"
)

// conveyor task wait blocks one CLI process until a task's observed delivery
// position moves, the task reaches a terminal state, or the timeout elapses
// (req-agent-skills REQ-4, AC-4.1 through AC-4.3; DEC-45; component-runtime).
// It reads the same task and run-order projections conveyor run uses and
// never mutates the task.

const (
	taskWaitDefaultTimeout    = 5 * time.Minute
	taskWaitTimeoutExitStatus = 2

	taskWaitReasonChanged  = "changed"
	taskWaitReasonTerminal = "terminal"
	taskWaitReasonTimeout  = "timeout"
)

const taskWaitLong = `Block until a task's observed state changes, the task reaches a terminal
state, or the timeout elapses.

Each observation holds the task state, the pending human gate (plan, merge,
plan revision, or human recovery, with the plan version when one applies),
the next claimable work order ID and stage, and the pending task-authored
proposals (kind, document ID, and version). The command reads
GET /v1/tasks/{id} and GET /v1/tasks/{id}/run-order with the ordinary server,
workspace, and credential resolution. It re-reads with jittered backoff from
250ms to a 2s ceiling and returns when any observed field differs from the
first observation. A merged, closed, or parked task returns at once, at start
or during the wait.

Transport errors and server 5xx responses after the first observation are
retried within the timeout. Authentication, authorization, not-found, and
malformed responses end the wait at once. A failed read never replaces the
last successful observation.

Output reports the current observation and why the wait ended: changed,
terminal, or timeout. --json prints the same fields as one JSON object. On
timeout, last_read_error names the most recent failed read when the final
poll failed.

Exit status:
  0  the observation changed or the task is merged, closed, or parked
  2  the timeout elapsed; output reports the last successful observation
  1  failure: invalid arguments, configuration, authentication, not found,
     malformed response, or a failed first observation`

// taskWaitTimeoutError is returned after a timeout result is printed. main()
// maps it to exit status 2 without an extra error message.
type taskWaitTimeoutError struct{}

func (*taskWaitTimeoutError) Error() string { return "task wait timed out" }

type taskWaitGate struct {
	Kind        string `json:"kind"`
	PlanVersion int    `json:"plan_version,omitempty"`
}

type taskWaitOrder struct {
	ID    string     `json:"id"`
	Stage core.Stage `json:"stage"`
}

type taskWaitProposal struct {
	Kind       string `json:"kind"`
	DocumentID string `json:"document_id"`
	Version    int    `json:"version"`
}

// taskWaitObservation holds only the compared semantic fields plus the read
// time. Titles, labels, capability flags, and progress are excluded so that
// presentation churn never ends a wait.
type taskWaitObservation struct {
	TaskID           string             `json:"task_id"`
	State            core.TaskState     `json:"state"`
	PendingGate      *taskWaitGate      `json:"pending_gate"`
	NextOrder        *taskWaitOrder     `json:"next_order"`
	PendingProposals []taskWaitProposal `json:"pending_proposals"`
	ObservedAt       time.Time          `json:"observed_at"`
}

type taskWaitResult struct {
	taskWaitObservation
	Reason        string `json:"reason"`
	TimedOut      bool   `json:"timed_out"`
	LastReadError string `json:"last_read_error,omitempty"`
}

type taskWaitOptions struct {
	timeout   time.Duration
	newPoller func() *runAdaptivePoller
	now       func() time.Time
}

func taskWaitCmd() *cobra.Command {
	timeout := taskWaitDefaultTimeout
	asJSON := false
	cmd := &cobra.Command{
		Use:   "wait <task-id>",
		Short: "Block until a task's state, gate, next order, or proposals change",
		Long:  taskWaitLong,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTaskWait(cmd.Context(), newClient(), cmd.OutOrStdout(), args[0], asJSON, taskWaitOptions{timeout: timeout})
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", taskWaitDefaultTimeout, "maximum wait; must be positive")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the result as one JSON object")
	return cmd
}

func runTaskWait(ctx context.Context, c *client, output io.Writer, taskID string, asJSON bool, options taskWaitOptions) error {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return fmt.Errorf("task ID is required")
	}
	if options.timeout <= 0 {
		return fmt.Errorf("--timeout must be positive, got %s", options.timeout)
	}
	if c.configErr != nil {
		return c.configErr
	}
	if options.newPoller == nil {
		options.newPoller = newRunAdaptivePoller
	}
	if options.now == nil {
		options.now = time.Now
	}
	result, err := waitForTask(ctx, c, taskID, options)
	if err != nil {
		return err
	}
	if err := writeTaskWaitResult(output, result, options.timeout, asJSON); err != nil {
		return err
	}
	if result.TimedOut {
		return &taskWaitTimeoutError{}
	}
	return nil
}

// waitForTask bounds every request and sleep by one timeout context. The first
// successful observation is the fixed baseline; caller cancellation is
// reported as a failure, distinct from the command's own timeout.
func waitForTask(ctx context.Context, c *client, taskID string, options taskWaitOptions) (taskWaitResult, error) {
	waitCtx, cancel := context.WithTimeout(ctx, options.timeout)
	defer cancel()

	baseline, err := c.observeTaskWait(waitCtx, taskID, options.now)
	if err != nil {
		if ctx.Err() != nil {
			return taskWaitResult{}, ctx.Err()
		}
		if waitCtx.Err() != nil {
			return taskWaitResult{}, fmt.Errorf("wait for task %s: first observation did not complete within %s: %w", taskID, options.timeout, err)
		}
		return taskWaitResult{}, c.taskWaitFailure(taskID, err)
	}
	if taskWaitTerminal(baseline.State) {
		return taskWaitResult{taskWaitObservation: baseline, Reason: taskWaitReasonTerminal}, nil
	}

	latest := baseline
	var lastErr error
	poller := options.newPoller()
	for {
		timer := time.NewTimer(poller.delay())
		select {
		case <-waitCtx.Done():
			timer.Stop()
			return taskWaitDeadline(ctx, latest, lastErr)
		case <-timer.C:
		}
		current, err := c.observeTaskWait(waitCtx, taskID, options.now)
		if err != nil {
			if waitCtx.Err() != nil {
				return taskWaitDeadline(ctx, latest, lastErr)
			}
			if !transientWorkerError(err) {
				return taskWaitResult{}, c.taskWaitFailure(taskID, err)
			}
			lastErr = err
			poller.observe(false)
			continue
		}
		latest, lastErr = current, nil
		if taskWaitTerminal(current.State) {
			return taskWaitResult{taskWaitObservation: current, Reason: taskWaitReasonTerminal}, nil
		}
		if !current.sameAs(baseline) {
			return taskWaitResult{taskWaitObservation: current, Reason: taskWaitReasonChanged}, nil
		}
		poller.observe(false)
	}
}

func taskWaitDeadline(ctx context.Context, latest taskWaitObservation, lastErr error) (taskWaitResult, error) {
	if ctx.Err() != nil {
		return taskWaitResult{}, ctx.Err()
	}
	result := taskWaitResult{taskWaitObservation: latest, Reason: taskWaitReasonTimeout, TimedOut: true}
	if lastErr != nil {
		result.LastReadError = lastErr.Error()
	}
	return result, nil
}

// observeTaskWait reads the task and its run-order projection. The task state
// comes from the later run-order response so state, gate, and order describe
// the same read.
func (c *client) observeTaskWait(ctx context.Context, taskID string, now func() time.Time) (taskWaitObservation, error) {
	var task core.Task
	if err := c.workerDoContext(ctx, http.MethodGet, "/v1/tasks/"+url.PathEscape(taskID), nil, &task, c.token); err != nil {
		return taskWaitObservation{}, err
	}
	if strings.TrimSpace(task.ID) == "" {
		return taskWaitObservation{}, fmt.Errorf("task read returned no task")
	}
	run, err := c.getTaskRunOrderContext(ctx, c.token, taskID)
	if err != nil {
		return taskWaitObservation{}, err
	}
	if run == nil {
		return taskWaitObservation{}, fmt.Errorf("run-order read returned no task")
	}
	state := run.Task.State
	if state == "" {
		state = task.State
	}
	if state == "" {
		return taskWaitObservation{}, fmt.Errorf("task read returned no state")
	}
	observation := taskWaitObservation{TaskID: task.ID, State: state, PendingProposals: []taskWaitProposal{}, ObservedAt: now().UTC()}
	if gate := run.Gate; gate != nil && strings.TrimSpace(gate.Kind) != "" {
		normalized := taskWaitGate{Kind: gate.Kind, PlanVersion: gate.PlanVersion}
		if gate.Kind == "spec" {
			normalized = taskWaitGate{Kind: "plan", PlanVersion: gate.SpecVersion}
		}
		observation.PendingGate = &normalized
	}
	if strings.TrimSpace(run.Order.ID) != "" {
		observation.NextOrder = &taskWaitOrder{ID: run.Order.ID, Stage: run.Order.Stage}
	}
	seen := map[taskWaitProposal]bool{}
	for _, proposal := range run.PendingProposals {
		identity := taskWaitProposal{Kind: proposal.Kind, DocumentID: proposal.DocumentID, Version: proposal.Version}
		if !seen[identity] {
			seen[identity] = true
			observation.PendingProposals = append(observation.PendingProposals, identity)
		}
	}
	sort.Slice(observation.PendingProposals, func(i, j int) bool {
		a, b := observation.PendingProposals[i], observation.PendingProposals[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.DocumentID != b.DocumentID {
			return a.DocumentID < b.DocumentID
		}
		return a.Version < b.Version
	})
	return observation, nil
}

func (o taskWaitObservation) sameAs(other taskWaitObservation) bool {
	if o.State != other.State || (o.PendingGate == nil) != (other.PendingGate == nil) || (o.NextOrder == nil) != (other.NextOrder == nil) {
		return false
	}
	if o.PendingGate != nil && *o.PendingGate != *other.PendingGate {
		return false
	}
	if o.NextOrder != nil && *o.NextOrder != *other.NextOrder {
		return false
	}
	if len(o.PendingProposals) != len(other.PendingProposals) {
		return false
	}
	for i := range o.PendingProposals {
		if o.PendingProposals[i] != other.PendingProposals[i] {
			return false
		}
	}
	return true
}

func taskWaitTerminal(state core.TaskState) bool {
	return state == core.TaskMerged || state == core.TaskClosed || state == core.TaskParked
}

func (c *client) taskWaitFailure(taskID string, err error) error {
	var response *workerHTTPError
	if errors.As(err, &response) {
		switch response.StatusCode {
		case http.StatusUnauthorized:
			return fmt.Errorf("wait for task %s: %w (%s)", taskID, err, c.credentialDiagnostic())
		case http.StatusNotFound:
			return fmt.Errorf("wait for task %s: task not found: %w", taskID, err)
		}
	}
	return fmt.Errorf("wait for task %s: %w", taskID, err)
}

func writeTaskWaitResult(w io.Writer, result taskWaitResult, timeout time.Duration, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(w).Encode(result)
	}
	gate := "none"
	if result.PendingGate != nil {
		gate = result.PendingGate.Kind
		if result.PendingGate.PlanVersion > 0 {
			gate += fmt.Sprintf(" v%d", result.PendingGate.PlanVersion)
		}
	}
	order := "none"
	if result.NextOrder != nil {
		order = fmt.Sprintf("%s (%s)", result.NextOrder.ID, result.NextOrder.Stage)
	}
	proposals := "none"
	if len(result.PendingProposals) > 0 {
		names := make([]string, 0, len(result.PendingProposals))
		for _, proposal := range result.PendingProposals {
			names = append(names, fmt.Sprintf("%s %s v%d", proposal.Kind, proposal.DocumentID, proposal.Version))
		}
		proposals = strings.Join(names, ", ")
	}
	ended := result.Reason
	if result.TimedOut {
		ended = fmt.Sprintf("%s after %s; the values above are the last successful observation", result.Reason, timeout)
	}
	lines := []string{
		fmt.Sprintf("task:              %s", result.TaskID),
		fmt.Sprintf("state:             %s", result.State),
		fmt.Sprintf("pending gate:      %s", gate),
		fmt.Sprintf("next order:        %s", order),
		fmt.Sprintf("pending proposals: %s", proposals),
		fmt.Sprintf("observed at:       %s", result.ObservedAt.Format(time.RFC3339)),
		fmt.Sprintf("wait ended:        %s", ended),
		fmt.Sprintf("timed out:         %t", result.TimedOut),
	}
	if result.LastReadError != "" {
		lines = append(lines, fmt.Sprintf("last read error:   %s", result.LastReadError))
	}
	_, err := fmt.Fprintln(w, strings.Join(lines, "\n"))
	return err
}
