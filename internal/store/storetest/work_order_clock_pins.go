package storetest

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// clockPinSnapshot is the stored pin of one order: both bodies and the
// governance digest, as the full read returns them.
type clockPinSnapshot struct {
	served     string
	governance string
	digest     string
}

func clockPinsOf(t *testing.T, order core.WorkOrder) clockPinSnapshot {
	t.Helper()
	served, err := json.Marshal(order.ServedRequirementSnapshot)
	requireOK(t, err)
	governance, err := json.Marshal(order.GovernanceSnapshot)
	requireOK(t, err)
	return clockPinSnapshot{served: string(served), governance: string(governance), digest: order.GovernanceSnapshotDigest}
}

// runWorkOrderClockPins seeds verify orders that pin a 64 KiB governance
// snapshot and a served-requirement snapshot, then runs every transition the
// order clock owns. After each, the stored pin bodies and digest equal the
// seeded bytes, so no lifecycle write cleared or rewrote them. A clock that
// changes nothing reads no pin body, and the per-task projected read returns
// only the named task's rows and no pin body (component-work-orders,
// Snapshot-free fold reads, Three clocks).
func runWorkOrderClockPins(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	governance := core.GovernanceSnapshot{
		Designs:                []core.GovernanceDesignContext{{ID: "component-clock-pins", Title: "Clock pins", Category: "component", Version: 1, Content: strings.Repeat("c", foldGovernanceBytes), Governs: []core.GovernedScope{}}},
		Decisions:              []core.Decision{},
		PendingDesignProposals: []core.PendingSystemDesignProposal{},
	}
	served := []core.ServedRequirementContext{{ID: "req-clock-pins", Version: 1, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep the pin"}}}}
	pinned := func(name string, lease, execution time.Duration) (core.WorkOrder, clockPinSnapshot) {
		t.Helper()
		order := newAggregateOrder(t, x, core.StageVerify)
		_, err := ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{ClaimantID: core.TaskRunClaimantID("worker"), SessionID: "clock-" + name, ClientToken: "clock-" + name, Lease: lease, ExecutionTimeout: execution, Requirements: served, Governance: &governance})
		requireOK(t, err)
		stored, err := st.GetWorkOrder(ctx, order.ID)
		requireOK(t, err)
		if stored.GovernanceSnapshot == nil || len(stored.GovernanceSnapshot.Designs[0].Content) != foldGovernanceBytes || stored.ServedRequirementSnapshot == nil || stored.GovernanceSnapshotDigest == "" {
			t.Fatalf("seed lost the pins of %s: %+v", order.ID, stored.GovernanceSnapshotDigest)
		}
		return stored, clockPinsOf(t, stored)
	}
	requirePins := func(label string, order core.WorkOrder, want clockPinSnapshot) core.WorkOrder {
		t.Helper()
		stored, err := st.GetWorkOrder(ctx, order.ID)
		requireOK(t, err)
		if got := clockPinsOf(t, stored); got != want {
			t.Fatalf("%s changed the stored pins of %s: digest %q to %q, bodies equal %v/%v", label, order.ID, want.digest, got.digest, got.served == want.served, got.governance == want.governance)
		}
		return stored
	}

	t.Run("ExplicitSnapshotReadPreservesBodyAndWorkspace", func(t *testing.T) {
		order, want := pinned("snapshot", time.Hour, time.Hour)
		full, err := st.ListTaskWorkOrdersSnapshot(ctx, order.TaskID)
		requireOK(t, err)
		if len(full) != 1 || clockPinsOf(t, full[0]) != want {
			t.Fatal("explicit task snapshot read changed the served pin bodies")
		}
		task, err := st.GetTask(ctx, order.TaskID)
		requireOK(t, err)
		if full[0].Assignee != task.Assignee {
			t.Fatalf("snapshot assignee = %q, want %q", full[0].Assignee, task.Assignee)
		}
		projected, err := st.ListTaskWorkOrders(ctx, order.TaskID)
		requireOK(t, err)
		if len(projected) != 1 || projected[0].GovernanceSnapshot != nil || projected[0].ServedRequirementSnapshot != nil {
			t.Fatal("lifecycle read returned authority pin bodies")
		}
		other, err := st.ListTaskWorkOrdersSnapshot(store.WithWorkspace(ctx, "clock-other-workspace"), order.TaskID)
		requireOK(t, err)
		if len(other) != 0 {
			t.Fatal("explicit snapshot read crossed the workspace boundary")
		}
	})

	// The task detail reads the snapshot-free orders and which pins each
	// holds; neither read moves a pin body, and the pins equal what the
	// snapshot read carries (component-web-task-surfaces invariant 22).
	t.Run("AuditPinsReadNoPinBody", func(t *testing.T) {
		order, _ := pinned("audit", time.Hour, time.Hour)
		governanceOnly := core.GovernanceSnapshot{Designs: []core.GovernanceDesignContext{}, Decisions: []core.Decision{}, PendingDesignProposals: []core.PendingSystemDesignProposal{}}
		siblings := map[string]core.WorkOrderClaim{
			"served":     {Requirements: served},
			"governance": {Governance: &governanceOnly},
			"empty":      {Requirements: []core.ServedRequirementContext{}},
		}
		siblingIDs := map[string]string{}
		for name, claim := range siblings {
			sibling := newAggregateOrder(t, x, core.StageVerify)
			claim.ClaimantID, claim.SessionID, claim.ClientToken = core.TaskRunClaimantID("worker"), "audit-"+name, "audit-"+name
			claim.Lease, claim.ExecutionTimeout = time.Hour, time.Hour
			_, err := ClaimWorkOrder(ctx, st, sibling.ID, claim)
			requireOK(t, err)
			siblingIDs[name] = sibling.ID
		}
		unclaimed := newAggregateOrder(t, x, core.StageVerify)
		if x.DrainReadBytes != nil {
			x.DrainReadBytes()
		}
		pins, err := st.ListTaskWorkOrderAuditPins(ctx, order.TaskID)
		requireOK(t, err)
		_, err = st.ListTaskWorkOrders(ctx, order.TaskID)
		requireOK(t, err)
		if x.DrainReadBytes != nil {
			received := x.DrainReadBytes()
			if got := received["ListTaskWorkOrderAuditPins"] + received["ListTaskWorkOrders"]; got >= foldGovernanceBytes {
				t.Fatalf("detail order reads received %d bytes, want fewer than one %d-byte governance body", got, foldGovernanceBytes)
			}
		}
		if want := (store.WorkOrderAuditPins{ServedRequirements: true, Governance: true}); len(pins) != 1 || pins[order.ID] != want {
			t.Fatalf("audit pins = %+v, want only %s with %+v", pins, order.ID, want)
		}
		// Each order's pins equal what its snapshot read carries, including
		// a served pin alone, a governance pin alone, an empty served list,
		// and an order with no claim.
		for _, taskOrder := range append(slicesOfIDs(siblingIDs), unclaimed.ID) {
			stored, err := st.GetWorkOrder(ctx, taskOrder)
			requireOK(t, err)
			full, err := st.ListTaskWorkOrdersSnapshot(ctx, stored.TaskID)
			requireOK(t, err)
			got, err := st.ListTaskWorkOrderAuditPins(ctx, stored.TaskID)
			requireOK(t, err)
			for _, snapshot := range full {
				want := store.WorkOrderAuditPins{ServedRequirements: len(snapshot.ServedRequirementSnapshot) > 0, Governance: snapshot.GovernanceSnapshot != nil}
				if got[snapshot.ID] != want || want.Any() != hasPins(got, snapshot.ID) {
					t.Fatalf("audit pins of %s = %+v, snapshot read holds %+v", snapshot.ID, got[snapshot.ID], want)
				}
			}
		}
		for name, want := range map[string]store.WorkOrderAuditPins{"served": {ServedRequirements: true}, "governance": {Governance: true}, "empty": {}} {
			stored, err := st.GetWorkOrder(ctx, siblingIDs[name])
			requireOK(t, err)
			got, err := st.ListTaskWorkOrderAuditPins(ctx, stored.TaskID)
			requireOK(t, err)
			if got[stored.ID] != want {
				t.Fatalf("%s order audit pins = %+v, want %+v", name, got[stored.ID], want)
			}
		}
		other, err := st.ListTaskWorkOrderAuditPins(store.WithWorkspace(ctx, "clock-other-workspace"), order.TaskID)
		requireOK(t, err)
		if len(other) != 0 {
			t.Fatal("audit pin read crossed the workspace boundary")
		}
	})

	t.Run("NoOpClockReadsNoPinBody", func(t *testing.T) {
		order, want := pinned("noop", time.Hour, time.Hour)
		if x.DrainReadBytes != nil {
			x.DrainReadBytes()
		}
		tickTask(t, x, order.TaskID, time.Now().UTC())
		if x.DrainReadBytes != nil {
			if got := x.DrainReadBytes()["ApplyWorkOrderClock"]; got >= foldGovernanceBytes {
				t.Fatalf("a no-op clock received %d bytes, want fewer than one %d-byte governance body", got, foldGovernanceBytes)
			}
		}
		stored := requirePins("no-op clock", order, want)
		if stored.State != core.WorkOrderClaimed {
			t.Fatalf("no-op clock moved %s to %s", order.ID, stored.State)
		}
	})

	t.Run("LeaseExpiryKeepsPins", func(t *testing.T) {
		order, want := pinned("lease", time.Minute, time.Hour)
		tickTask(t, x, order.TaskID, time.Now().UTC().Add(2*time.Minute))
		stored := requirePins("lease expiry", order, want)
		if stored.State != core.WorkOrderQueued || stored.LastAttemptOutcome != core.WorkOrderOutcomeExpired {
			t.Fatalf("expired lease left %s as %s/%s", order.ID, stored.State, stored.LastAttemptOutcome)
		}
	})

	t.Run("ExecutionTimeoutKeepsPinsAndNamesThem", func(t *testing.T) {
		order, want := pinned("timeout", time.Hour, time.Minute)
		tickTask(t, x, order.TaskID, time.Now().UTC().Add(2*time.Minute))
		stored := requirePins("execution timeout", order, want)
		if stored.State != core.WorkOrderTimedOut && stored.State != core.WorkOrderQueued {
			t.Fatalf("elapsed execution deadline left %s as %s", order.ID, stored.State)
		}
		events, err := st.ListEvents(ctx, order.TaskID)
		requireOK(t, err)
		found := false
		for _, event := range events {
			if event.Kind != "work_order.timed_out" {
				continue
			}
			found = true
			if !strings.Contains(string(event.Payload), want.digest) || !strings.Contains(string(event.Payload), "governance_references") {
				t.Fatalf("timed_out event lost the pin identity: %s", event.Payload)
			}
		}
		if !found {
			t.Fatal("clock persisted no work_order.timed_out event")
		}
	})

	t.Run("QueueBlockedKeepsPins", func(t *testing.T) {
		order, want := pinned("blocked", time.Minute, time.Hour)
		tickTask(t, x, order.TaskID, time.Now().UTC().Add(2*time.Minute))
		requirePins("lease expiry before the block", order, want)
		dependency := newAggregateTask(t, x)
		_, err := st.AddTaskDependency(ctx, store.DependencyAdditionRequest{TaskID: order.TaskID, DependsOnTaskID: dependency.ID, Reason: "clock pin dependency", RequestID: core.NewTaskID()})
		requireOK(t, err)
		tickTask(t, x, order.TaskID, time.Now().UTC().Add(3*time.Minute))
		blocked := requirePins("blocked clock", order, want)
		if blocked.State != core.WorkOrderQueued || blocked.QueueBlockedAt.IsZero() {
			t.Fatalf("blocked verify order %s state=%s blocked_at=%v, want queued with the clock paused", order.ID, blocked.State, blocked.QueueBlockedAt)
		}
		_, err = st.RemoveTaskDependency(ctx, store.DependencyRemovalRequest{TaskID: order.TaskID, DependsOnTaskID: dependency.ID, Reason: "clock pin resume", RequestID: core.NewTaskID()})
		requireOK(t, err)
		tickTask(t, x, order.TaskID, time.Now().UTC().Add(4*time.Minute))
		resumed := requirePins("resumed clock", order, want)
		if !resumed.QueueBlockedAt.IsZero() || resumed.QueueDeadline.Before(blocked.QueueDeadline) {
			t.Fatalf("queue clock of %s did not resume: blocked_at=%v deadline %v to %v", order.ID, resumed.QueueBlockedAt, blocked.QueueDeadline, resumed.QueueDeadline)
		}
	})

	t.Run("LegacyStaleKeepsPins", func(t *testing.T) {
		order, want := pinned("stale", time.Hour, time.Hour)
		seedStoredStale(t, x, order, false, time.Time{})
		tickTask(t, x, order.TaskID, time.Now().UTC())
		stored := requirePins("legacy stale normalization", order, want)
		if stored.State == core.WorkOrderStale {
			t.Fatalf("clock left %s stale", order.ID)
		}
	})

	t.Run("ProjectedReadIsScopedAndBodyFree", func(t *testing.T) {
		order, want := pinned("scoped", time.Hour, time.Hour)
		pinned("unrelated", time.Hour, time.Hour)
		if x.DrainReadBytes != nil {
			x.DrainReadBytes()
		}
		if x.DrainReadRows != nil {
			x.DrainReadRows()
		}
		orders, err := st.ListTaskWorkOrders(ctx, order.TaskID)
		requireOK(t, err)
		if len(orders) != 1 || orders[0].ID != order.ID || orders[0].GovernanceSnapshotDigest != want.digest || snapshotBytes(t, orders) != 0 {
			t.Fatalf("ListTaskWorkOrders(%s) = %d orders, %d snapshot bytes", order.TaskID, len(orders), snapshotBytes(t, orders))
		}
		if x.DrainReadBytes != nil {
			if got := x.DrainReadBytes()["ListTaskWorkOrders"]; got >= foldGovernanceBytes {
				t.Fatalf("ListTaskWorkOrders received %d bytes, want fewer than one %d-byte governance body", got, foldGovernanceBytes)
			}
		}
		if x.DrainReadRows != nil {
			if got := x.DrainReadRows()["ListTaskWorkOrders"]; got != 1 {
				t.Fatalf("ListTaskWorkOrders returned %d database rows for a task with one order, want 1", got)
			}
		}
	})
}

func slicesOfIDs(ids map[string]string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id)
	}
	return out
}

func hasPins(pins map[string]store.WorkOrderAuditPins, id string) bool {
	_, ok := pins[id]
	return ok
}
