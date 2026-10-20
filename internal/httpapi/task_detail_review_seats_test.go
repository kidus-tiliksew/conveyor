package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// The review seat projection of the bounded task-detail read
// (feature-review-seat-minimum RS-7, RS-3; req-review-gates-evidence AC-2.7,
// AC-2.8). Every case writes rounds, additions, and plans through the store's
// own commands and reads them back through enrichReviewSeatSummaries.

type seatMetaFixture struct {
	t   *testing.T
	st  store.Store
	ctx context.Context
}

func newSeatMetaFixture(t *testing.T) *seatMetaFixture {
	t.Helper()
	return &seatMetaFixture{t: t, st: store.NewMemory(), ctx: store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), historyWorkspace)}
}

// frozenSeats freezes a seat list of n seats; a negative n leaves the task
// without a frozen list so it resolves against the workspace policy.
func (f *seatMetaFixture) task(id string, frozenSeats int) core.Task {
	f.t.Helper()
	task := core.Task{ID: id, Workspace: historyWorkspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, NextStage: core.StageReview, ReviewedHeadSHA: "head", CreatedAt: time.Now().UTC()}
	task.SetupContract = config.ExecutionSetup{MaxBounces: 3, ExecutionSettings: config.ContextualExecutionSettings{Review: config.ReviewExecutionSettings{TimeoutText: "1h"}}}
	if frozenSeats >= 0 {
		task.SetupContract.Review.Seats = make([]config.ReviewSeat, frozenSeats)
	}
	requireOKHTTP(f.t, f.st.CreateTask(f.ctx, task))
	stored, err := f.st.GetTask(f.ctx, id)
	requireOKHTTP(f.t, err)
	return stored
}

func seatMetaMembers(taskID string, round, from, count int) ([]core.Job, []core.WorkOrder) {
	now := time.Now().UTC()
	var jobs []core.Job
	var orders []core.WorkOrder
	for seat := from; seat < from+count; seat++ {
		id := fmt.Sprintf("%s-review-%d-seat-%d", taskID, round, seat)
		jobs = append(jobs, core.Job{ID: id, TaskID: taskID, Stage: core.StageReview, State: core.JobPending})
		orders = append(orders, core.WorkOrder{ID: id, JobID: id, TaskID: taskID, Stage: core.StageReview, ReviewRound: round, ReviewSeat: seat,
			HeadSHA: "head", BaselineSHA: "base", QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now})
	}
	return jobs, orders
}

// recordedRound opens a round through CreateReviewRoundCommand, which
// revalidates the metadata and writes the round's record.
func (f *seatMetaFixture) recordedRound(taskID string, round int, seats store.ReviewRoundSeatResolution) []core.WorkOrder {
	f.t.Helper()
	jobs, orders := seatMetaMembers(taskID, round, 1, seats.SeatCount)
	_, err := taskops.ExecuteWorkOrder(f.ctx, f.st, taskID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (struct{}, error) {
		return struct{}{}, f.st.CreateReviewRoundCommand(f.ctx, lease, taskID, seats, jobs, orders)
	})
	requireOKHTTP(f.t, err)
	return orders
}

// legacyRound persists seat orders with no record, as rounds created before
// the record existed; round 0 seats each form their own panel.
func (f *seatMetaFixture) legacyRound(taskID string, round, count int) []core.WorkOrder {
	f.t.Helper()
	jobs, orders := seatMetaMembers(taskID, round, 1, count)
	if round == 0 {
		for i := range orders {
			orders[i].ID = fmt.Sprintf("%s-legacy-%d", taskID, i+1)
			jobs[i].ID, orders[i].JobID = orders[i].ID, orders[i].ID
		}
	}
	for i := range jobs {
		requireOKHTTP(f.t, f.st.CreateJob(f.ctx, jobs[i]))
		requireOKHTTP(f.t, storetest.CreateWorkOrder(f.ctx, f.st, orders[i]))
	}
	return orders
}

