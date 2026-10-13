package storetest

import (
	"errors"
	"sync"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// runTaskBranchConcurrentClaims races creates and attaches for one open-task
// branch: exactly one writer wins and every loser is refused naming it
// (req-task-branch-assignment AC-4.1; DEC-68, component-persistence).
func runTaskBranchConcurrentClaims(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	const creators, attachersN = 8, 8
	race := func(n int, fn func(i int) error) []error {
		start := make(chan struct{})
		results := make([]error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				results[i] = fn(i)
			}()
		}
		close(start)
		wg.Wait()
		return results
	}
	settle := func(results []error, ids []string) string {
		t.Helper()
		winner := ""
		for i, err := range results {
			if err == nil {
				if winner != "" {
					t.Fatalf("two winners: %s and %s", winner, ids[i])
				}
				winner = ids[i]
			}
		}
		if winner == "" {
			t.Fatalf("no winner: %v", results)
		}
		for i, err := range results {
			if ids[i] == winner {
				continue
			}
			var inUse *store.BranchInUseError
			if !errors.Is(err, store.ErrTaskBranchConflict) || !errors.As(err, &inUse) || inUse.OtherTaskID != winner {
				t.Fatalf("loser %s: want conflict naming %s, got %v", ids[i], winner, err)
			}
		}
		return winner
	}

	branch := "race/create-" + core.NewTaskID()
	ids := make([]string, creators)
	for i := range ids {
		ids[i] = core.NewTaskID()
	}
	settle(race(creators, func(i int) error {
		return st.CreateTask(ctx, core.Task{ID: ids[i], Workspace: x.Workspace, Repo: "conveyor", Title: "race", BaseBranch: "main", Branch: branch, State: core.TaskQueued, NextStage: core.StageImplement})
	}), ids)
	created := 0
	for _, id := range ids {
		events, err := st.ListEvents(ctx, id)
		requireOK(t, err)
		for _, e := range events {
			if e.Kind == "task.created" {
				created++
			}
		}
	}
	if created != 1 {
		t.Fatalf("task.created events across contenders=%d", created)
	}

	target := "race/attach-" + core.NewTaskID()
	attachers := make([]string, attachersN)
	for i := range attachers {
		attachers[i] = newAggregateTask(t, x).ID
	}
	winner := settle(race(attachersN, func(i int) error {
		_, err := st.AttachTaskBranch(ctx, attachers[i], target)
		return err
	}), attachers)
	for _, id := range attachers {
		task, err := st.GetTask(ctx, id)
		requireOK(t, err)
		if (task.Branch == target) != (id == winner) {
			t.Fatalf("task %s branch=%s, winner %s", id, task.Branch, winner)
		}
	}
}
