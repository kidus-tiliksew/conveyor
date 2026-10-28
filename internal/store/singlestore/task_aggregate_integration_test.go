package singlestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/s2log"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func taskAggregateFixture(t *testing.T) (*Store, context.Context) {
	t.Helper()
	s := integrationStore(t)
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "task-tests")
	if _, err := s.CreateWorkspace(ctx, "task-tests", "Tasks", &config.Config{Workspace: "task-tests"}); err != nil {
		t.Fatal(err)
	}
	return s, ctx
}
func aggregateTask(ctx context.Context, id string) core.Task {
	return core.Task{ID: id, Workspace: documentWorkspace(ctx), Branch: "conveyor/" + id, Title: id, State: core.TaskRunning, NextStage: core.StageImplement}
}
func taskTestOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestTaskCallbackCleanupIntegration(t *testing.T) {
	s, ctx := taskAggregateFixture(t)
	taskTestOK(t, s.CreateTask(ctx, aggregateTask(ctx, "locked")))
	t.Run("cancellation holds until callback returns", func(t *testing.T) {
		ownerCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		held, release := make(chan struct{}), make(chan struct{})
		done := make(chan error, 1)
		sentinel := errors.New("callback cancelled after cleanup")
		go func() {
			done <- s.WithTaskSideEffectLock(ownerCtx, "locked", func(context.Context) error { close(held); <-release; return sentinel })
		}()
		select {
		case <-held:
		case err := <-done:
			t.Fatal(err)
		case <-time.After(5 * time.Second):
			t.Fatal("callback did not acquire")
		}
		cancel()
		waitCtx, stop := context.WithTimeout(ctx, 200*time.Millisecond)
		_, err := taskops.New(s).SetAssignee(waitCtx, "locked", "")
		stop()
		if err == nil {
			close(release)
			t.Fatal("cancellation released lock before callback returned")
		}
		close(release)
		if err = <-done; err != sentinel {
			t.Fatalf("callback error changed: %v", err)
		}
		_, err = taskops.New(s).SetAssignee(ctx, "locked", "")
		taskTestOK(t, err)
	})
	t.Run("panic releases and escaped context cannot bypass", func(t *testing.T) {
		var escaped context.Context
		panicValue := &struct{}{}
		func() {
			defer func() {
				if recover() != panicValue {
					t.Error("callback panic changed")
				}
			}()
			_ = s.WithTaskSideEffectLock(ctx, "locked", func(inner context.Context) error { escaped = inner; panic(panicValue) })
		}()
		taskTestOK(t, s.WithTaskSideEffectLock(ctx, "locked", func(context.Context) error {
			timeout, cancel := context.WithTimeout(escaped, 200*time.Millisecond)
			defer cancel()
			_, err := taskops.New(s).SetAssignee(timeout, "locked", "")
			if err == nil {
				return errors.New("escaped context bypassed new owner")
			}
			return nil
		}))
		_, err := taskops.New(s).SetAssignee(ctx, "locked", "")
		taskTestOK(t, err)
	})
	t.Run("ownership does not transfer to another Store", func(t *testing.T) {
		other := &Store{db: s.db, log: s.log}
		taskTestOK(t, s.WithTaskSideEffectLock(ctx, "locked", func(inner context.Context) error {
			timeout, cancel := context.WithTimeout(inner, 200*time.Millisecond)
			defer cancel()
			_, err := taskops.New(other).SetAssignee(timeout, "locked", "")
			if err == nil {
				return errors.New("another Store reused callback ownership")
			}
			return nil
		}))
	})
	requireNoConnectionsInUse(t, s.db)
}

