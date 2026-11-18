package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// Tests for conveyor task wait (req-agent-skills REQ-4, AC-4.1 through AC-4.3;
// component-cli-onboarding).

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
	status int
	abort  bool
	block  bool
	state  core.TaskState
	title  string
	gate   *taskWaitTestGate
	order  core.WorkOrder
	// orders are more of this task's orders, served with their own state
	// and claimability.
	orders    []core.WorkOrder
	proposals []taskWaitTestProposal
	// nextAction, when set, is embedded as next_action in the task read, as
	// GET /v1/tasks/{id} does.
	nextAction *nextActionView
	// frozenGateOff serves a merge_execution step with the frozen
	// merge_approval off, as an enforcing task whose risk policy requires a
	// person reads (DEC-124(2)).
	frozenGateOff bool
}

// taskWaitTestGate is the gate a step serves. The fixture projects it onto
// the reads task wait makes: the task's state and next_action kind, the
// latest plan, and the activity's merge readiness.
type taskWaitTestGate struct {
	Kind              string
	Label             string
	Summary           string
	CanOperate        bool
	CanRequestChanges bool
	SpecVersion       int
	PlanVersion       int
	MergeReadiness    *taskMergeReadiness
}

// taskWaitTestProposal is a task-authored pending proposal a step serves.
type taskWaitTestProposal struct {
	Kind       string
	DocumentID string
	Title      string
	Version    int
	CanConfirm bool
	ActorHint  string
}

func (g *taskWaitTestGate) nextActionKind() string {
	if g == nil {
		return "working"
	}
	switch g.Kind {
	case "spec":
		return "plan_approval"
	case "plan_revision":
		return "plan_revision"
	case "merge":
		return "merge_approval"
	case taskWaitGateMergeExecution:
		return "merge_execution"
	case taskWaitGateCheckHold:
		return "check_hold"
	default:
		return "job_failure"
	}
}

