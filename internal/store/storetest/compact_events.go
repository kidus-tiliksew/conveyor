package storetest

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

const (
	// compactSnapshotMarker opens the historical design body, so a body that
	// crosses the read appears in the payload bytes.
	compactSnapshotMarker = "compact-snapshot-body-marker:"
	// compactSnapshotBodyBytes is the size of each historical event's design
	// body.
	compactSnapshotBodyBytes = 1 << 20
	// compactReadPayloadBound bounds the compact read's payload bytes. The
	// fixture's compact payloads total a few KiB, and one embedded body alone
	// is 1 MiB.
	compactReadPayloadBound = 64 << 10
)

// runCompactEventReads pins ListCompactEvents against ListEvents over a
// ledger that holds compact work-order events beside historical ones that
// embed snapshot bodies (component-persistence, Compact task event reads).
// The payload column is the only column that can carry a body, and each
// driver returns exactly the bytes it receives for it, so the payload bytes
// the compact read returns bound what it transferred.
func runCompactEventReads(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	order := newAggregateOrder(t, x)
	claimed, err := ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{ClaimantID: core.TaskRunClaimantID("compact-worker"), SessionID: "compact-session", ClientToken: "compact-token", Agent: "claude-code", Model: "claude-opus-5-5", Lease: time.Minute, ExecutionTimeout: time.Hour})
	requireOK(t, err)

	// A historical work order as releases before the compact projection
	// serialized it: the complete JSON with both snapshot bodies.
	historical := claimed
	historical.Progress = "historical progress before the compact projection"
	historical.GovernanceSnapshot = &core.GovernanceSnapshot{Designs: []core.GovernanceDesignContext{{
		ID: "design-compact-read", Title: "Compact read", Category: "Component design", Version: 3,
		Content: compactSnapshotMarker + strings.Repeat("s", compactSnapshotBodyBytes),
		Governs: []core.GovernedScope{{Repository: "conveyor", Paths: []string{"internal/**"}}},
	}}}
	historical.ServedRequirementSnapshot = []core.ServedRequirementContext{{ID: "req-compact-read", Title: "Compact read", Version: 2}}
	updated := historical
	updated.Progress = "historical update before the compact projection"
	at := seedTime()
	x.SeedEvents(t, ctx, 0, []core.Event{
		{ID: 1, TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.claimed", At: at, Payload: core.JSONPayload(historical)},
		{ID: 2, TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.updated", At: at.Add(time.Second), Payload: core.JSONPayload(updated)},
		// Only work_order.* payloads are projected: this key survives.
		{ID: 3, TaskID: order.TaskID, Kind: "review.completed", At: at.Add(2 * time.Second), Payload: core.JSONPayload(map[string]any{"governance_snapshot": "kept on other kinds", "verdict": "approve", "review_work_order_id": "compact-review"})},
	})

	full, err := st.ListEvents(ctx, order.TaskID)
	requireOK(t, err)
	compact, err := st.ListCompactEvents(ctx, order.TaskID)
	requireOK(t, err)
	if len(compact) != len(full) {
		t.Fatalf("compact read returned %d events, full read %d", len(compact), len(full))
	}

	historicalWant := map[string]json.RawMessage{
		"work_order.claimed": store.WorkOrderEventPayload(historical),
		"work_order.updated": store.WorkOrderEventPayload(updated),
	}
	fullBytes, compactBytes, historicalSeen, compactWorkOrderEvents := 0, 0, 0, 0
	for i := range full {
		f, c := full[i], compact[i]
		if f.ID != c.ID || f.TaskID != c.TaskID || f.JobID != c.JobID || f.Kind != c.Kind || f.ActorID != c.ActorID || f.ActorRole != c.ActorRole || !f.At.Equal(c.At) {
			t.Fatalf("event %d identity differs: full=%+v compact=%+v", i, eventIdentity(f), eventIdentity(c))
		}
		fullBytes += len(f.Payload)
		compactBytes += len(c.Payload)
		if bytes.Contains(c.Payload, []byte(compactSnapshotMarker)) {
			t.Fatalf("compact %s event %d carries the snapshot body", c.Kind, c.ID)
		}
		want := exactJSON(t, f.Payload)
		if strings.HasPrefix(f.Kind, "work_order.") {
			if fields, ok := want.(map[string]any); ok {
				delete(fields, "governance_snapshot")
				delete(fields, "served_requirement_snapshot")
			}
		}
		if got := exactJSON(t, c.Payload); !reflect.DeepEqual(got, want) {
			t.Fatalf("compact %s event %d payload=%v, want the full payload without snapshot keys %v", c.Kind, c.ID, got, want)
		}
		if bytes.Contains(f.Payload, []byte(compactSnapshotMarker)) {
			historicalSeen++
			if got, want := exactJSON(t, c.Payload), exactJSON(t, historicalWant[f.Kind]); !reflect.DeepEqual(got, want) {
				t.Fatalf("historical %s event %d decodes to %v, want WorkOrderEventPayload %v", f.Kind, f.ID, got, want)
			}
		} else if strings.HasPrefix(f.Kind, "work_order.") {
			compactWorkOrderEvents++
		}
		if f.Kind == "review.completed" && !bytes.Contains(c.Payload, []byte("kept on other kinds")) {
			t.Fatalf("compact read projected a review.completed payload: %s", c.Payload)
		}
	}
	if historicalSeen != 2 || compactWorkOrderEvents == 0 {
		t.Fatalf("fixture read %d historical and %d compact work-order events, want 2 and at least 1", historicalSeen, compactWorkOrderEvents)
	}
	if fullBytes <= 2*compactSnapshotBodyBytes {
		t.Fatalf("full read payloads total %d bytes, want the two embedded bodies (> %d)", fullBytes, 2*compactSnapshotBodyBytes)
	}
	if compactBytes >= compactReadPayloadBound {
		t.Fatalf("compact read payloads total %d bytes, want < %d", compactBytes, compactReadPayloadBound)
	}

	// Every fold the switched consumers run sees the same facts.
	task, err := st.GetTask(ctx, order.TaskID)
	requireOK(t, err)
	orders, err := st.ListTaskWorkOrders(ctx, order.TaskID)
	requireOK(t, err)
	now := at.Add(time.Hour)
	recovery := config.DefaultRecoveryPolicy()
	flow := store.FoldTaskFlow(full, nextaction.NextAction{}, recovery, now)
	if encoded := string(core.JSONPayload(flow)); !strings.Contains(encoded, historical.Progress) {
		t.Fatalf("task flow does not fold the historical events: %s", encoded)
	}
	requireSameFold(t, "FoldTaskFlow", flow, store.FoldTaskFlow(compact, nextaction.NextAction{}, recovery, now))
	requireSameFold(t, "FilterFactoryActs", store.FilterFactoryActs(full, time.Time{}, 100), store.FilterFactoryActs(compact, time.Time{}, 100))
	requireSameFold(t, "FoldNextActionHistory", store.FoldNextActionHistory(full, orders), store.FoldNextActionHistory(compact, orders))
	requireSameFold(t, "PlanRevisionGateState", foldResults(store.PlanRevisionGateState(task, full)), foldResults(store.PlanRevisionGateState(task, compact)))
	requireSameFold(t, "CurrentReviewOrders", store.CurrentReviewOrders(orders, full), store.CurrentReviewOrders(orders, compact))
	requireSameFold(t, "InterruptedReviewRecoveryNeeded", store.InterruptedReviewRecoveryNeeded(task, orders, full), store.InterruptedReviewRecoveryNeeded(task, orders, compact))
	requireSameFold(t, "ReviewRecoveryNeeded", store.ReviewRecoveryNeeded(orders, full), store.ReviewRecoveryNeeded(orders, compact))
	requireSameFold(t, "ReviewVerdictDiagnostics", store.ReviewVerdictDiagnostics(orders, full, now), store.ReviewVerdictDiagnostics(orders, compact, now))
	requireSameFold(t, "MergeConflictDispatchState", store.MergeConflictDispatchState(full, "", "", orders), store.MergeConflictDispatchState(compact, "", "", orders))
	requireSameFold(t, "ActiveTaskContextReferences", foldResults(store.ActiveTaskContextReferences(full)), foldResults(store.ActiveTaskContextReferences(compact)))
	requireSameFold(t, "AtMergeGate", store.AtMergeGate(task, full), store.AtMergeGate(task, compact))
	requireSameFold(t, "LatestForgeFailure", store.LatestForgeFailure(full), store.LatestForgeFailure(compact))
	// The checkpoint match binds the retained attempt to its claim event.
	retained := claimed
	retained.LastAttemptID = claimed.AttemptID
	claim := core.WorkOrderClaimIdentity{WorkerID: claimed.WorkerID, ClaimantID: claimed.ClaimantID, SessionID: claimed.SessionID}
	matched := 0
	for i := range full {
		want := store.ReleasedCheckpointClaimEventMatches(full[i], retained, claim)
		if got := store.ReleasedCheckpointClaimEventMatches(compact[i], retained, claim); got != want {
			t.Fatalf("ReleasedCheckpointClaimEventMatches(event %d) compact=%v full=%v", full[i].ID, got, want)
		}
		if want {
			matched++
		}
	}
	if matched == 0 {
		t.Fatal("no claimed event matched the claim identity on either read")
	}
}

type compactEventIdentity struct {
	ID                                 int64
	TaskID, JobID, Kind, ActorID, Role string
	At                                 time.Time
}

func eventIdentity(e core.Event) compactEventIdentity {
	return compactEventIdentity{e.ID, e.TaskID, e.JobID, e.Kind, e.ActorID, string(e.ActorRole), e.At}
}

// exactJSON decodes payload keeping each number's exact text, so a changed
// numeric rendering is a difference.
func exactJSON(t *testing.T, payload json.RawMessage) any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("decode payload %q: %v", payload, err)
	}
	return value
}

func foldResults(values ...any) []any { return values }

func requireSameFold(t *testing.T, name string, full, compact any) {
	t.Helper()
	if !reflect.DeepEqual(full, compact) {
		t.Fatalf("%s differs between the reads:\nfull:    %#v\ncompact: %#v", name, full, compact)
	}
}
