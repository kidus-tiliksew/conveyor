package workorder

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// riskTaskStore freezes a risk policy into the evidence fixture's task as the
// task is created, so the GitHub-backed submission fixture serves a task whose
// frozen risk mode is record or enforce.
type riskTaskStore struct {
	store.Store
	policy *riskpolicy.Policy
}

func (s riskTaskStore) CreateTask(ctx context.Context, task core.Task) error {
	if task.ID == "evidence-task" && s.policy != nil {
		policy := *s.policy
		task.SetupContract.Risk = &policy
	}
	return s.Store.CreateTask(ctx, task)
}

func withRiskPolicy(mode riskpolicy.Mode) func(store.Store) store.Store {
	return func(st store.Store) store.Store {
		if mode == "" {
			return st
		}
		return riskTaskStore{Store: st, policy: &riskpolicy.Policy{Mode: mode, Tiers: riskpolicy.DefaultTable()}}
	}
}

func riskEventIndex(t *testing.T, st store.Store, ctx context.Context, taskID, kind string) int {
	t.Helper()
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	for i, event := range events {
		if event.Kind == kind {
			return i
		}
	}
	return -1
}

// TestSubmissionRecordsPathRiskAtBase proves a submitted head's path minimums
// are read at the pull request's base SHA and recorded before the first
// handoff write; a malformed declaration gives high and never refuses the
// submission; a head already rated reads nothing again; and mode off makes no
// declaration read (req-risk-based-review AC-1.1, AC-3.2, AC-3.3, AC-3.5,
// AC-5.1; feature-risk-based-review RR-2, RR-3).
func TestSubmissionRecordsPathRiskAtBase(t *testing.T) {
	head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	for _, tc := range []struct {
		name        string
		mode        riskpolicy.Mode
		declaration riskpolicy.DeclarationInput
		preRated    bool
		wantTier    riskpolicy.Tier
		wantReason  string
		wantReads   int
	}{
		{name: "WorkflowPathIsMedium", mode: riskpolicy.ModeRecord, declaration: riskpolicy.DeclarationInput{State: riskpolicy.DeclarationAbsent},
			wantTier: riskpolicy.TierMedium, wantReason: "GitHub Actions workflows (medium)", wantReads: 1},
		{name: "MalformedDeclarationIsHigh", mode: riskpolicy.ModeEnforce, declaration: riskpolicy.DeclarationInput{State: riskpolicy.DeclarationPresent, Bytes: []byte("schema_version: 2\n")},
			wantTier: riskpolicy.TierHigh, wantReason: "declaration is malformed", wantReads: 1},
		{name: "PreRatedHeadReadsNothing", mode: riskpolicy.ModeRecord, declaration: riskpolicy.DeclarationInput{State: riskpolicy.DeclarationAbsent},
			preRated: true, wantTier: riskpolicy.TierLow, wantReason: "recorded before this submission", wantReads: 0},
		{name: "ModeOffReadsNothing", declaration: riskpolicy.DeclarationInput{State: riskpolicy.DeclarationAbsent}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := documentationSubmission(t, "absent", "", []string{".github/workflows/ci.yml", "internal/server.go"}, withRiskPolicy(tc.mode))
			reads := 0
			f.service.DiscoverRiskDeclaration = func(_ context.Context, slug, revision string) riskpolicy.DeclarationInput {
				reads++
				if slug != "acme/app" || revision != base {
					t.Fatalf("declaration read at %s@%s, want the pull request base %s", slug, revision, base)
				}
				return tc.declaration
			}
			if tc.preRated {
				if _, _, err := f.st.RecordRiskRating(f.ctx, core.RiskRating{TaskID: f.task.ID, Key: core.PathRiskKey(head), Source: core.RiskSourcePathRule,
					Tier: riskpolicy.TierLow, HeadSHA: head, BaseSHA: base, Reason: "recorded before this submission"}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.service.SubmitForReview(f.ctx, f.job.ID, "implementer", head); err != nil {
				t.Fatalf("submission refused: %v", err)
			}
			ratings, err := f.st.ListTaskRiskRatings(f.ctx, f.task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.mode == "" {
				if reads != 0 || len(ratings) != 0 || riskEventIndex(t, f.st, f.ctx, f.task.ID, core.RiskRatedEvent) >= 0 {
					t.Fatalf("mode off: declaration reads=%d ratings=%+v", reads, ratings)
				}
				return
			}
			if reads != tc.wantReads || len(ratings) != 1 || ratings[0].HeadSHA != head || ratings[0].Tier != tc.wantTier || !strings.Contains(ratings[0].Reason, tc.wantReason) {
				t.Fatalf("declaration reads=%d ratings=%+v, want one %s rating with reason %q", reads, ratings, tc.wantTier, tc.wantReason)
			}
			rated, opened := riskEventIndex(t, f.st, f.ctx, f.task.ID, core.RiskRatedEvent), riskEventIndex(t, f.st, f.ctx, f.task.ID, "pull_request.opened")
			if rated < 0 || opened < 0 || rated > opened {
				t.Fatalf("risk.rated at %d, pull_request.opened at %d; the head must be rated before the handoff", rated, opened)
			}
		})
	}
}

// TestSubmissionUnreadPathListGivesHigh proves a changed-path list that cannot
// be read gives high with a recorded reason and never refuses the submission,
// on a repository whose submission needs no other path read
// (req-risk-based-review AC-5.4; component-risk-review invariant 7).
func TestSubmissionUnreadPathListGivesHigh(t *testing.T) {
	policy := riskpolicy.Policy{Mode: riskpolicy.ModeRecord, Tiers: riskpolicy.DefaultTable()}
	f := newReviewSeatsFixture(t, "risk-unread", config.PolicyContract{Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, 1)}, Risk: &policy}, 0)
	pathReads := 0
	f.service.SubmissionChangedPaths = func(context.Context, *config.Config, core.Task) ([]string, error) {
		pathReads++
		return nil, errors.New("compare lists 300 or more files")
	}
	if _, err := f.submit(nil); err != nil {
		t.Fatalf("submission refused for a risk read: %v", err)
	}
	ratings, err := f.st.ListTaskRiskRatings(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pathReads != 1 || len(ratings) != 1 || ratings[0].Tier != riskpolicy.TierHigh ||
		!strings.Contains(ratings[0].Reason, "changed paths could not be read in full: compare lists 300 or more files") {
		t.Fatalf("path reads=%d ratings=%+v", pathReads, ratings)
	}
}

// riskVerdictFixture submits the implement order and opens a review round of
// seats seats on a task whose frozen risk mode is mode.
func riskVerdictFixture(t *testing.T, name string, mode riskpolicy.Mode, seats int) (*reviewSeatsFixture, []core.WorkOrder, *int, *int) {
	t.Helper()
	setup := config.PolicyContract{Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, seats)}}
	if mode != "" {
		setup.Risk = &riskpolicy.Policy{Mode: mode, Tiers: riskpolicy.DefaultTable()}
	}
	f := newReviewSeatsFixture(t, name, setup, 0)
	reads, pathReads := 0, 0
	f.service.DiscoverRiskDeclaration = func(context.Context, string, string) riskpolicy.DeclarationInput {
		reads++
		return riskpolicy.DeclarationInput{State: riskpolicy.DeclarationAbsent}
	}
	f.service.SubmissionChangedPaths = func(context.Context, *config.Config, core.Task) ([]string, error) {
		pathReads++
		return []string{"internal/server.go"}, nil
	}
	if _, err := f.submit(nil); err != nil {
		t.Fatal(err)
	}
	if err := f.dispatcher.DispatchNow(f.ctx, f.task.ID); err != nil {
		t.Fatal(err)
	}
	orders, _ := f.round(t, 1)
	for _, order := range orders {
		if _, err := f.service.Claim(f.ctx, order.ID, core.WorkOrderClaim{SessionID: "risk-review-" + order.ID, ClientToken: "token-" + order.ID, ClaimantID: "reviewer-" + order.ID,
			OwnerUserID: "usr-reviewer", Agent: "codex", Model: "reviewer-model-" + order.ID[len(order.ID)-1:], Lease: time.Hour}); err != nil {
			t.Fatal(err)
		}
	}
	return f, orders, &reads, &pathReads
}

