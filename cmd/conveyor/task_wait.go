package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/spf13/cobra"
)

// conveyor task wait blocks one CLI process until a task's observed delivery
// position moves, the task reaches a terminal state, or the timeout elapses
// (req-agent-skills REQ-4, AC-4.1 through AC-4.3; DEC-84;
// component-cli-onboarding). It reads only routes a self-claimed session's
// credential holds: the task, the workspace work-order listing, and, at a gate,
// the latest plan or the task activity. It reads no run-order or worker route
// (DEC-97(1)) and never mutates the task. A pending document proposal is no task
// gate, so it never enters an observation (req-260810-23b69f REQ-3).

const (
	taskWaitDefaultTimeout    = 5 * time.Minute
	taskWaitTimeoutExitStatus = 2

	taskWaitReasonChanged  = "changed"
	taskWaitReasonTerminal = "terminal"
	taskWaitReasonTimeout  = "timeout"
	taskWaitReasonReached  = "reached"
)

const taskWaitLong = `Block until a task's observed state changes, the task reaches a terminal
state, or the timeout elapses.

Each observation holds the task state, the pending human gate (plan, merge,
merge execution, plan revision, or human recovery, with the plan version when
one applies and the merge readiness state for merge execution), and the next
claimable work order ID and stage. The command reads GET /v1/tasks/{id} and
GET /v1/work-orders, plus GET /v1/tasks/{id}/spec at a plan or plan-revision
gate and GET /v1/tasks/{id}/activity at a merge-execution gate, with the
ordinary server, workspace, and credential resolution. It re-reads with jittered backoff from
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

--until operator|terminal|stage=<stage> instead waits for a target. Each poll
decodes the next_action embedded in GET /v1/tasks/{id}; the wait ends with
reason reached when next_action.actor is operator, when the task is merged,
closed, or parked (terminal), or when next_action.current_order.stage equals
the named work-order stage (spec, implement, verify, review). A task that turns
terminal before another target ends with reason terminal. Every observed
change of actor, kind, or current order stage prints one line:
<observed_at> <label>: actor=<actor> kind=<kind> stage=<stage>
(to stderr under --json). Label-only changes print nothing.

Exit status:
  0  the observation changed or the task is merged, closed, or parked; with
     --until, the target was reached or the task is merged, closed, or parked
  2  the timeout elapsed; output reports the last successful observation
  1  failure: invalid arguments (including an invalid --until), configuration,
     authentication, not found, malformed response, or a failed first
     observation`

// taskWaitTimeoutError is returned after a timeout result is printed. main()
// maps it to exit status 2 without an extra error message.
type taskWaitTimeoutError struct{}

func (*taskWaitTimeoutError) Error() string { return "task wait timed out" }

type taskWaitGate struct {
	Kind        string `json:"kind"`
	PlanVersion int    `json:"plan_version,omitempty"`
	// MergeReadiness is the readiness state of a merge_execution gate, empty
	// when the server could not read readiness. A change ends the wait
	// (req-agent-skills AC-4.1; component-cli-onboarding).
	MergeReadiness string `json:"merge_readiness,omitempty"`
}

type taskWaitOrder struct {
	ID    string     `json:"id"`
	Stage core.Stage `json:"stage"`
}

// taskWaitNextAction is the part of the server's next_action a --until wait
// compares and prints. It is set only when --until is given.
type taskWaitNextAction struct {
	Label string `json:"label"`
	Actor string `json:"actor"`
	Kind  string `json:"kind"`
	Stage string `json:"stage,omitempty"`
}

// taskWaitObservation holds only the compared semantic fields plus the read
// time. Titles, labels, capability flags, and progress are excluded so that
// presentation churn never ends a wait.
type taskWaitObservation struct {
	TaskID      string              `json:"task_id"`
	State       core.TaskState      `json:"state"`
	PendingGate *taskWaitGate       `json:"pending_gate"`
	NextOrder   *taskWaitOrder      `json:"next_order"`
	NextAction  *taskWaitNextAction `json:"next_action,omitempty"`
	ObservedAt  time.Time           `json:"observed_at"`
}

type taskWaitResult struct {
	taskWaitObservation
	Reason        string `json:"reason"`
	TimedOut      bool   `json:"timed_out"`
	Until         string `json:"until,omitempty"`
	LastReadError string `json:"last_read_error,omitempty"`
}

type taskWaitOptions struct {
	timeout   time.Duration
	newPoller func() *taskWaitPoller
	now       func() time.Time
	// until is the raw --until value; empty keeps the change-detection wait.
	until string
	// progress receives --until change lines; nil means the command output.
	progress io.Writer
}

// taskWaitTarget is a parsed --until value.
type taskWaitTarget struct {
	raw   string
	actor string
	stage core.Stage
	// terminal is true for --until terminal.
	terminal bool
}

