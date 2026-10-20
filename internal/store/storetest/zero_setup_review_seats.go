package storetest

import (
	"reflect"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// RunZeroSetupReviewSeatsConformance proves that every backend persists a
// task's setup contract without changing its review panel. A zero contract
// and stored contracts whose seats are absent or null carry no frozen seat
// count, so the production review sweep builds the new round with every
// configured workspace seat. A contract frozen to an empty seat array stays
// frozen with a minimum of one seat, so the retried round carries round 1's
// two seats (feature-review-seat-minimum RS-2; req-review-gates-evidence
// AC-2.4); one frozen to two seats reads back frozen at two.
func RunZeroSetupReviewSeatsConformance(t *testing.T, x Fixture, sweep ReviewSweepFunc) {
	st, ctx, workspace := x.Backend, x.Context, x.Workspace
	restore := setAutomaticRecoveryBound(t, st, ctx, 1)
	defer restore()
	cfg := automaticReviewSweepConfig(workspace, config.RecoveryPolicy{AutomaticRecoveries: 1, Window: config.DefaultRecoveryWindow, WindowText: config.DefaultRecoveryWindowText})
	for _, tc := range []struct {
		name   string
		raw    string // stored contract; empty keeps the zero contract CreateTask wrote
		frozen bool
		seats  int
	}{
		{"ZeroContract", "", false, len(cfg.Review.Seats)},
		{"SeatsAbsent", `{"max_bounces":0,"stage_timeouts":{"implement":"","review":"","spec":"","verify":""},"review":{}}`, false, len(cfg.Review.Seats)},
		{"SeatsNull", `{"max_bounces":0,"stage_timeouts":{"implement":"","review":"","spec":"","verify":""},"review":{"seats":null}}`, false, len(cfg.Review.Seats)},
		{"SeatsEmpty", `{"max_bounces":0,"stage_timeouts":{"implement":"","review":"","spec":"","verify":""},"review":{"seats":[]}}`, true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireReviewSweep(t, sweep)
			task, _ := seedSweepableReviewRound(t, st, ctx, workspace)
			if tc.raw != "" {
				if x.SeedSetupContract == nil {
					t.Fatal("factory must seed a stored setup contract")
				}
				x.SeedSetupContract(t, ctx, task.ID, tc.raw)
			}
			stored, err := st.GetTask(ctx, task.ID)
			requireOK(t, err)
			if stored.SetupContract.HasFrozenPolicy() != tc.frozen || (stored.SetupContract.Review.Seats != nil) != tc.frozen ||
				(!tc.frozen && !reflect.DeepEqual(stored.SetupContract, config.ExecutionSetup{})) {
				t.Fatalf("setup contract read back as %+v, want frozen=%t", stored.SetupContract, tc.frozen)
			}
			if _, err := sweep(ctx, st, cfg, automaticSweepHead, time.Now().UTC().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if seats := reviewSeatsByRound(t, st, ctx, task.ID); len(seats[2]) != tc.seats {
				t.Fatalf("round 2 seats=%v, want %d", seats[2], tc.seats)
			}
		})
	}
	t.Run("FrozenSeatCountsSurvive", func(t *testing.T) {
		for _, want := range []int{0, 2} {
			setup := config.ExecutionSetup{Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, want)}}
			task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "conveyor", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageReview, SetupContract: setup, CreatedAt: time.Now().UTC()}
			task.Branch = "conveyor/task-" + task.ID
			requireOK(t, st.CreateTask(ctx, task))
			stored, err := st.GetTask(ctx, task.ID)
			requireOK(t, err)
			if got := stored.SetupContract.Review.Seats; !stored.SetupContract.HasFrozenPolicy() || got == nil || len(got) != want {
				t.Fatalf("contract frozen to %d seats read back as frozen=%t seats=%#v", want, stored.SetupContract.HasFrozenPolicy(), got)
			}
		}
	})
}