func (f *seatMetaFixture) plan(taskID string, seats *int, approve bool) {
	f.t.Helper()
	spec, err := f.st.CreateSpecVersion(f.ctx, core.SpecVersion{TaskID: taskID, Content: "plan", Acceptance: core.JSONPayload([]any{}), Decomposition: core.JSONPayload([]any{}), ReviewSeats: seats})
	requireOKHTTP(f.t, err)
	if approve {
		requireOKHTTP(f.t, f.st.ApproveSpecVersion(f.ctx, taskID, spec.Version))
	}
}

func (f *seatMetaFixture) addSeats(taskID, requestID string, add int) store.ReviewSeatAdditionResult {
	f.t.Helper()
	ctx := store.WithActor(f.ctx, store.Actor{ID: store.UserActorID("usr_a"), Role: core.ActorUser})
	result, err := taskops.ExecuteWorkOrder(ctx, f.st, taskID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (store.ReviewSeatAdditionResult, error) {
		return f.st.AddReviewSeatsCommand(ctx, lease, store.ReviewSeatAdditionRequest{TaskID: taskID, RequestID: requestID, Reason: "second opinion", Add: add})
	})
	requireOKHTTP(f.t, err)
	return result
}

func (f *seatMetaFixture) completeRound(taskID string, round int, verdict string, orders []core.WorkOrder) {
	f.t.Helper()
	reviews := make([]map[string]any, 0, len(orders))
	for _, order := range orders {
		reviews = append(reviews, map[string]any{"review_work_order_id": order.ID, "verdict": verdict})
	}
	requireOKHTTP(f.t, f.st.AppendEvent(f.ctx, core.Event{TaskID: taskID, Kind: "review.round_completed", At: time.Now().UTC(),
		Payload: core.JSONPayload(map[string]any{"review_round": round, "verdict": verdict, "summary": "round " + fmt.Sprint(round), "reviews": reviews})}))
}

func (f *seatMetaFixture) supersede(taskID string, orderIDs ...string) {
	f.t.Helper()
	requireOKHTTP(f.t, f.st.AppendEvent(f.ctx, core.Event{TaskID: taskID, Kind: "task.setup.changed", At: time.Now().UTC(),
		Payload: core.JSONPayload(map[string]any{"review_transition": map[string]any{"superseded_work_order_ids": orderIDs}})}))
}

// seatSummariesJSON is the wire shape the dashboard reads.
type seatSummariesJSON struct {
	ReviewRound *struct {
		Verdict     *string `json:"verdict"`
		ReviewCount *int    `json:"review_count"`
		EventID     *string `json:"event_id"`
		At          *string `json:"at"`
		seatMetaJSON
	} `json:"review_round"`
	ReviewRoundSeats *struct {
		ByRound map[string]seatMetaJSON `json:"by_round"`
		ByOrder map[string]seatMetaJSON `json:"by_order"`
	} `json:"review_round_seats"`
	ReviewSeatPolicy *struct {
		MinimumSeatCount int  `json:"minimum_seat_count"`
		PlannedSeatCount *int `json:"planned_seat_count"`
	} `json:"review_seat_policy"`
	ReviewRounds struct {
		ByRound map[string]json.RawMessage `json:"by_round"`
	} `json:"review_rounds"`
	raw map[string]json.RawMessage
}

type seatMetaJSON struct {
	ReviewRound      *int    `json:"review_round"`
	SeatCount        *int    `json:"seat_count"`
	SeatCountSource  *string `json:"seat_count_source"`
	MinimumSeatCount *int    `json:"minimum_seat_count"`
	PlannedSeatCount *int    `json:"planned_seat_count"`
	CarriedFromRound *int    `json:"carried_from_round"`
	Superseded       *bool   `json:"superseded"`
}

// want describes an entry; zero planned and carried mean absent.
type seatMetaWant struct {
	round, count     int
	source           string
	minimum, planned int
	carried          int
	superseded       bool
}

