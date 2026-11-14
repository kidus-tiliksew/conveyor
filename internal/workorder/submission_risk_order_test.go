package workorder

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// riskTableStore freezes a risk policy with an explicit tier table into the
// evidence fixture's task, so a tier can require more agent reviews than the
// fixture's two-seat minimum.
func withRiskTable(mode riskpolicy.Mode, table riskpolicy.Table) func(store.Store) store.Store {
	return func(st store.Store) store.Store {
		return riskTaskStore{Store: st, policy: &riskpolicy.Policy{Mode: mode, Tiers: table}}
	}
}

// raisingTable gives medium three agent reviews, above the fixture's
// two-seat minimum.
func raisingTable() riskpolicy.Table {
	table := riskpolicy.DefaultTable()
	table.Medium.AgentReviews, table.High.AgentReviews = 3, 3
	return table
}

func taskRiskOrders(t *testing.T, f evidenceSubmissionFixture) []core.RiskOrder {
	t.Helper()
	orders, err := f.st.ListTaskRiskOrders(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return orders
}

func reviewSeatOrders(t *testing.T, f evidenceSubmissionFixture) []core.WorkOrder {
	t.Helper()
	orders, err := f.st.ListTaskWorkOrders(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var seats []core.WorkOrder
	for _, order := range orders {
		if order.Stage == core.StageReview {
			seats = append(seats, order)
		}
	}
	return seats
}

func rateRiskOrder(t *testing.T, f evidenceSubmissionFixture, id string, tier riskpolicy.Tier) {
	t.Helper()
	claim := core.WorkOrderClaim{ClaimantID: "run:risk-agent", SessionID: "risk-agent", ClientToken: "risk-agent-token", Agent: "claude-code", Model: "risk-model", Lease: time.Hour, ExecutionTimeout: time.Hour}
	if _, err := storetest.For(f.st).ClaimWorkOrder(f.ctx, id, claim); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.ExecuteWorkOrder(f.ctx, f.st, f.task.ID, core.WorkOrderCmdSubmitRiskRating, func(lease taskops.TaskLease) (core.RiskRating, error) {
		return f.st.CompleteRiskOrderCommand(f.ctx, lease, store.RiskOrderCompleteRequest{WorkOrderID: id,
			Claim: core.WorkOrderClaimIdentity{ClaimantID: claim.ClaimantID, SessionID: claim.SessionID}, Tier: tier, Reason: "rated " + string(tier)})
	}); err != nil {
		t.Fatal(err)
	}
}

// TestSubmissionCreatesHeadRiskOrder proves that a submission for a task
// whose frozen mode is record or enforce creates one risk order for the head
// and its pull request base, names it as risk_work_order_id, cancels an open
// risk order of an earlier head, and that a replay names the same order and
// creates none; mode off and a head rated high get no order
// (req-risk-based-review AC-1.1, AC-5.8, AC-5.11, AC-12.3, AC-12.6;
// feature-risk-based-review RR-12).
func TestSubmissionCreatesHeadRiskOrder(t *testing.T) {
	head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	for _, tc := range []struct {
		name        string
		mode        riskpolicy.Mode
		declaration riskpolicy.DeclarationInput
		wantOrder   bool
	}{
		{name: "Record", mode: riskpolicy.ModeRecord, declaration: riskpolicy.DeclarationInput{State: riskpolicy.DeclarationAbsent}, wantOrder: true},
		{name: "Enforce", mode: riskpolicy.ModeEnforce, declaration: riskpolicy.DeclarationInput{State: riskpolicy.DeclarationAbsent}, wantOrder: true},
		{name: "HighPathMinimum", mode: riskpolicy.ModeEnforce, declaration: riskpolicy.DeclarationInput{State: riskpolicy.DeclarationPresent, Bytes: []byte("schema_version: 2\n")}},
		{name: "Off", declaration: riskpolicy.DeclarationInput{State: riskpolicy.DeclarationAbsent}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := documentationSubmission(t, "absent", "", []string{"internal/server.go"}, withRiskPolicy(tc.mode))
			f.service.DiscoverRiskDeclaration = func(context.Context, string, string) riskpolicy.DeclarationInput { return tc.declaration }
			earlier := ""
			if tc.mode != "" {
				order, err := taskops.ExecuteWorkOrder(f.ctx, f.st, f.task.ID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (core.WorkOrder, error) {
					order, _, err := f.st.CreateRiskOrderCommand(f.ctx, lease, store.RiskOrderCreateRequest{TaskID: f.task.ID,
						Subject: core.RiskOrderSubject{HeadSHA: strings.Repeat("c", 40), BaseSHA: base}, ChangedPaths: []string{"internal/old.go"}})
					return order, err
				})
				if err != nil {
					t.Fatal(err)
				}
				earlier = order.ID
			}
			result, err := f.service.SubmitForReview(f.ctx, f.job.ID, "implementer", head)
			if err != nil {
				t.Fatalf("submission refused: %v", err)
			}
			id, named := result["risk_work_order_id"].(string)
			if named != tc.wantOrder {
				t.Fatalf("risk_work_order_id named=%t (%v), want %t", named, result["risk_work_order_id"], tc.wantOrder)
			}
			if earlier != "" {
				order, getErr := f.st.GetWorkOrder(f.ctx, earlier)
				if getErr != nil || order.State != core.WorkOrderCancelled {
					t.Fatalf("earlier head risk order=%+v err=%v, want cancelled", order, getErr)
				}
			}
			if !tc.wantOrder {
				for _, order := range taskRiskOrders(t, f) {
					if order.Subject.HeadSHA == head {
						t.Fatalf("head got a risk order %+v", order)
					}
				}
				return
			}
			current, ok := store.HeadRiskOrderFor(taskRiskOrders(t, f), head)
			if !ok || current.Order.ID != id || current.Subject.BaseSHA != base || current.Order.State != core.WorkOrderQueued ||
				len(current.ChangedPaths) != 1 || current.ChangedPaths[0] != "internal/server.go" {
				t.Fatalf("head risk order=%+v ok=%t, want %s at base %s", current, ok, id, base)
			}
			count := len(taskRiskOrders(t, f))
			replay, err := f.service.SubmitForReview(f.ctx, f.job.ID, "implementer", head)
			if err != nil || replay["replayed"] != true || replay["risk_work_order_id"] != id || len(taskRiskOrders(t, f)) != count {
				t.Fatalf("replay=%+v err=%v orders=%d, want the same order and no new one", replay, err, len(taskRiskOrders(t, f)))
			}
		})
	}
}

// TestRoundWaitsForTheHeadRiskOrder proves that an enforcing task's review
// dispatch opens no round while the head's risk order is queued or claimed,
// and that after the risk agent's rating raises the tier the round opens with
// the tier's agent reviews and source risk, above a declared count that the
// submission admitted. A record task's round opens at once at its declared
// count (req-risk-based-review AC-1.6, AC-5.10, AC-7.1;
// req-review-gates-evidence AC-2.3; feature-risk-based-review RR-12).
func TestRoundWaitsForTheHeadRiskOrder(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, mode := range []riskpolicy.Mode{riskpolicy.ModeRecord, riskpolicy.ModeEnforce} {
		t.Run(string(mode), func(t *testing.T) {
			f := documentationSubmission(t, "absent", "", []string{"internal/server.go"}, withRiskTable(mode, raisingTable()))
			f.service.DiscoverRiskDeclaration = func(context.Context, string, string) riskpolicy.DeclarationInput {
				return riskpolicy.DeclarationInput{State: riskpolicy.DeclarationAbsent}
			}
			declared := 2
			result, err := f.service.SubmitForReviewWithReviewSeats(f.ctx, f.job.ID, "implementer", head, &declared)
			if err != nil {
				t.Fatalf("submission refused: %v", err)
			}
			riskID, _ := result["risk_work_order_id"].(string)
			if riskID == "" {
				t.Fatalf("submission result %+v names no risk order", result)
			}
			for range 2 {
				if err = f.service.Dispatcher.DispatchNow(f.ctx, f.task.ID); err != nil {
					t.Fatal(err)
				}
			}
			seats := reviewSeatOrders(t, f)
			if mode == riskpolicy.ModeRecord {
				// Under record no round builder reads the risk order.
				if len(seats) != declared {
					t.Fatalf("record round seats=%d, want the declared %d", len(seats), declared)
				}
				return
			}
			if len(seats) != 0 {
				t.Fatalf("enforcing round opened %d seats while its head's risk order is queued", len(seats))
			}
			records, err := f.st.ListReviewRoundSeats(f.ctx, f.task.ID)
			if err != nil || len(records) != 0 {
				t.Fatalf("held round recorded seats %+v err=%v", records, err)
			}
			rateRiskOrder(t, f, riskID, riskpolicy.TierMedium)
			if err = f.service.Dispatcher.DispatchNow(f.ctx, f.task.ID); err != nil {
				t.Fatal(err)
			}
			if seats = reviewSeatOrders(t, f); len(seats) != 3 {
				t.Fatalf("released round seats=%d, want the medium tier's 3", len(seats))
			}
			records, err = f.st.ListReviewRoundSeats(f.ctx, f.task.ID)
			if err != nil || len(records) != 1 || records[0].SeatCountSource != store.ReviewSeatSourceRisk || records[0].ImplementationWorkOrderID != f.job.ID {
				t.Fatalf("round record=%+v err=%v, want source risk naming the implement order", records, err)
			}
			for _, seat := range seats {
				if seat.HeadSHA != head {
					t.Fatalf("seat %s reviews head %s, want %s", seat.ID, seat.HeadSHA, head)
				}
			}
		})
	}
}

// verifyRiskTaskStore freezes verify_stage on and an enforcing risk policy
// into the evidence fixture's task.
type verifyRiskTaskStore struct{ riskTaskStore }

func (s verifyRiskTaskStore) CreateTask(ctx context.Context, task core.Task) error {
	if task.ID == "evidence-task" {
		task.SetupContract.VerifyStage = true
		task.SetupContract.StageTimeouts.Verify = "1h"
	}
	return s.riskTaskStore.CreateTask(ctx, task)
}

// TestVerifyAndRiskOrdersAreClaimableTogether proves that with verify_stage
// on, the submitted head's risk order and its verify order are both claimable
// and are claimed side by side: risk is no serial stage
// (feature-risk-based-review RR-12).
func TestVerifyAndRiskOrdersAreClaimableTogether(t *testing.T) {
	head := strings.Repeat("a", 40)
	f := documentationSubmission(t, "absent", "", []string{"internal/server.go"}, func(st store.Store) store.Store {
		return verifyRiskTaskStore{riskTaskStore{Store: st, policy: &riskpolicy.Policy{Mode: riskpolicy.ModeEnforce, Tiers: riskpolicy.DefaultTable()}}}
	})
	f.service.DiscoverRiskDeclaration = func(context.Context, string, string) riskpolicy.DeclarationInput {
		return riskpolicy.DeclarationInput{State: riskpolicy.DeclarationAbsent}
	}
	result, err := f.service.SubmitForReview(f.ctx, f.job.ID, "implementer", head)
	if err != nil {
		t.Fatalf("submission refused: %v", err)
	}
	riskID, _ := result["risk_work_order_id"].(string)
	if riskID == "" || result["next_stage"] != core.StageVerify {
		t.Fatalf("submission result=%+v, want a risk order and the verify stage", result)
	}
	if err = f.service.Dispatcher.DispatchNow(f.ctx, f.task.ID); err != nil {
		t.Fatal(err)
	}
	orders, err := f.st.ListTaskWorkOrders(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	verifyID := ""
	for _, order := range orders {
		if order.Stage == core.StageVerify && order.State == core.WorkOrderQueued {
			verifyID = order.ID
		}
	}
	if verifyID == "" {
		t.Fatalf("no queued verify order beside the risk order: %+v", orders)
	}
	for _, id := range []string{riskID, verifyID} {
		order, getErr := f.st.GetWorkOrder(f.ctx, id)
		if getErr != nil || !order.Claimable {
			t.Fatalf("order %s=%+v err=%v, want claimable", id, order, getErr)
		}
	}
	for i, id := range []string{riskID, verifyID} {
		session := []string{"risk-agent", "verifier"}[i]
		if _, err = storetest.For(f.st).ClaimWorkOrder(f.ctx, id, core.WorkOrderClaim{ClaimantID: "run:" + session, SessionID: session, ClientToken: session + "-token",
			Lease: time.Hour, ExecutionTimeout: time.Hour}); err != nil {
			t.Fatalf("claim %s: %v", id, err)
		}
	}
}
