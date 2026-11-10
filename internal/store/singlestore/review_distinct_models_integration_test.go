package singlestore

// Distinct review seat names under the cross-partition visibility window
// (req-review-gates-evidence AC-2.10; component-review; component-persistence,
// SingleStore visibility). Writing a seat's pre-claim row image back stands for
// a predecessor whose lock row was released before its order row became
// visible. Removing its work_order.claimed event as well stands for a
// predecessor with neither record visible.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// seedReviewSeats creates one review round through the real round command.
func seedReviewSeats(t *testing.T, st *Store, prefix string, seats int, distinct bool) (context.Context, string, []core.WorkOrder) {
	t.Helper()
	ws := prefix + "-" + core.NewTaskID()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), ws)
	if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: ws, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	id := core.NewTaskID()
	task := core.Task{ID: id, Workspace: ws, Repo: "repo", BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, NextStage: core.StageReview, ReviewedHeadSHA: "submitted-head", CreatedAt: time.Now().UTC()}
	task.SetupContract = config.PolicyContract{MaxBounces: 3, Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, seats), DistinctModels: distinct}, StageTimeouts: config.PolicyStageTimeouts{Review: "1h"}}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := store.LoadReviewRoundSeatInputs(ctx, st, stored, store.ReviewSeatMinimum(stored.SetupContract, 0), 1)
	if err != nil {
		t.Fatal(err)
	}
	resolution := store.ResolveReviewRoundSeats(inputs)
	now := time.Now().UTC()
	var jobs []core.Job
	var orders []core.WorkOrder
	for seat := 1; seat <= resolution.SeatCount; seat++ {
		orderID := id + "-review-1-seat-" + string(rune('0'+seat))
		jobs = append(jobs, core.Job{ID: orderID, TaskID: id, Stage: core.StageReview, State: core.JobPending})
		orders = append(orders, core.WorkOrder{ID: orderID, JobID: orderID, TaskID: id, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: seat,
			HeadSHA: "submitted-head", BaselineSHA: "submitted-base", QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now})
	}
	if _, err = taskops.ExecuteWorkOrder(ctx, st, id, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (struct{}, error) {
		return struct{}{}, st.CreateReviewRoundCommand(ctx, lease, id, resolution, jobs, orders)
	}); err != nil {
		t.Fatal(err)
	}
	return ctx, ws, orders
}

func claimReviewSeat(ctx context.Context, st *Store, order core.WorkOrder, model string) (core.WorkOrder, error) {
	session := order.ID + "-" + core.NewTaskID()
	return storetest.ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{ClaimantID: "seat-worker", SessionID: session, ClientToken: session + "-token", Model: model, Agent: "seat-test", Lease: time.Hour, ExecutionTimeout: time.Hour})
}

func storedReviewSeat(t *testing.T, st *Store, ctx context.Context, id string) core.WorkOrder {
	t.Helper()
	order, err := getOrderRow(ctx, st.db, id)
	if err != nil {
		t.Fatal(err)
	}
	return order
}

func writeReviewSeatImage(t *testing.T, st *Store, ctx context.Context, image core.WorkOrder) {
	t.Helper()
	if err := st.withTx(ctx, func(tx *sql.Tx) error { return orderWrite(ctx, tx, image) }); err != nil {
		t.Fatal(err)
	}
}

// claimVisibleSeat claims a seat and returns its pre-claim and claimed images.
func claimVisibleSeat(t *testing.T, st *Store, ctx context.Context, order core.WorkOrder, model string) (before, claimed core.WorkOrder) {
	t.Helper()
	before = storedReviewSeat(t, st, ctx, order.ID)
	if _, err := claimReviewSeat(ctx, st, order, model); err != nil {
		t.Fatalf("claim of seat %d with %q: %v", order.ReviewSeat, model, err)
	}
	return before, storedReviewSeat(t, st, ctx, order.ID)
}

// seatClaimGap is a predecessor seat claim that committed and released its lock
// row while its records stay out of sight.
type seatClaimGap struct {
	claimed core.WorkOrder
	event   *hiddenEvent
}