func TestTaskAggregateConstraintsIntegration(t *testing.T) {
	s, ctx := taskAggregateFixture(t)
	t.Run("invalid state and missing parent roll back", func(t *testing.T) {
		bad := aggregateTask(ctx, "bad-state")
		bad.State = "invented"
		if err := s.CreateTask(ctx, bad); err == nil {
			t.Fatal("invalid task state accepted")
		}
		bad = aggregateTask(ctx, "bad-parent")
		bad.ParentTaskID = "absent"
		if err := s.CreateTask(ctx, bad); err == nil {
			t.Fatal("missing parent accepted")
		}
		for _, id := range []string{"bad-state", "bad-parent"} {
			var count int
			taskTestOK(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE id=?`, id).Scan(&count))
			if count != 0 {
				t.Fatal("invalid task left a projection")
			}
			events, err := s.ListEvents(ctx, id)
			taskTestOK(t, err)
			if len(events) != 0 {
				t.Fatal("invalid task left events")
			}
		}
	})
	t.Run("concurrent branch uniqueness", func(t *testing.T) {
		start := make(chan struct{})
		results := make(chan error, 2)
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				task := aggregateTask(ctx, fmt.Sprintf("unique-%d", i))
				task.Branch = "same-branch"
				results <- s.CreateTask(ctx, task)
			}(i)
		}
		close(start)
		wg.Wait()
		close(results)
		success := 0
		for err := range results {
			if err == nil {
				success++
			}
		}
		if success != 1 {
			t.Fatalf("uniqueness winners=%d", success)
		}
		var count int
		taskTestOK(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE workspace_id=? AND kind='task.created'`, documentWorkspace(ctx)).Scan(&count))
		if count != 1 {
			t.Fatalf("events after competing create=%d", count)
		}
	})
	t.Run("work order parent and state validation", func(t *testing.T) {
		task := aggregateTask(ctx, "order-task")
		taskTestOK(t, s.CreateTask(ctx, task))
		job := core.Job{ID: "order-job", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
		taskTestOK(t, s.CreateJob(ctx, job))
		for _, bad := range []core.WorkOrder{{ID: "bad-order-state", JobID: job.ID, TaskID: task.ID, Stage: core.StageImplement, State: "invented"}, {ID: "bad-order-parent", JobID: "absent", TaskID: task.ID, Stage: core.StageImplement}} {
			_, err := taskops.ExecuteWorkOrder(ctx, s, task.ID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (bool, error) { return false, s.CreateWorkOrderCommand(ctx, lease, bad) })
			if err == nil {
				t.Fatal("invalid work order accepted")
			}
		}
	})
	t.Run("ledger rules reject mutation before SQL", func(t *testing.T) {
		for _, table := range []string{"events", "interventions"} {
			for _, op := range []string{"UPDATE", "DELETE"} {
				err := s.taskTx(ctx, "order-task", func(tx *sql.Tx) error {
					_, err := writeRow(ctx, tx, rowWrite{table: table, operation: op, values: map[string]any{"actor_id": "changed"}, where: map[string]any{"workspace_id": documentWorkspace(ctx)}})
					return err
				})
				if err == nil {
					t.Fatalf("%s %s accepted", op, table)
				}
			}
		}
	})
	t.Run("queue fold failure rolls back task and audit event", func(t *testing.T) {
		task := aggregateTask(ctx, "queue-failure")
		task.State = core.TaskQueued
		stream := logqueue.StreamFor(queue.DispatchTaskArgs{}.Kind(), task.ID)
		_, err := s.log.Append(ctx, documentWorkspace(ctx), stream, 0, []eventlog.NewEvent{{Kind: logqueue.KindEnqueued, Payload: []byte(`{"max_attempts":"invalid"}`), At: time.Now().UTC()}})
		taskTestOK(t, err)
		if err = s.CreateTask(ctx, task); err == nil {
			t.Fatal("malformed queue stream did not reject enqueue")
		}
		var count int
		taskTestOK(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE workspace_id=? AND id=?`, documentWorkspace(ctx), task.ID).Scan(&count))
		if count != 0 {
			t.Fatal("queue failure committed task")
		}
		events, err := s.ListEvents(ctx, task.ID)
		taskTestOK(t, err)
		if len(events) != 0 {
			t.Fatal("queue failure committed audit event")
		}
	})
}

func TestReviewAcceptanceTransactionIntegration(t *testing.T) {
	s, ctx := taskAggregateFixture(t)
	for _, verdict := range []string{"approve", "changes_requested"} {
		t.Run(verdict, func(t *testing.T) {
			task := aggregateTask(ctx, "review-"+verdict)
			task.NextStage = core.StageReview
			taskTestOK(t, s.CreateTask(ctx, task))
			job := core.Job{ID: task.ID + "-seat", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}
			order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1}
			taskTestOK(t, storetest.For(s).CreateReviewRound(ctx, task.ID, []core.Job{job}, []core.WorkOrder{order}))
			claimed, err := storetest.For(s).ClaimWorkOrder(ctx, order.ID, core.WorkOrderClaim{SessionID: task.ID + "-session", ClientToken: task.ID + "-token", Lease: time.Minute, ExecutionTimeout: time.Hour})
			taskTestOK(t, err)
			decision := core.ReviewDecision{TaskID: task.ID, JobID: job.ID, ReviewWorkOrderID: order.ID, ClaimSession: claimed.SessionID, ReviewRound: 1, ReviewSeat: 1, Verdict: verdict, ReasonCode: verdict, Summary: "checked", Feedback: "Update the implementation.", ReviewedCommitSHA: "head", PublicationEligible: true, MaxBounces: 3}
			// A rejected audit insert must leave the claimed attempt and all
			// acceptance projections intact for a retry with the same decision.
			bad := store.WithActor(ctx, store.Actor{ID: "bad\x00actor", Role: core.ActorUser})
			if err = taskops.New(s).AcceptReviewDecision(bad, decision); err == nil {
				t.Fatal("invalid audit actor accepted")
			}
			current, err := s.GetWorkOrder(ctx, order.ID)
			taskTestOK(t, err)
			if current.State != core.WorkOrderClaimed {
				t.Fatal("failed acceptance settled claim")
			}
			taskTestOK(t, taskops.New(s).AcceptReviewDecision(ctx, decision))
			taskTestOK(t, taskops.New(s).AcceptReviewDecision(ctx, decision))
			current, err = s.GetWorkOrder(ctx, order.ID)
			taskTestOK(t, err)
			if current.State != core.WorkOrderCompleted {
				t.Fatalf("accepted order=%s", current.State)
			}
			var publications int
			taskTestOK(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM review_publications WHERE workspace_id=? AND review_work_order_id=?`, documentWorkspace(ctx), order.ID).Scan(&publications))
			if publications != 1 {
				t.Fatalf("publication intents=%d", publications)
			}
			queued, err := logqueue.Load(ctx, s.log, documentWorkspace(ctx), logqueue.StreamFor(queue.ReviewPublicationArgs{}.Kind(), order.ID))
			taskTestOK(t, err)
			if !queued.Active() {
				t.Fatal("publication queue intent missing")
			}
			count, err := s.CountEvents(ctx, task.ID, "review.accepted")
			taskTestOK(t, err)
			if count != 1 {
				t.Fatalf("acceptance retry appended %d verdicts", count)
			}
			currentTask, err := s.GetTask(ctx, task.ID)
			taskTestOK(t, err)
			want := core.TaskAwaiting
			if verdict == "changes_requested" {
				want = core.TaskQueued
			}
			if currentTask.State != want {
				t.Fatalf("accepted task=%s want %s", currentTask.State, want)
			}
		})
	}
}

