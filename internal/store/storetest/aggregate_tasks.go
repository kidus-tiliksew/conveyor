package storetest

import (
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// fixtureTaskIDSequence numbers every fixture task ID handed out in this
// process. It starts at zero and only increases.
var fixtureTaskIDSequence atomic.Uint64

// fixtureTaskID returns a task ID that no other call in this process returns:
// core.NewTaskID() followed by a process-wide sequence number, shaped
// <yymmdd>-<6 hex>-<n>. core.NewTaskID() alone draws from about 16.7 million
// values a day, so the roughly 1,000 tasks the review-summary stress cases
// create in one store collided a few percent of runs. The sequence makes
// repeats impossible within a process; the random part keeps IDs spread across
// SingleStore partitions, which those stress cases need. Production task IDs
// keep the core.NewTaskID() format (component-task-lifecycle).
func fixtureTaskID() string {
	return core.NewTaskID() + "-" + strconv.FormatUint(fixtureTaskIDSequence.Add(1), 10)
}

func newAggregateTask(t *testing.T, x Fixture, stages ...core.Stage) core.Task {
	t.Helper()
	id := fixtureTaskID()
	stage := core.StageImplement
	if len(stages) > 0 {
		stage = stages[0]
	}
	task := core.Task{ID: id, Workspace: x.Workspace, Repo: "conveyor", Title: id, BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, NextStage: stage, CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
	if stage == core.StageVerify {
		task.ReviewedHeadSHA = "submitted-head"
		task.SetupContract.VerifyStage = true
	}
	requireOK(t, x.Backend.CreateTask(x.Context, task))
	return task
}

func runTaskPagination(t *testing.T, x Fixture) {
	created := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	ids := []string{"oldest-" + core.NewTaskID(), "zeta-" + core.NewTaskID(), "alpha-" + core.NewTaskID(), "newest-" + core.NewTaskID()}
	for i, at := range []time.Time{created.Add(-time.Hour), created, created, created.Add(time.Hour)} {
		task := core.Task{ID: ids[i], Workspace: x.Workspace, Title: ids[i], Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + ids[i], State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: at}
		requireOK(t, x.Backend.CreateTask(x.Context, task))
	}
	RunTaskOperationsPaginationConformance(t, TaskOperationsFixture{Store: x.Backend, Context: x.Context, WantTotal: 4, WantOrder: []string{ids[3], ids[2], ids[1], ids[0]}, Filter: store.TaskFilter{Repositories: []string{"conveyor"}}})
}