func forceSeatClaimGap(t *testing.T, st *Store, ctx context.Context, ws string, before, claimed core.WorkOrder, hideClaimEvent bool) seatClaimGap {
	t.Helper()
	gap := seatClaimGap{claimed: claimed}
	if hideClaimEvent {
		var id int64
		if err := st.db.QueryRowContext(ctx, `SELECT id FROM events_columnstore WHERE workspace_id=? AND task_id=? AND job_id=? AND kind='work_order.claimed'`, ws, claimed.TaskID, claimed.JobID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		hidden := hideEvent(t, st, ws, id)
		gap.event = &hidden
	}
	writeReviewSeatImage(t, st, ctx, before)
	return gap
}

func (g seatClaimGap) close(t *testing.T, st *Store, ctx context.Context) {
	t.Helper()
	writeReviewSeatImage(t, st, ctx, g.claimed)
	if g.event != nil {
		g.event.restore(t, st)
	}
}

// seatClaimHolder reads the round's claim holder, or "" before any claim
// recorded one, so a test without the claim fails on the decision it proves
// rather than on the missing row.
func seatClaimHolder(t *testing.T, st *Store, key string) string {
	t.Helper()
	var holder string
	err := st.db.QueryRowContext(t.Context(), "SELECT holder FROM conveyor_locks WHERE `key`=?", lockRowKey(key)).Scan(&holder)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	return holder
}

func requireSeatStillQueued(t *testing.T, st *Store, ctx context.Context, order core.WorkOrder) {
	t.Helper()
	seat := storedReviewSeat(t, st, ctx, order.ID)
	if seat.State != core.WorkOrderQueued || seat.SessionID != "" || seat.AttemptID != "" || seat.Model != "" {
		t.Fatalf("seat %d state=%s session=%q attempt=%q model=%q, want it untouched", order.ReviewSeat, seat.State, seat.SessionID, seat.AttemptID, seat.Model)
	}
}

func requireModelRefusal(t *testing.T, err error, held ...string) {
	t.Helper()
	if !errors.Is(err, store.ErrReviewSeatModelNotDistinct) {
		t.Fatalf("err=%v, want %v", err, store.ErrReviewSeatModelNotDistinct)
	}
	for _, name := range held {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("refusal %q does not name held model %q", err, name)
		}
	}
}

var seatClaimGaps = []struct {
	name           string
	hideClaimEvent bool
}{
	{"order row unseen behind a visible event", false},
	{"order row and event unseen", true},
}

// The failure scenario of the task: seat 1 claims gpt-6 and commits, and its
// lock row releases before its order row is visible. Seat 2 acquires the task
// lock and would read seat 1 as still queued. It waits and is then refused.
func TestReviewSeatClaimWaitsForUnseenPredecessorIntegration(t *testing.T) {
	st := integrationStore(t)
	for _, tc := range seatClaimGaps {
		t.Run(tc.name, func(t *testing.T) {
			ctx, ws, orders := seedReviewSeats(t, st, "seat-window", 2, true)
			before, claimed := claimVisibleSeat(t, st, ctx, orders[0], "gpt-6")
			key := reviewSeatModelClaimKey(ws, claimed.TaskID, 1)
			holder := seatClaimHolder(t, st, key)

			gap := forceSeatClaimGap(t, st, ctx, ws, before, claimed, tc.hideClaimEvent)
			waitCtx, obs := observeClaimedRecordWait(ctx)
			type outcome struct {
				order core.WorkOrder
				err   error
			}
			done := make(chan outcome, 1)
			go func() {
				order, err := claimReviewSeat(waitCtx, st, orders[1], "GPT-6")
				done <- outcome{order, err}
			}()
			select {
			case <-obs.waits:
			case o := <-done:
				t.Fatalf("seat 2 decided while seat 1 was unseen: order=%+v err=%v", o.order, o.err)
			case <-time.After(time.Minute):
				t.Fatal("seat 2 never waited for seat 1")
			}
			select {
			case o := <-done:
				t.Fatalf("seat 2 decided while seat 1 was unseen: order=%+v err=%v", o.order, o.err)
			default:
			}
			gap.close(t, st, ctx)
			requireModelRefusal(t, (<-done).err, "gpt-6")
			requireSeatStillQueued(t, st, ctx, orders[1])
			if got := seatClaimHolder(t, st, key); got != holder {
				t.Fatalf("a refused claim moved the round's claim from %q to %q", holder, got)
			}
			record, err := parseClaimedRecord(holder)
			if err != nil {
				t.Fatal(err)
			}
			note, err := parseReviewSeatClaimNote(record.Note)
			if err != nil {
				t.Fatal(err)
			}
			if note.OrderID != claimed.ID || note.AttemptID != claimed.AttemptID || note.ReplacedAt.UnixMicro() != before.UpdatedAt.UnixMicro() {
				t.Fatalf("note=%+v, want order %s attempt %s replacing %s", note, claimed.ID, claimed.AttemptID, before.UpdatedAt)
			}
		})
	}
}

