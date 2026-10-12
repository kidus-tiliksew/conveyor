package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
)

// Tests for conveyor task wait (req-agent-skills REQ-4, AC-4.1 through AC-4.3;
// DEC-45; component-runtime).

const (
	taskWaitTestToken     = "wait-test-token"
	taskWaitTestWorkspace = "demo"
	taskWaitTestTaskID    = "261004-abc123"
	taskWaitHelperEnv     = "CONVEYOR_TASK_WAIT_HELPER"
	taskWaitHelperArgsEnv = "CONVEYOR_TASK_WAIT_HELPER_ARGS"
)

var taskWaitTestTime = time.Date(2026, 10, 4, 8, 0, 0, 0, time.UTC)

// taskWaitStep is one server observation. A nonzero status fails the task read
// of that observation; abort drops the connection instead of answering.
type taskWaitStep struct {
	status    int
	abort     bool
	block     bool
	state     core.TaskState
	title     string
	gate      *workerservice.TaskRunGate
	order     core.WorkOrder
	proposals []workerservice.TaskRunProposal
	// nextAction, when set, is embedded as next_action in the task read, as
	// GET /v1/tasks/{id} does.
	nextAction *nextActionView
}

type taskWaitFixture struct {
	t      *testing.T
	mu     sync.Mutex
	steps  []taskWaitStep
	next   int
	active taskWaitStep
	reads  int
	server *httptest.Server
}

func newTaskWaitFixture(t *testing.T, steps ...taskWaitStep) *taskWaitFixture {
	t.Helper()
	f := &taskWaitFixture{t: t, steps: steps}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *taskWaitFixture) client() *client {
	return &client{base: f.server.URL, token: taskWaitTestToken, workspace: taskWaitTestWorkspace}
}

func (f *taskWaitFixture) taskReads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

