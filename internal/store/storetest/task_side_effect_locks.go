package storetest

import (
	"context"
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
