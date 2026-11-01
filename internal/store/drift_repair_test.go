package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// TestDriftRecorder: a report reaches the bound recorder, a context without
// one drops it, and the payload names the pass, mismatch, and repair
// (component-persistence, Drift repair reports).
func TestDriftRecorder(t *testing.T) {
	ReportDriftRepair(context.Background(), DriftQueuedTaskWithoutJob.For("unbound"))
	var got []DriftRepair
	ctx := WithDriftRecorder(context.Background(), func(r DriftRepair) { got = append(got, r) })
	ReportDriftRepair(ctx, DriftBlueprintParentOpen.For("parent"))
	if len(got) != 1 || got[0].TaskID != "parent" || got[0].Pass != DriftPassBlueprintClosures {
		t.Fatalf("reported=%+v", got)
	}
	var payload map[string]string
	if err := json.Unmarshal(DriftRepairedPayload(got[0]), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["pass"] != DriftPassBlueprintClosures || payload["mismatch"] == "" || payload["repair"] == "" || len(payload) != 3 {
		t.Fatalf("payload=%v", payload)
	}
}

// TestVolatileReviewPublicationReconcileReportsEachRepair: the volatile
// backend reports a missing publication once, after the repair, and a repeat
// reports nothing.
func TestVolatileReviewPublicationReconcileReportsEachRepair(t *testing.T) {
	st := NewVolatileBackend()
	ctx := WithWorkspace(WithActor(context.Background(), SystemActor()), "demo")
	task := core.Task{ID: "review-drift", Workspace: "demo", Repo: "app", State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "review.completed", Payload: core.JSONPayload(map[string]any{
		"publication_eligible": true, "review_work_order_id": task.ID + "-review-1", "verdict": "approve",
	})}); err != nil {
		t.Fatal(err)
	}
	var reported []DriftRepair
	recorded := WithDriftRecorder(ctx, func(r DriftRepair) { reported = append(reported, r) })
	if n, err := st.ReconcileReviewPublications(recorded); err != nil || n != 1 {
		t.Fatalf("first reconcile n=%d err=%v", n, err)
	}
	if len(reported) != 1 || reported[0] != DriftReviewPublicationMissing.For(task.ID) {
		t.Fatalf("reported=%+v", reported)
	}
	if n, err := st.ReconcileReviewPublications(recorded); err != nil || n != 0 || len(reported) != 1 {
		t.Fatalf("repeat reconcile n=%d err=%v reported=%+v", n, err, reported)
	}
}

// TestSelectFactoryActsListsDriftRepairs: a system drift.repaired event is a
// factory act whose summary is "<mismatch>: <repair>"; other acts carry no
// summary (feature-operator-next-action NA-5; req-quiet-idle-runtime AC-4.2).
func TestSelectFactoryActsListsDriftRepairs(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	repair := DriftQueuedTaskWithoutJob.For("t")
	acts := SelectFactoryActs([]core.Event{
		{ID: 1, TaskID: "t", Kind: DriftRepairedEvent, ActorRole: core.ActorSystem, At: now, Payload: DriftRepairedPayload(repair)},
		{ID: 2, TaskID: "t", Kind: "merge.confirmed", ActorRole: core.ActorRunner, At: now.Add(time.Second)},
		{ID: 3, TaskID: "t", Kind: DriftRepairedEvent, ActorRole: core.ActorUser, At: now, Payload: DriftRepairedPayload(repair)},
	}, now, 100)
	if len(acts) != 2 || acts[1].Kind != DriftRepairedEvent || acts[1].Summary != repair.Mismatch+": "+repair.Repair || acts[0].Summary != "" {
		t.Fatalf("acts=%+v", acts)
	}
}
