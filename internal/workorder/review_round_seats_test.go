package workorder

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

const seatAdditionWorkspace = "demo"

// seatAdditionTask stores a review task whose round 1 has count seats. A
// legacy round is persisted without its review_round_seats record.
func seatAdditionTask(t *testing.T, st store.Store, id string, count int, legacy bool) []core.WorkOrder {
	t.Helper()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), seatAdditionWorkspace)
	task := core.Task{ID: id, Workspace: seatAdditionWorkspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	jobs, orders := make([]core.Job, 0, count), make([]core.WorkOrder, 0, count)
	for seat := 1; seat <= count; seat++ {
		jobID := fmt.Sprintf("%s-review-1-seat-%d", id, seat)
		jobs = append(jobs, core.Job{ID: jobID, TaskID: id, Stage: core.StageReview, State: core.JobPending})
		orders = append(orders, core.WorkOrder{ID: jobID, JobID: jobID, TaskID: id, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: seat, ReviewKind: "refresh", ReviewScope: "delta",
			BaselineSHA: "round-base", HeadSHA: "round-head", ExecutionTimeoutText: "50m", QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now})
	}
	if legacy {
		for i := range jobs {
			if err := st.CreateJob(ctx, jobs[i]); err != nil {
				t.Fatal(err)
			}
			if err := storetest.CreateWorkOrder(ctx, st, orders[i]); err != nil {
				t.Fatal(err)
			}
		}
		return orders
	}
	if err := storetest.CreateReviewRound(ctx, st, id, jobs, orders); err != nil {
		t.Fatal(err)
	}
	return orders
}

func seatAdditionOperator(ctx context.Context) context.Context {
	ctx = store.WithWorkspace(ctx, seatAdditionWorkspace)
	ctx = store.WithCredential(ctx, core.AuthenticatedCredential{ID: "pat_operator", OwnerUserID: "operator", Kind: core.CredentialUser, Scope: core.CredentialScopeUser})
	return store.WithActor(ctx, store.Actor{ID: store.UserActorID("operator"), Role: core.ActorUser})
}

func seatAdditionOrders(t *testing.T, st store.Store, taskID string) []core.WorkOrder {
	t.Helper()
	orders, err := st.ListTaskWorkOrders(store.WithWorkspace(t.Context(), seatAdditionWorkspace), taskID)
	if err != nil {
		t.Fatal(err)
	}
	return orders
}

// TestAddReviewSeatsRequiresUserCredential proves a direct service call
// cannot fabricate an operator from an actor alone, an agent credential, or
// a mismatched actor, and that a refusal writes nothing (RS-5).
func TestAddReviewSeatsRequiresUserCredential(t *testing.T) {
	st := store.NewMemory()
	seatAdditionTask(t, st, "seat-auth", 1, false)
	service := &Service{Store: st}
	request := store.ReviewSeatAdditionRequest{TaskID: "seat-auth", RequestID: "auth-1", Reason: "second opinion", Add: 1}
	base := store.WithWorkspace(t.Context(), seatAdditionWorkspace)
	user := core.AuthenticatedCredential{ID: "pat_operator", OwnerUserID: "operator", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}
	agent := core.AuthenticatedCredential{ID: "agt_operator", OwnerUserID: "operator", Kind: core.CredentialAgent, Scope: core.CredentialScopeUser}
	userActor := store.Actor{ID: store.UserActorID("operator"), Role: core.ActorUser}
	for name, ctx := range map[string]context.Context{
		"ActorOnly":        store.WithActor(base, userActor),
		"AgentCredential":  store.WithActor(store.WithCredential(base, agent), userActor),
		"AgentActor":       store.WithActor(store.WithCredential(base, user), store.Actor{ID: store.AgentActorID("operator"), Role: core.ActorAgent}),
		"MismatchedActor":  store.WithActor(store.WithCredential(base, user), store.Actor{ID: store.UserActorID("someone-else"), Role: core.ActorUser}),
		"CredentialNoUser": store.WithCredential(base, user),
	} {
		if _, err := service.AddReviewSeats(ctx, request); !errors.Is(err, store.ErrReviewSeatAdditionForbidden) {
			t.Fatalf("%s: error=%v, want forbidden", name, err)
		}
	}
	if orders := seatAdditionOrders(t, st, "seat-auth"); len(orders) != 1 {
		t.Fatalf("refused additions wrote orders: %+v", orders)
	}
	if _, err := service.AddReviewSeats(seatAdditionOperator(t.Context()), request); err != nil {
		t.Fatalf("operator addition: %v", err)
	}
}

// lockRecordingStore records whether the store command ran inside the task
// side-effect lock's callback and with its locked context.
type lockRecordingStore struct {
	store.Store
	locked     []string
	insideLock bool
	sawLocked  bool
}

type lockMarker struct{}

func (s *lockRecordingStore) WithTaskSideEffectLock(ctx context.Context, taskID string, fn func(context.Context) error) error {
	return s.Store.WithTaskSideEffectLock(ctx, taskID, func(locked context.Context) error {
		s.locked = append(s.locked, taskID)
		s.insideLock = true
		defer func() { s.insideLock = false }()
		return fn(context.WithValue(locked, lockMarker{}, taskID))
	})
}

