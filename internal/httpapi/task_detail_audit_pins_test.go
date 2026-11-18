package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// snapshotDetailStore serves the task detail the orders it read before it
// stopped reading pin bodies: ListTaskWorkOrders returns the snapshot read,
// and the pin read reports nothing, so every audit_available mark comes from
// stripping a body, as before.
type snapshotDetailStore struct{ store.Store }

func (s snapshotDetailStore) ListTaskWorkOrders(ctx context.Context, taskID string) ([]core.WorkOrder, error) {
	return s.Store.ListTaskWorkOrdersSnapshot(ctx, taskID)
}

func (s snapshotDetailStore) ListTaskWorkOrderAuditPins(context.Context, string) (map[string]store.WorkOrderAuditPins, error) {
	return map[string]store.WorkOrderAuditPins{}, nil
}

// TestTaskDetailPinnedOrdersMatchSnapshotRead proves the detail response
// bytes are unchanged now that it reads no pin body: orders that hold a
// served-requirement pin, a governance pin, both, an empty served list, or
// neither are marked audit_available exactly as the snapshot read marked
// them, in the complete and bounded views (component-web-task-surfaces
// invariant 22).
func TestTaskDetailPinnedOrdersMatchSnapshotRead(t *testing.T) {
	const sentinel = "DETAIL-AUDIT-PIN-BODY-SENTINEL"
	f := newDetailFixture(t, 1)
	// A verify order pins both bodies at claim, so the task's frozen policy
	// has the verify stage on, as the store conformance fixture sets it.
	id := core.NewTaskID()
	task := core.Task{ID: id, Workspace: "demo", Title: "pinned " + id, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, NextStage: core.StageVerify, ReviewedHeadSHA: "submitted-head", CreatedAt: f.now}
	task.SetupContract.VerifyStage = true
	if err := f.st.CreateTask(f.ctx, task); err != nil {
		t.Fatal(err)
	}
	served := []core.ServedRequirementContext{{ID: "req-detail-pins", Version: 1, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: sentinel}}}}
	governance := &core.GovernanceSnapshot{
		Designs:                []core.GovernanceDesignContext{{ID: "component-detail-pins", Title: "Detail pins", Category: "component", Version: 1, Content: strings.Repeat(sentinel, 64), Governs: []core.GovernedScope{}}},
		Decisions:              []core.Decision{},
		PendingDesignProposals: []core.PendingSystemDesignProposal{},
	}
	claims := []core.WorkOrderClaim{
		{Requirements: served, Governance: governance},
		{Requirements: served},
		{Governance: governance},
		{Requirements: []core.ServedRequirementContext{}},
		{},
	}
	const worker = "detail-pins-worker"
	for i, claim := range claims {
		id := fmt.Sprintf("%s-verify-%d", task.ID, i+1)
		if err := f.st.CreateJob(f.ctx, core.Job{ID: id, TaskID: task.ID, Stage: core.StageVerify, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
		at := f.now.Add(time.Duration(i+1) * time.Second)
		if err := storetest.CreateWorkOrder(f.ctx, f.st, core.WorkOrder{ID: id, TaskID: task.ID, JobID: id, Stage: core.StageVerify, State: core.WorkOrderQueued, HeadSHA: task.ReviewedHeadSHA, QueueEnteredAt: at, QueueDeadline: at.Add(time.Hour), CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
		claim.ClaimantID, claim.SessionID, claim.ClientToken = worker, "pins-"+id, "pins-"+id
		claim.Lease, claim.ExecutionTimeout = time.Hour, time.Hour
		claimed, err := storetest.ClaimWorkOrder(f.ctx, f.st, id, claim)
		if err != nil {
			t.Fatalf("claim %s: %v", id, err)
		}
		// Review and verify claims pin authority. Only one order of a task can
		// be claimed; the last stays live.
		if i < len(claims)-1 {
			if _, err = storetest.ReleaseWorkerClaim(f.ctx, f.st, id, worker, core.WorkOrderRelease{SessionID: claimed.SessionID, Outcome: core.WorkOrderOutcomeChildFailure, Reason: "harness exited", InitialRetryDelay: time.Nanosecond, MaximumRetryDelay: time.Nanosecond}); err != nil {
				t.Fatalf("release %s: %v", id, err)
			}
		}
	}

	d := newDetailServer(f)
	previous := NewServer(snapshotDetailStore{Store: f.st})
	previous.BearerToken, previous.Workspace = d.server.BearerToken, d.server.Workspace
	for _, bounded := range []bool{false, true} {
		label := fmt.Sprintf("bounded=%v", bounded)
		before := serveDetail(t, previous.getTaskActivity, task.ID, bounded)
		d.counter.reset()
		current := serveDetail(t, d.server.getTaskActivity, task.ID, bounded)
		calls := d.counter.reset()
		if before.Code != 200 || current.Code != 200 {
			t.Fatalf("%s: snapshot read %d %s, current %d %s", label, before.Code, before.Body, current.Code, current.Body)
		}
		if !bytes.Equal(before.Body.Bytes(), current.Body.Bytes()) {
			t.Fatalf("%s: body differs\nsnapshot read: %s\ncurrent:       %s", label, before.Body, current.Body)
		}
		if calls["ListTaskWorkOrdersSnapshot"] != 0 || calls["ListTaskWorkOrders"] != 1 || calls["ListTaskWorkOrderAuditPins"] != 1 {
			t.Fatalf("%s: order reads %v, want one snapshot-free read and one pin read", label, calls)
		}
		if bytes.Contains(current.Body.Bytes(), []byte(sentinel)) {
			t.Fatalf("%s: response carried a pin body", label)
		}
		var body struct {
			WorkOrders []map[string]json.RawMessage `json:"work_orders"`
		}
		if err := json.Unmarshal(current.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		marked := map[string]bool{}
		for _, order := range body.WorkOrders {
			var id string
			if err := json.Unmarshal(order["id"], &id); err != nil {
				t.Fatal(err)
			}
			marked[id] = string(order["audit_available"]) == "true"
		}
		for i, want := range []bool{true, true, true, false, false} {
			id := fmt.Sprintf("%s-verify-%d", task.ID, i+1)
			if marked[id] != want {
				t.Fatalf("%s: %s audit_available=%v, want %v", label, id, marked[id], want)
			}
		}
	}
}
