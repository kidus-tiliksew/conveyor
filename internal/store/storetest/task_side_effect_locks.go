package storetest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// runDurableTaskSideEffectLocks pins the database command/callback lock
// contract. The volatile test double does not own database-session locks.
func runDurableTaskSideEffectLocks(t *testing.T, x Fixture) {
	if !x.Backend.IsDurable() {
		return
	}
	st := x.Backend
	for _, different := range []string{"same task", "different task", "different workspace"} {
		t.Run(different, func(t *testing.T) {
			first := newAggregateTask(t, x)
			target := first
			ctx, cancel := context.WithTimeout(x.Context, 10*time.Second)
			defer cancel()
			commandCtx := ctx
			if different == "different task" {
				target = newAggregateTask(t, x)
			}
			if different == "different workspace" {
				ws := "locks-" + core.NewTaskID()
				_, err := st.CreateWorkspace(ctx, ws, ws, x.Config)
				requireOK(t, err)
				other := x
				other.Workspace = ws
				other.Context = store.WithWorkspace(ctx, ws)
				target = newAggregateTask(t, other)
				commandCtx = other.Context
			}
			held := make(chan struct{})
			release := make(chan struct{})
			callbackDone := make(chan error, 1)
			go func() {
				callbackDone <- st.WithTaskSideEffectLock(ctx, first.ID, func(context.Context) error {
					close(held)
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
			}()
			select {
			case <-held:
			case err := <-callbackDone:
				t.Fatalf("callback lock: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			started := make(chan struct{})
			commandDone := make(chan error, 1)
			go func() {
				close(started)
				_, err := taskops.New(st).Perform(commandCtx, target.ID, taskops.Command{Kind: core.TaskGateMerge})
				commandDone <- err
			}()
			<-started
			if different == "same task" {
				select {
				case err := <-commandDone:
					close(release)
					<-callbackDone
					t.Fatalf("command crossed callback lock: %v", err)
				case <-time.After(200 * time.Millisecond):
				}
				current, err := st.GetTask(ctx, target.ID)
				requireOK(t, err)
				if current.State != core.TaskRunning {
					t.Fatalf("command changed task while callback held lock: %s", current.State)
				}
				close(release)
				requireOK(t, <-callbackDone)
				select {
				case err := <-commandDone:
					requireOK(t, err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			} else {
				select {
				case err := <-commandDone:
					requireOK(t, err)
				case <-ctx.Done():
					t.Fatal("unrelated task command blocked behind callback")
				}
				close(release)
				requireOK(t, <-callbackDone)
			}
		})
	}
	t.Run("nested command rollback", func(t *testing.T) {
		task := newAggregateTask(t, x)
		ctx, cancel := context.WithTimeout(x.Context, 10*time.Second)
		defer cancel()
		before, err := st.ListEvents(ctx, task.ID)
		requireOK(t, err)
		callbackError := errors.New("callback result")
		err = st.WithTaskSideEffectLock(ctx, task.ID, func(locked context.Context) error {
			bad := store.WithActor(locked, store.Actor{ID: "invalid\x00actor", Role: core.ActorUser})
			if _, err := taskops.New(st).Perform(bad, task.ID, taskops.Command{Kind: core.TaskGateMerge}); err == nil {
				return fmt.Errorf("invalid audit actor accepted")
			}
			current, err := st.GetTask(locked, task.ID)
			if err != nil {
				return err
			}
			if current.State != core.TaskRunning {
				return fmt.Errorf("failed nested command committed state %s", current.State)
			}
			after, err := st.ListEvents(locked, task.ID)
			if err != nil {
				return err
			}
			if len(after) != len(before) {
				return fmt.Errorf("failed nested command committed an event")
			}
			if _, err := taskops.New(st).Perform(locked, task.ID, taskops.Command{Kind: core.TaskGateMerge}); err != nil {
				return err
			}
			return callbackError
		})
		if err != callbackError {
			t.Fatalf("callback error changed or nested command failed: %v", err)
		}
		current, err := st.GetTask(ctx, task.ID)
		requireOK(t, err)
		if current.State != core.TaskAwaiting {
			t.Fatalf("successful nested command did not commit: %s", current.State)
		}
	})
}

// PolicyChangeLockBound bounds each policy change made through the side-effect
// lock path. A change that waits on its own caller's lock fails the test when
// this deadline ends instead of stalling the job.
const PolicyChangeLockBound = 5 * time.Second

// ChangeTaskPolicyUnderSideEffectLock changes one task's frozen policy through
// the path POST /v1/tasks/{id}/policy takes: WithTaskSideEffectLock, then
// taskops.ExecuteSetupChange and ChangeTaskPolicyCommand on the locked context
// (component-task-lifecycle invariant 44; component-persistence invariant 6).
func ChangeTaskPolicyUnderSideEffectLock(ctx context.Context, st store.Store, request store.SetupChangeRequest) (store.SetupChangeResult, error) {
	var result store.SetupChangeResult
	err := st.WithTaskSideEffectLock(ctx, request.TaskID, func(locked context.Context) error {
		var changeErr error
		result, changeErr = taskops.ExecuteSetupChange(locked, st, request.TaskID, func(lease taskops.TaskLease) (store.SetupChangeResult, error) {
			return st.ChangeTaskPolicyCommand(locked, lease, request)
		})
		return changeErr
	})
	return result, err
}

type policyChangeOutcome struct {
	result store.SetupChangeResult
	err    error
}

// boundedPolicyChange starts one lock-path policy change when start closes and
// delivers its outcome on the returned buffered channel, so a test that stops
// waiting never strands the goroutine.
func boundedPolicyChange(ctx context.Context, st store.Store, request store.SetupChangeRequest, start <-chan struct{}) <-chan policyChangeOutcome {
	done := make(chan policyChangeOutcome, 1)
	go func() {
		<-start
		result, err := ChangeTaskPolicyUnderSideEffectLock(ctx, st, request)
		done <- policyChangeOutcome{result: result, err: err}
	}()
	return done
}

// awaitPolicyChange fails the test when the change does not return before the
// bound's context deadline, or returns an error.
func awaitPolicyChange(t *testing.T, ctx context.Context, name string, done <-chan policyChangeOutcome) store.SetupChangeResult {
	t.Helper()
	select {
	case outcome := <-done:
		if outcome.err != nil {
			t.Fatalf("%s through WithTaskSideEffectLock failed within %s: %v", name, PolicyChangeLockBound, outcome.err)
		}
		return outcome.result
	case <-ctx.Done():
		t.Fatalf("%s through WithTaskSideEffectLock did not finish within %s: %v", name, PolicyChangeLockBound, ctx.Err())
	}
	return store.SetupChangeResult{}
}

func countTaskEvents(t *testing.T, ctx context.Context, st store.Store, taskID, kind string) int {
	t.Helper()
	events, err := st.ListEvents(ctx, taskID)
	requireOK(t, err)
	count := 0
	for _, event := range events {
		if event.Kind == kind {
			count++
		}
	}
	return count
}

func requireSameSetupChangeResult(t *testing.T, name string, want, got store.SetupChangeResult) {
	t.Helper()
	wantJSON, err := json.Marshal(want)
	requireOK(t, err)
	gotJSON, err := json.Marshal(got)
	requireOK(t, err)
	if !bytes.Equal(wantJSON, gotJSON) {
		t.Fatalf("%s replay result changed:\nwant %s\n got %s", name, wantJSON, gotJSON)
	}
}

// RunTaskPolicyChangeUnderSideEffectLock pins that a durable backend completes
// a policy change made under the task's side-effect lock, alone and with two
// overlapping changes for one task, and that replay identity holds for both
// (component-persistence invariant 6; component-task-lifecycle invariant 44).
// newTask creates a running task in ctx's workspace. ctx carries the human
// operator actor the policy route records.
func RunTaskPolicyChangeUnderSideEffectLock(t *testing.T, ctx context.Context, st store.Store, newTask func(*testing.T) core.Task) {
	t.Run("single change", func(t *testing.T) {
		task := newTask(t)
		bounded, cancel := context.WithTimeout(ctx, PolicyChangeLockBound)
		defer cancel()
		request := store.SetupChangeRequest{TaskID: task.ID, RequestID: "lock-path-single-" + task.ID, Reason: "longer verification", Policy: &store.TaskPolicyChange{StageTimeouts: map[string]string{"verify": "2h"}}}
		start := make(chan struct{})
		close(start)
		result := awaitPolicyChange(t, bounded, "policy change", boundedPolicyChange(bounded, st, request, start))
		if result.RequestID != request.RequestID || result.ReviewTransition != "policy_only" || result.Task.SetupContract.StageTimeouts.Verify != "2h" {
			t.Fatalf("policy change result=%+v", result)
		}
		stored, err := st.GetTask(ctx, task.ID)
		requireOK(t, err)
		if stored.SetupContract.StageTimeouts.Verify != "2h" {
			t.Fatalf("policy change was not recorded: contract=%+v", stored.SetupContract)
		}
		if got := countTaskEvents(t, ctx, st, task.ID, "task.setup.changed"); got != 1 {
			t.Fatalf("task.setup.changed events=%d, want 1", got)
		}
	})
	t.Run("concurrent changes", func(t *testing.T) {
		task := newTask(t)
		bounded, cancel := context.WithTimeout(ctx, PolicyChangeLockBound)
		defer cancel()
		distinct := true
		requests := []store.SetupChangeRequest{
			{TaskID: task.ID, RequestID: "lock-path-verify-" + task.ID, Reason: "longer verification", Policy: &store.TaskPolicyChange{StageTimeouts: map[string]string{"verify": "2h"}}},
			{TaskID: task.ID, RequestID: "lock-path-models-" + task.ID, Reason: "distinct review models", Policy: &store.TaskPolicyChange{ReviewDistinctModels: &distinct}},
		}
		// Both changes leave the barrier together, so the second contends for
		// the task lock while the first holds it.
		start := make(chan struct{})
		pending := make([]<-chan policyChangeOutcome, len(requests))
		for i, request := range requests {
			pending[i] = boundedPolicyChange(bounded, st, request, start)
		}
		close(start)
		results := make([]store.SetupChangeResult, len(requests))
		for i, done := range pending {
			results[i] = awaitPolicyChange(t, bounded, fmt.Sprintf("concurrent policy change %s", requests[i].RequestID), done)
			if results[i].RequestID != requests[i].RequestID {
				t.Fatalf("concurrent policy change %d result=%+v", i, results[i])
			}
		}
		stored, err := st.GetTask(ctx, task.ID)
		requireOK(t, err)
		if stored.SetupContract.StageTimeouts.Verify != "2h" || !stored.SetupContract.Review.DistinctModels {
			t.Fatalf("serialized policy changes lost one change: contract=%+v", stored.SetupContract)
		}
		if got := countTaskEvents(t, ctx, st, task.ID, "task.setup.changed"); got != len(requests) {
			t.Fatalf("task.setup.changed events=%d, want %d", got, len(requests))
		}
		// Replay identity: the same request ID and inputs return the recorded
		// result and write nothing; the same request ID with other inputs is
		// refused.
		replayCtx, replayCancel := context.WithTimeout(ctx, PolicyChangeLockBound)
		defer replayCancel()
		for i, request := range requests {
			started := make(chan struct{})
			close(started)
			replayed := awaitPolicyChange(t, replayCtx, "policy change replay "+request.RequestID, boundedPolicyChange(replayCtx, st, request, started))
			requireSameSetupChangeResult(t, request.RequestID, results[i], replayed)
			conflicting := request
			conflicting.Reason = "different inputs"
			if _, err := ChangeTaskPolicyUnderSideEffectLock(replayCtx, st, conflicting); !errors.Is(err, store.ErrSetupChangeConflict) {
				t.Fatalf("request %s reused with different inputs: err=%v, want ErrSetupChangeConflict", request.RequestID, err)
			}
		}
		if got := countTaskEvents(t, ctx, st, task.ID, "task.setup.changed"); got != len(requests) {
			t.Fatalf("replays wrote task.setup.changed events: got %d, want %d", got, len(requests))
		}
	})
}