func (s *lockRecordingStore) AddReviewSeatsCommand(ctx context.Context, lease taskops.TaskLease, request store.ReviewSeatAdditionRequest) (store.ReviewSeatAdditionResult, error) {
	s.sawLocked = s.insideLock && ctx.Value(lockMarker{}) == request.TaskID
	return s.Store.AddReviewSeatsCommand(ctx, lease, request)
}

// TestAddReviewSeatsUsesTaskSideEffectLock proves the command runs inside
// the task side-effect lock with the locked context.
func TestAddReviewSeatsUsesTaskSideEffectLock(t *testing.T) {
	st := &lockRecordingStore{Store: store.NewMemory()}
	seatAdditionTask(t, st, "seat-lock", 1, false)
	service := &Service{Store: st}
	if _, err := service.AddReviewSeats(seatAdditionOperator(t.Context()), store.ReviewSeatAdditionRequest{TaskID: "seat-lock", RequestID: "lock-1", Reason: "r", Add: 1}); err != nil {
		t.Fatal(err)
	}
	if len(st.locked) != 1 || st.locked[0] != "seat-lock" || !st.sawLocked {
		t.Fatalf("locked=%v sawLocked=%t", st.locked, st.sawLocked)
	}
}

// TestAddReviewSeatsPreservesRoundMetadata proves added seats keep the
// round's kind, scope, baseline, head, and timeout without a forge lookup,
// take the configured queue clock, and that a legacy round gets a carried
// record whose minimum is the workspace seat-list length.
func TestAddReviewSeatsPreservesRoundMetadata(t *testing.T) {
	st := store.NewMemory()
	round := seatAdditionTask(t, st, "seat-meta", 2, true)
	cfg := &config.Config{Workspace: seatAdditionWorkspace, Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, 3)}, WorkOrderQueueTimeout: 2 * time.Hour}
	service := &Service{Store: st, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil },
		ReviewTarget: func(context.Context, string, string) (github.ReviewTarget, error) {
			t.Fatal("seat addition looked up the forge head")
			return github.ReviewTarget{}, nil
		}}
	result, err := service.AddReviewSeats(seatAdditionOperator(t.Context()), store.ReviewSeatAdditionRequest{TaskID: "seat-meta", RequestID: "meta-1", Reason: "r", Add: 2})
	if err != nil {
		t.Fatal(err)
	}
	if result.ReviewRound != 1 || result.PriorSeatCount != 2 || result.SeatCount != 4 {
		t.Fatalf("result=%+v", result)
	}
	added := 0
	for _, order := range seatAdditionOrders(t, st, "seat-meta") {
		if order.ReviewSeat <= 2 {
			continue
		}
		added++
		if order.ReviewKind != round[0].ReviewKind || order.ReviewScope != round[0].ReviewScope || order.BaselineSHA != round[0].BaselineSHA || order.HeadSHA != round[0].HeadSHA ||
			order.ExecutionTimeoutText != round[0].ExecutionTimeoutText || order.QueueDeadline.Sub(order.QueueEnteredAt) != 2*time.Hour || order.RequiredModel != "" || order.RequiredHarness != "" {
			t.Fatalf("added seat=%+v", order)
		}
	}
	if added != 2 {
		t.Fatalf("added %d seats", added)
	}
	records, err := st.ListReviewRoundSeats(store.WithWorkspace(t.Context(), seatAdditionWorkspace), "seat-meta")
	if err != nil || len(records) != 1 || records[0].SeatCount != 4 || records[0].SeatCountSource != store.ReviewSeatSourceCarried || records[0].MinimumSeatCount != 3 {
		t.Fatalf("records=%+v err=%v", records, err)
	}
}

// TestAddReviewSeatsReplay proves an identical request returns the original
// result without writes and a changed body with the same ID conflicts.
func TestAddReviewSeatsReplay(t *testing.T) {
	st := store.NewMemory()
	seatAdditionTask(t, st, "seat-replay", 1, false)
	service := &Service{Store: st}
	ctx := seatAdditionOperator(t.Context())
	request := store.ReviewSeatAdditionRequest{TaskID: "seat-replay", RequestID: "replay-1", Reason: "second opinion", Add: 1}
	first, err := service.AddReviewSeats(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	again, err := service.AddReviewSeats(ctx, request)
	if err != nil || again != first {
		t.Fatalf("replay=%+v err=%v, want %+v", again, err, first)
	}
	if orders := seatAdditionOrders(t, st, "seat-replay"); len(orders) != 2 {
		t.Fatalf("replay wrote orders: %d", len(orders))
	}
	request.Add = 3
	if _, err := service.AddReviewSeats(ctx, request); !errors.Is(err, store.ErrReviewSeatAdditionConflict) {
		t.Fatalf("changed replay error=%v", err)
	}
	request.Add, request.TaskID = 1, "missing-task"
	request.RequestID = "replay-missing"
	if _, err := service.AddReviewSeats(ctx, request); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing task error=%v", err)
	}
}