// SingleStore task and work-order list reads hydrate every relation with a
// fixed number of workspace-scoped queries, so their query count does not
// grow with the workspace's tasks, and each listed task equals the per-task
// read: dependencies, BlockingTaskIDs, children, assignee profile or nil when
// its binding is gone, GitHub lifecycle or nil, and the pull-request close
// projection (component-persistence; req-task-lifecycle-and-queue AC-2.6,
// AC-4.4). A chunked IN list returns the same rows.
func TestTaskListHydrationQueryBoundIntegration(t *testing.T) {
	st := integrationStore(t)
	workspace := "task-hydration"
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), workspace)
	_, err := st.BootstrapIdentity(ctx, config.FirstOperatorIdentity{OrganizationName: "Hydration", Email: "owner@example.test", DisplayName: "Owner"}, "hydration-token")
	taskTestOK(t, err)
	_, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}})
	taskTestOK(t, err)
	owner, err := st.VerifyPersonalAccessToken(ctx, "hydration-token")
	taskTestOK(t, err)
	ownerCtx := store.WithActor(store.WithCredential(ctx, core.AuthenticatedCredential{ID: "bootstrap", OwnerUserID: owner.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}), store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	_, err = st.GrantWorkspaceRole(ownerCtx, owner.Email, workspace, core.WorkspaceRoleOperator)
	taskTestOK(t, err)
	departed, err := st.ProvisionIdentityUser(ownerCtx, "departed@example.test", "Departed")
	taskTestOK(t, err)
	_, err = st.GrantWorkspaceRole(ownerCtx, departed.Email, workspace, core.WorkspaceRoleExecutor)
	taskTestOK(t, err)
	plane := taskops.New(st)

	created := time.Now().UTC().Add(-time.Hour)
	newTask := func(id string, edit func(*core.Task), dependencies ...string) {
		t.Helper()
		created = created.Add(time.Second)
		task := core.Task{ID: id, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, Title: id, State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: created}
		if edit != nil {
			edit(&task)
		}
		taskTestOK(t, st.CreateTaskWithDependencies(ctx, task, dependencies))
	}
	// Each group carries every relation the list reads hydrate.
	seedGroups := func(from, to int) {
		t.Helper()
		for i := from; i < to; i++ {
			prefix := fmt.Sprintf("g%02d-", i)
			newTask(prefix+"dep-merged", nil)
			newTask(prefix+"dep-open", nil)
			newTask(prefix+"dependent", nil, prefix+"dep-open", prefix+"dep-merged")
			_, err := plane.Perform(ctx, prefix+"dep-merged", taskops.Command{Kind: core.TaskMergeRecover})
			taskTestOK(t, err)
			for _, child := range []struct {
				id      string
				version int
				sub     string
			}{{prefix + "child-late", 2, "SUB-0"}, {prefix + "child-b", 1, "SUB-2"}, {prefix + "child-a", 1, "SUB-1"}} {
				newTask(child.id, func(task *core.Task) {
					task.ParentTaskID, task.OriginSpecVersion, task.OriginSubID = prefix+"dependent", child.version, child.sub
				})
			}
			_, err = plane.SetAssignee(ownerCtx, prefix+"dependent", owner.ID)
			taskTestOK(t, err)
			taskTestOK(t, st.QueueGitHubLifecycle(ctx, core.GitHubLifecycle{TaskID: prefix + "dependent", Repository: "acme/conveyor", SpecVersion: 1}))
			for _, id := range []string{prefix + "dependent", prefix + "child-a"} {
				job := core.Job{ID: id + "-implement", TaskID: id, Stage: core.StageImplement, StartedAt: time.Now().UTC()}
				taskTestOK(t, st.CreateJob(ctx, job))
				taskTestOK(t, storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: id, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, CreatedAt: time.Now().UTC()}))
			}
		}
	}
	seedGroups(0, 2)
	// A binding removed under an assignment reads as no assignee.
	newTask("departed-assignee", nil)
	_, err = plane.SetAssignee(ownerCtx, "departed-assignee", departed.ID)
	taskTestOK(t, err)
	if _, err = sharedDatabase.admin.ExecContext(t.Context(), "DELETE FROM `"+sharedDatabase.name+"`.workspace_role_bindings WHERE workspace_id=? AND user_id=?", workspace, departed.ID); err != nil {
		t.Fatal(err)
	}
	newTask("restarted", func(task *core.Task) { task.State = core.TaskRunning })
	taskTestOK(t, st.AppendEvent(ctx, core.Event{TaskID: "restarted", Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"number": 7, "url": "https://github.com/acme/conveyor/pull/7"})}))
	restart, err := plane.StartOver(ctx, core.TaskStartOverRequest{TaskID: "restarted", RequestID: "hydration-restart", Reason: "replace"})
	taskTestOK(t, err)
	taskTestOK(t, st.QueuePullRequestClose(ctx, core.PullRequestClose{WorkspaceID: workspace, TaskID: "restarted", Repository: "acme/conveyor", Branch: "conveyor/task-restarted", SuccessorID: restart.Successor.ID, SuccessorBranch: restart.Successor.Branch, Reason: "replace", RestartingOperatorID: "operator", ForgeAuthorClass: core.ForgeAuthorWorkspace, State: "queued", Number: 7, URL: "https://github.com/acme/conveyor/pull/7"}))

	recorder := &selectedBytesRecorder{}
	connector, err := mysql.NewConnector(sharedDatabase.cfg)
	taskTestOK(t, err)
	db := sql.OpenDB(recordingConnector{Connector: connector, recorder: recorder})
	t.Cleanup(func() { _ = db.Close() })
	recorded := &Store{db: db, log: s2log.New(db)}
	filter := store.TaskFilter{Repositories: []string{"conveyor"}}
	type listing struct {
		tasks  map[string][]core.Task
		orders []core.WorkOrder
		counts map[string]int
	}
	list := func() listing {
		t.Helper()
		out := listing{tasks: map[string][]core.Task{}, counts: map[string]int{}}
		for name, read := range map[string]func() ([]core.Task, error){
			"ListTasks":         func() ([]core.Task, error) { return recorded.ListTasks(ctx) },
			"ListTasksFiltered": func() ([]core.Task, error) { return recorded.ListTasksFiltered(ctx, filter) },
			"ListTaskPage": func() ([]core.Task, error) {
				page, err := recorded.ListTaskPage(ctx, store.TaskOperationsQuery{TaskFilter: filter, Limit: 200})
				return page.Tasks, err
			},
		} {
			recorder.start()
			tasks, err := read()
			queries, _ := recorder.stop()
			taskTestOK(t, err)
			out.tasks[name], out.counts[name] = tasks, len(queries)
		}
		recorder.start()
		orders, err := recorded.ListWorkOrders(ctx)
		queries, _ := recorder.stop()
		taskTestOK(t, err)
		out.orders, out.counts["ListWorkOrders"] = orders, len(queries)
		return out
	}
	requireEqualsSingleReads := func(label string, got listing) {
		t.Helper()
		single := map[string]core.Task{}
		for name, tasks := range got.tasks {
			for _, task := range tasks {
				want, ok := single[task.ID]
				if !ok {
					want, err = st.GetTask(ctx, task.ID)
					taskTestOK(t, err)
					single[task.ID] = want
				}
				if !reflect.DeepEqual(task, want) {
					t.Fatalf("%s %s %s differs from GetTask:\n got=%+v\nwant=%+v", label, name, task.ID, task, want)
				}
			}
		}
		for _, order := range got.orders {
			want := single[order.TaskID].Assignee
			if !reflect.DeepEqual(order.Assignee, want) {
				t.Fatalf("%s work order %s assignee=%+v, task read=%+v", label, order.ID, order.Assignee, want)
			}
		}
		// Pin the oracle itself so two equally wrong reads cannot agree.
		dependent, departedTask, restarted := single["g00-dependent"], single["departed-assignee"], single["restarted"]
		if len(dependent.Dependencies) != 2 || dependent.Dependencies[0].ID != "g00-dep-merged" || !reflect.DeepEqual(dependent.BlockingTaskIDs, []string{"g00-dep-open"}) {
			t.Fatalf("%s dependent relations=%+v blocking=%v", label, dependent.Dependencies, dependent.BlockingTaskIDs)
		}
		if len(dependent.Children) != 3 || dependent.Children[0].ID != "g00-child-a" || dependent.Children[1].ID != "g00-child-b" || dependent.Children[2].ID != "g00-child-late" {
			t.Fatalf("%s dependent children=%+v", label, dependent.Children)
		}
		if dependent.Assignee == nil || dependent.Assignee.Email != owner.Email || dependent.Assignee.DisplayName != "Owner" || dependent.GitHub == nil {
			t.Fatalf("%s dependent assignee=%+v github=%+v", label, dependent.Assignee, dependent.GitHub)
		}
		if departedTask.Assignee != nil || departedTask.GitHub != nil {
			t.Fatalf("%s departed assignee=%+v github=%+v", label, departedTask.Assignee, departedTask.GitHub)
		}
		if restarted.PullRequestClose == nil || restarted.PullRequestCloseState != "queued" {
			t.Fatalf("%s restarted close=%+v", label, restarted.PullRequestClose)
		}
	}

	small := list()
	requireEqualsSingleReads("small", small)
	seedGroups(2, 12)
	large := list()
	requireEqualsSingleReads("large", large)
	if len(large.tasks["ListTasks"]) <= 3*len(small.tasks["ListTasks"]) || len(large.orders) <= 3*len(small.orders) {
		t.Fatalf("large workspace did not grow: tasks %d->%d orders %d->%d", len(small.tasks["ListTasks"]), len(large.tasks["ListTasks"]), len(small.orders), len(large.orders))
	}
	if !reflect.DeepEqual(small.counts, large.counts) {
		t.Fatalf("list query counts grew with row count: %d tasks %v, %d tasks %v", len(small.tasks["ListTasks"]), small.counts, len(large.tasks["ListTasks"]), large.counts)
	}
	for name, count := range large.counts {
		// One row read, one count for the page, five relation reads.
		if count > 7 {
			t.Fatalf("%s issued %d queries", name, count)
		}
	}
	t.Logf("query counts at %d and %d tasks: %v", len(small.tasks["ListTasks"]), len(large.tasks["ListTasks"]), large.counts)

	previous := hydrationChunk
	hydrationChunk = 2
	t.Cleanup(func() { hydrationChunk = previous })
	chunked := list()
	if !reflect.DeepEqual(chunked.tasks, large.tasks) || !reflect.DeepEqual(chunked.orders, large.orders) {
		t.Fatal("chunked hydration differs from the single-chunk read")
	}
}