func (f *taskWaitFixture) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		f.t.Errorf("task wait sent %s %s; only GET is allowed", r.Method, r.URL.Path)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if got := r.Header.Get("Authorization"); got != "Bearer "+taskWaitTestToken {
		f.t.Errorf("Authorization = %q", got)
	}
	if got := r.Header.Get("X-Workspace-ID"); got != taskWaitTestWorkspace {
		f.t.Errorf("X-Workspace-ID = %q", got)
	}
	taskPath := "/v1/tasks/" + taskWaitTestTaskID
	switch r.URL.Path {
	case taskPath:
		f.mu.Lock()
		step := f.steps[min(f.next, len(f.steps)-1)]
		f.next++
		f.reads++
		f.active = step
		f.mu.Unlock()
		switch {
		case step.block:
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
			return
		case step.abort:
			panic(http.ErrAbortHandler)
		case step.status != 0:
			http.Error(w, http.StatusText(step.status), step.status)
			return
		}
		if step.nextAction != nil {
			_ = json.NewEncoder(w).Encode(struct {
				core.Task
				NextAction *nextActionView `json:"next_action"`
			}{core.Task{ID: taskWaitTestTaskID, State: step.state, Title: step.title}, step.nextAction})
			return
		}
		_ = json.NewEncoder(w).Encode(core.Task{ID: taskWaitTestTaskID, State: step.state, Title: step.title})
	case taskPath + "/run-order":
		f.mu.Lock()
		step := f.active
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(workerservice.DispatchOrder{
			Order: step.order, Task: core.Task{ID: taskWaitTestTaskID, State: step.state, Title: step.title},
			Gate: step.gate, PendingProposals: step.proposals, Dispatch: "run", Auth: "user",
		})
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

func taskWaitTestOptions(timeout time.Duration) taskWaitOptions {
	return taskWaitOptions{
		timeout: timeout,
		newPoller: func() *runAdaptivePoller {
			return &runAdaptivePoller{
				base: time.Millisecond, maximum: 2 * time.Millisecond, current: time.Millisecond,
				jitter: func(delay, _ time.Duration) time.Duration { return delay },
			}
		},
		now: func() time.Time { return taskWaitTestTime },
	}
}

func runTaskWaitJSON(t *testing.T, f *taskWaitFixture, timeout time.Duration) (taskWaitResult, error) {
	t.Helper()
	var out bytes.Buffer
	err := runTaskWait(t.Context(), f.client(), &out, taskWaitTestTaskID, true, taskWaitTestOptions(timeout))
	var result taskWaitResult
	if out.Len() > 0 {
		if decodeErr := json.Unmarshal(out.Bytes(), &result); decodeErr != nil {
			t.Fatalf("decode %q: %v", out.String(), decodeErr)
		}
	}
	return result, err
}

func TestTaskWaitReturnsWhenGateClears(t *testing.T) {
	gate := &workerservice.TaskRunGate{Kind: "spec", Label: "spec approval gate", SpecVersion: 1, CanOperate: true}
	f := newTaskWaitFixture(t,
		taskWaitStep{state: core.TaskAwaiting, gate: gate},
		taskWaitStep{state: core.TaskAwaiting, gate: gate},
		taskWaitStep{state: core.TaskQueued, order: core.WorkOrder{ID: "261004-abc123-implement-1", Stage: core.StageImplement}},
	)
	result, err := runTaskWaitJSON(t, f, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reason != taskWaitReasonChanged || result.TimedOut || result.PendingGate != nil || result.State != core.TaskQueued {
		t.Fatalf("result = %+v", result)
	}
	if result.NextOrder == nil || result.NextOrder.ID != "261004-abc123-implement-1" || result.NextOrder.Stage != core.StageImplement {
		t.Fatalf("next order = %+v", result.NextOrder)
	}
	if f.taskReads() != 3 {
		t.Fatalf("task reads = %d, want 3", f.taskReads())
	}
}

func TestTaskWaitReturnsWhenNextOrderAppears(t *testing.T) {
	f := newTaskWaitFixture(t,
		taskWaitStep{state: core.TaskRunning},
		taskWaitStep{state: core.TaskRunning, order: core.WorkOrder{ID: "261004-abc123-review-1-seat-1", Stage: core.StageReview}},
	)
	result, err := runTaskWaitJSON(t, f, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reason != taskWaitReasonChanged || result.State != core.TaskRunning || result.NextOrder == nil || result.NextOrder.Stage != core.StageReview {
		t.Fatalf("result = %+v", result)
	}
}

func TestTaskWaitReturnsWhenProposalResolves(t *testing.T) {
	proposal := workerservice.TaskRunProposal{Kind: "design", DocumentID: "component-runtime", Title: "Runtime", Version: 17, CanConfirm: true}
	f := newTaskWaitFixture(t,
		taskWaitStep{state: core.TaskRunning, proposals: []workerservice.TaskRunProposal{proposal}},
		taskWaitStep{state: core.TaskRunning, proposals: []workerservice.TaskRunProposal{proposal}},
		taskWaitStep{state: core.TaskRunning},
	)
	result, err := runTaskWaitJSON(t, f, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reason != taskWaitReasonChanged || result.PendingProposals == nil || len(result.PendingProposals) != 0 {
		t.Fatalf("result = %+v", result)
	}
}

func TestTaskWaitTimesOutWithLastObservation(t *testing.T) {
	f := newTaskWaitFixture(t, taskWaitStep{state: core.TaskAwaiting, gate: &workerservice.TaskRunGate{Kind: "merge"}})
	result, err := runTaskWaitJSON(t, f, 50*time.Millisecond)
	var timedOut *taskWaitTimeoutError
	if !errors.As(err, &timedOut) {
		t.Fatalf("err = %v, want timeout result", err)
	}
	if result.Reason != taskWaitReasonTimeout || !result.TimedOut || result.State != core.TaskAwaiting ||
		result.PendingGate == nil || result.PendingGate.Kind != "merge" || result.LastReadError != "" {
		t.Fatalf("result = %+v", result)
	}
	if f.taskReads() < 2 {
		t.Fatalf("task reads = %d; the wait did not poll", f.taskReads())
	}
}

func TestTaskWaitReturnsTerminalStateAtStart(t *testing.T) {
	for _, state := range []core.TaskState{core.TaskMerged, core.TaskClosed, core.TaskParked} {
		t.Run(string(state), func(t *testing.T) {
			f := newTaskWaitFixture(t, taskWaitStep{state: state})
			result, err := runTaskWaitJSON(t, f, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if result.Reason != taskWaitReasonTerminal || result.TimedOut || result.State != state {
				t.Fatalf("result = %+v", result)
			}
			if f.taskReads() != 1 {
				t.Fatalf("task reads = %d, want 1", f.taskReads())
			}
		})
	}
}

func TestTaskWaitReturnsTerminalTransition(t *testing.T) {
	f := newTaskWaitFixture(t,
		taskWaitStep{state: core.TaskRunning},
		taskWaitStep{state: core.TaskClosed},
	)
	result, err := runTaskWaitJSON(t, f, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reason != taskWaitReasonTerminal || result.State != core.TaskClosed {
		t.Fatalf("result = %+v", result)
	}
}

func TestTaskWaitRetriesTransientErrorsThenReportsChange(t *testing.T) {
	f := newTaskWaitFixture(t,
		taskWaitStep{state: core.TaskRunning},
		taskWaitStep{status: http.StatusServiceUnavailable},
		taskWaitStep{abort: true},
		taskWaitStep{status: http.StatusBadGateway},
		taskWaitStep{state: core.TaskAwaiting, gate: &workerservice.TaskRunGate{Kind: "merge"}},
	)
	result, err := runTaskWaitJSON(t, f, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reason != taskWaitReasonChanged || result.State != core.TaskAwaiting || result.LastReadError != "" {
		t.Fatalf("result = %+v", result)
	}
	if f.taskReads() != 5 {
		t.Fatalf("task reads = %d, want 5", f.taskReads())
	}
}

func TestTaskWaitTimeoutAfterTransientErrorsReportsLastObservation(t *testing.T) {
	f := newTaskWaitFixture(t,
		taskWaitStep{state: core.TaskRunning, order: core.WorkOrder{ID: "o-1", Stage: core.StageImplement}},
		taskWaitStep{status: http.StatusServiceUnavailable},
	)
	result, err := runTaskWaitJSON(t, f, 50*time.Millisecond)
	var timedOut *taskWaitTimeoutError
	if !errors.As(err, &timedOut) {
		t.Fatalf("err = %v, want timeout result", err)
	}
	if !result.TimedOut || result.State != core.TaskRunning || result.NextOrder == nil || result.NextOrder.ID != "o-1" ||
		!strings.Contains(result.LastReadError, "503") {
		t.Fatalf("result = %+v", result)
	}
}

func TestTaskWaitFailsOnAuthentication(t *testing.T) {
	t.Run("start", func(t *testing.T) {
		f := newTaskWaitFixture(t, taskWaitStep{status: http.StatusUnauthorized})
		var out bytes.Buffer
		err := runTaskWait(t.Context(), f.client(), &out, taskWaitTestTaskID, false, taskWaitTestOptions(5*time.Second))
		if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "token from an untracked credential source") {
			t.Fatalf("err = %v", err)
		}
		var timedOut *taskWaitTimeoutError
		if errors.As(err, &timedOut) || out.Len() != 0 {
			t.Fatalf("failure produced a result: %q", out.String())
		}
	})
	t.Run("during wait", func(t *testing.T) {
		f := newTaskWaitFixture(t,
			taskWaitStep{state: core.TaskRunning},
			taskWaitStep{status: http.StatusForbidden},
			taskWaitStep{state: core.TaskQueued},
		)
		_, err := runTaskWaitJSON(t, f, 5*time.Second)
		if err == nil || !strings.Contains(err.Error(), "403") {
			t.Fatalf("err = %v", err)
		}
		if f.taskReads() != 2 {
			t.Fatalf("task reads = %d; an authorization failure was retried", f.taskReads())
		}
	})
}

func TestTaskWaitFailsOnNotFound(t *testing.T) {
	f := newTaskWaitFixture(t, taskWaitStep{status: http.StatusNotFound})
	_, err := runTaskWaitJSON(t, f, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "task not found") {
		t.Fatalf("err = %v", err)
	}
}

func TestTaskWaitFailsOnMalformedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{not json"))
	}))
	t.Cleanup(server.Close)
	c := &client{base: server.URL, token: taskWaitTestToken, workspace: taskWaitTestWorkspace}
	err := runTaskWait(t.Context(), c, io.Discard, taskWaitTestTaskID, false, taskWaitTestOptions(5*time.Second))
	if err == nil {
		t.Fatal("malformed response did not fail")
	}
	var timedOut *taskWaitTimeoutError
	if errors.As(err, &timedOut) {
		t.Fatalf("malformed response reported a timeout: %v", err)
	}
}

func TestTaskWaitIgnoresPresentationChurnAndNormalizesOrder(t *testing.T) {
	first := []workerservice.TaskRunProposal{
		{Kind: "requirement", DocumentID: "req-agent-skills", Title: "Skills", Version: 4},
		{Kind: "design", DocumentID: "component-runtime", Title: "Runtime", Version: 17},
		{Kind: "design", DocumentID: "component-runtime", Title: "Runtime duplicate", Version: 17},
	}
	second := []workerservice.TaskRunProposal{
		{Kind: "design", DocumentID: "component-runtime", Title: "Runtime renamed", Version: 17, CanConfirm: true, ActorHint: "an operator can confirm"},
		{Kind: "requirement", DocumentID: "req-agent-skills", Title: "Skills renamed", Version: 4},
	}
	f := newTaskWaitFixture(t,
		taskWaitStep{state: core.TaskAwaiting, title: "one", proposals: first,
			gate: &workerservice.TaskRunGate{Kind: "spec", Label: "spec approval gate", Summary: "submitted execution plan v2", SpecVersion: 2}},
		taskWaitStep{state: core.TaskAwaiting, title: "two", proposals: second,
			gate: &workerservice.TaskRunGate{Kind: "spec", Label: "relabelled", Summary: "other", SpecVersion: 2, CanOperate: true, CanRequestChanges: true}},
	)
	result, err := runTaskWaitJSON(t, f, 60*time.Millisecond)
	var timedOut *taskWaitTimeoutError
	if !errors.As(err, &timedOut) {
		t.Fatalf("presentation churn ended the wait: err=%v result=%+v", err, result)
	}
	if result.PendingGate == nil || *result.PendingGate != (taskWaitGate{Kind: "plan", PlanVersion: 2}) {
		t.Fatalf("gate = %+v", result.PendingGate)
	}
	want := []taskWaitProposal{{Kind: "design", DocumentID: "component-runtime", Version: 17}, {Kind: "requirement", DocumentID: "req-agent-skills", Version: 4}}
	if len(result.PendingProposals) != len(want) || result.PendingProposals[0] != want[0] || result.PendingProposals[1] != want[1] {
		t.Fatalf("proposals = %+v", result.PendingProposals)
	}
}

func TestTaskWaitDetectsGateAndProposalVersionChanges(t *testing.T) {
	for name, steps := range map[string][]taskWaitStep{
		"plan version": {
			{state: core.TaskAwaiting, gate: &workerservice.TaskRunGate{Kind: "spec", SpecVersion: 1}},
			{state: core.TaskAwaiting, gate: &workerservice.TaskRunGate{Kind: "spec", SpecVersion: 2}},
		},
		"gate kind": {
			{state: core.TaskAwaiting, gate: &workerservice.TaskRunGate{Kind: "merge"}},
			{state: core.TaskAwaiting, gate: &workerservice.TaskRunGate{Kind: "plan_revision", PlanVersion: 1}},
		},
		"proposal version": {
			{state: core.TaskRunning, proposals: []workerservice.TaskRunProposal{{Kind: "design", DocumentID: "component-runtime", Version: 17}}},
			{state: core.TaskRunning, proposals: []workerservice.TaskRunProposal{{Kind: "design", DocumentID: "component-runtime", Version: 18}}},
		},
		"proposal tier": {
			{state: core.TaskRunning, proposals: []workerservice.TaskRunProposal{{Kind: "design", DocumentID: "x", Version: 1}}},
			{state: core.TaskRunning, proposals: []workerservice.TaskRunProposal{{Kind: "requirement", DocumentID: "x", Version: 1}}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newTaskWaitFixture(t, steps...)
			result, err := runTaskWaitJSON(t, f, 5*time.Second)
			if err != nil || result.Reason != taskWaitReasonChanged {
				t.Fatalf("err=%v result=%+v", err, result)
			}
		})
	}
}

func TestTaskWaitJSONNormalizesAbsentFields(t *testing.T) {
	f := newTaskWaitFixture(t, taskWaitStep{state: core.TaskMerged})
	var out bytes.Buffer
	if err := runTaskWait(t.Context(), f.client(), &out, taskWaitTestTaskID, true, taskWaitTestOptions(time.Second)); err != nil {
		t.Fatal(err)
	}
	if strings.Count(strings.TrimSpace(out.String()), "\n") != 0 {
		t.Fatalf("JSON output is not one object line: %q", out.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]string{
		"task_id": `"` + taskWaitTestTaskID + `"`, "state": `"merged"`, "pending_gate": "null", "next_order": "null",
		"pending_proposals": "[]", "observed_at": `"2026-10-04T08:00:00Z"`, "reason": `"terminal"`, "timed_out": "false",
	} {
		if got := string(raw[field]); got != want {
			t.Errorf("%s = %s, want %s", field, got, want)
		}
	}
	if _, ok := raw["last_read_error"]; ok {
		t.Errorf("last_read_error present without a failed read: %s", out.String())
	}
}

func TestTaskWaitHumanOutputMatchesJSONFields(t *testing.T) {
	steps := []taskWaitStep{
		{state: core.TaskRunning},
		{state: core.TaskAwaiting, gate: &workerservice.TaskRunGate{Kind: "spec", SpecVersion: 3},
			proposals: []workerservice.TaskRunProposal{{Kind: "design", DocumentID: "component-runtime", Version: 17}}},
	}
	var out bytes.Buffer
	if err := runTaskWait(t.Context(), newTaskWaitFixture(t, steps...).client(), &out, taskWaitTestTaskID, false, taskWaitTestOptions(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"task:              " + taskWaitTestTaskID,
		"state:             awaiting_human",
		"pending gate:      plan v3",
		"next order:        none",
		"pending proposals: design component-runtime v17",
		"observed at:       2026-10-04T08:00:00Z",
		"wait ended:        changed",
		"timed out:         false",
	} {
		if !strings.Contains(out.String(), line+"\n") {
			t.Errorf("human output lacks %q:\n%s", line, out.String())
		}
	}

	out.Reset()
	err := runTaskWait(t.Context(), newTaskWaitFixture(t, steps[0]).client(), &out, taskWaitTestTaskID, false, taskWaitTestOptions(20*time.Millisecond))
	var timedOut *taskWaitTimeoutError
	if !errors.As(err, &timedOut) {
		t.Fatalf("err = %v", err)
	}
	for _, line := range []string{
		"wait ended:        timeout after 20ms; the values above are the last successful observation",
		"timed out:         true",
	} {
		if !strings.Contains(out.String(), line+"\n") {
			t.Errorf("timeout output lacks %q:\n%s", line, out.String())
		}
	}
}

func TestTaskWaitRejectsInvalidArgumentsBeforeRequests(t *testing.T) {
	f := newTaskWaitFixture(t, taskWaitStep{state: core.TaskRunning})
	for _, timeout := range []time.Duration{0, -time.Second} {
		err := runTaskWait(t.Context(), f.client(), io.Discard, taskWaitTestTaskID, false, taskWaitTestOptions(timeout))
		if err == nil || !strings.Contains(err.Error(), "--timeout must be positive") {
			t.Fatalf("timeout %s err = %v", timeout, err)
		}
	}
	if err := runTaskWait(t.Context(), f.client(), io.Discard, "  ", false, taskWaitTestOptions(time.Second)); err == nil || !strings.Contains(err.Error(), "task ID is required") {
		t.Fatalf("blank task ID err = %v", err)
	}
	configErr := errors.New("resolve server: bad")
	c := f.client()
	c.configErr = configErr
	if err := runTaskWait(t.Context(), c, io.Discard, taskWaitTestTaskID, false, taskWaitTestOptions(time.Second)); !errors.Is(err, configErr) {
		t.Fatalf("config err = %v", err)
	}
	for _, args := range [][]string{{"--timeout", "soon", taskWaitTestTaskID}, {"--timeout", "0s", taskWaitTestTaskID}, {}} {
		command := taskWaitCmd()
		command.SetArgs(args)
		command.SetOut(io.Discard)
		command.SetErr(io.Discard)
		if err := command.Execute(); err == nil {
			t.Fatalf("args %q were accepted", args)
		}
	}
	if f.taskReads() != 0 {
		t.Fatalf("invalid arguments sent %d task reads", f.taskReads())
	}
}

func TestTaskWaitCommandDefaultsAndHelp(t *testing.T) {
	command := taskWaitCmd()
	if got := command.Flags().Lookup("timeout").DefValue; got != "5m0s" {
		t.Fatalf("--timeout default = %s", got)
	}
	if command.Flags().Lookup("json") == nil {
		t.Fatal("--json flag missing")
	}
	for _, text := range []string{"0  the observation changed or the task is merged, closed, or parked", "2  the timeout elapsed", "1  failure", "250ms to a 2s ceiling"} {
		if !strings.Contains(command.Long, text) {
			t.Errorf("help lacks %q", text)
		}
	}
}

func TestTaskWaitBoundsBlockedReadsByTimeout(t *testing.T) {
	t.Run("during wait", func(t *testing.T) {
		f := newTaskWaitFixture(t, taskWaitStep{state: core.TaskRunning}, taskWaitStep{block: true})
		started := time.Now()
		result, err := runTaskWaitJSON(t, f, 100*time.Millisecond)
		var timedOut *taskWaitTimeoutError
		if !errors.As(err, &timedOut) || result.State != core.TaskRunning {
			t.Fatalf("err=%v result=%+v", err, result)
		}
		if elapsed := time.Since(started); elapsed > 3*time.Second {
			t.Fatalf("blocked read held the wait for %s", elapsed)
		}
	})
	t.Run("first observation", func(t *testing.T) {
		f := newTaskWaitFixture(t, taskWaitStep{block: true})
		started := time.Now()
		_, err := runTaskWaitJSON(t, f, 100*time.Millisecond)
		var timedOut *taskWaitTimeoutError
		if err == nil || errors.As(err, &timedOut) || !strings.Contains(err.Error(), "first observation did not complete") {
			t.Fatalf("err = %v", err)
		}
		if elapsed := time.Since(started); elapsed > 3*time.Second {
			t.Fatalf("blocked first read held the wait for %s", elapsed)
		}
	})
}

func TestTaskWaitCallerCancellationIsNotTimeout(t *testing.T) {
	f := newTaskWaitFixture(t, taskWaitStep{state: core.TaskRunning})
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(30*time.Millisecond, cancel)
	err := runTaskWait(ctx, f.client(), io.Discard, taskWaitTestTaskID, false, taskWaitTestOptions(5*time.Second))
	var timedOut *taskWaitTimeoutError
	if !errors.Is(err, context.Canceled) || errors.As(err, &timedOut) {
		t.Fatalf("err = %v", err)
	}
}

// TestTaskWaitProcessHelper runs the real CLI entry point in a child process.
func TestTaskWaitProcessHelper(t *testing.T) {
	if os.Getenv(taskWaitHelperEnv) != "1" {
		t.Skip("child process helper")
	}
	os.Args = append([]string{"conveyor"}, strings.Split(os.Getenv(taskWaitHelperArgsEnv), "\x1f")...)
	main()
	os.Exit(0)
}

func TestTaskWaitProcessExitStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		steps  []taskWaitStep
		args   []string
		status int
		stdout string
		stderr string
	}{
		{name: "changed", steps: []taskWaitStep{{state: core.TaskRunning}, {state: core.TaskQueued}}, args: []string{"--json"}, status: 0, stdout: `"reason":"changed"`},
		{name: "terminal", steps: []taskWaitStep{{state: core.TaskParked}}, status: 0, stdout: "wait ended:        terminal"},
		{name: "timeout", steps: []taskWaitStep{{state: core.TaskRunning}}, args: []string{"--json", "--timeout", "300ms"}, status: 2, stdout: `"timed_out":true`},
		{name: "auth failure", steps: []taskWaitStep{{status: http.StatusUnauthorized}}, status: 1, stderr: "error: wait for task"},
		{name: "invalid timeout", steps: []taskWaitStep{{state: core.TaskRunning}}, args: []string{"--timeout", "-1s"}, status: 1, stderr: "--timeout must be positive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTaskWaitFixture(t, tc.steps...)
			args := append([]string{"--workspace", taskWaitTestWorkspace, "task", "wait", taskWaitTestTaskID}, tc.args...)
			command := exec.Command(os.Args[0], "-test.run=^TestTaskWaitProcessHelper$")
			command.Dir = t.TempDir()
			home := t.TempDir()
			command.Env = append(taskWaitChildEnvironment(),
				taskWaitHelperEnv+"=1", taskWaitHelperArgsEnv+"="+strings.Join(args, "\x1f"),
				"HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
				"CONVEYOR_ADDR="+f.server.URL, "CONVEYOR_API_TOKEN="+taskWaitTestToken,
			)
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			err := command.Run()
			status := 0
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				status = exitErr.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if status != tc.status {
				t.Fatalf("exit status = %d, want %d\nstdout: %s\nstderr: %s", status, tc.status, stdout.String(), stderr.String())
			}
			if tc.stdout != "" && !strings.Contains(stdout.String(), tc.stdout) {
				t.Fatalf("stdout lacks %q: %s", tc.stdout, stdout.String())
			}
			if tc.stderr != "" && !strings.Contains(stderr.String(), tc.stderr) {
				t.Fatalf("stderr lacks %q: %s", tc.stderr, stderr.String())
			}
			if tc.stderr == "" && strings.TrimSpace(stderr.String()) != "" {
				t.Fatalf("unexpected stderr: %s", stderr.String())
			}
		})
	}
}

// taskWaitChildEnvironment drops inherited Conveyor settings so the child
// resolves only the fixture server, token, and workspace.
func taskWaitChildEnvironment() []string {
	var environment []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "CONVEYOR_") || name == "HOME" || name == "XDG_CONFIG_HOME" {
			continue
		}
		environment = append(environment, entry)
	}
	return environment
}