func requireSeatMeta(t *testing.T, label string, got seatMetaJSON, want seatMetaWant) {
	t.Helper()
	intOr := func(value *int) int {
		if value == nil {
			return -1
		}
		return *value
	}
	if intOr(got.ReviewRound) != want.round || intOr(got.SeatCount) != want.count || got.SeatCountSource == nil || *got.SeatCountSource != want.source || intOr(got.MinimumSeatCount) != want.minimum {
		t.Fatalf("%s: round=%d count=%d source=%v minimum=%d, want %+v", label, intOr(got.ReviewRound), intOr(got.SeatCount), got.SeatCountSource, intOr(got.MinimumSeatCount), want)
	}
	if (want.planned == 0) != (got.PlannedSeatCount == nil) || (want.planned != 0 && *got.PlannedSeatCount != want.planned) {
		t.Fatalf("%s: planned=%d, want %d", label, intOr(got.PlannedSeatCount), want.planned)
	}
	if (want.carried == 0) != (got.CarriedFromRound == nil) || (want.carried != 0 && *got.CarriedFromRound != want.carried) {
		t.Fatalf("%s: carried_from_round=%d, want %d", label, intOr(got.CarriedFromRound), want.carried)
	}
	if (got.Superseded != nil && *got.Superseded) != want.superseded || (got.Superseded != nil && !*got.Superseded) {
		t.Fatalf("%s: superseded=%v, want %v", label, got.Superseded, want.superseded)
	}
}

// project runs the bounded view's summary path on the complete stored task.
func (f *seatMetaFixture) project(srv *Server, taskID string) (seatSummariesJSON, error) {
	f.t.Helper()
	task, err := f.st.GetTask(f.ctx, taskID)
	requireOKHTTP(f.t, err)
	events, err := f.st.ListEvents(f.ctx, taskID)
	requireOKHTTP(f.t, err)
	orders, err := f.st.ListTaskWorkOrders(f.ctx, taskID)
	requireOKHTTP(f.t, err)
	summaries := deriveTaskDetailSummaries(events, nil, orders)
	if err := srv.enrichReviewSeatSummaries(f.ctx, task, orders, events, &summaries); err != nil {
		return seatSummariesJSON{}, err
	}
	data, err := json.Marshal(summaries)
	requireOKHTTP(f.t, err)
	var decoded seatSummariesJSON
	requireOKHTTP(f.t, json.Unmarshal(data, &decoded))
	requireOKHTTP(f.t, json.Unmarshal(data, &decoded.raw))
	return decoded, nil
}

func (f *seatMetaFixture) mustProject(srv *Server, taskID string) seatSummariesJSON {
	f.t.Helper()
	got, err := f.project(srv, taskID)
	requireOKHTTP(f.t, err)
	return got
}

func seatResolution(count int, source string, minimum int, implementation string) store.ReviewRoundSeatResolution {
	return store.ReviewRoundSeatResolution{SeatCount: count, Source: source, MinimumSeatCount: minimum, FloorSeatCount: minimum, ImplementationWorkOrderID: implementation}
}

func seatsPtr(value int) *int { return &value }

func workspaceSeatsProvider(seats int, calls *int, err error) func(context.Context) (*config.Config, error) {
	return func(context.Context) (*config.Config, error) {
		*calls++
		if err != nil {
			return nil, err
		}
		cfg := &config.Config{}
		cfg.Review.Seats = make([]config.ReviewSeat, seats)
		return cfg, nil
	}
}

