package singlestore

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

const clockRaceWorkspace = "clock-race"

// clockRace is one scenario's store, context, and order under two ticks.
type clockRace struct {
	st            *Store
	ctx           context.Context
	taskID, order string
}

func newClockRace(t *testing.T, st *Store) *clockRace {
	t.Helper()
	base := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), clockRaceWorkspace)
	ctx, cancel := context.WithTimeout(base, 2*time.Minute)
	t.Cleanup(cancel)
	if _, err := st.CreateWorkspace(ctx, clockRaceWorkspace, "Clock race", &config.Config{Workspace: clockRaceWorkspace}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	taskID := core.NewTaskID()
	if err := st.CreateTask(ctx, core.Task{ID: taskID, Workspace: clockRaceWorkspace, Repo: "repo", BaseBranch: "main", Branch: "conveyor/task-" + taskID, Title: taskID, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	order := taskID + "-implement"
	if err := st.CreateJob(ctx, core.Job{ID: order, TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err := storetest.CreateWorkOrder(ctx, st, core.WorkOrder{ID: order, TaskID: taskID, JobID: order, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	return &clockRace{st: st, ctx: ctx, taskID: taskID, order: order}
}

func (r *clockRace) claim(t *testing.T, lease, execution time.Duration) {
	t.Helper()
	if _, err := storetest.ClaimWorkOrder(r.ctx, r.st, r.order, core.WorkOrderClaim{ClaimantID: "worker", SessionID: "race-session", ClientToken: "race-token", Lease: lease, ExecutionTimeout: execution}); err != nil {
		t.Fatal(err)
	}
}

// set writes order columns directly, to place the order at a state its scenario
// starts from.
func (r *clockRace) set(t *testing.T, assignments string, args ...any) {
	t.Helper()
	args = append(args, clockRaceWorkspace, r.order)
	if _, err := r.st.db.ExecContext(r.ctx, `UPDATE work_orders SET `+assignments+` WHERE workspace_id=? AND id=?`, args...); err != nil {
		t.Fatal(err)
	}
}

// timedOut leaves the order timed_out with a scheduled recovery, by the clock's
// own acts.
func (r *clockRace) timedOut(t *testing.T) core.WorkOrder {
	t.Helper()
	r.claim(t, time.Hour, time.Minute)
	if _, err := taskops.New(r.st).TickOrderClock(r.ctx, time.Now().UTC().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	return r.read(t, core.WorkOrderTimedOut)
}

func (r *clockRace) read(t *testing.T, state core.WorkOrderState) core.WorkOrder {
	t.Helper()
	order, err := r.st.GetWorkOrder(r.ctx, r.order)
	if err != nil {
		t.Fatal(err)
	}
	if state != "" && order.State != state {
		t.Fatalf("order state=%s, want %s", order.State, state)
	}
	return order
}

func (r *clockRace) events(t *testing.T, kind string) int {
	t.Helper()
	events, err := r.st.ListEvents(r.ctx, r.taskID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, event := range events {
		if event.Kind == kind {
			n++
		}
	}
	return n
}

func (r *clockRace) ledgerRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := r.st.db.QueryRowContext(r.ctx, `SELECT COUNT(*) FROM work_order_recoveries WHERE workspace_id=? AND work_order_id=?`, clockRaceWorkspace, r.order).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// awaitBlockedQuery returns once the server reports a blocked statement whose
// text holds every fragment. It reads the server's own view, so no timing
// decides the outcome.
func (r *clockRace) awaitBlockedQuery(t *testing.T, fragments ...string) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	pattern := "%" + strings.Join(fragments, "%") + "%"
	for {
		var n int
		if err := r.st.db.QueryRowContext(r.ctx, `SELECT COUNT(*) FROM information_schema.MV_BLOCKED_QUERIES WHERE QUERY_TEXT LIKE ?`, pattern).Scan(&n); err != nil {
			t.Fatalf("read blocked queries: %v", err)
		}
		if n > 0 {
			return
		}
		select {
		case <-r.ctx.Done():
			t.Fatalf("no blocked statement matching %q: %v", pattern, r.ctx.Err())
		case <-ticker.C:
		}
	}
}

// withoutTaskOperationLock makes this context own the task's operation lock, so
// lockTaskOperation takes no lock row. A commit across partitions can release
// that row before the order row is visible, so the lock proves nothing about
// what the next holder reads (component-persistence invariant 8). Removing it
// opens that same window on demand instead of waiting for a commit to lag.
func (r *clockRace) withoutTaskOperationLock(ctx context.Context) context.Context {
	owner := &taskLockOwner{store: r.st, workspace: clockRaceWorkspace, task: r.taskID}
	owner.active.Store(true)
	return context.WithValue(ctx, taskLockOwnerKey{}, owner)
}

// overlappingTicks drives two ticks through the same window a lagging commit
// opens. The first tick reads the orders and stops before it writes. The second
// starts while the first holds that read and must get as far as it can: the old
// read completes with the image the first tick is about to replace, and the
// locked read is reported blocked by the server. The first tick then finishes,
// and the second goes on with what it read or now reads. Barriers order every
// step, so the outcome depends on no timing.
func (r *clockRace) overlappingTicks(t *testing.T, point clockOrderRead, oldRead bool, atFirst, atSecond time.Time) (errFirst, errSecond error) {
	t.Helper()
	firstRead, secondRead := make(chan struct{}), make(chan struct{})
	releaseFirst, releaseSecond := make(chan struct{}), make(chan struct{})
	letFirstGo := sync.OnceFunc(func() { close(releaseFirst) })
	letSecondGo := sync.OnceFunc(func() { close(releaseSecond) })
	pause := func(reached, resume chan struct{}) *clockOrderReadObserver {
		var reachedOnce sync.Once
		return &clockOrderReadObserver{unlockedRead: oldRead, read: func(at clockOrderRead) {
			if at == point {
				reachedOnce.Do(func() { close(reached) })
				<-resume
			}
		}}
	}
	ctx := r.withoutTaskOperationLock(r.ctx)
	firstDone, secondDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(firstDone)
		_, errFirst = taskops.New(r.st).TickOrderClock(observeClockOrderReads(ctx, pause(firstRead, releaseFirst)), atFirst)
	}()
	var secondStarted bool
	defer func() {
		letFirstGo()
		letSecondGo()
		<-firstDone
		if secondStarted {
			<-secondDone
		}
	}()
	select {
	case <-firstRead:
	case <-firstDone:
		t.Fatalf("first tick finished without reading: %v", errFirst)
	case <-r.ctx.Done():
		t.Fatalf("first tick never read: %v", r.ctx.Err())
	}
	secondStarted = true
	go func() {
		defer close(secondDone)
		_, errSecond = taskops.New(r.st).TickOrderClock(observeClockOrderReads(ctx, pause(secondRead, releaseSecond)), atSecond)
	}()
	if oldRead {
		select {
		case <-secondRead:
		case <-secondDone:
			t.Fatalf("second tick finished without reading: %v", errSecond)
		case <-r.ctx.Done():
			t.Fatalf("second tick never read: %v", r.ctx.Err())
		}
	} else {
		r.awaitBlockedQuery(t, "FROM work_orders WHERE workspace_id=", r.taskID, "FOR UPDATE")
		select {
		case <-secondRead:
			t.Fatal("second tick read the orders while the first tick held their row locks")
		default:
		}
	}
	letFirstGo()
	<-firstDone
	letSecondGo()
	<-secondDone
	return errFirst, errSecond
}

// TestAutomaticRecoveryOverlappingTicksIntegration runs each act the clock's
// order read serves under two overlapping ticks, once with the row lock and once
// on the old read. The control proves the barrier reaches the double write where
// one exists, and shows which acts the recovery ledger already protected.
func TestAutomaticRecoveryOverlappingTicksIntegration(t *testing.T) {
	st := integrationStore(t)
	type outcome struct {
		events map[string]int
		ledger int
	}
	for _, scenario := range []struct {
		name    string
		point   clockOrderRead
		seed    func(*testing.T, *clockRace) (first, second time.Time)
		locked  outcome
		oldRead outcome
		check   func(t *testing.T, r *clockRace, oldRead bool, first, second time.Time)
	}{
		{
			name:  "exhausted from an order past its execution deadline",
			point: clockOrderReadTransitions,
			seed: func(t *testing.T, r *clockRace) (time.Time, time.Time) {
				r.claim(t, time.Hour, time.Minute)
				r.set(t, `automatic_recovery_count=2, automatic_recovery_window_started_at=?`, time.Now().UTC())
				at := time.Now().UTC().Add(2 * time.Minute)
				return at, at
			},
			locked:  outcome{events: map[string]int{"work_order.recovery_exhausted": 1, "work_order.timed_out": 1}},
			oldRead: outcome{events: map[string]int{"work_order.recovery_exhausted": 2, "work_order.timed_out": 2}},
			check: func(t *testing.T, r *clockRace, _ bool, _, _ time.Time) {
				order := r.read(t, core.WorkOrderTimedOut)
				if !order.RetrySuppressed || order.RetrySuppressionReason != core.AutomaticRecoveryExhaustedReason || order.AutomaticRecoveryCount != 2 {
					t.Fatalf("exhausted order=%+v", order)
				}
			},
		},
		{
			name:  "exhausted from a timed out order",
			point: clockOrderReadRecovery,
			seed: func(t *testing.T, r *clockRace) (time.Time, time.Time) {
				r.timedOut(t)
				r.set(t, `next_automatic_recovery_at=NULL, automatic_recovery_count=2, automatic_recovery_window_started_at=?`, time.Now().UTC())
				at := time.Now().UTC()
				return at, at
			},
			locked:  outcome{events: map[string]int{"work_order.recovery_exhausted": 1}},
			oldRead: outcome{events: map[string]int{"work_order.recovery_exhausted": 2}},
			check: func(t *testing.T, r *clockRace, _ bool, _, _ time.Time) {
				order := r.read(t, core.WorkOrderTimedOut)
				if !order.RetrySuppressed || order.RetrySuppressionReason != core.AutomaticRecoveryExhaustedReason {
					t.Fatalf("exhausted order=%+v", order)
				}
			},
		},
		{
			name:  "schedule",
			point: clockOrderReadRecovery,
			seed: func(t *testing.T, r *clockRace) (time.Time, time.Time) {
				r.timedOut(t)
				r.set(t, `next_automatic_recovery_at=NULL`)
				at := time.Now().UTC().Truncate(time.Microsecond)
				return at, at.Add(time.Second)
			},
			locked:  outcome{events: map[string]int{}},
			oldRead: outcome{events: map[string]int{}},
			check: func(t *testing.T, r *clockRace, oldRead bool, first, second time.Time) {
				// The second tick runs one second later, so a repeat moves the
				// schedule to its own time.
				want := first.Add(store.AutomaticRecoveryDelay(1))
				if oldRead {
					want = second.Add(store.AutomaticRecoveryDelay(1))
				}
				order := r.read(t, core.WorkOrderTimedOut)
				if !order.NextAutomaticRecoveryAt.Equal(want) {
					t.Fatalf("next automatic recovery=%s, want %s", order.NextAutomaticRecoveryAt, want)
				}
			},
		},
		{
			name:  "recover",
			point: clockOrderReadRecovery,
			seed: func(t *testing.T, r *clockRace) (time.Time, time.Time) {
				at := r.timedOut(t).NextAutomaticRecoveryAt
				return at, at
			},
			locked:  outcome{events: map[string]int{"work_order.recovered": 1}, ledger: 1},
			oldRead: outcome{events: map[string]int{"work_order.recovered": 1}, ledger: 1},
			check: func(t *testing.T, r *clockRace, _ bool, _, _ time.Time) {
				order := r.read(t, core.WorkOrderQueued)
				if order.AutomaticRecoveryCount != 1 || order.RedispatchCount != 1 {
					t.Fatalf("recovered order=%+v", order)
				}
			},
		},
		{
			name:  "redispatch",
			point: clockOrderReadRecovery,
			seed: func(t *testing.T, r *clockRace) (time.Time, time.Time) {
				now := time.Now().UTC()
				r.set(t, `state='stale', next_automatic_recovery_at=?`, now)
				return now, now
			},
			locked:  outcome{events: map[string]int{"work_order.redispatched": 1}, ledger: 1},
			oldRead: outcome{events: map[string]int{"work_order.redispatched": 1}, ledger: 1},
			check: func(t *testing.T, r *clockRace, _ bool, _, _ time.Time) {
				order := r.read(t, core.WorkOrderQueued)
				if order.AutomaticRecoveryCount != 1 || order.RedispatchCount != 1 {
					t.Fatalf("redispatched order=%+v", order)
				}
			},
		},
		{
			name:  "answered authority",
			point: clockOrderReadRecovery,
			seed: func(t *testing.T, r *clockRace) (time.Time, time.Time) {
				design, initial, err := r.st.CreateSystemDesign(r.ctx, core.SystemDesign{ID: "design-clock-race", Title: "Clock race", Category: "Component design"}, core.SystemDesignVersion{Content: clockRaceDesign("one"), Origin: core.SystemDesignOriginOperator})
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err = r.st.ConfirmSystemDesignVersion(r.ctx, design.ID, initial.Version); err != nil {
					t.Fatal(err)
				}
				r.claim(t, time.Hour, time.Hour)
				checkpoint := &core.WorkOrderCheckpoint{DecisionRequest: "Confirm the cited revision.", Class: core.WorkOrderCheckpointClassAuthorityConflict, Citations: []core.WorkOrderAuthorityConflictCitation{{DocumentID: design.ID, CitedVersion: initial.Version, StatementOrSectionID: "Clock race"}}}
				if _, err = storetest.ReleaseWorkerClaim(r.ctx, r.st, r.order, "worker", core.WorkOrderRelease{SessionID: "race-session", Reason: core.WorkOrderReleaseReasonOperatorCheckpointReached, Checkpoint: checkpoint, Cause: core.WorkOrderReleaseCauseOperatorAction, Outcome: core.WorkOrderOutcomeReleased}); err != nil {
					t.Fatal(err)
				}
				revision, err := r.st.ProposeSystemDesignVersion(r.ctx, core.SystemDesignVersion{DocumentID: design.ID, Content: clockRaceDesign("two"), Origin: core.SystemDesignOriginOperator})
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err = r.st.ConfirmSystemDesignVersion(r.ctx, design.ID, revision.Version); err != nil {
					t.Fatal(err)
				}
				at := time.Now().UTC()
				return at, at
			},
			locked:  outcome{events: map[string]int{"work_order.recovered": 1}, ledger: 1},
			oldRead: outcome{events: map[string]int{"work_order.recovered": 1}, ledger: 1},
			check: func(t *testing.T, r *clockRace, _ bool, _, _ time.Time) {
				order := r.read(t, core.WorkOrderQueued)
				if order.Checkpoint != nil || order.RedispatchCount != 1 || order.AutomaticRecoveryCount != 0 {
					t.Fatalf("answered order=%+v", order)
				}
			},
		},
		{
			name:  "expired lease",
			point: clockOrderReadTransitions,
			seed: func(t *testing.T, r *clockRace) (time.Time, time.Time) {
				r.claim(t, time.Minute, time.Hour)
				at := time.Now().UTC().Add(2 * time.Minute)
				return at, at
			},
			locked:  outcome{events: map[string]int{"work_order.expired": 1}},
			oldRead: outcome{events: map[string]int{"work_order.expired": 2}},
			check: func(t *testing.T, r *clockRace, _ bool, _, _ time.Time) {
				r.read(t, core.WorkOrderQueued)
			},
		},
	} {
		for _, mode := range []struct {
			name    string
			oldRead bool
			want    outcome
		}{{"locked read", false, scenario.locked}, {"control old read", true, scenario.oldRead}} {
			t.Run(scenario.name+"/"+mode.name, func(t *testing.T) {
				integrationStore(t)
				r := newClockRace(t, st)
				first, second := scenario.seed(t, r)
				errFirst, errSecond := r.overlappingTicks(t, scenario.point, mode.oldRead, first, second)
				if errFirst != nil || errSecond != nil {
					t.Fatalf("tick errors: first=%v second=%v", errFirst, errSecond)
				}
				for kind, want := range mode.want.events {
					got := r.events(t, kind)
					t.Logf("%s events=%d", kind, got)
					if got != want {
						t.Fatalf("%s events=%d, want %d", kind, got, want)
					}
				}
				if got := r.ledgerRows(t); got != mode.want.ledger {
					t.Fatalf("recovery ledger rows=%d, want %d", got, mode.want.ledger)
				}
				scenario.check(t, r, mode.oldRead, first, second)
			})
		}
	}
}

func clockRaceDesign(revision string) string {
	return "# Clock race " + revision + "\n\n```conveyor:governs\n- repo: repo\n  paths:\n    - internal/store/**\n```"
}

// TestAutomaticRecoveryRetryableLoserIntegration holds the order rows until the
// tick's bounded wait runs out. The tick returns store.ErrRetryable with nothing
// written, and the repeat the deadline alarm would send performs the whole act
// once, so the failure loses no recovery.
func TestAutomaticRecoveryRetryableLoserIntegration(t *testing.T) {
	previous := clockOrderLockWait
	clockOrderLockWait = 500 * time.Millisecond
	t.Cleanup(func() { clockOrderLockWait = previous })
	st := integrationStore(t)
	r := newClockRace(t, st)
	r.claim(t, time.Hour, time.Minute)
	r.set(t, `automatic_recovery_count=2, automatic_recovery_window_started_at=?`, time.Now().UTC())
	before := r.read(t, core.WorkOrderClaimed)
	at := time.Now().UTC().Add(2 * time.Minute)

	holder, err := st.db.BeginTx(r.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()
	var held string
	if err = holder.QueryRowContext(r.ctx, `SELECT id FROM work_orders WHERE workspace_id=? AND id=? FOR UPDATE`, clockRaceWorkspace, r.order).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if _, err = taskops.New(st).TickOrderClock(r.ctx, at); !errors.Is(err, store.ErrRetryable) {
		t.Fatalf("tick while the order rows are held err=%v, want %v", err, store.ErrRetryable)
	}
	if got := r.events(t, "work_order.timed_out") + r.events(t, "work_order.recovery_exhausted"); got != 0 {
		t.Fatalf("a retryable tick appended %d events", got)
	}
	if after := r.read(t, core.WorkOrderClaimed); after.RetrySuppressed || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("a retryable tick wrote the order: %+v", after)
	}
	if err = holder.Rollback(); err != nil {
		t.Fatal(err)
	}

	if _, err = taskops.New(st).TickOrderClock(r.ctx, at); err != nil {
		t.Fatalf("retried tick: %v", err)
	}
	if got := r.events(t, "work_order.recovery_exhausted"); got != 1 {
		t.Fatalf("exhausted events=%d after the retry", got)
	}
	if got := r.events(t, "work_order.timed_out"); got != 1 {
		t.Fatalf("timed out events=%d after the retry", got)
	}
	if order := r.read(t, core.WorkOrderTimedOut); order.RetrySuppressionReason != core.AutomaticRecoveryExhaustedReason {
		t.Fatalf("order after the retry=%+v", order)
	}
}

// TestClockOrderRowLockKeepsLockOrderIntegration stops a tick while it holds the
// task-operation lock and the order rows, and starts each command that writes
// those rows. Each waits at the task-operation lock, the first lock in
// component-persistence invariant 7's order, so it holds no order row the clock
// could wait for. Each then finishes with no lock timeout or deadlock.
func TestClockOrderRowLockKeepsLockOrderIntegration(t *testing.T) {
	st := integrationStore(t)
	for _, path := range []struct {
		name    string
		seed    func(*testing.T, *clockRace) time.Time
		command func(context.Context, *clockRace) error
	}{
		{
			name: "claim",
			seed: func(t *testing.T, r *clockRace) time.Time {
				r.claim(t, time.Minute, time.Hour)
				at := time.Now().UTC().Add(2 * time.Minute)
				if _, err := taskops.New(r.st).TickOrderClock(r.ctx, at); err != nil {
					t.Fatal(err)
				}
				r.read(t, core.WorkOrderQueued)
				r.set(t, `next_automatic_recovery_at=NULL, retry_suppressed=false`)
				return at
			},
			command: func(ctx context.Context, r *clockRace) error {
				_, err := storetest.ClaimWorkOrder(ctx, r.st, r.order, core.WorkOrderClaim{ClaimantID: "worker", SessionID: "race-next", ClientToken: "race-next-token", Lease: time.Hour, ExecutionTimeout: time.Hour})
				return err
			},
		},
		{
			name: "renew",
			seed: func(t *testing.T, r *clockRace) time.Time {
				r.claim(t, time.Hour, 2*time.Hour)
				return time.Now().UTC().Add(90 * time.Minute)
			},
			command: func(ctx context.Context, r *clockRace) error {
				_, err := storetest.RenewWorkerClaim(ctx, r.st, r.order, "worker", "race-session", time.Hour)
				return err
			},
		},
		{
			name: "release",
			seed: func(t *testing.T, r *clockRace) time.Time {
				r.claim(t, time.Hour, 2*time.Hour)
				return time.Now().UTC().Add(90 * time.Minute)
			},
			command: func(ctx context.Context, r *clockRace) error {
				_, err := storetest.ReleaseWorkerClaim(ctx, r.st, r.order, "worker", core.WorkOrderRelease{SessionID: "race-session", Reason: "released during the tick", Cause: core.WorkOrderReleaseCauseOperatorAction, Outcome: core.WorkOrderOutcomeReleased})
				return err
			},
		},
		{
			name: "recover",
			seed: func(t *testing.T, r *clockRace) time.Time {
				r.timedOut(t)
				r.set(t, `next_automatic_recovery_at=NULL`)
				return time.Now().UTC()
			},
			command: func(ctx context.Context, r *clockRace) error {
				_, err := storetest.RecoverWorkOrder(ctx, r.st, r.order, "race-recover", time.Hour)
				return err
			},
		},
	} {
		t.Run(path.name, func(t *testing.T) {
			integrationStore(t)
			r := newClockRace(t, st)
			at := path.seed(t, r)

			held, resume := make(chan struct{}), make(chan struct{})
			var heldOnce sync.Once
			tickDone := make(chan error, 1)
			go func() {
				_, err := taskops.New(st).TickOrderClock(observeClockOrderReads(r.ctx, &clockOrderReadObserver{read: func(point clockOrderRead) {
					if point == clockOrderReadTransitions {
						heldOnce.Do(func() { close(held) })
						<-resume
					}
				}}), at)
				tickDone <- err
			}()
			release := sync.OnceFunc(func() { close(resume) })
			defer release()
			select {
			case <-held:
			case err := <-tickDone:
				t.Fatalf("tick finished without reading the orders: %v", err)
			case <-r.ctx.Done():
				t.Fatal(r.ctx.Err())
			}

			commandDone := make(chan error, 1)
			go func() { commandDone <- path.command(r.ctx, r) }()
			r.awaitBlockedQuery(t, "conveyor_locks", lockRowKey("task-operation:"+clockRaceWorkspace+":"+r.taskID))
			select {
			case err := <-commandDone:
				t.Fatalf("command finished while the tick held the task: %v", err)
			default:
			}
			release()
			if err := <-tickDone; err != nil {
				t.Fatalf("tick: %v", err)
			}
			// The tick may have ended the claim the command names, so a domain
			// refusal is a legal answer. A lock timeout or deadlock is not.
			if err := <-commandDone; errors.Is(err, store.ErrRetryable) {
				t.Fatalf("command after the tick: %v", err)
			}
		})
	}
}