func taskWaitMergeExecutionGate(state, summary string) *workerservice.TaskRunGate {
	gate := &workerservice.TaskRunGate{Kind: workerservice.TaskRunGateMergeExecution, Label: "merge execution pending", Summary: summary, CanOperate: true}
	if state != "" {
		gate.MergeReadiness = &workerservice.TaskRunMergeReadiness{State: state, HeadSHA: "0123456789abcdef", URL: "https://github.test/pr/" + summary}
	}
	return gate
}

// After approval a merge-gated task reports the pending merge act, and the
// wait returns on the move to merged (req-agent-skills AC-4.1, AC-4.2;
// component-cli-onboarding).
func TestTaskWaitReportsPendingMergeExecutionUntilMerged(t *testing.T) {
	approval := &workerservice.TaskRunGate{Kind: "merge", Label: "merge approval gate", CanOperate: true}
	execution := taskWaitMergeExecutionGate("MERGEABLE", "1")
	steps := []taskWaitStep{
		{state: core.TaskAwaiting, gate: approval},
		{state: core.TaskApproved, gate: execution},
	}
	result, err := runTaskWaitJSON(t, newTaskWaitFixture(t, steps...), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := taskWaitGate{Kind: workerservice.TaskRunGateMergeExecution, MergeReadiness: "MERGEABLE"}
	if result.Reason != taskWaitReasonChanged || result.State != core.TaskApproved || result.PendingGate == nil || *result.PendingGate != want || result.NextOrder != nil {
		t.Fatalf("result = %+v gate %+v", result, result.PendingGate)
	}
	var raw bytes.Buffer
	if err := runTaskWait(t.Context(), newTaskWaitFixture(t, steps...).client(), &raw, taskWaitTestTaskID, true, taskWaitTestOptions(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw.String(), `"pending_gate":{"kind":"merge_execution","merge_readiness":"MERGEABLE"}`) {
		t.Fatalf("JSON pending gate = %s", raw.String())
	}
	var human bytes.Buffer
	if err := runTaskWait(t.Context(), newTaskWaitFixture(t, steps...).client(), &human, taskWaitTestTaskID, false, taskWaitTestOptions(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"state:             approved", "pending gate:      merge_execution (readiness MERGEABLE)", "wait ended:        changed"} {
		if !strings.Contains(human.String(), line+"\n") {
			t.Errorf("human output lacks %q:\n%s", line, human.String())
		}
	}

	merged := newTaskWaitFixture(t,
		taskWaitStep{state: core.TaskApproved, gate: execution},
		taskWaitStep{state: core.TaskApproved, gate: execution},
		taskWaitStep{state: core.TaskMerged},
	)
	result, err = runTaskWaitJSON(t, merged, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reason != taskWaitReasonTerminal || result.State != core.TaskMerged || result.PendingGate != nil || merged.taskReads() != 3 {
		t.Fatalf("result = %+v reads=%d", result, merged.taskReads())
	}
}

func TestTaskWaitMergeReadinessChangeEndsWait(t *testing.T) {
	f := newTaskWaitFixture(t,
		taskWaitStep{state: core.TaskApproved, gate: taskWaitMergeExecutionGate("UNKNOWN", "1")},
		// Summary and readiness URL churn is presentation and never ends a wait.
		taskWaitStep{state: core.TaskApproved, gate: taskWaitMergeExecutionGate("UNKNOWN", "2")},
		taskWaitStep{state: core.TaskApproved, gate: taskWaitMergeExecutionGate("MERGEABLE", "2")},
	)
	result, err := runTaskWaitJSON(t, f, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reason != taskWaitReasonChanged || result.PendingGate == nil || result.PendingGate.MergeReadiness != "MERGEABLE" || f.taskReads() != 3 {
		t.Fatalf("result = %+v reads=%d", result, f.taskReads())
	}

	var human bytes.Buffer
	err = runTaskWait(t.Context(), newTaskWaitFixture(t, taskWaitStep{state: core.TaskApproved, gate: taskWaitMergeExecutionGate("", "unread")}).client(), &human, taskWaitTestTaskID, false, taskWaitTestOptions(20*time.Millisecond))
	var timedOut *taskWaitTimeoutError
	if !errors.As(err, &timedOut) || !strings.Contains(human.String(), "pending gate:      merge_execution (readiness unavailable)\n") {
		t.Fatalf("err=%v output:\n%s", err, human.String())
	}
}

func untilAction(label, actor, kind, stage string) *nextActionView {
	action := &nextActionView{Label: label, Actor: actor, Kind: kind}
	if stage != "" {
		action.CurrentOrder = &nextActionOrder{ID: "o-" + stage, Stage: stage}
	}
	return action
}

func runTaskWaitUntil(t *testing.T, f *taskWaitFixture, until string, timeout time.Duration) (taskWaitResult, string, error) {
	t.Helper()
	var out, progress bytes.Buffer
	options := taskWaitTestOptions(timeout)
	options.until = until
	options.progress = &progress
	err := runTaskWait(t.Context(), f.client(), &out, taskWaitTestTaskID, true, options)
	var result taskWaitResult
	if out.Len() > 0 {
		if decodeErr := json.Unmarshal(out.Bytes(), &result); decodeErr != nil {
			t.Fatalf("decode %q: %v", out.String(), decodeErr)
		}
	}
	return result, progress.String(), err
}

// TestTaskWaitUntil covers --until targets (req-operator-next-action AC-5.4).
func TestTaskWaitUntil(t *testing.T) {
	t.Run("operator reached after intermediate changes", func(t *testing.T) {
		f := newTaskWaitFixture(t,
			taskWaitStep{state: core.TaskRunning, nextAction: untilAction("Implementing", "agent", "running", "implement")},
			// Label-only change: no line, and the wait does not end.
			taskWaitStep{state: core.TaskRunning, nextAction: untilAction("Implementing (attempt 2)", "agent", "running", "implement")},
			// State and stage change: one line, still no end.
			taskWaitStep{state: core.TaskQueued, nextAction: untilAction("Implementing (attempt 2)", "agent", "running", "review")},
			// Kind change with the same label: one line.
			taskWaitStep{state: core.TaskQueued, nextAction: untilAction("Implementing (attempt 2)", "agent", "review_pending", "review")},
			taskWaitStep{state: core.TaskAwaiting, nextAction: untilAction("Merge approval pending", "operator", "merge_approval", "")},
		)
		result, progress, err := runTaskWaitUntil(t, f, "operator", 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if result.Reason != taskWaitReasonReached || result.TimedOut || result.Until != "operator" || result.NextAction == nil || result.NextAction.Actor != "operator" {
			t.Fatalf("result = %+v", result)
		}
		want := strings.Join([]string{
			"2026-10-04T08:00:00Z Implementing: actor=agent kind=running stage=implement",
			"2026-10-04T08:00:00Z Implementing (attempt 2): actor=agent kind=running stage=review",
			"2026-10-04T08:00:00Z Implementing (attempt 2): actor=agent kind=review_pending stage=review",
			"2026-10-04T08:00:00Z Merge approval pending: actor=operator kind=merge_approval stage=none",
			"",
		}, "\n")
		if progress != want {
			t.Fatalf("progress:\n%s\nwant:\n%s", progress, want)
		}
		if f.taskReads() != 5 {
			t.Fatalf("task reads = %d, want 5", f.taskReads())
		}
	})
	t.Run("terminal reached", func(t *testing.T) {
		f := newTaskWaitFixture(t,
			taskWaitStep{state: core.TaskRunning, nextAction: untilAction("Implementing", "agent", "running", "implement")},
			taskWaitStep{state: core.TaskQueued, nextAction: untilAction("Queued", "agent", "queued", "review")},
			taskWaitStep{state: core.TaskMerged, nextAction: untilAction("Merged", "none", "merged", "")},
		)
		result, _, err := runTaskWaitUntil(t, f, "terminal", 5*time.Second)
		if err != nil || result.Reason != taskWaitReasonReached || result.State != core.TaskMerged {
			t.Fatalf("err=%v result=%+v", err, result)
		}
	})
	t.Run("stage reached", func(t *testing.T) {
		f := newTaskWaitFixture(t,
			taskWaitStep{state: core.TaskRunning, nextAction: untilAction("Planning", "agent", "running", "spec")},
			taskWaitStep{state: core.TaskAwaiting, nextAction: untilAction("Plan approval pending", "operator", "plan_approval", "spec")},
			taskWaitStep{state: core.TaskRunning, nextAction: untilAction("Implementing", "agent", "running", "implement")},
		)
		result, _, err := runTaskWaitUntil(t, f, "stage=implement", 5*time.Second)
		if err != nil || result.Reason != taskWaitReasonReached || result.NextAction == nil || result.NextAction.Stage != "implement" || f.taskReads() != 3 {
			t.Fatalf("err=%v result=%+v reads=%d", err, result, f.taskReads())
		}
	})
	t.Run("terminal before target ends with terminal", func(t *testing.T) {
		f := newTaskWaitFixture(t,
			taskWaitStep{state: core.TaskRunning, nextAction: untilAction("Implementing", "agent", "running", "implement")},
			taskWaitStep{state: core.TaskClosed, nextAction: untilAction("Closed", "none", "closed", "")},
		)
		result, _, err := runTaskWaitUntil(t, f, "operator", 5*time.Second)
		if err != nil || result.Reason != taskWaitReasonTerminal {
			t.Fatalf("err=%v result=%+v", err, result)
		}
	})
	t.Run("never reached times out", func(t *testing.T) {
		f := newTaskWaitFixture(t, taskWaitStep{state: core.TaskRunning, nextAction: untilAction("Implementing", "agent", "running", "implement")})
		result, progress, err := runTaskWaitUntil(t, f, "stage=review", 50*time.Millisecond)
		var timedOut *taskWaitTimeoutError
		if !errors.As(err, &timedOut) || !result.TimedOut || result.Reason != taskWaitReasonTimeout {
			t.Fatalf("err=%v result=%+v", err, result)
		}
		if strings.Count(progress, "\n") != 1 {
			t.Fatalf("unchanged polls printed lines:\n%s", progress)
		}
	})
	t.Run("invalid until fails before the first read", func(t *testing.T) {
		for _, until := range []string{"bogus", "stage=", "stage=deploy", "stage=gate"} {
			f := newTaskWaitFixture(t, taskWaitStep{state: core.TaskRunning})
			_, _, err := runTaskWaitUntil(t, f, until, time.Second)
			if err == nil || !strings.Contains(err.Error(), "--until") || f.taskReads() != 0 {
				t.Fatalf("until=%q err=%v reads=%d", until, err, f.taskReads())
			}
		}
	})
	t.Run("failed first read fails", func(t *testing.T) {
		f := newTaskWaitFixture(t, taskWaitStep{status: http.StatusNotFound})
		_, _, err := runTaskWaitUntil(t, f, "operator", time.Second)
		var timedOut *taskWaitTimeoutError
		if err == nil || errors.As(err, &timedOut) || !strings.Contains(err.Error(), "task not found") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("process exit statuses", func(t *testing.T) {
		for _, tc := range []struct {
			args   []string
			steps  []taskWaitStep
			status int
			reads  int
		}{
			{[]string{"--until", "operator"}, []taskWaitStep{{state: core.TaskAwaiting, nextAction: untilAction("Plan approval pending", "operator", "plan_approval", "spec")}}, 0, 1},
			{[]string{"--until", "operator", "--timeout", "200ms"}, []taskWaitStep{{state: core.TaskRunning, nextAction: untilAction("Implementing", "agent", "running", "implement")}}, 2, -1},
			{[]string{"--until", "bogus"}, []taskWaitStep{{state: core.TaskRunning}}, 1, 0},
		} {
			f := newTaskWaitFixture(t, tc.steps...)
			_, status := runConveyorMain(t, f.server.URL, append([]string{"task", "wait", taskWaitTestTaskID}, tc.args...)...)
			if status != tc.status || (tc.reads >= 0 && f.taskReads() != tc.reads) {
				t.Fatalf("args=%v exit=%d want %d reads=%d", tc.args, status, tc.status, f.taskReads())
			}
		}
	})
	t.Run("without until output is unchanged", func(t *testing.T) {
		steps := []taskWaitStep{
			{state: core.TaskRunning, nextAction: untilAction("Implementing", "agent", "running", "implement")},
			{state: core.TaskQueued, nextAction: untilAction("Queued", "agent", "queued", "implement")},
		}
		var out bytes.Buffer
		if err := runTaskWait(t.Context(), newTaskWaitFixture(t, steps...).client(), &out, taskWaitTestTaskID, true, taskWaitTestOptions(5*time.Second)); err != nil {
			t.Fatal(err)
		}
		wantJSON := `{"task_id":"261004-abc123","state":"queued","pending_gate":null,"next_order":null,"pending_proposals":[],"observed_at":"2026-10-04T08:00:00Z","reason":"changed","timed_out":false}` + "\n"
		if out.String() != wantJSON {
			t.Fatalf("json = %q\nwant %q", out.String(), wantJSON)
		}
		out.Reset()
		if err := runTaskWait(t.Context(), newTaskWaitFixture(t, steps...).client(), &out, taskWaitTestTaskID, false, taskWaitTestOptions(5*time.Second)); err != nil {
			t.Fatal(err)
		}
		wantHuman := strings.Join([]string{
			"task:              261004-abc123",
			"state:             queued",
			"pending gate:      none",
			"next order:        none",
			"pending proposals: none",
			"observed at:       2026-10-04T08:00:00Z",
			"wait ended:        changed",
			"timed out:         false",
			"",
		}, "\n")
		if out.String() != wantHuman {
			t.Fatalf("human:\n%s\nwant:\n%s", out.String(), wantHuman)
		}
	})
}