func parseTaskWaitTarget(raw string) (taskWaitTarget, error) {
	target := taskWaitTarget{raw: raw}
	switch {
	case raw == "operator":
		target.actor = "operator"
	case raw == "terminal":
		target.terminal = true
	case strings.HasPrefix(raw, "stage="):
		target.stage = core.Stage(strings.TrimPrefix(raw, "stage="))
		if !core.ValidWorkOrderStage(target.stage) {
			return taskWaitTarget{}, fmt.Errorf("--until stage=%s: unknown work-order stage (want spec, implement, verify, or review)", target.stage)
		}
	default:
		return taskWaitTarget{}, fmt.Errorf("--until must be operator, terminal, or stage=<stage>, got %q", raw)
	}
	return target, nil
}

func (t taskWaitTarget) reached(o taskWaitObservation) bool {
	if o.NextAction == nil {
		return false
	}
	switch {
	case t.actor != "":
		return o.NextAction.Actor == t.actor
	case t.stage != "":
		return o.NextAction.Stage == string(t.stage)
	}
	return false
}

func taskWaitCmd() *cobra.Command {
	timeout := taskWaitDefaultTimeout
	asJSON := false
	until := ""
	cmd := &cobra.Command{
		Use:   "wait <task-id>",
		Short: "Block until a task's state, gate, or next order changes",
		Long:  taskWaitLong,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			options := taskWaitOptions{timeout: timeout, until: until}
			if asJSON {
				options.progress = cmd.ErrOrStderr()
			}
			return runTaskWait(cmd.Context(), newClient(), cmd.OutOrStdout(), args[0], asJSON, options)
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", taskWaitDefaultTimeout, "maximum wait; must be positive")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the result as one JSON object")
	cmd.Flags().StringVar(&until, "until", "", "wait for a target: operator, terminal, or stage=<stage>")
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
	var target *taskWaitTarget
	if options.until != "" {
		parsed, err := parseTaskWaitTarget(options.until)
		if err != nil {
			return err
		}
		target = &parsed
	}
	if c.configErr != nil {
		return c.configErr
	}
	if options.newPoller == nil {
		options.newPoller = newTaskWaitPoller
	}
	if options.now == nil {
		options.now = time.Now
	}
	if options.progress == nil {
		options.progress = output
	}
	var result taskWaitResult
	var err error
	if target != nil {
		result, err = waitForTaskTarget(ctx, c, taskID, *target, options)
	} else {
		result, err = waitForTask(ctx, c, taskID, options)
	}
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

	baseline, err := c.observeTaskWait(waitCtx, taskID, options.now, false)
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
		current, err := c.observeTaskWait(waitCtx, taskID, options.now, false)
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

// waitForTaskTarget is the --until form of waitForTask: the same bounded
// reads and retries, but the wait ends only on the target or a terminal task,
// and each change of actor, kind, or stage prints one progress line.
func waitForTaskTarget(ctx context.Context, c *client, taskID string, target taskWaitTarget, options taskWaitOptions) (taskWaitResult, error) {
	waitCtx, cancel := context.WithTimeout(ctx, options.timeout)
	defer cancel()

	finish := func(o taskWaitObservation, reason string, lastErr error, timedOut bool) (taskWaitResult, error) {
		result := taskWaitResult{taskWaitObservation: o, Reason: reason, TimedOut: timedOut, Until: target.raw}
		if timedOut && lastErr != nil {
			result.LastReadError = lastErr.Error()
		}
		return result, nil
	}
	var printed *taskWaitNextAction
	settle := func(o taskWaitObservation) (string, error) {
		if printed == nil || o.NextAction.Actor != printed.Actor || o.NextAction.Kind != printed.Kind || o.NextAction.Stage != printed.Stage {
			printed = o.NextAction
			stage := o.NextAction.Stage
			if stage == "" {
				stage = "none"
			}
			if _, err := fmt.Fprintf(options.progress, "%s %s: actor=%s kind=%s stage=%s\n", o.ObservedAt.Format(time.RFC3339), o.NextAction.Label, o.NextAction.Actor, o.NextAction.Kind, stage); err != nil {
				return "", err
			}
		}
		switch {
		case taskWaitTerminal(o.State) && target.terminal:
			return taskWaitReasonReached, nil
		case taskWaitTerminal(o.State):
			return taskWaitReasonTerminal, nil
		case target.reached(o):
			return taskWaitReasonReached, nil
		}
		return "", nil
	}

	latest, err := c.observeTaskWait(waitCtx, taskID, options.now, true)
	if err != nil {
		if ctx.Err() != nil {
			return taskWaitResult{}, ctx.Err()
		}
		if waitCtx.Err() != nil {
			return taskWaitResult{}, fmt.Errorf("wait for task %s: first observation did not complete within %s: %w", taskID, options.timeout, err)
		}
		return taskWaitResult{}, c.taskWaitFailure(taskID, err)
	}
	if reason, err := settle(latest); err != nil || reason != "" {
		if err != nil {
			return taskWaitResult{}, err
		}
		return finish(latest, reason, nil, false)
	}
	var lastErr error
	poller := options.newPoller()
	for {
		timer := time.NewTimer(poller.delay())
		select {
		case <-waitCtx.Done():
			timer.Stop()
			if ctx.Err() != nil {
				return taskWaitResult{}, ctx.Err()
			}
			return finish(latest, taskWaitReasonTimeout, lastErr, true)
		case <-timer.C:
		}
		current, err := c.observeTaskWait(waitCtx, taskID, options.now, true)
		if err != nil {
			if waitCtx.Err() != nil {
				if ctx.Err() != nil {
					return taskWaitResult{}, ctx.Err()
				}
				return finish(latest, taskWaitReasonTimeout, lastErr, true)
			}
			if !transientWorkerError(err) {
				return taskWaitResult{}, c.taskWaitFailure(taskID, err)
			}
			lastErr = err
			poller.observe(false)
			continue
		}
		latest, lastErr = current, nil
		reason, err := settle(current)
		if err != nil {
			return taskWaitResult{}, err
		}
		if reason != "" {
			return finish(current, reason, nil, false)
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

// taskWaitPoller spaces the reads of one wait: jittered delays from 250 ms
// doubling to a 2 s ceiling, so a real transition is observed within that
// ceiling and concurrent waits do not synchronize (component-cli-onboarding).
type taskWaitPoller struct {
	base, maximum, current time.Duration
	jitter                 func(time.Duration, time.Duration) time.Duration
}

const taskWaitPollInterval = 250 * time.Millisecond

func newTaskWaitPoller() *taskWaitPoller {
	return &taskWaitPoller{base: taskWaitPollInterval, maximum: taskWaitPollInterval * 8, current: taskWaitPollInterval, jitter: taskWaitPollJitter}
}

func taskWaitPollJitter(delay, maximum time.Duration) time.Duration {
	spread := delay / 10
	if spread <= 0 {
		return delay
	}
	delay += time.Duration(rand.Int64N(int64(spread)*2+1)) - spread
	if delay > maximum {
		return maximum
	}
	return delay
}

func (p *taskWaitPoller) delay() time.Duration { return p.jitter(p.current, p.maximum) }

func (p *taskWaitPoller) observe(changed bool) {
	if changed {
		p.current = p.base
		return
	}
	p.current = min(p.current*2, p.maximum)
}

const taskWaitGateMergeExecution = "merge_execution"

// taskWaitStageOrder ranks claimable stages the way a session works them.
var taskWaitStageOrder = map[core.Stage]int{core.StageSpec: 0, core.StageImplement: 1, core.StageVerify: 2, core.StageReview: 3}

// taskWaitAwaitingGate maps the next action of an awaiting_human task to the
// reported gate kind; any other kind is a human recovery gate.
func taskWaitAwaitingGate(kind string) string {
	switch kind {
	case nextaction.KindPlanApproval:
		return "plan"
	case nextaction.KindPlanRevision:
		return "plan_revision"
	case nextaction.KindMergeApproval:
		return "merge"
	case nextaction.KindMergeConflict, nextaction.KindMergeExecution:
		return taskWaitGateMergeExecution
	default:
		return "human"
	}
}

// observeTaskWait reads the task with its embedded next action and the
// workspace work-order listing, and at a gate the latest plan version or the
// merge readiness (component-cli-onboarding).
func (c *client) observeTaskWait(ctx context.Context, taskID string, now func() time.Time, withNextAction bool) (taskWaitObservation, error) {
	var task struct {
		core.Task
		NextAction *nextActionView `json:"next_action"`
	}
	escaped := url.PathEscape(taskID)
	if err := c.workerDoContext(ctx, http.MethodGet, "/v1/tasks/"+escaped, nil, &task, c.token); err != nil {
		return taskWaitObservation{}, err
	}
	if strings.TrimSpace(task.ID) == "" {
		return taskWaitObservation{}, fmt.Errorf("task read returned no task")
	}
	if task.State == "" {
		return taskWaitObservation{}, fmt.Errorf("task read returned no state")
	}
	var orders []core.WorkOrder
	if err := c.workerDoContext(ctx, http.MethodGet, "/v1/work-orders", nil, &orders, c.token); err != nil {
		return taskWaitObservation{}, err
	}
	observation := taskWaitObservation{TaskID: task.ID, State: task.State, ObservedAt: now().UTC()}
	action := task.NextAction
	if withNextAction {
		if action == nil || strings.TrimSpace(action.Actor) == "" {
			return taskWaitObservation{}, fmt.Errorf("task read returned no next_action")
		}
		observation.NextAction = &taskWaitNextAction{Label: action.Label, Actor: action.Actor, Kind: action.Kind}
		if action.CurrentOrder != nil {
			observation.NextAction.Stage = action.CurrentOrder.Stage
		}
	}
	actionKind := ""
	if action != nil {
		actionKind = action.Kind
	}
	observation.NextOrder = taskWaitNextOrder(orders, task.ID)
	// A pending plan revision is read even beside a claimable order, so a new
	// plan version ends the wait wherever the order stands (AC-4.1).
	planRevision := task.State == core.TaskAwaiting && actionKind == nextaction.KindPlanRevision
	if observation.NextOrder == nil || planRevision {
		gate, err := c.taskWaitGate(ctx, task.Task, actionKind)
		if err != nil {
			return taskWaitObservation{}, err
		}
		observation.PendingGate = gate
	}
	return observation, nil
}

// taskWaitNextOrder selects the task's queued, claimable order of a known
// stage: spec, implement, verify, then review, then the oldest queue entry.
func taskWaitNextOrder(orders []core.WorkOrder, taskID string) *taskWaitOrder {
	var eligible []core.WorkOrder
	for _, order := range orders {
		if order.TaskID == taskID && order.State == core.WorkOrderQueued && order.Claimable && core.ValidWorkOrderStage(order.Stage) {
			eligible = append(eligible, order)
		}
	}
	if len(eligible) == 0 {
		return nil
	}
	sort.Slice(eligible, func(i, j int) bool {
		a, b := eligible[i], eligible[j]
		if a.Stage != b.Stage {
			return taskWaitStageOrder[a.Stage] < taskWaitStageOrder[b.Stage]
		}
		if !a.QueueEnteredAt.Equal(b.QueueEnteredAt) {
			return a.QueueEnteredAt.Before(b.QueueEnteredAt)
		}
		return a.ID < b.ID
	})
	return &taskWaitOrder{ID: eligible[0].ID, Stage: eligible[0].Stage}
}

// taskWaitGate derives the pending gate of a task with no claimable order.
// An approved task behind the merge gate has the merge act pending; an
// awaiting_human task maps its next action.
func (c *client) taskWaitGate(ctx context.Context, task core.Task, actionKind string) (*taskWaitGate, error) {
	switch {
	case task.State == core.TaskApproved && task.MergeApproval:
		readiness, err := c.taskWaitMergeReadiness(ctx, task.ID)
		if err != nil {
			return nil, err
		}
		return &taskWaitGate{Kind: taskWaitGateMergeExecution, MergeReadiness: readiness}, nil
	case task.State != core.TaskAwaiting:
		return nil, nil
	}
	gate := &taskWaitGate{Kind: taskWaitAwaitingGate(actionKind)}
	if gate.Kind == "plan" || gate.Kind == "plan_revision" {
		version, err := c.taskWaitPlanVersion(ctx, task.ID)
		if err != nil {
			return nil, err
		}
		gate.PlanVersion = version
	}
	return gate, nil
}

// taskWaitPlanVersion reads the latest plan version; a task without a plan
// reports 0.
func (c *client) taskWaitPlanVersion(ctx context.Context, taskID string) (int, error) {
	var spec core.SpecVersion
	err := c.workerDoContext(ctx, http.MethodGet, "/v1/tasks/"+url.PathEscape(taskID)+"/spec", nil, &spec, c.token)
	var response *workerHTTPError
	if errors.As(err, &response) && response.StatusCode == http.StatusNotFound {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return spec.Version, nil
}

// taskWaitMergeReadiness reads the readiness state the task activity carries
// for an approved task. A server error leaves the state empty, which reports
// readiness as unavailable; any other failure fails the observation.
func (c *client) taskWaitMergeReadiness(ctx context.Context, taskID string) (string, error) {
	var activity struct {
		MergeReadiness *taskMergeReadiness `json:"merge_readiness"`
	}
	err := c.workerDoContext(ctx, http.MethodGet, "/v1/tasks/"+url.PathEscape(taskID)+"/activity?view=bounded", nil, &activity, c.token)
	var response *workerHTTPError
	if errors.As(err, &response) && response.StatusCode >= 500 && response.StatusCode <= 599 {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if activity.MergeReadiness == nil {
		return "", nil
	}
	return activity.MergeReadiness.State, nil
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
		if result.PendingGate.Kind == taskWaitGateMergeExecution {
			readiness := result.PendingGate.MergeReadiness
			if readiness == "" {
				readiness = "unavailable"
			}
			gate += " (readiness " + readiness + ")"
		}
	}
	order := "none"
	if result.NextOrder != nil {
		order = fmt.Sprintf("%s (%s)", result.NextOrder.ID, result.NextOrder.Stage)
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
