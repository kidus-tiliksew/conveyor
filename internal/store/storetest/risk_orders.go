package storetest

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// RunRiskOrderConformance proves on every backend the risk order's store
// contract: the risk stage and its subject round-trip, creation is idempotent
// per task and subject and skipped for an off or high task, completion refuses
// a missing tier or reason without changing the claim and otherwise records
// exactly one risk agent rating and its event, cancellation is idempotent and
// records no rating, a raise or rating to high cancels every open risk order
// in its own act, and a risk claim is refused for any session ID or client
// token that claimed a plan, implementation, or review order of the task,
// including released attempts (req-risk-based-review AC-4.3, AC-5.5, AC-5.6,
// AC-12.1 to AC-12.3; feature-risk-based-review RR-11, RR-15;
// component-risk-review invariant 12).
func RunRiskOrderConformance(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	record := riskpolicy.Policy{Mode: riskpolicy.ModeRecord, Tiers: riskpolicy.DefaultTable()}
	enforce := riskpolicy.Policy{Mode: riskpolicy.ModeEnforce, Tiers: riskpolicy.DefaultTable()}
	repo := "conveyor"
	if x.Config != nil && len(x.Config.Repos) > 0 {
		repo = x.Config.Repos[0].Name
	}
	fileTask := func(t *testing.T, policy *riskpolicy.Policy, next core.Stage) core.Task {
		t.Helper()
		id := core.NewTaskID()
		task := core.Task{ID: id, Workspace: x.Workspace, Repo: repo, Title: id, BaseBranch: "main", Branch: "conveyor/task-" + id,
			State: core.TaskRunning, NextStage: next, PolicyVersion: 1, CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
		task.SetupContract = config.PolicyContract{MaxBounces: 3, Risk: policy,
			StageTimeouts: config.PolicyStageTimeouts{Spec: "30m", Implement: "4h", Verify: "1h", Review: "45m"}}
		requireOK(t, st.CreateTask(ctx, task))
		stored, err := st.GetTask(ctx, id)
		requireOK(t, err)
		return stored
	}
	head := func(sha string) core.RiskOrderSubject {
		return core.RiskOrderSubject{HeadSHA: sha, BaseSHA: "base-" + sha}
	}
	rules := []riskpolicy.RuleMatch{{Source: riskpolicy.RuleBuiltin, Index: 0, Description: "GitHub Actions workflows", MinTier: riskpolicy.TierMedium, MinHumanApprovals: 1, Paths: []string{".github/workflows/ci.yml"}}}
	type created struct {
		order core.WorkOrder
		made  bool
	}
	create := func(t *testing.T, request store.RiskOrderCreateRequest) (core.WorkOrder, bool, error) {
		t.Helper()
		out, err := taskops.ExecuteWorkOrder(ctx, st, request.TaskID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (created, error) {
			order, made, err := st.CreateRiskOrderCommand(ctx, lease, request)
			return created{order, made}, err
		})
		return out.order, out.made, err
	}
	mustCreate := func(t *testing.T, request store.RiskOrderCreateRequest) core.WorkOrder {
		t.Helper()
		order, made, err := create(t, request)
		requireOK(t, err)
		if !made {
			t.Fatalf("risk order for %+v was not created: %+v", request.Subject, order)
		}
		return order
	}
	complete := func(t *testing.T, taskID string, request store.RiskOrderCompleteRequest) (core.RiskRating, error) {
		t.Helper()
		return taskops.ExecuteWorkOrder(ctx, st, taskID, core.WorkOrderCmdSubmitRiskRating, func(lease taskops.TaskLease) (core.RiskRating, error) {
			return st.CompleteRiskOrderCommand(ctx, lease, request)
		})
	}
	cancel := func(t *testing.T, taskID, id, reason string) (core.WorkOrder, bool, error) {
		t.Helper()
		out, err := taskops.ExecuteWorkOrder(ctx, st, taskID, core.WorkOrderCmdCancel, func(lease taskops.TaskLease) (created, error) {
			order, made, err := st.CancelRiskOrderCommand(ctx, lease, store.RiskOrderCancelRequest{WorkOrderID: id, Reason: reason})
			return created{order, made}, err
		})
		return out.order, out.made, err
	}
	claim := func(id, session, token string) (core.WorkOrder, error) {
		return ClaimWorkOrder(ctx, st, id, core.WorkOrderClaim{ClaimantID: "run:risk-owner", SessionID: session, ClientToken: token,
			Agent: "claude-code", Model: "model-" + session, Lease: time.Hour, ExecutionTimeout: time.Hour})
	}
	identity := func(session string) core.WorkOrderClaimIdentity {
		return core.WorkOrderClaimIdentity{ClaimantID: "run:risk-owner", SessionID: session}
	}
	ratings := func(t *testing.T, taskID string) []core.RiskRating {
		t.Helper()
		listed, err := st.ListTaskRiskRatings(ctx, taskID)
		requireOK(t, err)
		return listed
	}
	riskOrders := func(t *testing.T, taskID string) []core.RiskOrder {
		t.Helper()
		listed, err := st.ListTaskRiskOrders(ctx, taskID)
		requireOK(t, err)
		return listed
	}
	countEvents := func(t *testing.T, taskID, kind string) int {
		t.Helper()
		events, err := st.ListEvents(ctx, taskID)
		requireOK(t, err)
		n := 0
		for _, event := range events {
			if event.Kind == kind {
				n++
			}
		}
		return n
	}
	order := func(t *testing.T, id string) core.WorkOrder {
		t.Helper()
		got, err := st.GetWorkOrder(ctx, id)
		requireOK(t, err)
		return got
	}
	stageOrder := func(t *testing.T, taskID string, stage core.Stage) core.WorkOrder {
		t.Helper()
		id := core.NewTaskID()
		requireOK(t, st.CreateJob(ctx, core.Job{ID: id, TaskID: taskID, Stage: stage, State: core.JobPending}))
		now := time.Now().UTC()
		requireOK(t, CreateWorkOrder(ctx, st, core.WorkOrder{ID: id, TaskID: taskID, JobID: id, Stage: stage, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}))
		return order(t, id)
	}

	t.Run("CreateIsIdempotentPerTaskAndSubject", func(t *testing.T) {
		task := fileTask(t, &record, core.StageReview)
		before := time.Now().UTC()
		first := mustCreate(t, store.RiskOrderCreateRequest{TaskID: task.ID, Subject: head("h1"), ChangedPaths: []string{".github/workflows/ci.yml", "a.go"}, MatchedRules: rules, QueueTimeout: 2 * time.Hour})
		if first.ID != core.RiskOrderID(task.ID, 1) || first.JobID != first.ID || first.Stage != core.StageRisk || first.State != core.WorkOrderQueued ||
			first.HeadSHA != "h1" || first.BaselineSHA != "base-h1" || first.ExecutionTimeoutText != "45m" {
			t.Fatalf("first risk order=%+v", first)
		}
		if deadline := first.QueueDeadline.Sub(first.QueueEnteredAt); deadline != 2*time.Hour || first.QueueEnteredAt.Before(before.Add(-time.Second)) {
			t.Fatalf("queue clock entered=%s deadline=%s, want the workspace queue timeout", first.QueueEnteredAt, first.QueueDeadline)
		}
		stored := order(t, first.ID)
		if stored.Stage != core.StageRisk || stored.State != core.WorkOrderQueued || !stored.Claimable || stored.ExecutionTimeoutText != "45m" {
			t.Fatalf("stored risk order=%+v", stored)
		}
		replay, made, err := create(t, store.RiskOrderCreateRequest{TaskID: task.ID, Subject: head("h1"), ChangedPaths: []string{"other.go"}})
		requireOK(t, err)
		if made || replay.ID != first.ID {
			t.Fatalf("replayed subject made=%t order=%s, want %s", made, replay.ID, first.ID)
		}
		plan := mustCreate(t, store.RiskOrderCreateRequest{TaskID: task.ID, Subject: core.RiskOrderSubject{PlanVersion: 2}})
		otherBase := mustCreate(t, store.RiskOrderCreateRequest{TaskID: task.ID, Subject: core.RiskOrderSubject{HeadSHA: "h1", BaseSHA: "base-two"}, ChangedPaths: []string{}})
		if plan.ID != core.RiskOrderID(task.ID, 2) || plan.HeadSHA != "" || otherBase.ID != core.RiskOrderID(task.ID, 3) {
			t.Fatalf("plan order=%+v other base=%+v", plan, otherBase)
		}
		listed := riskOrders(t, task.ID)
		if len(listed) != 3 {
			t.Fatalf("risk orders=%+v, want 3", listed)
		}
		if listed[0].Order.ID != first.ID || listed[0].Subject != head("h1") || !slices.Equal(listed[0].ChangedPaths, []string{".github/workflows/ci.yml", "a.go"}) ||
			len(listed[0].MatchedRules) != 1 || listed[0].MatchedRules[0].Description != "GitHub Actions workflows" || !slices.Equal(listed[0].MatchedRules[0].Paths, rules[0].Paths) {
			t.Fatalf("head risk order did not round-trip: %+v", listed[0])
		}
		if listed[1].Subject.PlanVersion != 2 || listed[1].Subject.Kind() != riskpolicy.RiskSubjectPlan || len(listed[1].ChangedPaths) != 0 || listed[1].ChangedPaths == nil {
			t.Fatalf("plan risk order did not round-trip: %+v", listed[1])
		}
		if listed[2].Subject.BaseSHA != "base-two" || listed[2].ChangedPaths == nil || len(listed[2].ChangedPaths) != 0 {
			t.Fatalf("known-empty head paths did not round-trip: %+v", listed[2])
		}
		if got := countEvents(t, task.ID, "work_order.created"); got != 3 {
			t.Fatalf("work_order.created events=%d, want 3", got)
		}
		// A risk order changes neither the task's state nor its next stage.
		after, err := st.GetTask(ctx, task.ID)
		requireOK(t, err)
		if after.State != task.State || after.NextStage != task.NextStage || after.PlanState != task.PlanState {
			t.Fatalf("risk order creation moved the task: %+v", after)
		}
		for _, invalid := range []store.RiskOrderCreateRequest{
			{TaskID: task.ID, Subject: core.RiskOrderSubject{PlanVersion: 3, HeadSHA: "h9", BaseSHA: "b9"}},
			{TaskID: task.ID, Subject: core.RiskOrderSubject{HeadSHA: "h9"}},
			{TaskID: task.ID},
			{TaskID: task.ID, Subject: core.RiskOrderSubject{PlanVersion: 3}, ChangedPaths: []string{"a.go"}},
			{TaskID: task.ID, Subject: head("h9"), ChangedPaths: []string{" "}},
		} {
			if _, made, err := create(t, invalid); err == nil || made {
				t.Fatalf("invalid request %+v was accepted", invalid)
			}
		}
		if got := len(riskOrders(t, task.ID)); got != 3 {
			t.Fatalf("refused requests left %d risk orders, want 3", got)
		}
	})

	t.Run("OffAndHighTasksGetNoOrder", func(t *testing.T) {
		off := fileTask(t, nil, core.StageReview)
		got, made, err := create(t, store.RiskOrderCreateRequest{TaskID: off.ID, Subject: head("off")})
		requireOK(t, err)
		if made || got.ID != "" || len(riskOrders(t, off.ID)) != 0 {
			t.Fatalf("off task created %+v made=%t", got, made)
		}
		task := fileTask(t, &record, core.StageReview)
		early := mustCreate(t, store.RiskOrderCreateRequest{TaskID: task.ID, Subject: head("early")})
		_, err = st.RaiseTaskRiskTier(ctx, core.RiskRating{TaskID: task.ID, Tier: riskpolicy.TierHigh, Reason: "touches billing", ActorID: "user:operator"}, store.RiskSeatSettings{})
		requireOK(t, err)
		got, made, err = create(t, store.RiskOrderCreateRequest{TaskID: task.ID, Subject: head("late")})
		requireOK(t, err)
		if made || got.ID != "" {
			t.Fatalf("high task created %+v made=%t", got, made)
		}
		replay, made, err := create(t, store.RiskOrderCreateRequest{TaskID: task.ID, Subject: head("early")})
		requireOK(t, err)
		if made || replay.ID != early.ID || replay.State != core.WorkOrderCancelled {
			t.Fatalf("replay on a high task=%+v made=%t, want the cancelled earlier order", replay, made)
		}
	})

	t.Run("CompletionRefusalLeavesOrderClaimed", func(t *testing.T) {
		task := fileTask(t, &record, core.StageReview)
		risk := mustCreate(t, store.RiskOrderCreateRequest{TaskID: task.ID, Subject: head("hr"), ChangedPaths: []string{"a.go"}})
		claimed, err := claim(risk.ID, "rater", "rater-token")
		requireOK(t, err)
		eventsBefore, err := st.ListEvents(ctx, task.ID)
		requireOK(t, err)
		for _, request := range []store.RiskOrderCompleteRequest{
			{WorkOrderID: risk.ID, Claim: identity("rater"), Reason: "no tier"},
			{WorkOrderID: risk.ID, Claim: identity("rater"), Tier: "critical", Reason: "unknown tier"},
			{WorkOrderID: risk.ID, Claim: identity("rater"), Tier: riskpolicy.TierLow},
			{WorkOrderID: risk.ID, Claim: identity("rater"), Tier: riskpolicy.TierLow, Reason: "  "},
			{WorkOrderID: risk.ID, Claim: identity("rater"), Tier: riskpolicy.TierLow, Reason: "two\nlines"},
			{WorkOrderID: risk.ID, Claim: identity("rater"), Tier: riskpolicy.TierLow, Reason: strings.Repeat("x", core.MaxRiskReasonRunes+1)},
		} {
			if _, err := complete(t, task.ID, request); err == nil {
				t.Fatalf("completion %+v was accepted", request)
			}
		}
		if _, err := complete(t, task.ID, store.RiskOrderCompleteRequest{WorkOrderID: risk.ID, Claim: identity("someone-else"), Tier: riskpolicy.TierLow, Reason: "not mine"}); !errors.Is(err, store.ErrWorkOrderClaimLost) {
			t.Fatalf("completion by another session err=%v, want claim lost", err)
		}
		eventsAfter, err := st.ListEvents(ctx, task.ID)
		requireOK(t, err)
		still := order(t, risk.ID)
		if len(eventsAfter) != len(eventsBefore) || len(ratings(t, task.ID)) != 0 || still.State != core.WorkOrderClaimed ||
			still.SessionID != "rater" || still.AttemptID != claimed.AttemptID || !still.LeaseExpiresAt.Equal(claimed.LeaseExpiresAt) {
			t.Fatalf("refused completions changed the order %+v or wrote %d events", still, len(eventsAfter)-len(eventsBefore))
		}
	})

	t.Run("CompletionRecordsOneRiskAgentRating", func(t *testing.T) {
		task := fileTask(t, &record, core.StageReview)
		_, _, err := st.RecordRiskRating(ctx, core.RiskRating{TaskID: task.ID, Key: core.PathRiskKey("hc"), Source: core.RiskSourcePathRule, Tier: riskpolicy.TierMedium,
			HeadSHA: "hc", BaseSHA: "base-hc", DeclarationState: riskpolicy.DeclarationAbsent, Reason: "matched path rules: GitHub Actions workflows (medium)"})
		requireOK(t, err)
		risk := mustCreate(t, store.RiskOrderCreateRequest{TaskID: task.ID, Subject: head("hc"), ChangedPaths: []string{"b.go", ".github/workflows/ci.yml"}, MatchedRules: rules})
		_, err = claim(risk.ID, "rater-c", "rater-c-token")
		requireOK(t, err)
		rating, err := complete(t, task.ID, store.RiskOrderCompleteRequest{WorkOrderID: risk.ID, Claim: identity("rater-c"), Tier: riskpolicy.TierLow, Reason: "a small handler change"})
		requireOK(t, err)
		if rating.Source != core.RiskSourceRiskAgent || rating.Key != core.RiskAgentKey(risk.ID) || rating.RiskWorkOrderID != risk.ID || rating.RiskModel != "model-rater-c" ||
			rating.HeadSHA != "hc" || rating.BaseSHA != "base-hc" || rating.Tier != riskpolicy.TierLow || rating.Seq != 2 ||
			!slices.Equal(rating.CoveredPaths, []string{"b.go", ".github/workflows/ci.yml"}) || len(rating.MatchedRules) != 1 {
			t.Fatalf("risk agent rating=%+v", rating)
		}
		if done := order(t, risk.ID); done.State != core.WorkOrderCompleted || done.Claimable {
			t.Fatalf("completed risk order=%+v", done)
		}
		listed := ratings(t, task.ID)
		if len(listed) != 2 || listed[1].Key != rating.Key || listed[1].RiskModel != "model-rater-c" || !slices.Equal(listed[1].CoveredPaths, rating.CoveredPaths) || listed[1].PlanPending {
			t.Fatalf("listed ratings=%+v", listed)
		}
		// A lower risk agent rating never lowers the tier (AC-5.5).
		if setter, ok := core.RiskTierOf(listed); !ok || setter.Tier != riskpolicy.TierMedium || setter.Source != core.RiskSourcePathRule {
			t.Fatalf("tier setter=%+v, want medium from the path rule", setter)
		}
		events, err := st.ListEvents(ctx, task.ID)
		requireOK(t, err)
		var rated []map[string]any
		for _, event := range events {
			if event.Kind == core.RiskRatedEvent {
				var payload map[string]any
				requireOK(t, json.Unmarshal(event.Payload, &payload))
				rated = append(rated, payload)
			}
			if strings.Contains(string(event.Payload), "client_token") || strings.Contains(string(event.Payload), "rater-c-token") {
				t.Fatalf("event %s carries the client token: %s", event.Kind, event.Payload)
			}
		}
		if len(rated) != 2 || rated[1]["source"] != "risk_agent" || rated[1]["risk_work_order_id"] != risk.ID || rated[1]["risk_model"] != "model-rater-c" ||
			rated[1]["head_sha"] != "hc" || rated[1]["base_sha"] != "base-hc" {
			t.Fatalf("risk.rated payloads=%v", rated)
		}
		if covered, ok := rated[1]["covered_paths"].([]any); !ok || len(covered) != 2 {
			t.Fatalf("risk.rated covered_paths=%v", rated[1]["covered_paths"])
		}
		// A replay by the completing session returns the recorded rating and
		// writes nothing.
		replay, err := complete(t, task.ID, store.RiskOrderCompleteRequest{WorkOrderID: risk.ID, Claim: identity("rater-c"), Tier: riskpolicy.TierHigh, Reason: "a different answer"})
		requireOK(t, err)
		if replay.Key != rating.Key || replay.Tier != riskpolicy.TierLow || countEvents(t, task.ID, core.RiskRatedEvent) != 2 || len(ratings(t, task.ID)) != 2 {
			t.Fatalf("replay=%+v, want the recorded rating and no new write", replay)
		}
		if _, err := complete(t, task.ID, store.RiskOrderCompleteRequest{WorkOrderID: risk.ID, Claim: identity("other"), Tier: riskpolicy.TierHigh, Reason: "late"}); !errors.Is(err, store.ErrWorkOrderClaimLost) {
			t.Fatalf("completion of a completed order by another session err=%v", err)
		}
		snapshot := store.LatestCompletedHeadRiskPaths(riskOrders(t, task.ID))
		if snapshot == nil || !slices.Equal(*snapshot, []string{"b.go", ".github/workflows/ci.yml"}) {
			t.Fatalf("latest completed head paths=%v", snapshot)
		}
		if _, made, err := cancel(t, task.ID, risk.ID, "later head"); err != nil || made {
			t.Fatalf("cancel of a completed order made=%t err=%v", made, err)
		}
	})

	t.Run("CancelIsIdempotentAndRecordsNoRating", func(t *testing.T) {
		task := fileTask(t, &record, core.StageReview)
		risk := mustCreate(t, store.RiskOrderCreateRequest{TaskID: task.ID, Subject: head("hx")})
		_, err := claim(risk.ID, "rater-x", "rater-x-token")
		requireOK(t, err)
		if _, _, err := cancel(t, task.ID, risk.ID, "two\nlines"); err == nil {
			t.Fatal("a multi-line cancel reason was accepted")
		}
		cancelled, made, err := cancel(t, task.ID, risk.ID, "a later head was submitted")
		requireOK(t, err)
		if !made || cancelled.State != core.WorkOrderCancelled || cancelled.SessionID != "" || cancelled.Claimable {
			t.Fatalf("cancelled=%+v made=%t", cancelled, made)
		}
		again, made, err := cancel(t, task.ID, risk.ID, "a later head was submitted")
		requireOK(t, err)
		if made || again.State != core.WorkOrderCancelled || countEvents(t, task.ID, "work_order.cancelled") != 1 {
			t.Fatalf("replayed cancel made=%t order=%+v", made, again)
		}
		// The cancelled claim's late result records nothing.
		if _, err := complete(t, task.ID, store.RiskOrderCompleteRequest{WorkOrderID: risk.ID, Claim: identity("rater-x"), Tier: riskpolicy.TierLow, Reason: "late result"}); !errors.Is(err, store.ErrWorkOrderCancelled) {
			t.Fatalf("late completion err=%v, want cancelled", err)
		}
		if len(ratings(t, task.ID)) != 0 || store.LatestCompletedHeadRiskPaths(riskOrders(t, task.ID)) != nil {
			t.Fatal("a cancelled order supplied a rating or a rated-path snapshot")
		}
		if _, err := claim(risk.ID, "rater-y", "rater-y-token"); err == nil {
			t.Fatal("a cancelled risk order was claimed")
		}
		implement := stageOrder(t, task.ID, core.StageImplement)
		if _, _, err := cancel(t, task.ID, implement.ID, "not a risk order"); !errors.Is(err, store.ErrNotRiskOrder) {
			t.Fatalf("cancel of an implementation order err=%v", err)
		}
	})

	t.Run("RaiseToHighCancelsOpenRiskOrders", func(t *testing.T) {
		task := fileTask(t, &enforce, core.StageReview)
		done := mustCreate(t, store.RiskOrderCreateRequest{TaskID: task.ID, Subject: head("h-done")})
		_, err := claim(done.ID, "rater-d", "rater-d-token")
		requireOK(t, err)
		_, err = complete(t, task.ID, store.RiskOrderCompleteRequest{WorkOrderID: done.ID, Claim: identity("rater-d"), Tier: riskpolicy.TierLow, Reason: "small"})
		requireOK(t, err)
		queued := mustCreate(t, store.RiskOrderCreateRequest{TaskID: task.ID, Subject: head("h-queued")})
		claimedOrder := mustCreate(t, store.RiskOrderCreateRequest{TaskID: task.ID, Subject: core.RiskOrderSubject{PlanVersion: 1}})
		_, err = claim(claimedOrder.ID, "rater-p", "rater-p-token")
		requireOK(t, err)
		// A raise to medium cancels nothing.
		_, err = st.RaiseTaskRiskTier(ctx, core.RiskRating{TaskID: task.ID, Tier: riskpolicy.TierMedium, Reason: "touches config", ActorID: "user:operator"}, store.RiskSeatSettings{})
		requireOK(t, err)
		if order(t, queued.ID).State != core.WorkOrderQueued || order(t, claimedOrder.ID).State != core.WorkOrderClaimed {
			t.Fatal("a raise below high cancelled a risk order")
		}
		_, err = st.RaiseTaskRiskTier(ctx, core.RiskRating{TaskID: task.ID, Tier: riskpolicy.TierHigh, Reason: "touches billing", ActorID: "user:operator"}, store.RiskSeatSettings{})
		requireOK(t, err)
		for _, id := range []string{queued.ID, claimedOrder.ID} {
			if got := order(t, id); got.State != core.WorkOrderCancelled || got.SessionID != "" {
				t.Fatalf("risk order %s after the raise to high=%+v", id, got)
			}
		}
		if got := order(t, done.ID); got.State != core.WorkOrderCompleted {
			t.Fatalf("completed risk order after the raise=%+v", got)
		}
		if got := countEvents(t, task.ID, "work_order.cancelled"); got != 2 {
			t.Fatalf("work_order.cancelled events=%d, want 2", got)
		}
		if _, err := complete(t, task.ID, store.RiskOrderCompleteRequest{WorkOrderID: claimedOrder.ID, Claim: identity("rater-p"), Tier: riskpolicy.TierLow, Reason: "late"}); !errors.Is(err, store.ErrWorkOrderCancelled) {
			t.Fatalf("late completion after the raise err=%v", err)
		}
		listed := ratings(t, task.ID)
		if len(listed) != 3 || listed[0].Source != core.RiskSourceRiskAgent {
			t.Fatalf("ratings after the raise=%+v, want the completed rating and both raises", listed)
		}
	})

	t.Run("RatingToHighCancelsOtherOpenOrders", func(t *testing.T) {
		task := fileTask(t, &record, core.StageReview)
		headOrder := mustCreate(t, store.RiskOrderCreateRequest{TaskID: task.ID, Subject: head("h-high"), ChangedPaths: []string{"billing.go"}})
		other := mustCreate(t, store.RiskOrderCreateRequest{TaskID: task.ID, Subject: head("h-next")})
		_, err := claim(headOrder.ID, "rater-h", "rater-h-token")
		requireOK(t, err)
		_, err = complete(t, task.ID, store.RiskOrderCompleteRequest{WorkOrderID: headOrder.ID, Claim: identity("rater-h"), Tier: riskpolicy.TierHigh, Reason: "changes billing"})
		requireOK(t, err)
		if got := order(t, other.ID); got.State != core.WorkOrderCancelled {
			t.Fatalf("other open risk order after a high rating=%+v", got)
		}
		if got := order(t, headOrder.ID); got.State != core.WorkOrderCompleted {
			t.Fatalf("rated order=%+v", got)
		}
	})

	t.Run("PlanRatingCountsOnlyOnceApproved", func(t *testing.T) {
		task := fileTask(t, &record, core.StageReview)
		version, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: task.ID, Content: "## Approach\nRate the plan.", Acceptance: []byte(`[]`), Decomposition: []byte(`[]`)})
		requireOK(t, err)
		planOrder := mustCreate(t, store.RiskOrderCreateRequest{TaskID: task.ID, Subject: core.RiskOrderSubject{PlanVersion: version.Version}})
		headOrder := mustCreate(t, store.RiskOrderCreateRequest{TaskID: task.ID, Subject: head("h-plan")})
		_, err = claim(planOrder.ID, "rater-plan", "rater-plan-token")
		requireOK(t, err)
		rating, err := complete(t, task.ID, store.RiskOrderCompleteRequest{WorkOrderID: planOrder.ID, Claim: identity("rater-plan"), Tier: riskpolicy.TierHigh, Reason: "the plan rewrites billing"})
		requireOK(t, err)
		if rating.PlanVersion != version.Version || rating.HeadSHA != "" || rating.CoveredPaths != nil {
			t.Fatalf("plan rating=%+v", rating)
		}
		listed := ratings(t, task.ID)
		if len(listed) != 1 || !listed[0].PlanPending {
			t.Fatalf("unapproved plan rating=%+v, want pending", listed)
		}
		if _, ok := core.RiskTierOf(listed); ok {
			t.Fatal("an unapproved plan version's rating set the tier")
		}
		// The unapproved high rating does not reach high, so the head order
		// stays open, and a new head still gets a risk order.
		if got := order(t, headOrder.ID); got.State != core.WorkOrderQueued {
			t.Fatalf("head order after an unapproved high plan rating=%+v", got)
		}
		mustCreate(t, store.RiskOrderCreateRequest{TaskID: task.ID, Subject: head("h-plan-2")})
		requireOK(t, st.ApproveSpecVersion(ctx, task.ID, version.Version))
		listed = ratings(t, task.ID)
		if len(listed) != 1 || listed[0].PlanPending {
			t.Fatalf("approved plan rating=%+v, want counting", listed)
		}
		if setter, ok := core.RiskTierOf(listed); !ok || setter.Tier != riskpolicy.TierHigh || setter.Source != core.RiskSourceRiskAgent {
			t.Fatalf("tier setter after approval=%+v", setter)
		}
		if got, made, err := create(t, store.RiskOrderCreateRequest{TaskID: task.ID, Subject: head("h-plan-3")}); err != nil || made {
			t.Fatalf("a high task created %+v made=%t err=%v", got, made, err)
		}
	})

	t.Run("LegacySourcesKeepCounting", func(t *testing.T) {
		task := fileTask(t, &record, core.StageReview)
		_, _, err := st.RecordRiskRating(ctx, core.RiskRating{TaskID: task.ID, Key: core.PlanRiskKey(1), Source: core.RiskSourcePlan, Tier: riskpolicy.TierMedium, PlanVersion: 1, Reason: "plan tier line"})
		requireOK(t, err)
		risk := mustCreate(t, store.RiskOrderCreateRequest{TaskID: task.ID, Subject: head("h-legacy")})
		_, err = claim(risk.ID, "rater-l", "rater-l-token")
		requireOK(t, err)
		_, err = complete(t, task.ID, store.RiskOrderCompleteRequest{WorkOrderID: risk.ID, Claim: identity("rater-l"), Tier: riskpolicy.TierLow, Reason: "small"})
		requireOK(t, err)
		listed := ratings(t, task.ID)
		if setter, ok := core.RiskTierOf(listed); !ok || setter.Tier != riskpolicy.TierMedium || setter.Source != core.RiskSourcePlan || listed[0].PlanPending {
			t.Fatalf("tier setter=%+v ok=%t ratings=%+v, want medium from the recorded plan source", setter, ok, listed)
		}
	})

	t.Run("ClaimRefusesSessionsAndTokensOfTheTask", func(t *testing.T) {
		task := fileTask(t, &record, core.StageSpec)
		spec := stageOrder(t, task.ID, core.StageSpec)
		_, err := claim(spec.ID, "planner", "planner-token")
		requireOK(t, err)
		implement := stageOrder(t, task.ID, core.StageImplement)
		// A released attempt clears the row's session and token, so only the
		// claim ledger and the claim events still name them.
		_, err = claim(implement.ID, "implementer-old", "implementer-old-token")
		requireOK(t, err)
		_, err = taskops.ExecuteWorkOrder(ctx, st, task.ID, core.WorkOrderCmdRelease, func(lease taskops.TaskLease) (core.WorkOrder, error) {
			return st.ReleaseWorkerClaimCommand(ctx, lease, implement.ID, identity("implementer-old"),
				core.WorkOrderRelease{SessionID: "implementer-old", Reason: "harness crashed", Outcome: core.WorkOrderOutcomeChildFailure,
					FailureDetail: "harness crashed", InitialRetryDelay: time.Millisecond, MaximumRetryDelay: time.Millisecond})
		})
		requireOK(t, err)
		if released := order(t, implement.ID); released.SessionID != "" || released.State != core.WorkOrderQueued {
			t.Fatalf("released implementation order=%+v", released)
		}
		time.Sleep(20 * time.Millisecond)
		// The retried attempt holds the row's session and token from here on.
		_, err = claim(implement.ID, "implementer", "implementer-token")
		requireOK(t, err)
		jobs, seats := riskReviewRound(task.ID, 1)
		requireOK(t, CreateReviewRound(ctx, st, task.ID, jobs, seats))
		_, err = claim(seats[0].ID, "reviewer-round-1", "reviewer-round-1-token")
		requireOK(t, err)
		// The first round settles with changes requested, so its seat is
		// history when the second round opens.
		requireOK(t, taskops.New(st).AcceptReviewDecision(ctx, core.ReviewDecision{TaskID: task.ID, JobID: seats[0].JobID, ReviewWorkOrderID: seats[0].ID,
			ReviewRound: 1, ReviewSeat: 1, Verdict: "changes_requested", ReasonCode: "tests_missing", Summary: "add tests", Feedback: "add tests",
			MaxBounces: 3, ReviewedCommitSHA: "h-round-1", HeadSHA: "h-round-1", ReviewerModel: "model", PolicyVersion: 1, ClaimSession: "reviewer-round-1"}))
		jobs, seats = riskReviewRound(task.ID, 1)
		for i := range seats {
			seats[i].ReviewRound = 2
		}
		requireOK(t, CreateReviewRound(ctx, st, task.ID, jobs, seats))
		_, err = claim(seats[0].ID, "reviewer", "reviewer-token")
		requireOK(t, err)
		risk := mustCreate(t, store.RiskOrderCreateRequest{TaskID: task.ID, Subject: head("h-claim")})
		before := order(t, risk.ID)
		for _, attempt := range []struct{ session, token string }{
			{"planner", "fresh-1"}, {"fresh-2", "planner-token"},
			{"implementer", "fresh-3"}, {"fresh-4", "implementer-token"},
			{"implementer-old", "fresh-5"}, {"fresh-6", "implementer-old-token"},
			{"reviewer", "fresh-7"}, {"fresh-8", "reviewer-token"},
			{"reviewer-round-1", "fresh-9"}, {"fresh-10", "reviewer-round-1-token"},
		} {
			if _, err := claim(risk.ID, attempt.session, attempt.token); !errors.Is(err, store.ErrRiskOrderNotIndependent) {
				t.Fatalf("risk claim by session %q token %q err=%v, want refusal", attempt.session, attempt.token, err)
			}
			after := order(t, risk.ID)
			if after.State != core.WorkOrderQueued || !after.Claimable || after.SessionID != "" || !after.QueueDeadline.Equal(before.QueueDeadline) ||
				!after.ExecutionStartedAt.IsZero() || after.AttemptID != "" {
				t.Fatalf("refused risk claim changed the order: %+v", after)
			}
		}
		// Another task's sessions and tokens stay eligible.
		other := fileTask(t, &record, core.StageReview)
		otherRisk := mustCreate(t, store.RiskOrderCreateRequest{TaskID: other.ID, Subject: head("h-other")})
		if _, err := claim(otherRisk.ID, "implementer", "implementer-token"); err != nil {
			t.Fatalf("another task's risk claim err=%v", err)
		}
		// A fresh session and token claim the order, and the task state stays.
		taskBefore, err := st.GetTask(ctx, task.ID)
		requireOK(t, err)
		claimed, err := claim(risk.ID, "independent", "independent-token")
		requireOK(t, err)
		if claimed.State != core.WorkOrderClaimed || claimed.SessionID != "independent" || claimed.Model != "model-independent" {
			t.Fatalf("independent risk claim=%+v", claimed)
		}
		taskAfter, err := st.GetTask(ctx, task.ID)
		requireOK(t, err)
		if taskAfter.State != taskBefore.State {
			t.Fatalf("a risk claim moved the task from %s to %s", taskBefore.State, taskAfter.State)
		}
		// Risk orders run beside each other: a second risk order of the task
		// is claimable while the first is claimed.
		second := mustCreate(t, store.RiskOrderCreateRequest{TaskID: task.ID, Subject: core.RiskOrderSubject{PlanVersion: 4}})
		if _, err := claim(second.ID, "independent-2", "independent-2-token"); err != nil {
			t.Fatalf("second risk order claim err=%v", err)
		}
	})

	t.Run("CompletionAndCancellationSerialize", func(t *testing.T) {
		for round := 0; round < 4; round++ {
			task := fileTask(t, &record, core.StageReview)
			risk := mustCreate(t, store.RiskOrderCreateRequest{TaskID: task.ID, Subject: head("h-race")})
			_, err := claim(risk.ID, "racer", "racer-token")
			requireOK(t, err)
			var wg sync.WaitGroup
			var completeErr, cancelErr error
			var cancelled bool
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, completeErr = complete(t, task.ID, store.RiskOrderCompleteRequest{WorkOrderID: risk.ID, Claim: identity("racer"), Tier: riskpolicy.TierMedium, Reason: "race"})
			}()
			go func() {
				defer wg.Done()
				_, cancelled, cancelErr = cancel(t, task.ID, risk.ID, "a later head was submitted")
			}()
			wg.Wait()
			requireOK(t, cancelErr)
			final := order(t, risk.ID)
			listed := ratings(t, task.ID)
			switch {
			case completeErr == nil && !cancelled:
				if final.State != core.WorkOrderCompleted || len(listed) != 1 {
					t.Fatalf("completion won but order=%+v ratings=%d", final, len(listed))
				}
			case errors.Is(completeErr, store.ErrWorkOrderCancelled) && cancelled:
				if final.State != core.WorkOrderCancelled || len(listed) != 0 {
					t.Fatalf("cancellation won but order=%+v ratings=%d", final, len(listed))
				}
			default:
				t.Fatalf("complete err=%v cancelled=%t order=%+v ratings=%d", completeErr, cancelled, final, len(listed))
			}
		}
	})

	t.Run("RaiseToHighAndCompletionSerialize", func(t *testing.T) {
		for round := 0; round < 4; round++ {
			task := fileTask(t, &record, core.StageReview)
			risk := mustCreate(t, store.RiskOrderCreateRequest{TaskID: task.ID, Subject: head("h-raise")})
			_, err := claim(risk.ID, "racer-r", "racer-r-token")
			requireOK(t, err)
			var wg sync.WaitGroup
			var completeErr, raiseErr error
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, completeErr = complete(t, task.ID, store.RiskOrderCompleteRequest{WorkOrderID: risk.ID, Claim: identity("racer-r"), Tier: riskpolicy.TierLow, Reason: "race"})
			}()
			go func() {
				defer wg.Done()
				_, raiseErr = st.RaiseTaskRiskTier(ctx, core.RiskRating{TaskID: task.ID, Tier: riskpolicy.TierHigh, Reason: "race raise", ActorID: "user:operator"}, store.RiskSeatSettings{})
			}()
			wg.Wait()
			requireOK(t, raiseErr)
			final := order(t, risk.ID)
			agent := 0
			for _, rating := range ratings(t, task.ID) {
				if rating.Source == core.RiskSourceRiskAgent {
					agent++
				}
			}
			switch {
			case completeErr == nil:
				if final.State != core.WorkOrderCompleted || agent != 1 {
					t.Fatalf("completion first but order=%+v agent ratings=%d", final, agent)
				}
			case errors.Is(completeErr, store.ErrWorkOrderCancelled):
				if final.State != core.WorkOrderCancelled || agent != 0 {
					t.Fatalf("raise first but order=%+v agent ratings=%d", final, agent)
				}
			default:
				t.Fatalf("complete err=%v order=%+v", completeErr, final)
			}
		}
	})

	t.Run("ScopedToWorkspace", func(t *testing.T) {
		task := fileTask(t, &record, core.StageReview)
		risk := mustCreate(t, store.RiskOrderCreateRequest{TaskID: task.ID, Subject: head("h-scope")})
		foreign := store.WithWorkspace(ctx, x.Workspace+"-risk-order-foreign")
		if listed, err := st.ListTaskRiskOrders(foreign, task.ID); !errors.Is(err, store.ErrNotFound) || len(listed) != 0 {
			t.Fatalf("foreign read %d risk orders err=%v", len(listed), err)
		}
		_, err := taskops.ExecuteWorkOrder(foreign, st, task.ID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (core.WorkOrder, error) {
			order, _, err := st.CreateRiskOrderCommand(foreign, lease, store.RiskOrderCreateRequest{TaskID: task.ID, Subject: head("h-foreign")})
			return order, err
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("foreign create err=%v, want not found", err)
		}
		_, err = taskops.ExecuteWorkOrder(foreign, st, task.ID, core.WorkOrderCmdCancel, func(lease taskops.TaskLease) (core.WorkOrder, error) {
			order, _, err := st.CancelRiskOrderCommand(foreign, lease, store.RiskOrderCancelRequest{WorkOrderID: risk.ID, Reason: "foreign"})
			return order, err
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("foreign cancel err=%v, want not found", err)
		}
		if got := order(t, risk.ID); got.State != core.WorkOrderQueued || len(riskOrders(t, task.ID)) != 1 {
			t.Fatalf("foreign writes changed the task's risk orders: %+v", got)
		}
	})
}