func TestTaskDetailReviewSeatMetadata(t *testing.T) {
	t.Run("Declared", func(t *testing.T) {
		f := newSeatMetaFixture(t)
		f.task("declared", 2)
		f.recordedRound("declared", 1, seatResolution(3, store.ReviewSeatSourceDeclared, 2, "declared-implement-1"))
		got := f.mustProject(NewServer(f.st), "declared")
		requireSeatMeta(t, "by_round", got.ReviewRoundSeats.ByRound["1"], seatMetaWant{round: 1, count: 3, source: "declared", minimum: 2})
		requireSeatMeta(t, "review_round", got.ReviewRound.seatMetaJSON, seatMetaWant{round: 1, count: 3, source: "declared", minimum: 2})
		if got.ReviewSeatPolicy == nil || got.ReviewSeatPolicy.MinimumSeatCount != 2 || got.ReviewSeatPolicy.PlannedSeatCount != nil {
			t.Fatalf("policy=%+v", got.ReviewSeatPolicy)
		}
	})
	t.Run("Planned", func(t *testing.T) {
		f := newSeatMetaFixture(t)
		f.task("planned", 2)
		f.plan("planned", seatsPtr(4), true)
		f.recordedRound("planned", 1, seatResolution(4, store.ReviewSeatSourcePlanned, 2, ""))
		got := f.mustProject(NewServer(f.st), "planned")
		requireSeatMeta(t, "by_round", got.ReviewRoundSeats.ByRound["1"], seatMetaWant{round: 1, count: 4, source: "planned", minimum: 2, planned: 4})
		if got.ReviewSeatPolicy.MinimumSeatCount != 2 || got.ReviewSeatPolicy.PlannedSeatCount == nil || *got.ReviewSeatPolicy.PlannedSeatCount != 4 {
			t.Fatalf("policy=%+v", got.ReviewSeatPolicy)
		}
	})
	t.Run("CarriedSparsePreviousRound", func(t *testing.T) {
		f := newSeatMetaFixture(t)
		f.task("sparse", 2)
		first := f.recordedRound("sparse", 1, seatResolution(2, store.ReviewSeatSourceMinimum, 2, ""))
		f.completeRound("sparse", 1, "changes_requested", first)
		// No round 2 exists: the predecessor of round 3 is round 1, never R-1.
		f.recordedRound("sparse", 3, seatResolution(2, store.ReviewSeatSourceCarried, 2, ""))
		got := f.mustProject(NewServer(f.st), "sparse")
		if _, ok := got.ReviewRoundSeats.ByRound["2"]; ok {
			t.Fatal("projection invented round 2")
		}
		requireSeatMeta(t, "round 1", got.ReviewRoundSeats.ByRound["1"], seatMetaWant{round: 1, count: 2, source: "minimum", minimum: 2})
		requireSeatMeta(t, "round 3", got.ReviewRoundSeats.ByRound["3"], seatMetaWant{round: 3, count: 2, source: "carried", minimum: 2, carried: 1})
		requireSeatMeta(t, "review_round", got.ReviewRound.seatMetaJSON, seatMetaWant{round: 3, count: 2, source: "carried", minimum: 2, carried: 1})
	})
	t.Run("Minimum", func(t *testing.T) {
		f := newSeatMetaFixture(t)
		f.task("minimum", 2)
		f.recordedRound("minimum", 1, seatResolution(2, store.ReviewSeatSourceMinimum, 2, ""))
		got := f.mustProject(NewServer(f.st), "minimum")
		requireSeatMeta(t, "by_round", got.ReviewRoundSeats.ByRound["1"], seatMetaWant{round: 1, count: 2, source: "minimum", minimum: 2})
		if len(got.ReviewRoundSeats.ByOrder) != 0 {
			t.Fatalf("round-zero entries for a positive round: %v", got.ReviewRoundSeats.ByOrder)
		}
	})
	t.Run("RecordedAddition", func(t *testing.T) {
		f := newSeatMetaFixture(t)
		f.task("added", 2)
		f.recordedRound("added", 1, seatResolution(2, store.ReviewSeatSourceMinimum, 2, ""))
		if result := f.addSeats("added", "add-1", 2); result.SeatCount != 4 {
			t.Fatalf("addition=%+v", result)
		}
		got := f.mustProject(NewServer(f.st), "added")
		// The record keeps its source and stored minimum; additions raise the count.
		requireSeatMeta(t, "recorded addition", got.ReviewRoundSeats.ByRound["1"], seatMetaWant{round: 1, count: 4, source: "minimum", minimum: 2})

		// An addition to a legacy round writes a carried record with no
		// predecessor, so no carried_from_round is invented.
		f.task("legacy-added", 2)
		f.legacyRound("legacy-added", 1, 2)
		f.addSeats("legacy-added", "add-2", 1)
		legacy := f.mustProject(NewServer(f.st), "legacy-added")
		requireSeatMeta(t, "legacy addition", legacy.ReviewRoundSeats.ByRound["1"], seatMetaWant{round: 1, count: 3, source: "carried", minimum: 2})
	})
	t.Run("LegacyRound", func(t *testing.T) {
		f := newSeatMetaFixture(t)
		f.task("legacy", 3)
		f.legacyRound("legacy", 1, 2)
		got := f.mustProject(NewServer(f.st), "legacy")
		requireSeatMeta(t, "legacy", got.ReviewRoundSeats.ByRound["1"], seatMetaWant{round: 1, count: 2, source: "minimum", minimum: 3})
		requireSeatMeta(t, "review_round", got.ReviewRound.seatMetaJSON, seatMetaWant{round: 1, count: 2, source: "minimum", minimum: 3})
	})
	t.Run("LegacyRoundZero", func(t *testing.T) {
		f := newSeatMetaFixture(t)
		f.task("zero", 2)
		orders := f.legacyRound("zero", 0, 2)
		f.supersede("zero", orders[1].ID)
		got := f.mustProject(NewServer(f.st), "zero")
		if len(got.ReviewRoundSeats.ByRound) != 0 || got.ReviewRound != nil {
			t.Fatalf("round-zero orders produced a positive round: %+v %+v", got.ReviewRoundSeats.ByRound, got.ReviewRound)
		}
		requireSeatMeta(t, "zero 1", got.ReviewRoundSeats.ByOrder[orders[0].ID], seatMetaWant{round: 0, count: 1, source: "minimum", minimum: 2})
		requireSeatMeta(t, "zero 2", got.ReviewRoundSeats.ByOrder[orders[1].ID], seatMetaWant{round: 0, count: 0, source: "minimum", minimum: 2, superseded: true})
	})
	t.Run("SupersededOrders", func(t *testing.T) {
		f := newSeatMetaFixture(t)
		f.task("retired", 2)
		legacy := f.legacyRound("retired", 1, 3)
		f.supersede("retired", legacy[2].ID)
		got := f.mustProject(NewServer(f.st), "retired")
		requireSeatMeta(t, "legacy superseded", got.ReviewRoundSeats.ByRound["1"], seatMetaWant{round: 1, count: 2, source: "minimum", minimum: 2, superseded: true})
		requireSeatMeta(t, "review_round", got.ReviewRound.seatMetaJSON, seatMetaWant{round: 1, count: 2, source: "minimum", minimum: 2, superseded: true})

		f.task("retired-recorded", 2)
		recorded := f.recordedRound("retired-recorded", 1, seatResolution(2, store.ReviewSeatSourceMinimum, 2, ""))
		f.supersede("retired-recorded", recorded[0].ID)
		got = f.mustProject(NewServer(f.st), "retired-recorded")
		requireSeatMeta(t, "recorded superseded", got.ReviewRoundSeats.ByRound["1"], seatMetaWant{round: 1, count: 2, source: "minimum", minimum: 2, superseded: true})
	})
	t.Run("FrozenEmptyPolicy", func(t *testing.T) {
		f := newSeatMetaFixture(t)
		f.task("frozen-empty", 0)
		f.legacyRound("frozen-empty", 1, 1)
		calls := 0
		srv := NewServer(f.st)
		srv.ConfigProvider = workspaceSeatsProvider(3, &calls, nil)
		got := f.mustProject(srv, "frozen-empty")
		if calls != 0 {
			t.Fatalf("a frozen seat list read the workspace configuration %d times", calls)
		}
		if got.ReviewSeatPolicy.MinimumSeatCount != 1 {
			t.Fatalf("frozen-empty minimum=%d", got.ReviewSeatPolicy.MinimumSeatCount)
		}
		requireSeatMeta(t, "frozen empty", got.ReviewRoundSeats.ByRound["1"], seatMetaWant{round: 1, count: 1, source: "minimum", minimum: 1})
	})
	t.Run("AbsentPolicyWorkspaceFallback", func(t *testing.T) {
		f := newSeatMetaFixture(t)
		f.task("absent", -1)
		f.legacyRound("absent", 1, 3)
		calls := 0
		srv := NewServer(f.st)
		srv.ConfigProvider = workspaceSeatsProvider(3, &calls, nil)
		got := f.mustProject(srv, "absent")
		if calls != 1 || got.ReviewSeatPolicy.MinimumSeatCount != 3 {
			t.Fatalf("calls=%d minimum=%d", calls, got.ReviewSeatPolicy.MinimumSeatCount)
		}
		requireSeatMeta(t, "workspace minimum", got.ReviewRoundSeats.ByRound["1"], seatMetaWant{round: 1, count: 3, source: "minimum", minimum: 3})

		failure := errors.New("workspace configuration unavailable")
		srv.ConfigProvider = workspaceSeatsProvider(3, &calls, failure)
		if _, err := f.project(srv, "absent"); !errors.Is(err, failure) {
			t.Fatalf("configuration failure err=%v", err)
		}
		// Without a provider the workspace counts no seats, as in Service.AddReviewSeats.
		if got := f.mustProject(NewServer(f.st), "absent"); got.ReviewSeatPolicy.MinimumSeatCount != 1 {
			t.Fatalf("no-provider minimum=%d", got.ReviewSeatPolicy.MinimumSeatCount)
		}
	})
	t.Run("NewerUnapprovedPlan", func(t *testing.T) {
		f := newSeatMetaFixture(t)
		f.task("draft", 2)
		f.plan("draft", seatsPtr(3), true)
		f.plan("draft", seatsPtr(5), false)
		f.recordedRound("draft", 1, seatResolution(3, store.ReviewSeatSourcePlanned, 2, ""))
		got := f.mustProject(NewServer(f.st), "draft")
		if got.ReviewSeatPolicy.PlannedSeatCount == nil || *got.ReviewSeatPolicy.PlannedSeatCount != 3 {
			t.Fatalf("policy planned=%v, want the approved 3", got.ReviewSeatPolicy.PlannedSeatCount)
		}
		requireSeatMeta(t, "approved contribution", got.ReviewRoundSeats.ByRound["1"], seatMetaWant{round: 1, count: 3, source: "planned", minimum: 2, planned: 3})
	})
	t.Run("NoApprovedPlan", func(t *testing.T) {
		f := newSeatMetaFixture(t)
		f.task("unapproved", 2)
		f.plan("unapproved", seatsPtr(4), false)
		got := f.mustProject(NewServer(f.st), "unapproved")
		if got.ReviewSeatPolicy.MinimumSeatCount != 2 || got.ReviewSeatPolicy.PlannedSeatCount != nil {
			t.Fatalf("unapproved plan policy=%+v", got.ReviewSeatPolicy)
		}
		if got.ReviewRound != nil || len(got.ReviewRoundSeats.ByRound) != 0 || len(got.ReviewRoundSeats.ByOrder) != 0 {
			t.Fatalf("no round expected: %+v", got.ReviewRoundSeats)
		}
		// An approved version without a count clears an earlier approved count.
		f.task("cleared", 2)
		f.plan("cleared", seatsPtr(4), true)
		f.plan("cleared", nil, true)
		if got := f.mustProject(NewServer(f.st), "cleared"); got.ReviewSeatPolicy.PlannedSeatCount != nil {
			t.Fatalf("cleared planned=%d", *got.ReviewSeatPolicy.PlannedSeatCount)
		}
	})
	t.Run("OpenRoundDoesNotApprove", func(t *testing.T) {
		f := newSeatMetaFixture(t)
		f.task("open", 2)
		first := f.recordedRound("open", 1, seatResolution(2, store.ReviewSeatSourceMinimum, 2, ""))
		f.completeRound("open", 1, "approve", first)
		second := f.recordedRound("open", 2, seatResolution(2, store.ReviewSeatSourceCarried, 2, ""))
		got := f.mustProject(NewServer(f.st), "open")
		var round map[string]json.RawMessage
		requireOKHTTP(t, json.Unmarshal(got.raw["review_round"], &round))
		for _, key := range []string{"verdict", "summary", "review_count", "event_id", "at"} {
			if _, ok := round[key]; ok {
				t.Fatalf("open round summary carries settlement field %q: %s", key, got.raw["review_round"])
			}
		}
		requireSeatMeta(t, "open round", got.ReviewRound.seatMetaJSON, seatMetaWant{round: 2, count: 2, source: "carried", minimum: 2, carried: 1})
		if _, ok := got.ReviewRounds.ByRound["1"]; !ok || len(got.ReviewRounds.ByRound) != 1 {
			t.Fatalf("review_rounds changed: %v", got.ReviewRounds.ByRound)
		}

		// Once the latest round settles, its settlement fields return beside the seat fields.
		f.completeRound("open", 2, "changes_requested", second)
		got = f.mustProject(NewServer(f.st), "open")
		if got.ReviewRound.Verdict == nil || *got.ReviewRound.Verdict != "changes_requested" || got.ReviewRound.ReviewCount == nil || *got.ReviewRound.ReviewCount != 2 || got.ReviewRound.EventID == nil {
			t.Fatalf("settled summary=%s", got.raw["review_round"])
		}
		requireSeatMeta(t, "settled round", got.ReviewRound.seatMetaJSON, seatMetaWant{round: 2, count: 2, source: "carried", minimum: 2, carried: 1})
	})
}