type taskWaitFixture struct {
	t      *testing.T
	mu     sync.Mutex
	steps  []taskWaitStep
	next   int
	active taskWaitStep
	reads  int
	paths  []string
	tags   []string
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

func (f *taskWaitFixture) requestPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

func (f *taskWaitFixture) requestTags() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.tags...)
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
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.RequestURI())
	f.tags = append(f.tags, r.Header.Get("If-None-Match"))
	f.mu.Unlock()
	taskPath := "/v1/tasks/" + taskWaitTestTaskID
	if r.URL.Path == taskPath+"/observation" {
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
		view := taskWaitObservationView{TaskID: taskWaitTestTaskID, State: step.state, ClaimableOrders: taskWaitTestClaimableOrders(step)}
		if step.gate != nil && step.gate.Kind == taskWaitGateMergeExecution && step.state == core.TaskApproved && !step.frozenGateOff {
			view.MergeApproval = true
		}
		action := step.nextAction
		if action == nil {
			action = &nextActionView{Label: "Label " + step.title, Actor: "agent", Kind: step.gate.nextActionKind()}
		}
		view.NextAction = action
		body, err := json.Marshal(view)
		if err != nil {
			f.t.Errorf("marshal observation: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		// The tag names the body, the route, and the UTC minute, which is what
		// the server's validator covers (the database validation read plus the
		// route scope). It covers neither the gate's plan version nor the live
		// merge readiness, so a readiness change answers 304 and the wait must
		// re-read the gate to observe it (req-agent-skills AC-4.1, AC-4.3).
		tagged := fmt.Sprintf("%s|%s|%s", body, "observation", time.Now().UTC().Format("2006-01-02T15:04"))
		tag := fmt.Sprintf("%q", fmt.Sprintf("obs-%x", sha256.Sum256([]byte(tagged))))
		w.Header().Set("ETag", tag)
		if r.Header.Get("If-None-Match") == tag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
		return
	}
	f.mu.Lock()
	step := f.active
	f.mu.Unlock()
	switch r.URL.Path {
	case "/v1/pending-proposals":
		items := []core.PendingProposal{{ID: "other-design", Tier: "system_design", Version: 3, OriginType: "task", OriginID: "other-task"}}
		for _, proposal := range step.proposals {
			tier := proposal.Kind
			if tier == "design" {
				tier = "system_design"
			}
			items = append(items, core.PendingProposal{ID: proposal.DocumentID, Title: proposal.Title, Tier: tier, Version: proposal.Version, OriginType: "task", OriginID: taskWaitTestTaskID})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	case taskPath + "/spec":
		version := 0
		if step.gate != nil {
			version = max(step.gate.SpecVersion, step.gate.PlanVersion)
		}
		if version == 0 {
			http.Error(w, "spec not found", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(core.SpecVersion{TaskID: taskWaitTestTaskID, Version: version})
	case taskPath + "/activity":
		var readiness *taskMergeReadiness
		if step.gate != nil {
			readiness = step.gate.MergeReadiness
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"task": core.Task{ID: taskWaitTestTaskID, State: step.state}, "merge_readiness": readiness})
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

// taskWaitTestGateTag is the part of a step's tag that a later gate read
// reports: the plan version and the merge readiness state.
func taskWaitTestGateTag(step taskWaitStep) []byte {
	if step.gate == nil {
		return nil
	}
	return []byte(fmt.Sprintf("%d/%d/%v", step.gate.SpecVersion, step.gate.PlanVersion, step.gate.MergeReadiness))
}

// taskWaitTestClaimableOrders serves a step's orders as the observation's
// claimable list, in the order the step gives. The server filters and ranks
// that list, so each step states the wire order directly and no test ranker
// can hide a server or client regression (component-cli-onboarding invariant
// 22).
func taskWaitTestClaimableOrders(step taskWaitStep) []taskWaitOrder {
	claimable := []taskWaitOrder{}
	if step.order.ID != "" {
		claimable = append(claimable, taskWaitOrder{ID: step.order.ID, Stage: step.order.Stage})
	}
	for _, order := range step.orders {
		claimable = append(claimable, taskWaitOrder{ID: order.ID, Stage: order.Stage})
	}
	return claimable
}

func taskWaitTestOptions(timeout time.Duration) taskWaitOptions {
	return taskWaitOptions{
		timeout: timeout,
		newPoller: func() *taskWaitPoller {
			return &taskWaitPoller{
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
	gate := &taskWaitTestGate{Kind: "spec", Label: "spec approval gate", SpecVersion: 1, CanOperate: true}
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

// A pending document proposal is no task gate: its arrival, version change, or
// resolution never ends a wait, and the wait never reads the proposal queue
// (req-260810-23b69f REQ-3; req-agent-skills AC-4.1).
func TestTaskWaitIgnoresDocumentProposals(t *testing.T) {
	proposal := taskWaitTestProposal{Kind: "design", DocumentID: "component-runtime", Title: "Runtime", Version: 17, CanConfirm: true}
	revised := taskWaitTestProposal{Kind: "design", DocumentID: "component-runtime", Title: "Runtime", Version: 18}
	f := newTaskWaitFixture(t,
		taskWaitStep{state: core.TaskRunning},
		taskWaitStep{state: core.TaskRunning, proposals: []taskWaitTestProposal{proposal}},
		taskWaitStep{state: core.TaskRunning, proposals: []taskWaitTestProposal{revised}},
		taskWaitStep{state: core.TaskRunning},
	)
	result, err := runTaskWaitJSON(t, f, 60*time.Millisecond)
	var timedOut *taskWaitTimeoutError
	if !errors.As(err, &timedOut) || result.Reason != taskWaitReasonTimeout {
		t.Fatalf("a proposal change ended the wait: err=%v result=%+v", err, result)
	}
	for _, path := range f.requestPaths() {
		if path == "/v1/pending-proposals" {
			t.Fatal("task wait read the pending-proposal queue")
		}
	}
}

func TestTaskWaitTimesOutWithLastObservation(t *testing.T) {
	f := newTaskWaitFixture(t, taskWaitStep{state: core.TaskAwaiting, gate: &taskWaitTestGate{Kind: "merge"}})
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
		taskWaitStep{state: core.TaskAwaiting, gate: &taskWaitTestGate{Kind: "merge"}},
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
	f := newTaskWaitFixture(t,
		taskWaitStep{state: core.TaskAwaiting, title: "one",
			gate: &taskWaitTestGate{Kind: "spec", Label: "spec approval gate", Summary: "submitted execution plan v2", SpecVersion: 2}},
		taskWaitStep{state: core.TaskAwaiting, title: "two",
			gate: &taskWaitTestGate{Kind: "spec", Label: "relabelled", Summary: "other", SpecVersion: 2, CanOperate: true, CanRequestChanges: true}},
	)
	result, err := runTaskWaitJSON(t, f, 60*time.Millisecond)
	var timedOut *taskWaitTimeoutError
	if !errors.As(err, &timedOut) {
		t.Fatalf("presentation churn ended the wait: err=%v result=%+v", err, result)
	}
	if result.PendingGate == nil || *result.PendingGate != (taskWaitGate{Kind: "plan", PlanVersion: 2}) {
		t.Fatalf("gate = %+v", result.PendingGate)
	}
}

func TestTaskWaitDetectsGateVersionChanges(t *testing.T) {
	for name, steps := range map[string][]taskWaitStep{
		"plan version": {
			{state: core.TaskAwaiting, gate: &taskWaitTestGate{Kind: "spec", SpecVersion: 1}},
			{state: core.TaskAwaiting, gate: &taskWaitTestGate{Kind: "spec", SpecVersion: 2}},
		},
		"gate kind": {
			{state: core.TaskAwaiting, gate: &taskWaitTestGate{Kind: "merge"}},
			{state: core.TaskAwaiting, gate: &taskWaitTestGate{Kind: "plan_revision", PlanVersion: 1}},
		},
		"plan revision version": {
			{state: core.TaskAwaiting, gate: &taskWaitTestGate{Kind: "plan_revision", PlanVersion: 3}},
			{state: core.TaskAwaiting, gate: &taskWaitTestGate{Kind: "plan_revision", PlanVersion: 4}},
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
		"claimable_orders": "[]", "observed_at": `"2026-10-04T08:00:00Z"`, "reason": `"terminal"`, "timed_out": "false",
	} {
		if got := string(raw[field]); got != want {
			t.Errorf("%s = %s, want %s", field, got, want)
		}
	}
	if _, ok := raw["last_read_error"]; ok {
		t.Errorf("last_read_error present without a failed read: %s", out.String())
	}
	if _, ok := raw["pending_proposals"]; ok {
		t.Errorf("pending_proposals is retired: %s", out.String())
	}
}

func TestTaskWaitHumanOutputMatchesJSONFields(t *testing.T) {
	steps := []taskWaitStep{
		{state: core.TaskRunning},
		{state: core.TaskAwaiting, gate: &taskWaitTestGate{Kind: "spec", SpecVersion: 3}},
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
		"claimable orders:  none",
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

// The wait polls the narrow observation route and sends the tag it holds, so
// an unchanged poll transfers no body (req-agent-skills AC-4.2, AC-4.3).
func TestTaskWaitPollsObservationWithIfNoneMatch(t *testing.T) {
	f := newTaskWaitFixture(t,
		taskWaitStep{state: core.TaskRunning},
		taskWaitStep{state: core.TaskRunning},
		taskWaitStep{state: core.TaskMerged},
	)
	if result, err := runTaskWaitJSON(t, f, 5*time.Second); err != nil || result.Reason != taskWaitReasonTerminal {
		t.Fatalf("err=%v result=%+v", err, result)
	}
	observationPath := "/v1/tasks/" + taskWaitTestTaskID + "/observation"
	paths := f.requestPaths()
	if len(paths) < 3 {
		t.Fatalf("polls=%v, want at least three", paths)
	}
	for _, path := range paths {
		if path != observationPath {
			t.Fatalf("the wait read %s, want %s alone", path, observationPath)
		}
	}
	tags := f.requestTags()
	if tags[0] != "" {
		t.Fatalf("the first poll sent If-None-Match %q", tags[0])
	}
	for i, tag := range tags[1:] {
		if tag == "" {
			t.Fatalf("poll %d sent no If-None-Match", i+2)
		}
	}
}

// The cadence starts at two seconds, doubles to a fifteen-second ceiling while
// nothing changes, and returns to two seconds after a change
// (req-agent-skills AC-4.2).
func TestTaskWaitPollerDoublesToTheCeiling(t *testing.T) {
	poller := &taskWaitPoller{base: taskWaitPollInterval, maximum: taskWaitPollCeiling, current: taskWaitPollInterval, jitter: func(delay, _ time.Duration) time.Duration { return delay }}
	delays := make([]time.Duration, 0, 5)
	for range 5 {
		delays = append(delays, poller.delay())
		poller.observe(false)
	}
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second, 15 * time.Second}
	if !slices.Equal(delays, want) {
		t.Fatalf("delays=%v, want %v", delays, want)
	}
	poller.observe(true)
	if delay := poller.delay(); delay != 2*time.Second {
		t.Fatalf("delay after a change=%s, want 2s", delay)
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
	for _, text := range []string{"0  the observation changed or the task is merged, closed, or parked", "2  the timeout elapsed", "1  failure", "2s to a 15s ceiling"} {
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

func taskWaitMergeExecutionGate(state, summary string) *taskWaitTestGate {
	gate := &taskWaitTestGate{Kind: taskWaitGateMergeExecution, Label: "merge execution pending", Summary: summary, CanOperate: true}
	if state != "" {
		gate.MergeReadiness = &taskMergeReadiness{State: state, HeadSHA: "0123456789abcdef", URL: "https://github.test/pr/" + summary}
	}
	return gate
}

// After approval a merge-gated task reports the pending merge act, and the
// wait returns on the move to merged (req-agent-skills AC-4.1, AC-4.2;
// component-cli-onboarding).
func TestTaskWaitReportsPendingMergeExecutionUntilMerged(t *testing.T) {
	approval := &taskWaitTestGate{Kind: "merge", Label: "merge approval gate", CanOperate: true}
	execution := taskWaitMergeExecutionGate("MERGEABLE", "1")
	steps := []taskWaitStep{
		{state: core.TaskAwaiting, gate: approval},
		{state: core.TaskApproved, gate: execution},
	}
	result, err := runTaskWaitJSON(t, newTaskWaitFixture(t, steps...), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := taskWaitGate{Kind: taskWaitGateMergeExecution, MergeReadiness: "MERGEABLE"}
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

func TestTaskWaitUntilResetsBackoffOnChange(t *testing.T) {
	running := untilAction("Running", "agent", "running", "implement")
	reviewing := untilAction("Reviewing", "agent", "review_pending", "review")
	operator := untilAction("Approval", "operator", "plan_approval", "")
	f := newTaskWaitFixture(t,
		taskWaitStep{state: core.TaskRunning, nextAction: running},
		taskWaitStep{state: core.TaskRunning, nextAction: running},
		taskWaitStep{state: core.TaskRunning, nextAction: running},
		taskWaitStep{state: core.TaskRunning, nextAction: reviewing},
		taskWaitStep{state: core.TaskAwaiting, nextAction: operator},
	)
	var delays []time.Duration
	options := taskWaitTestOptions(time.Second)
	options.until = "operator"
	options.newPoller = func() *taskWaitPoller {
		return &taskWaitPoller{base: time.Millisecond, maximum: 8 * time.Millisecond, current: time.Millisecond,
			jitter: func(delay, _ time.Duration) time.Duration {
				delays = append(delays, delay)
				return delay
			}}
	}
	if err := runTaskWait(t.Context(), f.client(), io.Discard, taskWaitTestTaskID, true, options); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond, time.Millisecond}
	if !slices.Equal(delays, want) {
		t.Fatalf("delays = %v, want %v", delays, want)
	}
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
		wantJSON := `{"task_id":"261004-abc123","state":"queued","pending_gate":null,"next_order":null,"claimable_orders":[],"observed_at":"2026-10-04T08:00:00Z","reason":"changed","timed_out":false}` + "\n"
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
			"claimable orders:  none",
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

// Each observation reads only routes that survive the retired run-order and
// worker planes, and the next order is this task's queued claimable order
// (req-agent-skills AC-4.1; DEC-97(1); component-cli-onboarding).
func TestTaskWaitReadsOnlySurvivingRoutes(t *testing.T) {
	f := newTaskWaitFixture(t,
		taskWaitStep{state: core.TaskAwaiting, gate: &taskWaitTestGate{Kind: "spec", SpecVersion: 2}},
		taskWaitStep{state: core.TaskApproved, gate: taskWaitMergeExecutionGate("MERGEABLE", "1")},
	)
	if result, err := runTaskWaitJSON(t, f, 5*time.Second); err != nil || result.Reason != taskWaitReasonChanged {
		t.Fatalf("err=%v result=%+v", err, result)
	}
	allowed := map[string]bool{
		"/v1/tasks/" + taskWaitTestTaskID + "/observation":           true,
		"/v1/tasks/" + taskWaitTestTaskID + "/spec":                  true,
		"/v1/tasks/" + taskWaitTestTaskID + "/activity?view=bounded": true,
	}
	seen := map[string]bool{}
	for _, path := range f.requestPaths() {
		if !allowed[path] {
			t.Errorf("task wait read %s", path)
		}
		seen[path] = true
	}
	for path := range allowed {
		if !seen[path] {
			t.Errorf("task wait never read %s", path)
		}
	}
	result, err := runTaskWaitJSON(t, newTaskWaitFixture(t,
		taskWaitStep{state: core.TaskQueued, order: core.WorkOrder{ID: "261004-abc123-review-1-seat-1", Stage: core.StageReview}},
	), 20*time.Millisecond)
	var timedOut *taskWaitTimeoutError
	if !errors.As(err, &timedOut) || result.NextOrder == nil || result.NextOrder.ID != "261004-abc123-review-1-seat-1" {
		t.Fatalf("err=%v result=%+v; want only this task's claimable order", err, result)
	}
}

// A plan revision stays observed beside a claimable order, so a new plan version
// still ends the wait (req-agent-skills AC-4.1).
func TestTaskWaitPlanRevisionVersionBesideNextOrderEndsWait(t *testing.T) {
	order := core.WorkOrder{ID: "261004-abc123-implement-1", Stage: core.StageImplement}
	result, err := runTaskWaitJSON(t, newTaskWaitFixture(t,
		taskWaitStep{state: core.TaskAwaiting, order: order, gate: &taskWaitTestGate{Kind: "plan_revision", PlanVersion: 4}},
		taskWaitStep{state: core.TaskAwaiting, order: order, gate: &taskWaitTestGate{Kind: "plan_revision", PlanVersion: 5}},
	), 5*time.Second)
	if err != nil || result.Reason != taskWaitReasonChanged || result.NextOrder == nil || result.PendingGate == nil || result.PendingGate.PlanVersion != 5 {
		t.Fatalf("err=%v result=%+v", err, result)
	}
}

func TestTaskWaitReportsPlanRevisionGateAndNextOrderPrecedence(t *testing.T) {
	result, err := runTaskWaitJSON(t, newTaskWaitFixture(t,
		taskWaitStep{state: core.TaskAwaiting, gate: &taskWaitTestGate{Kind: "plan_revision", PlanVersion: 4}},
	), 20*time.Millisecond)
	var timedOut *taskWaitTimeoutError
	if !errors.As(err, &timedOut) {
		t.Fatal(err)
	}
	if result.PendingGate == nil || *result.PendingGate != (taskWaitGate{Kind: "plan_revision", PlanVersion: 4}) {
		t.Fatalf("result = %+v", result)
	}
}

// A wait stops at a check hold, whether the hold was there at the start or
// arrived during the wait. No later observation would resolve it, because it
// needs an operator (req-review-gates-evidence AC-1.6).
func TestTaskWaitStopsAtCheckHold(t *testing.T) {
	hold := &taskWaitTestGate{Kind: taskWaitGateCheckHold}
	t.Run("already held", func(t *testing.T) {
		f := newTaskWaitFixture(t, taskWaitStep{state: core.TaskApproved, gate: hold})
		result, err := runTaskWaitJSON(t, f, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if result.Reason != taskWaitReasonHeld || result.PendingGate == nil || result.PendingGate.Kind != taskWaitGateCheckHold {
			t.Fatalf("result = %+v", result)
		}
	})
	t.Run("held during the wait", func(t *testing.T) {
		merging := untilAction("Merging", "factory", "merging", "")
		f := newTaskWaitFixture(t,
			taskWaitStep{state: core.TaskApproved, nextAction: merging},
			taskWaitStep{state: core.TaskApproved, gate: hold},
		)
		result, err := runTaskWaitJSON(t, f, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if result.Reason != taskWaitReasonHeld {
			t.Fatalf("result = %+v", result)
		}
	})
	t.Run("until a stage never reached", func(t *testing.T) {
		f := newTaskWaitFixture(t,
			taskWaitStep{state: core.TaskApproved, nextAction: untilAction("Merging", "factory", "merging", "")},
			taskWaitStep{state: core.TaskApproved, nextAction: untilAction("Checks hold the merge", "operator", "check_hold", ""), gate: hold},
		)
		result, _, err := runTaskWaitUntil(t, f, "terminal", 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if result.Reason != taskWaitReasonHeld {
			t.Fatalf("result = %+v", result)
		}
	})
}

// An enforcing task held for a person after approval has its frozen merge gate
// off, and its stated merge_execution kind stops the wait at the pending merge
// act as the gate-on task does (DEC-124(2); req-risk-based-review AC-8.2).
func TestTaskWaitStopsAtARiskHeldMerge(t *testing.T) {
	merging := untilAction("Merging", "factory", "merging", "")
	steps := []taskWaitStep{
		{state: core.TaskApproved, nextAction: merging},
		{state: core.TaskApproved, gate: taskWaitMergeExecutionGate("MERGEABLE", "7"), frozenGateOff: true},
	}
	result, err := runTaskWaitJSON(t, newTaskWaitFixture(t, steps...), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := taskWaitGate{Kind: taskWaitGateMergeExecution, MergeReadiness: "MERGEABLE"}
	if result.PendingGate == nil || *result.PendingGate != want || result.State != core.TaskApproved {
		t.Fatalf("result = %+v gate %+v", result, result.PendingGate)
	}
}

// A risk order is claimable work like every other order: its creation ends a
// wait, and a repeated observation of the same risk order does not
// (component-cli-onboarding invariant 22; req-agent-skills AC-4.1).
func TestTaskWaitReturnsWhenRiskOrderBecomesClaimable(t *testing.T) {
	risk := core.WorkOrder{ID: "261004-abc123-risk-1", Stage: core.StageRisk}
	f := newTaskWaitFixture(t,
		taskWaitStep{state: core.TaskRunning},
		taskWaitStep{state: core.TaskRunning, order: risk},
	)
	result, err := runTaskWaitJSON(t, f, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reason != taskWaitReasonChanged || result.NextOrder == nil || result.NextOrder.ID != risk.ID || result.NextOrder.Stage != core.StageRisk {
		t.Fatalf("result = %+v", result)
	}

	stable := newTaskWaitFixture(t,
		taskWaitStep{state: core.TaskRunning, order: risk},
		taskWaitStep{state: core.TaskRunning, order: risk},
	)
	result, err = runTaskWaitJSON(t, stable, 60*time.Millisecond)
	var timedOut *taskWaitTimeoutError
	if !errors.As(err, &timedOut) || result.NextOrder == nil || result.NextOrder.ID != risk.ID || result.NextOrder.Stage != core.StageRisk {
		t.Fatalf("a repeated risk order ended the wait: err=%v result=%+v", err, result)
	}
}

// A wait sees ordinary work beside a risk order first, and the risk order once
// it stands alone.
func TestTaskWaitReportsOrdinaryWorkBesideARiskOrder(t *testing.T) {
	risk := core.WorkOrder{ID: "261004-abc123-risk-1", Stage: core.StageRisk, State: core.WorkOrderQueued, Claimable: true, QueueEnteredAt: taskWaitTestTime}
	f := newTaskWaitFixture(t,
		taskWaitStep{state: core.TaskQueued, order: core.WorkOrder{ID: "261004-abc123-implement-1", Stage: core.StageImplement}, orders: []core.WorkOrder{risk}},
		taskWaitStep{state: core.TaskRunning, orders: []core.WorkOrder{risk}},
	)
	result, err := runTaskWaitJSON(t, f, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reason != taskWaitReasonChanged || result.NextOrder == nil || result.NextOrder.ID != risk.ID || result.NextOrder.Stage != core.StageRisk {
		t.Fatalf("result = %+v", result)
	}
}

// --until stage=risk is a valid target, reached when the next action names a
// risk order.
func TestTaskWaitUntilRiskStage(t *testing.T) {
	if _, err := parseTaskWaitTarget("stage=risk"); err != nil {
		t.Fatalf("stage=risk was refused: %v", err)
	}
	if _, err := parseTaskWaitTarget("stage=deploy"); err == nil || !strings.Contains(err.Error(), "review, or risk") {
		t.Fatalf("unknown stage err = %v", err)
	}
	f := newTaskWaitFixture(t,
		taskWaitStep{state: core.TaskRunning, nextAction: untilAction("Implementing", "agent", "running", "implement")},
		taskWaitStep{state: core.TaskAwaiting, nextAction: untilAction("Waiting for a risk rating", "agent", "queued", "risk")},
	)
	result, _, err := runTaskWaitUntil(t, f, "stage=risk", 5*time.Second)
	if err != nil || result.Reason != taskWaitReasonReached || result.NextAction == nil || result.NextAction.Stage != "risk" {
		t.Fatalf("err=%v result=%+v", err, result)
	}
}

// claimableTestOrder is a queued, claimable order of the waited task that a
// step serves through the work-order listing.
func claimableTestOrder(id string, stage core.Stage, entered time.Time) core.WorkOrder {
	return core.WorkOrder{ID: id, Stage: stage, State: core.WorkOrderQueued, Claimable: true, QueueEnteredAt: entered}
}

// runTaskWaitBoth runs one wait in JSON and one in text over fresh fixtures of
// the same steps, and returns the raw JSON fields and the text output.
func runTaskWaitBoth(t *testing.T, timeout time.Duration, steps ...taskWaitStep) (map[string]json.RawMessage, string) {
	t.Helper()
	var out bytes.Buffer
	err := runTaskWait(t.Context(), newTaskWaitFixture(t, steps...).client(), &out, taskWaitTestTaskID, true, taskWaitTestOptions(timeout))
	var timedOut *taskWaitTimeoutError
	if err != nil && !errors.As(err, &timedOut) {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	out.Reset()
	err = runTaskWait(t.Context(), newTaskWaitFixture(t, steps...).client(), &out, taskWaitTestTaskID, false, taskWaitTestOptions(timeout))
	if err != nil && !errors.As(err, &timedOut) {
		t.Fatal(err)
	}
	return raw, out.String()
}

// The wait result names every claimable order of the task in claimable_orders,
// in next-order rank, as an empty list when there is none, in JSON and in text
// output. next_order stays the first entry (component-cli-onboarding invariant
// 22; req-agent-skills AC-4.1).
func TestTaskWaitReportsClaimableOrders(t *testing.T) {
	risk := claimableTestOrder("261004-abc123-risk-1", core.StageRisk, taskWaitTestTime)
	verify := claimableTestOrder("261004-abc123-verify-1", core.StageVerify, taskWaitTestTime.Add(5*time.Minute))
	for _, tc := range []struct {
		name      string
		changed   taskWaitStep
		wantList  string
		wantNext  string
		wantLines []string
	}{
		{
			name:      "no claimable order",
			changed:   taskWaitStep{state: core.TaskQueued},
			wantList:  `[]`,
			wantNext:  `null`,
			wantLines: []string{"next order:        none", "claimable orders:  none"},
		},
		{
			name:      "one order",
			changed:   taskWaitStep{state: core.TaskQueued, order: core.WorkOrder{ID: "261004-abc123-implement-1", Stage: core.StageImplement}},
			wantList:  `[{"id":"261004-abc123-implement-1","stage":"implement"}]`,
			wantNext:  `{"id":"261004-abc123-implement-1","stage":"implement"}`,
			wantLines: []string{"next order:        261004-abc123-implement-1 (implement)", "claimable orders:  261004-abc123-implement-1 (implement)"},
		},
		{
			name:     "verification beside risk",
			changed:  taskWaitStep{state: core.TaskRunning, orders: []core.WorkOrder{verify, risk}},
			wantList: `[{"id":"261004-abc123-verify-1","stage":"verify"},{"id":"261004-abc123-risk-1","stage":"risk"}]`,
			wantNext: `{"id":"261004-abc123-verify-1","stage":"verify"}`,
			wantLines: []string{
				"next order:        261004-abc123-verify-1 (verify)",
				"claimable orders:  261004-abc123-verify-1 (verify), 261004-abc123-risk-1 (risk)",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, text := runTaskWaitBoth(t, 5*time.Second, taskWaitStep{state: core.TaskAwaiting, gate: &taskWaitTestGate{Kind: "merge"}}, tc.changed)
			if got := string(raw["reason"]); got != `"changed"` {
				t.Fatalf("reason = %s", got)
			}
			if got := string(raw["claimable_orders"]); got != tc.wantList {
				t.Errorf("claimable_orders = %s, want %s", got, tc.wantList)
			}
			if got := string(raw["next_order"]); got != tc.wantNext {
				t.Errorf("next_order = %s, want %s", got, tc.wantNext)
			}
			for _, line := range append(tc.wantLines, "wait ended:        changed") {
				if !strings.Contains(text, line+"\n") {
					t.Errorf("text output lacks %q:\n%s", line, text)
				}
			}
		})
	}
}

// The wait reports the observation's claimable_orders in the server's order
// and takes the first entry as next_order. The served plan order entered the
// queue after the implementation order, so a client that ranked by queue
// entry, or took any entry but the first, would report the implementation
// order (component-cli-onboarding invariant 22; req-agent-skills AC-4.1).
func TestTaskWaitReportsTheServerRankedOrderAsNextOrder(t *testing.T) {
	implement := claimableTestOrder("261004-abc123-implement-1", core.StageImplement, taskWaitTestTime)
	spec := claimableTestOrder("261004-abc123-spec-2", core.StageSpec, taskWaitTestTime.Add(5*time.Minute))
	raw, text := runTaskWaitBoth(t, 5*time.Second,
		taskWaitStep{state: core.TaskRunning},
		taskWaitStep{state: core.TaskRunning, orders: []core.WorkOrder{spec, implement}},
	)
	if got := string(raw["reason"]); got != `"changed"` {
		t.Fatalf("reason = %s", got)
	}
	want := `[{"id":"261004-abc123-spec-2","stage":"spec"},{"id":"261004-abc123-implement-1","stage":"implement"}]`
	if got := string(raw["claimable_orders"]); got != want {
		t.Fatalf("claimable_orders = %s\nwant %s", got, want)
	}
	if got := string(raw["next_order"]); got != `{"id":"261004-abc123-spec-2","stage":"spec"}` {
		t.Fatalf("next_order = %s", got)
	}
	for _, line := range []string{
		"next order:        261004-abc123-spec-2 (spec)\n",
		"claimable orders:  261004-abc123-spec-2 (spec), 261004-abc123-implement-1 (implement)\n",
	} {
		if !strings.Contains(text, line) {
			t.Fatalf("text output lacks %q:\n%s", line, text)
		}
	}
}

// A change in claimable_orders alone does not end a wait: adding, removing, or
// replacing an order behind the same next order keeps the wait open, and the
// timeout reports the last successful list (component-cli-onboarding invariant
// 22; req-agent-skills AC-4.1, AC-4.3).
func TestTaskWaitClaimableOrdersChangeAloneDoesNotEndWait(t *testing.T) {
	implement := claimableTestOrder("261004-abc123-implement-1", core.StageImplement, taskWaitTestTime)
	risk := claimableTestOrder("261004-abc123-risk-1", core.StageRisk, taskWaitTestTime)
	review := claimableTestOrder("261004-abc123-review-1-seat-1", core.StageReview, taskWaitTestTime)
	verify := claimableTestOrder("261004-abc123-verify-1", core.StageVerify, taskWaitTestTime)
	steps := []taskWaitStep{
		{state: core.TaskRunning, orders: []core.WorkOrder{implement, risk}},
		{state: core.TaskRunning, orders: []core.WorkOrder{implement, review, risk}},
		{state: core.TaskRunning, orders: []core.WorkOrder{implement, review}},
		{state: core.TaskRunning, orders: []core.WorkOrder{implement, verify}},
	}
	wantList := []taskWaitOrder{{ID: implement.ID, Stage: core.StageImplement}, {ID: verify.ID, Stage: core.StageVerify}}
	for _, tc := range []struct {
		name    string
		steps   []taskWaitStep
		wantErr string
	}{
		{"last read succeeded", steps, ""},
		{"last read failed", append(append([]taskWaitStep(nil), steps...), taskWaitStep{status: http.StatusServiceUnavailable}), "503"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTaskWaitFixture(t, tc.steps...)
			result, err := runTaskWaitJSON(t, f, 150*time.Millisecond)
			var timedOut *taskWaitTimeoutError
			if !errors.As(err, &timedOut) || result.Reason != taskWaitReasonTimeout || !result.TimedOut {
				t.Fatalf("a claimable_orders change ended the wait: err=%v result=%+v", err, result)
			}
			if f.taskReads() <= len(tc.steps) {
				t.Fatalf("task reads = %d; the wait did not observe every step", f.taskReads())
			}
			if result.NextOrder == nil || *result.NextOrder != wantList[0] {
				t.Fatalf("next order = %+v", result.NextOrder)
			}
			if len(result.ClaimableOrders) != len(wantList) || result.ClaimableOrders[0] != wantList[0] || result.ClaimableOrders[1] != wantList[1] {
				t.Fatalf("claimable orders = %+v, want the last successful list %+v", result.ClaimableOrders, wantList)
			}
			if tc.wantErr == "" && result.LastReadError != "" || !strings.Contains(result.LastReadError, tc.wantErr) {
				t.Fatalf("last read error = %q", result.LastReadError)
			}
		})
	}
}
