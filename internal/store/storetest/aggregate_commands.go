package storetest

import (
	"bytes"
	"image"
	"image/png"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func runCommandRefusals(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	order := newAggregateOrder(t, x)
	before, err := st.ListEvents(ctx, order.TaskID)
	requireOK(t, err)
	refused := func(_ any, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("command accepted an unowned task lease")
		}
	}
	lease := taskops.TaskLease{}
	refused(st.ApplyTaskCommand(ctx, lease, order.TaskID, taskops.Command{Kind: core.TaskCancel}))
	refused(st.ChangeTaskSetupCommand(ctx, lease, store.SetupChangeRequest{TaskID: order.TaskID, RequestID: "setup", Reason: "fixture"}))
	refused(st.CreateConflictFixCommand(ctx, lease, store.ConflictFixRequest{TaskID: order.TaskID}))
	refused(st.RequestChangesCommand(ctx, lease, taskops.RequestChanges{TaskID: order.TaskID, Feedback: "fixture"}))
	refused(st.CancelPlanRevisionWorkOrderCommand(ctx, lease, order.ID, "absent-attempt"))
	if err := st.CreateIntervention(ctx, core.Intervention{TaskID: order.TaskID, Action: "invalid"}); err == nil {
		t.Fatal("invalid intervention accepted")
	}
	if _, err := st.RemoveTaskDependency(ctx, store.DependencyRemovalRequest{TaskID: order.TaskID, DependsOnTaskID: "absent", RequestID: "remove", Reason: "fixture"}); err == nil {
		t.Fatal("missing dependency removal accepted")
	}
	_, err = st.RefreshWorkOrderHarnessSnapshot(ctx, order.ID, &core.HarnessSnapshot{Name: "codex", Command: []string{"fixture"}})
	if err == nil {
		t.Fatal("unpinned harness accepted a snapshot refresh")
	}
	// Harness metadata does not append a lifecycle transition.
	after, err := st.ListEvents(ctx, order.TaskID)
	requireOK(t, err)
	if len(after) != len(before) {
		t.Fatal("refused command appended a partial event")
	}
	current, err := st.GetTask(ctx, order.TaskID)
	requireOK(t, err)
	if current.State != core.TaskRunning {
		t.Fatal("refused command changed task state")
	}
	if _, err := st.CreateClaimedVerificationEvidence(ctx, store.ClaimedVerificationEvidenceRequest{WorkOrderID: order.ID, SessionID: "absent", Name: "proof.txt", ContentType: "text/plain"}, []byte("proof")); err == nil {
		t.Fatal("unclaimed order accepted verification evidence")
	}
	claimed, err := ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{SessionID: "evidence", ClientToken: "fixture-evidence", ClaimantID: "worker", WorkerID: "worker", Lease: time.Minute, ExecutionTimeout: time.Hour})
	requireOK(t, err)
	var proof bytes.Buffer
	requireOK(t, png.Encode(&proof, image.NewRGBA(image.Rect(0, 0, 1, 1))))
	artifact, err := st.CreateClaimedVerificationEvidence(ctx, store.ClaimedVerificationEvidenceRequest{WorkOrderID: order.ID, WorkerID: "worker", SessionID: claimed.SessionID, ClientToken: "fixture-evidence", Name: "proof.png", ContentType: "image/png"}, proof.Bytes())
	requireOK(t, err)
	_, content, err := st.GetArtifact(ctx, artifact.ID)
	requireOK(t, err)
	if !bytes.Equal(content, proof.Bytes()) {
		t.Fatal("claimed evidence bytes differ")
	}
}

func runApprovalRefresh(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	task := newAggregateTask(t, x)
	requireOK(t, st.BindTaskApproval(ctx, task.ID, "approved-head"))
	changed, err := st.MarkTaskApprovalStale(ctx, task.ID, "approved-head", "new-head", "delta", "head advanced")
	requireOK(t, err)
	if !changed {
		t.Fatal("head advance did not invalidate approval")
	}
	requireOK(t, st.AdvanceTaskRefreshHead(ctx, task.ID, "newest-head"))
	requireOK(t, st.SkipTaskRefresh(ctx, task.ID, "newest-head", "fixture explicit skip"))
	current, err := st.GetTask(ctx, task.ID)
	requireOK(t, err)
	if current.ApprovalStale {
		t.Fatal("explicit refresh skip retained stale approval")
	}
}