// failingSeatReadStore fails one of the enrichment's reads.
type failingSeatReadStore struct {
	store.Store
	records, plan error
}

func (s *failingSeatReadStore) ListReviewRoundSeats(ctx context.Context, taskID string) ([]store.ReviewRoundSeats, error) {
	if s.records != nil {
		return nil, s.records
	}
	return s.Store.ListReviewRoundSeats(ctx, taskID)
}

func (s *failingSeatReadStore) GetApprovedSpecVersion(ctx context.Context, taskID string) (core.SpecVersion, bool, error) {
	if s.plan != nil {
		return core.SpecVersion{}, false, s.plan
	}
	return s.Store.GetApprovedSpecVersion(ctx, taskID)
}

func TestTaskActivityReviewSeatMetadata(t *testing.T) {
	f := newSeatMetaFixture(t)
	f.task("seat-detail", 2)
	f.plan("seat-detail", seatsPtr(3), true)
	first := f.recordedRound("seat-detail", 1, seatResolution(3, store.ReviewSeatSourcePlanned, 2, ""))
	f.completeRound("seat-detail", 1, "changes_requested", first)
	// The round events fall outside the bounded page of 100 human events.
	for i := range 120 {
		requireOKHTTP(t, f.st.AppendEvent(f.ctx, core.Event{TaskID: "seat-detail", Kind: "merge.requested", At: time.Now().UTC(), Payload: core.JSONPayload(map[string]any{"n": i})}))
	}
	f.recordedRound("seat-detail", 2, seatResolution(3, store.ReviewSeatSourceCarried, 2, ""))
	f.addSeats("seat-detail", "detail-add", 1)
	f.task("seat-plan", 2)
	f.plan("seat-plan", seatsPtr(3), true)

	h := newHistoryHarness(t, f.st)
	detail, _ := h.bounded("seat-detail")
	for _, event := range detail.Events {
		if event.Kind == "review.round_completed" {
			t.Fatal("fixture round completion is inside the bounded page")
		}
	}
	var got seatSummariesJSON
	requireOKHTTP(t, json.Unmarshal(detail.Raw["summaries"], &got))
	requireOKHTTP(t, json.Unmarshal(detail.Raw["summaries"], &got.raw))
	requireSeatMeta(t, "historical round", got.ReviewRoundSeats.ByRound["1"], seatMetaWant{round: 1, count: 3, source: "planned", minimum: 2, planned: 3})
	requireSeatMeta(t, "current round", got.ReviewRoundSeats.ByRound["2"], seatMetaWant{round: 2, count: 4, source: "carried", minimum: 2, planned: 3, carried: 1})
	requireSeatMeta(t, "latest summary", got.ReviewRound.seatMetaJSON, seatMetaWant{round: 2, count: 4, source: "carried", minimum: 2, planned: 3, carried: 1})
	if got.ReviewRound.Verdict != nil {
		t.Fatalf("open round 2 reads as settled: %s", got.raw["review_round"])
	}
	if _, ok := got.ReviewRounds.ByRound["1"]; !ok {
		t.Fatal("round 1 settlement lost from review_rounds")
	}

	// The minimum is served before any review round exists.
	plan, _ := h.bounded("seat-plan")
	var planned seatSummariesJSON
	requireOKHTTP(t, json.Unmarshal(plan.Raw["summaries"], &planned))
	if planned.ReviewSeatPolicy == nil || planned.ReviewSeatPolicy.MinimumSeatCount != 2 || planned.ReviewSeatPolicy.PlannedSeatCount == nil || *planned.ReviewSeatPolicy.PlannedSeatCount != 3 {
		t.Fatalf("pre-review policy=%+v", planned.ReviewSeatPolicy)
	}
	if planned.ReviewRound != nil || len(planned.ReviewRoundSeats.ByRound) != 0 {
		t.Fatalf("pre-review rounds=%+v", planned.ReviewRoundSeats)
	}

	// The complete view without view=bounded keeps its shape.
	complete := h.get(historyTokenA, "/v1/tasks/seat-detail/activity")
	if complete.Code != http.StatusOK || strings.Contains(complete.Body.String(), "review_round_seats") || strings.Contains(complete.Body.String(), "review_seat_policy") {
		t.Fatalf("complete view: %d carries seat summaries", complete.Code)
	}

	t.Run("WorkspaceIsolation", func(t *testing.T) {
		other := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "other")
		task := core.Task{ID: "seat-foreign", Workspace: "other", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-seat-foreign", State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC()}
		task.SetupContract.Review.Seats = make([]config.ReviewSeat, 4)
		requireOKHTTP(t, f.st.CreateTask(other, task))
		get := func(workspace, taskID string) *httptest.ResponseRecorder {
			request := httptest.NewRequest(http.MethodGet, "/v1/tasks/"+taskID+"/activity?view=bounded&workspace_id="+workspace, nil)
			request.Header.Set("Authorization", "Bearer "+historyTokenA)
			response := httptest.NewRecorder()
			h.handler.ServeHTTP(response, request)
			return response
		}
		// The memory store refuses the foreign read before the projection runs.
		if response := get(historyWorkspace, "seat-foreign"); response.Code == http.StatusOK || strings.Contains(response.Body.String(), "review_seat_policy") {
			t.Fatalf("foreign task through demo: %d %s", response.Code, response.Body.String())
		}
		response := get("other", "seat-foreign")
		if response.Code != http.StatusOK {
			t.Fatalf("foreign task in its workspace: %d %s", response.Code, response.Body.String())
		}
		var body struct {
			Summaries seatSummariesJSON `json:"summaries"`
		}
		requireOKHTTP(t, json.Unmarshal(response.Body.Bytes(), &body))
		if body.Summaries.ReviewSeatPolicy.MinimumSeatCount != 4 || body.Summaries.ReviewSeatPolicy.PlannedSeatCount != nil || len(body.Summaries.ReviewRoundSeats.ByRound) != 0 {
			t.Fatalf("foreign summaries leaked demo state: %+v %+v", body.Summaries.ReviewSeatPolicy, body.Summaries.ReviewRoundSeats)
		}
	})
	t.Run("ReadErrors", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			store store.Store
			cfg   func(context.Context) (*config.Config, error)
			task  string
		}{
			{"Records", &failingSeatReadStore{Store: f.st, records: errors.New("records unavailable")}, nil, "seat-detail"},
			{"ApprovedPlan", &failingSeatReadStore{Store: f.st, plan: errors.New("plan unavailable")}, nil, "seat-detail"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				failing := newHistoryHarness(t, tc.store)
				if response := failing.get(historyTokenA, "/v1/tasks/"+tc.task+"/activity?view=bounded"); response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "unavailable") {
					t.Fatalf("%s: %d %s", tc.name, response.Code, response.Body.String())
				}
			})
		}
		t.Run("Configuration", func(t *testing.T) {
			f.task("seat-absent", -1)
			failing := newHistoryHarness(t, f.st)
			calls := 0
			failing.srv.ConfigProvider = workspaceSeatsProvider(2, &calls, errors.New("configuration unavailable"))
			failing.handler = failing.srv.Handler()
			if response := failing.get(historyTokenA, "/v1/tasks/seat-absent/activity?view=bounded"); response.Code != http.StatusInternalServerError {
				t.Fatalf("configuration failure: %d %s", response.Code, response.Body.String())
			}
		})
	})
}