// A wait that runs out returns ErrRetryable with nothing written, whichever
// name the seat reported, and the seat stays claimable once seat 1 is visible.
func TestReviewSeatClaimTimeoutWritesNothingIntegration(t *testing.T) {
	st := integrationStore(t)
	shortClaimWait(t)
	for _, tc := range seatClaimGaps {
		t.Run(tc.name, func(t *testing.T) {
			ctx, ws, orders := seedReviewSeats(t, st, "seat-timeout", 2, true)
			before, claimed := claimVisibleSeat(t, st, ctx, orders[0], "gpt-6")
			key := reviewSeatModelClaimKey(ws, claimed.TaskID, 1)
			holder := seatClaimHolder(t, st, key)
			gap := forceSeatClaimGap(t, st, ctx, ws, before, claimed, tc.hideClaimEvent)
			events := countTaskEvents(t, st, ws, claimed.TaskID)
			seatBefore := storedReviewSeat(t, st, ctx, orders[1].ID)
			for _, model := range []string{"gpt-6", "claude-opus-5-5"} {
				if _, err := claimReviewSeat(ctx, st, orders[1], model); !errors.Is(err, store.ErrRetryable) {
					t.Fatalf("claim with %q err=%v, want ErrRetryable", model, err)
				}
			}
			if after := countTaskEvents(t, st, ws, claimed.TaskID); after != events {
				t.Fatalf("task events %d -> %d after timed-out claims", events, after)
			}
			requireSeatStillQueued(t, st, ctx, orders[1])
			if seatAfter := storedReviewSeat(t, st, ctx, orders[1].ID); !seatAfter.UpdatedAt.Equal(seatBefore.UpdatedAt) {
				t.Fatalf("seat row stamp %s -> %s after timed-out claims", seatBefore.UpdatedAt, seatAfter.UpdatedAt)
			}
			if got := seatClaimHolder(t, st, key); got != holder {
				t.Fatalf("a timed-out claim moved the round's claim from %q to %q", holder, got)
			}
			failures, err := st.ListWorkOrderClaimFailures(ctx, []string{orders[1].ID})
			if err != nil || len(failures) != 0 {
				t.Fatalf("claim failures=%+v err=%v", failures, err)
			}

			gap.close(t, st, ctx)
			_, err = claimReviewSeat(ctx, st, orders[1], "gpt-6")
			requireModelRefusal(t, err, "gpt-6")
			second, err := claimReviewSeat(ctx, st, orders[1], "claude-opus-5-5")
			if err != nil {
				t.Fatal(err)
			}
			record, err := parseClaimedRecord(claimHolder(t, st, key))
			if err != nil {
				t.Fatal(err)
			}
			note, err := parseReviewSeatClaimNote(record.Note)
			if err != nil || note.OrderID != second.ID || note.AttemptID != second.AttemptID {
				t.Fatalf("note=%+v err=%v, want seat 2's claim %s", note, err, second.AttemptID)
			}
		})
	}
}

