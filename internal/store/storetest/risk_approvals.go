package storetest

import (
	"context"
	"errors"
	"fmt"
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

// gatedApprovalStore answers every person's operate_gates capability from a
// fixed set, so the evaluation conformance reads one membership answer on
// every backend while the approval rows come from the backend under test.
type gatedApprovalStore struct {
	store.Store
	gates map[string]bool
}

func (s gatedApprovalStore) AuthorizeWorkspace(_ context.Context, userID, _ string, capability core.Capability) (bool, error) {
	return capability == core.CapabilityOperateGates && s.gates[userID], nil
}

// RunTaskApprovalConformance proves on every backend that human approvals of
// an enforcing task bind to the reviewed head and review round, count each
// person once, refuse a stale head, a non-enforcing task, and a task away from
// the merge gate with nothing recorded, record one audited event per new
// record, and stay at one row per person under concurrent requests. The
// shared evaluation counts the backend's rows the same way on each backend:
// a new round finds none, an untrusted author's approval does not count, and
// a waiver satisfies only the trusted-author condition (req-risk-based-review
// AC-8.3 to AC-8.8, AC-9.1 to AC-9.3; component-risk-review invariants 9 and
// 10; feature-risk-based-review RR-7).
func RunTaskApprovalConformance(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	const head = "approval-head"
	// Every task freezes the merge gate on, so one approval is required
	// before any rating (req-risk-based-review AC-2.1).
	fileTask := func(t *testing.T, mode riskpolicy.Mode, gate bool) core.Task {
		t.Helper()
		id := core.NewTaskID()
		task := core.Task{ID: id, Workspace: x.Workspace, Repo: "conveyor", Title: id, BaseBranch: "main", Branch: "conveyor/task-" + id,
			State: core.TaskRunning, NextStage: core.StageReview, ReviewedHeadSHA: head, PolicyVersion: 1, MergeApproval: true, CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
		task.SetupContract = config.PolicyContract{MaxBounces: 3, Risk: &riskpolicy.Policy{Mode: mode, Tiers: riskpolicy.DefaultTable()}, Review: config.ReviewPanel{Seats: []config.ReviewSeat{{}}}}
		requireOK(t, st.CreateTask(ctx, task))
		if gate {
			out, err := taskops.New(st).Perform(ctx, id, taskops.Command{Kind: core.TaskGateMerge, RecoveryStage: core.StageImplement, ProjectStages: true})
			requireOK(t, err)
			if out.Task.State != core.TaskAwaiting || out.Task.GateKind != core.GateKindMergeApproval {
				t.Fatalf("gate task=%+v", out.Task)
			}
		}
		stored, err := st.GetTask(ctx, id)
		requireOK(t, err)
		return stored
	}
	approval := func(taskID, user string, round int) core.TaskApproval {
		return core.TaskApproval{TaskID: taskID, Kind: core.TaskApprovalPerson, HeadSHA: head, ReviewRound: round, UserID: user, Source: "approval"}
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

	reviewRound := func(t *testing.T, taskID string, round int) {
		t.Helper()
		id := fmt.Sprintf("%s-review-%d-seat-1", taskID, round)
		requireOK(t, st.CreateJob(ctx, core.Job{ID: id, TaskID: taskID, Stage: core.StageReview, State: core.JobDone}))
		requireOK(t, For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: id, JobID: id, TaskID: taskID, Stage: core.StageReview, ReviewRound: round, ReviewSeat: 1,
			HeadSHA: head, QueueEnteredAt: time.Now().UTC(), QueueDeadline: time.Now().UTC().Add(time.Hour)}))
	}

	t.Run("RecordReplayAndRounds", func(t *testing.T) {
		task := fileTask(t, riskpolicy.ModeEnforce, true)
		first, created, err := st.RecordTaskApproval(ctx, approval(task.ID, "usr-a", 1))
		if err != nil || !created || first.UserID != "usr-a" || first.CreatedAt.IsZero() {
			t.Fatalf("first=%+v created=%t err=%v", first, created, err)
		}
		replay, created, err := st.RecordTaskApproval(ctx, approval(task.ID, "usr-a", 1))
		if err != nil || created || replay.UserID != "usr-a" || replay.ReviewRound != 1 {
			t.Fatalf("replay=%+v created=%t err=%v", replay, created, err)
		}
		_, created, err = st.RecordTaskApproval(ctx, approval(task.ID, "usr-b", 1))
		if err != nil || !created {
			t.Fatalf("second person created=%t err=%v", created, err)
		}
		// The same person in a later round is a new record.
		_, created, err = st.RecordTaskApproval(ctx, approval(task.ID, "usr-a", 2))
		if err != nil || !created {
			t.Fatalf("later round created=%t err=%v", created, err)
		}
		waiver := core.TaskApproval{TaskID: task.ID, Kind: core.TaskApprovalAuthorWaiver, HeadSHA: head, ReviewRound: 1, UserID: "usr-lead", Reason: "the author is away"}
		if _, created, err := st.RecordTaskApproval(ctx, waiver); err != nil || !created {
			t.Fatalf("waiver created=%t err=%v", created, err)
		}
		if _, created, err := st.RecordTaskApproval(ctx, waiver); err != nil || created {
			t.Fatalf("waiver replay created=%t err=%v", created, err)
		}
		listed, err := st.ListTaskApprovals(ctx, task.ID)
		requireOK(t, err)
		if len(listed) != 4 {
			t.Fatalf("listed=%+v, want 4 records", listed)
		}
		if n := countEvents(t, task.ID, core.TaskApprovalEvent); n != 3 {
			t.Fatalf("approval events=%d, want 3", n)
		}
		if n := countEvents(t, task.ID, core.TaskAuthorWaiverEvent); n != 1 {
			t.Fatalf("waiver events=%d, want 1", n)
		}
	})

	t.Run("RefusalsRecordNothing", func(t *testing.T) {
		gated := fileTask(t, riskpolicy.ModeEnforce, true)
		running := fileTask(t, riskpolicy.ModeEnforce, false)
		recorded := fileTask(t, riskpolicy.ModeRecord, true)
		for name, request := range map[string]core.TaskApproval{
			"StaleHead":       {TaskID: gated.ID, Kind: core.TaskApprovalPerson, HeadSHA: "other-head", ReviewRound: 1, UserID: "usr-a"},
			"NotAtGate":       approval(running.ID, "usr-a", 1),
			"RecordMode":      approval(recorded.ID, "usr-a", 1),
			"BlankPerson":     approval(gated.ID, " ", 1),
			"NoRound":         approval(gated.ID, "usr-a", 0),
			"MultiLineReason": {TaskID: gated.ID, Kind: core.TaskApprovalAuthorWaiver, HeadSHA: head, ReviewRound: 1, UserID: "usr-a", Reason: "one\ntwo"},
			"BlankReason":     {TaskID: gated.ID, Kind: core.TaskApprovalAuthorWaiver, HeadSHA: head, ReviewRound: 1, UserID: "usr-a", Reason: "  "},
			"ApprovalReason":  {TaskID: gated.ID, Kind: core.TaskApprovalPerson, HeadSHA: head, ReviewRound: 1, UserID: "usr-a", Reason: "why"},
			"UnknownKind":     {TaskID: gated.ID, Kind: "vote", HeadSHA: head, ReviewRound: 1, UserID: "usr-a"},
		} {
			if _, _, err := st.RecordTaskApproval(ctx, request); !errors.Is(err, store.ErrRefusal) {
				t.Fatalf("%s: err=%v, want a refusal", name, err)
			}
		}
		for _, task := range []core.Task{gated, running, recorded} {
			listed, err := st.ListTaskApprovals(ctx, task.ID)
			requireOK(t, err)
			if len(listed) != 0 || countEvents(t, task.ID, core.TaskApprovalEvent)+countEvents(t, task.ID, core.TaskAuthorWaiverEvent) != 0 {
				t.Fatalf("refusals recorded %+v", listed)
			}
		}
		if _, _, err := st.RecordTaskApproval(ctx, approval("missing-task", "usr-a", 1)); err == nil {
			t.Fatal("an approval of a missing task was recorded")
		}
	})

	t.Run("ConcurrentRequests", func(t *testing.T) {
		task := fileTask(t, riskpolicy.ModeEnforce, true)
		const writers = 6
		var wg sync.WaitGroup
		createdSame := make(chan bool, writers)
		errs := make(chan error, 2*writers)
		for i := 0; i < writers; i++ {
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, created, err := st.RecordTaskApproval(ctx, approval(task.ID, "usr-same", 1))
				if err != nil {
					errs <- err
					return
				}
				createdSame <- created
			}()
			go func(i int) {
				defer wg.Done()
				if _, created, err := st.RecordTaskApproval(ctx, approval(task.ID, fmt.Sprintf("usr-%d", i), 1)); err != nil || !created {
					errs <- fmt.Errorf("person %d created=%t err=%v", i, created, err)
				}
			}(i)
		}
		wg.Wait()
		close(errs)
		close(createdSame)
		for err := range errs {
			if !errors.Is(err, store.ErrRetryable) {
				t.Fatal(err)
			}
		}
		creations := 0
		for created := range createdSame {
			if created {
				creations++
			}
		}
		listed, err := st.ListTaskApprovals(ctx, task.ID)
		requireOK(t, err)
		same := 0
		for _, record := range listed {
			if record.UserID == "usr-same" {
				same++
			}
		}
		if same != 1 || creations > 1 {
			t.Fatalf("same person rows=%d creations=%d, want one", same, creations)
		}
		if n := countEvents(t, task.ID, core.TaskApprovalEvent); n != len(listed) {
			t.Fatalf("approval events=%d rows=%d", n, len(listed))
		}
	})

	t.Run("FinalApprovalCommitsWithTheMerge", func(t *testing.T) {
		plane := taskops.New(st)
		person := func(user string) context.Context {
			return store.WithActor(ctx, store.Actor{ID: store.UserActorID(user), Role: core.ActorUser})
		}
		approve := func(id string) core.Intervention {
			return core.Intervention{TaskID: id, Action: core.InterventionApprove, ReasonCode: "merge-at-gate"}
		}
		confirmed := func(id string) core.Event {
			return core.Event{TaskID: id, Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{"head_sha": head})}
		}
		final := func(id, user string) *core.TaskApproval {
			return &core.TaskApproval{TaskID: id, Kind: core.TaskApprovalPerson, HeadSHA: head, ReviewRound: 1, UserID: user, Source: "merge"}
		}
		people := func(t *testing.T, id string) []string {
			t.Helper()
			listed, err := st.ListTaskApprovals(ctx, id)
			requireOK(t, err)
			var users []string
			for _, record := range listed {
				users = append(users, record.UserID)
			}
			return users
		}

		// The gate merge records the final approval, attributed to its
		// person, in the transaction that completes the merge; a replay is
		// refused and records nothing more (req-risk-based-review AC-8.7).
		task := fileTask(t, riskpolicy.ModeEnforce, true)
		_, _, err := st.RecordTaskApproval(ctx, approval(task.ID, "usr-a", 1))
		requireOK(t, err)
		out, err := plane.MergeAtGateWithApproval(person("usr-final"), approve(task.ID), head, confirmed(task.ID), final(task.ID, "usr-final"))
		requireOK(t, err)
		if out.State != core.TaskMerged || fmt.Sprint(people(t, task.ID)) != "[usr-a usr-final]" {
			t.Fatalf("merged=%s approvals=%v", out.State, people(t, task.ID))
		}
		events, err := st.ListEvents(ctx, task.ID)
		requireOK(t, err)
		finalActor := ""
		for _, event := range events {
			if event.Kind == core.TaskApprovalEvent && strings.Contains(string(event.Payload), `"usr-final"`) {
				finalActor = event.ActorID
			}
		}
		if finalActor != store.UserActorID("usr-final") {
			t.Fatalf("final approval event actor=%q", finalActor)
		}
		before := countEvents(t, task.ID, core.TaskApprovalEvent)
		if _, err := plane.MergeAtGateWithApproval(person("usr-final"), approve(task.ID), head, confirmed(task.ID), final(task.ID, "usr-final")); err == nil {
			t.Fatal("a replayed gate merge was accepted")
		}
		if len(people(t, task.ID)) != 2 || countEvents(t, task.ID, core.TaskApprovalEvent) != before {
			t.Fatalf("replay recorded approvals %v", people(t, task.ID))
		}

		// A final approval that cannot be recorded refuses the whole merge:
		// the task stays at the gate with no intervention, confirmation, or
		// approval (no credit without the approval, no approval without the
		// merge).
		for name, task := range map[string]core.Task{"StaleHead": fileTask(t, riskpolicy.ModeEnforce, true), "RecordMode": fileTask(t, riskpolicy.ModeRecord, true)} {
			bad := final(task.ID, "usr-final")
			if name == "StaleHead" {
				bad.HeadSHA = "other-head"
			}
			if _, err := plane.MergeAtGateWithApproval(person("usr-final"), approve(task.ID), head, confirmed(task.ID), bad); err == nil {
				t.Fatalf("%s: merge with an unrecordable final approval was accepted", name)
			}
			current, err := st.GetTask(ctx, task.ID)
			requireOK(t, err)
			interventions, err := st.ListInterventions(ctx, task.ID)
			requireOK(t, err)
			if current.State != core.TaskAwaiting || len(interventions) != 0 || len(people(t, task.ID)) != 0 || countEvents(t, task.ID, "merge.confirmed") != 0 {
				t.Fatalf("%s: refused merge left state=%s interventions=%d approvals=%v", name, current.State, len(interventions), people(t, task.ID))
			}
		}

		// Concurrent final merges of one task: exactly one completes, and
		// only its person's approval is recorded.
		raced := fileTask(t, riskpolicy.ModeEnforce, true)
		const racers = 5
		var wg sync.WaitGroup
		wins := make(chan string, racers)
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func(user string) {
				defer wg.Done()
				if _, err := plane.MergeAtGateWithApproval(person(user), approve(raced.ID), head, confirmed(raced.ID), final(raced.ID, user)); err == nil {
					wins <- user
				}
			}(fmt.Sprintf("usr-racer-%d", i))
		}
		wg.Wait()
		close(wins)
		var winners []string
		for user := range wins {
			winners = append(winners, user)
		}
		if len(winners) != 1 || fmt.Sprint(people(t, raced.ID)) != fmt.Sprint(winners) || countEvents(t, raced.ID, core.TaskApprovalEvent) != 1 {
			t.Fatalf("winners=%v approvals=%v", winners, people(t, raced.ID))
		}

		// The approved-task merge confirmation records its final approval the
		// same way, and replaying the command is refused with nothing more.
		approved := fileTask(t, riskpolicy.ModeEnforce, true)
		_, err = plane.Perform(ctx, approved.ID, taskops.Command{Kind: core.TaskInterventionApproveReview})
		requireOK(t, err)
		_, err = plane.Perform(person("usr-final"), approved.ID, taskops.Command{Kind: core.TaskMergeConfirm, ProjectStages: true, FinalApproval: final(approved.ID, "usr-final")})
		requireOK(t, err)
		if current, _ := st.GetTask(ctx, approved.ID); current.State != core.TaskMerged || fmt.Sprint(people(t, approved.ID)) != "[usr-final]" {
			t.Fatalf("approved merge state=%s approvals=%v", current.State, people(t, approved.ID))
		}
		if _, err := plane.Perform(person("usr-final"), approved.ID, taskops.Command{Kind: core.TaskMergeConfirm, FinalApproval: final(approved.ID, "usr-final")}); err == nil {
			t.Fatal("a replayed merge confirmation was accepted")
		}
		if countEvents(t, approved.ID, core.TaskApprovalEvent) != 1 {
			t.Fatalf("replayed confirmation recorded approvals %v", people(t, approved.ID))
		}
	})

	t.Run("EvaluationCountsTheBackendRows", func(t *testing.T) {
		task := fileTask(t, riskpolicy.ModeEnforce, true)
		gated := gatedApprovalStore{Store: st, gates: map[string]bool{"usr-author": true, "usr-a": true, "usr-b": true, "usr-lead": true}}
		reviewRound(t, task.ID, 1)
		claim := core.WorkOrder{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, ClaimantID: core.TaskRunClaimantID("usr-author")}
		requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "work_order.claimed", Payload: store.WorkOrderEventPayload(claim)}))
		for _, user := range []string{"usr-a", "usr-a", "usr-author"} {
			_, _, err := st.RecordTaskApproval(ctx, approval(task.ID, user, 1))
			requireOK(t, err)
		}
		// Trusted author: two people count, the author's included (AC-8.4).
		evaluation, err := store.EvaluateTaskApprovals(ctx, gated, task, nil, "")
		requireOK(t, err)
		if status := evaluation.Status; status.Recorded != 2 || status.TrustedAuthorApprovalNeeded || status.UntrustedAuthor || evaluation.ReviewRound != 1 {
			t.Fatalf("trusted evaluation=%+v round=%d", status, evaluation.ReviewRound)
		}
		// A live trust list without the author makes the author untrusted:
		// the author's approval no longer counts and a non-author is needed
		// (AC-9.2, AC-9.3).
		evaluation, err = store.EvaluateTaskApprovals(ctx, gated, task, []string{"usr-b"}, "")
		requireOK(t, err)
		if status := evaluation.Status; status.Recorded != 1 || !status.UntrustedAuthor || status.NonAuthorApprovalNeeded {
			t.Fatalf("untrusted evaluation=%+v", status)
		}
		// A later review round finds none of the earlier approvals (AC-8.6).
		reviewRound(t, task.ID, 2)
		evaluation, err = store.EvaluateTaskApprovals(ctx, gated, task, nil, "")
		requireOK(t, err)
		if status := evaluation.Status; status.Recorded != 0 || evaluation.ReviewRound != 2 || !status.TrustedAuthorApprovalNeeded {
			t.Fatalf("new round evaluation=%+v round=%d", status, evaluation.ReviewRound)
		}
		// A non-author's waiver satisfies only the author condition (AC-8.5).
		_, _, err = st.RecordTaskApproval(ctx, core.TaskApproval{TaskID: task.ID, Kind: core.TaskApprovalAuthorWaiver, HeadSHA: head, ReviewRound: 2, UserID: "usr-lead", Reason: "away"})
		requireOK(t, err)
		evaluation, err = store.EvaluateTaskApprovals(ctx, gated, task, nil, "usr-b")
		requireOK(t, err)
		if status := evaluation.Status; status.TrustedAuthorApprovalNeeded || status.Recorded != 1 || !status.ProspectiveEligible {
			t.Fatalf("waived evaluation=%+v", status)
		}
	})
}
