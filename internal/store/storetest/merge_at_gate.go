package storetest

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// runMergeTaskAtGate is the shared one-act gate merge contract (DEC-64(1,7);
// component-task-lifecycle): one transaction records the operator's approve
// intervention, binds the reviewed head, appends merge.confirmed, and applies
// intervention.merge; it refuses outside the merge_approval gate and leaves
// nothing behind when any write fails.
func runMergeTaskAtGate(t *testing.T, x Fixture) {
	st, ctx, workspace := x.Backend, x.Context, x.Workspace
	plane := taskops.New(st)
	operator := store.WithActor(ctx, store.Actor{ID: store.UserActorID("gate-operator"), Role: core.ActorUser})
	gated := func(t *testing.T) string {
		t.Helper()
		id := createRunningTask(t, st, ctx, workspace)
		if _, err := plane.Perform(ctx, id, taskops.Command{Kind: core.TaskGateMerge, RecoveryStage: core.StageImplement, ProjectStages: true}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	approve := func(id string) core.Intervention {
		return core.Intervention{TaskID: id, Action: core.InterventionApprove, ReasonCode: "merge-at-gate"}
	}
	confirmed := func(id string) core.Event {
		return core.Event{TaskID: id, Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{"head_sha": "reviewed-head", "approving_operator_user_id": "gate-operator"})}
	}
	snapshot := func(t *testing.T, id string) (core.Task, int, int) {
		t.Helper()
		task, err := st.GetTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		events, err := st.ListEvents(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		interventions, err := st.ListInterventions(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return task, len(events), len(interventions)
	}

	t.Run("commits intervention, binding, confirmation, and edge", func(t *testing.T) {
		id := gated(t)
		_, beforeEvents, _ := snapshot(t, id)
		out, err := plane.MergeAtGate(operator, approve(id), "reviewed-head", confirmed(id))
		if err != nil {
			t.Fatal(err)
		}
		if out.State != core.TaskMerged || out.GateKind != "" || out.ApprovedHeadSHA != "reviewed-head" || out.ReviewedHeadSHA != "reviewed-head" {
			t.Fatalf("merged task=%+v", out)
		}
		stored, _, _ := snapshot(t, id)
		if stored.State != core.TaskMerged || stored.GateKind != "" || stored.ApprovedHeadSHA != "reviewed-head" {
			t.Fatalf("stored task=%+v", stored)
		}
		interventions, err := st.ListInterventions(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if len(interventions) != 1 || interventions[0].Action != core.InterventionApprove || interventions[0].ActorID != store.UserActorID("gate-operator") || interventions[0].ActorRole != core.ActorUser {
			t.Fatalf("interventions=%+v", interventions)
		}
		if userID, ok, err := store.ApprovingOperatorUserID(ctx, st, id); err != nil || !ok || userID != "gate-operator" {
			t.Fatalf("approving operator=%q ok=%v err=%v", userID, ok, err)
		}
		events, err := st.ListEvents(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		var kinds []string
		for _, event := range events[beforeEvents:] {
			kinds = append(kinds, event.Kind)
		}
		want := []string{"intervention.approve", "approval.bound", "merge.confirmed", "task.state_changed"}
		if len(kinds) < len(want) {
			t.Fatalf("appended=%v", kinds)
		}
		for i, kind := range want {
			if kinds[i] != kind {
				t.Fatalf("appended=%v want prefix %v", kinds, want)
			}
		}
		var changed struct {
			From    core.TaskState   `json:"from"`
			To      core.TaskState   `json:"to"`
			Command core.TaskCommand `json:"command"`
		}
		if err := json.Unmarshal(events[beforeEvents+3].Payload, &changed); err != nil || changed.From != core.TaskAwaiting || changed.To != core.TaskMerged || changed.Command != core.TaskInterventionMerge {
			t.Fatalf("state change=%+v err=%v", changed, err)
		}
		if _, err := plane.MergeAtGate(operator, approve(id), "reviewed-head", confirmed(id)); !isInvalidTransition(err) {
			t.Fatalf("second gate merge err=%v", err)
		}
	})

	t.Run("refuses outside the merge_approval gate", func(t *testing.T) {
		running := createRunningTask(t, st, ctx, workspace)
		planGate := createRunningTask(t, st, ctx, workspace)
		if _, err := plane.Perform(ctx, planGate, taskops.Command{Kind: core.TaskGateSpec}); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{running, planGate} {
			before, events, interventions := snapshot(t, id)
			if _, err := plane.MergeAtGate(operator, approve(id), "reviewed-head", confirmed(id)); !isInvalidTransition(err) {
				t.Fatalf("%s gate merge err=%v", before.State, err)
			}
			after, afterEvents, afterInterventions := snapshot(t, id)
			if after.State != before.State || after.GateKind != before.GateKind || after.ApprovedHeadSHA != "" || afterEvents != events || afterInterventions != interventions {
				t.Fatalf("refusal wrote: before=%+v after=%+v events %d->%d interventions %d->%d", before, after, events, afterEvents, interventions, afterInterventions)
			}
		}
	})

	t.Run("a failed event insert leaves no intervention and no state change", func(t *testing.T) {
		id := gated(t)
		before, events, interventions := snapshot(t, id)
		bad := confirmed(id)
		bad.ActorID = "user:bad\x00actor"
		if _, err := plane.MergeAtGate(operator, approve(id), "reviewed-head", bad); err == nil {
			t.Fatal("NUL audit actor accepted")
		}
		after, afterEvents, afterInterventions := snapshot(t, id)
		if after.State != core.TaskAwaiting || after.GateKind != core.GateKindMergeApproval || after.ApprovedHeadSHA != before.ApprovedHeadSHA || after.ReviewedHeadSHA != before.ReviewedHeadSHA || afterEvents != events || afterInterventions != interventions {
			t.Fatalf("rolled-back merge wrote: before=%+v after=%+v events %d->%d interventions %d->%d", before, after, events, afterEvents, interventions, afterInterventions)
		}
	})
}

func isInvalidTransition(err error) bool {
	var invalid *core.ErrInvalidTransition
	return errors.As(err, &invalid)
}
