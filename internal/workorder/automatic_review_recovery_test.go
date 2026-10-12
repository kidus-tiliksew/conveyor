package workorder

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// automaticReviewHarness is one running task at review whose round 1 has a
// verdict on seat 1 and a verified PR head, served by the operator path.
type automaticReviewHarness struct {
	ctx     context.Context
	st      store.Store
	cfg     *config.Config
	service *Service
	task    core.Task
	seat    string
}

func newAutomaticReviewHarness(t *testing.T) *automaticReviewHarness {
	t.Helper()
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "demo")
	st := store.NewMemory()
	h := &automaticReviewHarness{ctx: ctx, st: st}
	now := time.Now().UTC()
	h.task = core.Task{ID: "auto-review-" + core.NewTaskID(), Workspace: "demo", Repo: "app", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: now}
	h.task.Branch = "conveyor/task-" + h.task.ID
	if err := st.CreateTask(ctx, h.task); err != nil {
		t.Fatal(err)
	}
	var jobs []core.Job
	var orders []core.WorkOrder
	for seat := 1; seat <= 2; seat++ {
		id := h.task.ID + "-review-1-seat-" + string(rune('0'+seat))
		jobs = append(jobs, core.Job{ID: id, TaskID: h.task.ID, Stage: core.StageReview, State: core.JobPending})
		orders = append(orders, core.WorkOrder{ID: id, TaskID: h.task.ID, JobID: id, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: seat, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)})
	}
	if err := storetest.CreateReviewRound(ctx, st, h.task.ID, jobs, orders); err != nil {
		t.Fatal(err)
	}
	first, err := storetest.ClaimWorkOrder(ctx, st, orders[0].ID, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "verdict", ClientToken: "verdict", Lease: time.Hour, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	first.State = core.WorkOrderCompleted
	if err = storetest.UpdateWorkOrder(ctx, st, first, core.WorkOrderCmdSubmitReviewVerdict); err != nil {
		t.Fatal(err)
	}
	for _, event := range []core.Event{
		{TaskID: h.task.ID, JobID: first.JobID, Kind: "review.completed", Payload: core.JSONPayload(map[string]any{"review_work_order_id": first.ID, "review_round": 1, "verdict": "approve"})},
		{TaskID: h.task.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"number": 7, "head_sha": "approved-head"})},
	} {
		if err = st.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	h.seat = orders[1].ID
	// The memory store's AR-3 clock runs the default policy; the sweep reads
	// the same policy from the workspace config.
	h.cfg = &config.Config{
		Workspace: "demo", WorkOrderQueueTimeout: time.Hour, Recovery: config.DefaultRecoveryPolicy(),
		Repos:   []config.Repo{{Name: "app", Base: "main", GitHub: "acme/app"}},
		Routing: config.Routing{Stages: map[string]config.StageRoute{"review": {Execution: config.ExecutionMCP, Harness: "codex", TimeoutText: "45m"}}},
		Harnesses: []config.Harness{
			{Name: "codex", Command: []string{"codex", "{prompt}"}, ProbeCommand: []string{"codex", "--version"}, ProbeTimeoutText: "5s"},
		},
		Review: config.ReviewPanel{Seats: []config.ReviewSeat{{Model: "gpt-current", Harness: "codex"}, {Model: "gpt-second", Harness: "codex"}}},
	}
	h.service = &Service{
		Store:          st,
		ConfigProvider: func(context.Context) (*config.Config, error) { return h.cfg, nil },
		ReviewTarget: func(context.Context, string, string) (githubtrigger.ReviewTarget, error) {
			return githubtrigger.ReviewTarget{Number: 7, HeadSHA: "approved-head"}, nil
		},
	}
	return h
}