// riskVerdict builds a changes-requested verdict: the fixture's repository has
// no forge, so a settling approval would try an automatic merge.
func riskVerdict() pipeline.Review {
	applicable, citable := false, false
	return pipeline.Review{Verdict: "changes_requested", ReasonCode: "tests", Summary: "risk review", Feedback: "add a test",
		GovernanceAssessment: &core.GovernanceAssessment{DesignApplicable: &applicable, DecisionCitable: &citable, CitedIDs: []string{}, UnknownIDs: []string{}, UngovernedIDs: []string{}, SupersededIDs: []string{}, Conflicts: []string{}}}
}

// TestVerdictStatesNoTierInAnyMode proves a verdict states no tier: under
// enforce, record, and off a verdict without one is accepted, records no
// rating and no risk.rated event, adds no seat, and its review.completed
// payload carries no tier (req-risk-based-review AC-1.1, AC-5.7, AC-7.2;
// feature-risk-based-review RR-14).
func TestVerdictStatesNoTierInAnyMode(t *testing.T) {
	for _, mode := range []riskpolicy.Mode{riskpolicy.ModeEnforce, riskpolicy.ModeRecord, ""} {
		t.Run("Mode"+string(mode), func(t *testing.T) {
			f, orders, _, _ := riskVerdictFixture(t, "risk-verdict-"+string(mode), mode, 1)
			seat := orders[0]
			before, err := f.st.ListTaskRiskRatings(f.ctx, f.task.ID)
			if err != nil {
				t.Fatal(err)
			}
			seatsBefore, err := f.st.ListReviewRoundSeats(f.ctx, f.task.ID)
			if err != nil || len(seatsBefore) != 1 {
				t.Fatalf("mode %q: seat records=%+v err=%v, want one round", mode, seatsBefore, err)
			}
			ordersBefore := len(orders)
			rated := 0
			events, _ := f.st.ListEvents(f.ctx, f.task.ID)
			for _, event := range events {
				if event.Kind == core.RiskRatedEvent {
					rated++
				}
			}
			if _, err := f.service.SubmitVerdict(f.ctx, seat.ID, "risk-review-"+seat.ID, riskVerdict()); err != nil {
				t.Fatalf("mode %q refused a verdict without a tier: %v", mode, err)
			}
			after, err := f.st.ListTaskRiskRatings(f.ctx, f.task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(after) != len(before) {
				t.Fatalf("mode %q: verdict changed ratings from %+v to %+v", mode, before, after)
			}
			events, _ = f.st.ListEvents(f.ctx, f.task.ID)
			completed, ratedAfter, added := 0, 0, 0
			for _, event := range events {
				switch event.Kind {
				case core.RiskRatedEvent:
					ratedAfter++
				case "review.seats_added":
					added++
				case "review.completed":
					completed++
					if strings.Contains(string(event.Payload), "risk_tier") || strings.Contains(string(event.Payload), "risk_reason") {
						t.Fatalf("mode %q: review.completed carries a tier: %s", mode, event.Payload)
					}
				}
			}
			if completed != 1 || ratedAfter != rated || added != 0 {
				t.Fatalf("mode %q: review.completed=%d risk.rated %d->%d seats_added=%d", mode, completed, rated, ratedAfter, added)
			}
			records, err := f.st.ListReviewRoundSeats(f.ctx, f.task.ID)
			if err != nil || len(records) != 1 || records[0].SeatCount != seatsBefore[0].SeatCount {
				t.Fatalf("mode %q: seat records=%+v err=%v, want the round's %d seats unchanged", mode, records, err, seatsBefore[0].SeatCount)
			}
			if after, _ := f.round(t, 1); len(after) != ordersBefore {
				t.Fatalf("mode %q: round has %d seats after the verdict, want %d", mode, len(after), ordersBefore)
			}
		})
	}
}

// riskFlowSnapshot is every surface a record-mode rating must leave as mode
// off leaves it.
type riskFlowSnapshot struct {
	State      core.TaskState
	NextStage  core.Stage
	PlanState  core.PlanState
	GateKind   string
	Approved   bool
	SeatCounts []int
	Sources    []string
	OrderState []core.WorkOrderState
}

func riskFlow(t *testing.T, name string, mode riskpolicy.Mode) riskFlowSnapshot {
	t.Helper()
	f, orders, _, _ := riskVerdictFixture(t, name, mode, 2)
	for _, order := range orders {
		if _, err := f.service.SubmitVerdict(f.ctx, order.ID, "risk-review-"+order.ID, riskVerdict()); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.dispatcher.DispatchNow(f.ctx, f.task.ID); err != nil {
		t.Fatal(err)
	}
	task, err := f.st.GetTask(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	records, err := f.st.ListReviewRoundSeats(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	all, err := f.st.ListTaskWorkOrders(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := riskFlowSnapshot{State: task.State, NextStage: task.NextStage, PlanState: task.PlanState, GateKind: string(task.GateKind), Approved: task.ApprovedHeadSHA != ""}
	for _, record := range records {
		snapshot.SeatCounts = append(snapshot.SeatCounts, record.SeatCount)
		snapshot.Sources = append(snapshot.Sources, string(record.SeatCountSource))
	}
	for _, order := range all {
		snapshot.OrderState = append(snapshot.OrderState, order.State)
	}
	return snapshot
}

// TestRecordModeChangesNoFlow proves record-mode ratings change no stage,
// plan state, seat count, gate, order, or approval: a task with mode record
// reaches the same state as the same flow with mode off
// (req-risk-based-review AC-1.6, AC-2.2, AC-2.3).
func TestRecordModeChangesNoFlow(t *testing.T) {
	off := riskFlow(t, "risk-flow-off", "")
	record := riskFlow(t, "risk-flow-record", riskpolicy.ModeRecord)
	if !reflect.DeepEqual(off, record) {
		t.Fatalf("record mode changed the flow:\n off    %+v\n record %+v", off, record)
	}
	if off.NextStage != core.StageImplement || len(off.SeatCounts) != 1 || off.SeatCounts[0] != 2 || len(off.OrderState) != 4 || off.OrderState[3] != core.WorkOrderQueued {
		t.Fatalf("flow ended in %+v, want a bounce back to implement after a two-seat round", off)
	}
}

// TestReviewRoleCarriesNoRating: a review order's role asks for no tier in
// any risk mode (req-risk-based-review AC-5.7, AC-1.1;
// feature-risk-based-review RR-14).
func TestReviewRoleCarriesNoRating(t *testing.T) {
	for _, mode := range []riskpolicy.Mode{riskpolicy.ModeRecord, riskpolicy.ModeEnforce, ""} {
		f, orders, _, _ := riskVerdictFixture(t, "risk-role-"+string(mode), mode, 1)
		contract, err := f.service.contextForOrder(f.ctx, orders[0])
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"`risk_tier`", "`risk_reason`", "conveyor:risk-rating", "Risk rating ("} {
			if strings.Contains(contract.RolePrompt, forbidden) {
				t.Fatalf("mode %q: review role carries %q", mode, forbidden)
			}
		}
	}
}