// A seat whose claim ended stops counting under AC-2.10 even though the round's
// claim row still names it, and the next claim neither waits nor stalls.
func TestReviewSeatClaimEndedPredecessorStopsCountingIntegration(t *testing.T) {
	st := integrationStore(t)
	for _, tc := range []struct {
		name string
		end  func(t *testing.T, ctx context.Context, ws string, claimed core.WorkOrder)
	}{
		{"released", func(t *testing.T, ctx context.Context, _ string, claimed core.WorkOrder) {
			if _, err := storetest.ReleaseWorkerClaim(ctx, st, claimed.ID, "", core.WorkOrderRelease{SessionID: claimed.SessionID, Reason: "abandoned"}); err != nil {
				t.Fatal(err)
			}
		}},
		{"lease lapsed before the clock moved it", func(t *testing.T, ctx context.Context, _ string, claimed core.WorkOrder) {
			claimed.LeaseExpiresAt = time.Now().UTC().Add(-time.Minute)
			writeReviewSeatImage(t, st, ctx, claimed)
		}},
		{"cancelled", func(t *testing.T, ctx context.Context, _ string, claimed core.WorkOrder) {
			claimed.State = core.WorkOrderCancelled
			claimed.UpdatedAt = time.Now().UTC()
			clearOrderClaim(&claimed)
			writeReviewSeatImage(t, st, ctx, claimed)
		}},
		{"row deleted", func(t *testing.T, ctx context.Context, ws string, claimed core.WorkOrder) {
			if _, err := st.db.ExecContext(ctx, `DELETE FROM work_orders WHERE workspace_id=? AND id=?`, ws, claimed.ID); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, ws, orders := seedReviewSeats(t, st, "seat-ended", 2, true)
			_, claimed := claimVisibleSeat(t, st, ctx, orders[0], "gpt-6")
			tc.end(t, ctx, ws, claimed)
			waitCtx, obs := observeClaimedRecordWait(ctx)
			if _, err := claimReviewSeat(waitCtx, st, orders[1], "gpt-6"); err != nil {
				t.Fatal(err)
			}
			select {
			case <-obs.waits:
				t.Fatal("seat 2 waited for a claim that had ended")
			default:
			}
		})
	}
}

// Each holder waited for its predecessor before it wrote, so the next claim
// needs only the latest name on the claim row to see every earlier seat.
func TestReviewSeatClaimChainNamesEveryPredecessorIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx, ws, orders := seedReviewSeats(t, st, "seat-chain", 3, true)
	claimVisibleSeat(t, st, ctx, orders[0], "gpt-6")
	before, claimed := claimVisibleSeat(t, st, ctx, orders[1], "claude-opus-5-5")
	gap := forceSeatClaimGap(t, st, ctx, ws, before, claimed, false)
	waitCtx, obs := observeClaimedRecordWait(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := claimReviewSeat(waitCtx, st, orders[2], "GPT-6")
		done <- err
	}()
	select {
	case <-obs.waits:
	case err := <-done:
		t.Fatalf("seat 3 decided while seat 2 was unseen: err=%v", err)
	case <-time.After(time.Minute):
		t.Fatal("seat 3 never waited for seat 2")
	}
	gap.close(t, st, ctx)
	requireModelRefusal(t, <-done, "gpt-6", "claude-opus-5-5")
	requireSeatStillQueued(t, st, ctx, orders[2])
}

// A task whose frozen policy is off compares no names, so its claims touch no
// claim row and never wait.
func TestReviewSeatClaimPolicyOffTakesNoClaimIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx, ws, orders := seedReviewSeats(t, st, "seat-off", 2, false)
	waitCtx, obs := observeClaimedRecordWait(ctx)
	for _, order := range orders {
		if _, err := claimReviewSeat(waitCtx, st, order, "gpt-6"); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-obs.waits:
		t.Fatal("a policy-off claim waited")
	default:
	}
	var rows int
	if err := st.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM conveyor_locks WHERE `key`=?", lockRowKey(reviewSeatModelClaimKey(ws, orders[0].TaskID, 1))).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("claim rows=%d err=%v, want none", rows, err)
	}
}