func (h *automaticReviewHarness) expireSeat(t *testing.T, label string) {
	t.Helper()
	if _, err := storetest.ClaimWorkOrder(h.ctx, h.st, h.seat, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: label, ClientToken: label, Lease: time.Millisecond, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := taskops.New(h.st).TickOrderClock(h.ctx, time.Now().UTC().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
}

// exhaustSeat drives the AR-3 clock until seat 2 is queued and exhausted.
func (h *automaticReviewHarness) exhaustSeat(t *testing.T) core.WorkOrder {
	t.Helper()
	for attempt := range 12 {
		order, err := h.st.GetWorkOrder(h.ctx, h.seat)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case order.RetrySuppressionReason == core.AutomaticRecoveryExhaustedReason:
			return order
		case !order.NextAutomaticRecoveryAt.IsZero():
			if _, err = taskops.New(h.st).TickOrderClock(h.ctx, order.NextAutomaticRecoveryAt.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
		case order.State == core.WorkOrderQueued:
			h.expireSeat(t, "exhaust-"+string(rune('a'+attempt)))
		default:
			t.Fatalf("seat cannot reach exhaustion: %+v", order)
		}
	}
	t.Fatal("seat never exhausted")
	return core.WorkOrder{}
}

func (h *automaticReviewHarness) sweep(t *testing.T, now time.Time) int {
	t.Helper()
	acted, err := h.service.SweepAutomaticReviewRecovery(h.ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	return acted
}

func (h *automaticReviewHarness) orderCount(t *testing.T) int {
	t.Helper()
	orders, err := h.st.ListTaskWorkOrders(h.ctx, h.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return len(orders)
}

type automaticRoundPayload struct {
	Automatic   bool   `json:"automatic"`
	RequestID   string `json:"request_id"`
	Reason      string `json:"reason"`
	PriorRound  int    `json:"prior_round"`
	NewRound    int    `json:"new_round"`
	ReviewRound int    `json:"review_round"`
	ActorID     string `json:"-"`
	hasKey      bool
}

// roundEvents decodes every event of kind; hasKey reports an "automatic" key.
func (h *automaticReviewHarness) roundEvents(t *testing.T, kind string) []automaticRoundPayload {
	t.Helper()
	events, err := h.st.ListEvents(h.ctx, h.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var out []automaticRoundPayload
	for _, event := range events {
		if event.Kind != kind {
			continue
		}
		var payload automaticRoundPayload
		var keys map[string]json.RawMessage
		if err = json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(event.Payload, &keys); err != nil {
			t.Fatal(err)
		}
		_, payload.hasKey = keys["automatic"]
		payload.ActorID = event.ActorID
		out = append(out, payload)
	}
	return out
}

// D10, D12, D14: a round whose seat AR-3 exhausted gets exactly one automatic
// round retry, never while the clock still owns the seat, and the exhausted
// seat itself is never re-queued or changed.
func TestSweepAutomaticReviewRecoveryRetriesExhaustedRoundOnce(t *testing.T) {
	h := newAutomaticReviewHarness(t)
	h.expireSeat(t, "first")
	if scheduled, _ := h.st.GetWorkOrder(h.ctx, h.seat); scheduled.NextAutomaticRecoveryAt.IsZero() {
		t.Fatalf("AR-3 did not schedule the interrupted seat: %+v", scheduled)
	}
	if acted := h.sweep(t, time.Now().UTC()); acted != 0 || h.orderCount(t) != 2 {
		t.Fatalf("sweep acted=%d orders=%d while the clock owns the seat", acted, h.orderCount(t))
	}
	exhausted := h.exhaustSeat(t)

	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.service.SweepAutomaticReviewRecovery(h.ctx, time.Now().UTC()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if h.orderCount(t) != 4 {
		t.Fatalf("orders=%d, want one automatic round of two seats", h.orderCount(t))
	}
	retried := h.roundEvents(t, "review.round_retried")
	if len(retried) != 1 {
		t.Fatalf("round_retried events=%+v", retried)
	}
	got := retried[0]
	if !got.Automatic || got.RequestID != store.AutomaticReviewRequestID(h.task.ID, 1) || got.Reason != store.AutomaticReviewReason ||
		got.PriorRound != 1 || got.NewRound != 2 || got.ActorID != store.AutomaticReviewActor(h.task.ID).ID {
		t.Fatalf("automatic retry event=%+v", got)
	}
	after, err := h.st.GetWorkOrder(h.ctx, h.seat)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != exhausted.State || after.RetrySuppressionReason != exhausted.RetrySuppressionReason ||
		after.AutomaticRecoveryCount != exhausted.AutomaticRecoveryCount || !after.UpdatedAt.Equal(exhausted.UpdatedAt) {
		t.Fatalf("exhausted seat changed\nbefore=%+v\nafter=%+v", exhausted, after)
	}

	// The round the automatic retry created never gets its own automatic act:
	// interrupting it leaves the act to the operator.
	for _, order := range mustOrders(t, h) {
		if order.ReviewRound == 2 && order.ReviewSeat == 2 {
			h.seat = order.ID
		}
	}
	h.release(t, "round-two")
	if acted := h.sweep(t, time.Now().UTC()); acted != 0 || len(h.roundEvents(t, "review.round_recovered")) != 0 {
		t.Fatalf("second automatic act acted=%d", acted)
	}
	if _, err = h.service.RecoverInterruptedReviewRound(h.ctx, h.task.ID, "operator-recover"); err != nil {
		t.Fatalf("operator recovery after the automatic act: %v", err)
	}
	for _, event := range h.roundEvents(t, "review.round_recovered") {
		if event.Automatic || event.hasKey {
			t.Fatalf("operator recovery carries an automatic key: %+v", event)
		}
	}
}

// D16: the operator's round retry stays operator-attributed: its
// review.round_retried event carries no automatic key and is acted by the
// caller, never the automatic actor.
func TestRetryReviewRoundOperatorCarriesNoAutomaticKey(t *testing.T) {
	h := newAutomaticReviewHarness(t)
	h.expireSeat(t, "first")
	h.exhaustSeat(t)
	if _, err := h.service.RetryReviewRound(h.ctx, h.task.ID, "operator-retry", "operator reason"); err != nil {
		t.Fatal(err)
	}
	retried := h.roundEvents(t, "review.round_retried")
	if len(retried) != 1 {
		t.Fatalf("round_retried events=%+v", retried)
	}
	got := retried[0]
	if got.hasKey || got.RequestID != "operator-retry" || got.Reason != "operator reason" ||
		got.ActorID != store.SystemActor().ID || got.PriorRound != 1 || got.NewRound != 2 {
		t.Fatalf("operator retry event=%+v", got)
	}
}

func mustOrders(t *testing.T, h *automaticReviewHarness) []core.WorkOrder {
	t.Helper()
	orders, err := h.st.ListTaskWorkOrders(h.ctx, h.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return orders
}

// release ends the seat's claim with an ordinary operator release, which
// leaves it interrupted and outside the AR-3 clock.
func (h *automaticReviewHarness) release(t *testing.T, label string) {
	t.Helper()
	if _, err := storetest.ClaimWorkOrder(h.ctx, h.st, h.seat, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: label, ClientToken: label, Lease: time.Hour, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.ReleaseWorkerClaim(h.ctx, h.st, h.seat, "worker", core.WorkOrderRelease{SessionID: label, Reason: "operator release", Outcome: core.WorkOrderOutcomeReleased}); err != nil {
		t.Fatal(err)
	}
}

// D11, D12, D16: an interrupted round the clock does not own is recovered in
// place once; a second interruption of that round is left to the operator.
func TestSweepAutomaticReviewRecoveryRecoversInterruptedRoundOnce(t *testing.T) {
	h := newAutomaticReviewHarness(t)
	h.release(t, "first")
	if acted := h.sweep(t, time.Now().UTC()); acted != 1 || h.orderCount(t) != 2 {
		t.Fatalf("interrupted sweep acted=%d orders=%d", acted, h.orderCount(t))
	}
	recovered := h.roundEvents(t, "review.round_recovered")
	if len(recovered) != 1 || !recovered[0].Automatic || recovered[0].ReviewRound != 1 || recovered[0].ActorID != store.AutomaticReviewActor(h.task.ID).ID {
		t.Fatalf("round_recovered events=%+v", recovered)
	}
	seat, err := h.st.GetWorkOrder(h.ctx, h.seat)
	if err != nil || seat.State != core.WorkOrderQueued || seat.RetrySuppressed {
		t.Fatalf("recovered seat=%+v err=%v", seat, err)
	}
	h.release(t, "second")
	if acted := h.sweep(t, time.Now().UTC()); acted != 0 {
		t.Fatalf("second interruption acted=%d", acted)
	}
	if _, err = h.service.RecoverInterruptedReviewRound(h.ctx, h.task.ID, "operator-recover"); err != nil {
		t.Fatalf("operator recovery after the automatic act: %v", err)
	}
	for _, event := range h.roundEvents(t, "review.round_recovered") {
		if event.RequestID == "operator-recover" && event.hasKey {
			t.Fatalf("operator recovery carries an automatic key: %+v", event)
		}
	}
}

// D6, D13: no act at bound 0 or on a checkpoint seat. Task-state exclusions
// are covered by TestDecideAutomaticReviewAct.
func TestSweepAutomaticReviewRecoveryExclusions(t *testing.T) {
	t.Run("BoundZero", func(t *testing.T) {
		h := newAutomaticReviewHarness(t)
		h.exhaustSeat(t)
		h.cfg.Recovery.AutomaticRecoveries = 0
		if acted := h.sweep(t, time.Now().UTC()); acted != 0 || h.orderCount(t) != 2 {
			t.Fatalf("bound 0 acted=%d orders=%d", acted, h.orderCount(t))
		}
		h.cfg.Recovery.AutomaticRecoveries = 2
		if acted := h.sweep(t, time.Now().UTC()); acted != 1 {
			t.Fatalf("bound 2 acted=%d", acted)
		}
	})
	t.Run("Checkpoint", func(t *testing.T) {
		h := newAutomaticReviewHarness(t)
		if _, err := storetest.ClaimWorkOrder(h.ctx, h.st, h.seat, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "checkpoint", ClientToken: "checkpoint", Lease: time.Hour, ExecutionTimeout: time.Hour}); err != nil {
			t.Fatal(err)
		}
		if _, err := storetest.ReleaseWorkerClaim(h.ctx, h.st, h.seat, "worker", core.WorkOrderRelease{SessionID: "checkpoint", Reason: core.WorkOrderReleaseReasonOperatorCheckpointReached, Outcome: core.WorkOrderOutcomeReleased, Checkpoint: &core.WorkOrderCheckpoint{DecisionRequest: "hold"}}); err != nil {
			t.Fatal(err)
		}
		if acted := h.sweep(t, time.Now().UTC()); acted != 0 || len(h.roundEvents(t, "review.round_recovered")) != 0 {
			t.Fatalf("checkpoint seat acted=%d", acted)
		}
	})
}
