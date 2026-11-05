package storetest

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
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
		"work_order.claimed": store.CompactEventPayload("work_order.claimed", core.JSONPayload(historical)),
		"work_order.updated": store.CompactEventPayload("work_order.updated", core.JSONPayload(updated)),
	}
	liveWant := store.WorkOrderEventPayload(claimed)

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
				t.Fatalf("historical %s event %d decodes to %v, want CompactEventPayload %v", f.Kind, f.ID, got, want)
			}
		} else if strings.HasPrefix(f.Kind, "work_order.") {
			compactWorkOrderEvents++
			if f.Kind == "work_order.claimed" {
				if got, want := exactJSON(t, f.Payload), exactJSON(t, liveWant); !reflect.DeepEqual(got, want) {
					t.Fatalf("live %s event %d decodes to %v, want WorkOrderEventPayload %v", f.Kind, f.ID, got, want)
				}
			}
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

// activityMarkerRoles names one fixture's tasks by the marker fold each proves.
type activityMarkerRoles struct {
	// claimed is an implement task whose user bounce a historical claim cleared
	// and whose latest merge attempt failed.
	claimed string
	// bounced is an implement task whose user bounce follows a historical claim.
	bounced string
	// review is a task with one claimed and one queued seat of a review round.
	review string
	// lastEvents maps each task to its newest seeded event ID.
	lastEvents map[string]int64
}

func (r activityMarkerRoles) ids() []string { return []string{r.claimed, r.bounced, r.review} }

// activityMarkerTimes fixes the instants both fixtures share, so their markers
// differ only by task and event identity.
type activityMarkerTimes struct {
	// now is the read instant and at is the first seeded event's time, after
	// every ordinary fixture event.
	now, at time.Time
	// claimLease is the seat's claim lease, which sets the horizon, and
	// renewedLease is its later renewal, which sets the seat's diagnostic.
	claimLease, renewedLease time.Time
	// expiredLease is the queued seat's elapsed claim lease.
	expiredLease time.Time
}

// seedActivityMarkerFixture creates the task, order, and event shape the marker
// folds read. Each historical work_order.claimed event embeds a snapshot body
// of compactSnapshotBodyBytes, and encode decides the payload the ledger holds.
func seedActivityMarkerFixture(t *testing.T, x Fixture, times activityMarkerTimes, encode func(kind string, payload json.RawMessage) json.RawMessage) activityMarkerRoles {
	t.Helper()
	st, ctx := x.Backend, x.Context
	claim := func(id string) core.WorkOrder {
		t.Helper()
		claimed, err := ClaimWorkOrder(ctx, st, id, core.WorkOrderClaim{ClaimantID: core.TaskRunClaimantID("activity-marker"), SessionID: "activity-marker-" + id, ClientToken: "activity-marker-token-" + id, Agent: "claude-code", Model: "claude-opus-5-5", Lease: 6 * time.Hour, ExecutionTimeout: 24 * time.Hour})
		requireOK(t, err)
		return claimed
	}
	historicalClaim := func(order core.WorkOrder, lease time.Time, rank int64) core.Event {
		t.Helper()
		order.LeaseExpiresAt = lease
		order.GovernanceSnapshot = &core.GovernanceSnapshot{Designs: []core.GovernanceDesignContext{{
			ID: "design-activity-marker", Title: "Activity marker", Category: "Component design", Version: 1,
			Content: compactSnapshotMarker + strings.Repeat("s", compactSnapshotBodyBytes),
			Governs: []core.GovernedScope{{Repository: "conveyor", Paths: []string{"internal/**"}}},
		}}}
		order.ServedRequirementSnapshot = []core.ServedRequirementContext{{ID: "req-activity-marker", Title: "Activity marker", Version: 1}}
		return core.Event{ID: rank, TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.claimed", At: times.at.Add(time.Duration(rank) * time.Second), Payload: encode("work_order.claimed", core.JSONPayload(order))}
	}
	userBounce := func(taskID string, rank int64) core.Event {
		return core.Event{ID: rank, TaskID: taskID, Kind: "pipeline.bounced", At: times.at.Add(time.Duration(rank) * time.Second), Payload: core.JSONPayload(map[string]any{"source": store.UserRequestChangesSource})}
	}
	roles := activityMarkerRoles{lastEvents: map[string]int64{}}
	seed := func(taskID string, events ...core.Event) {
		t.Helper()
		stored := x.SeedEvents(t, ctx, 0, events)
		roles.lastEvents[taskID] = stored[len(stored)-1].ID
	}

	claimedOrder := claim(newAggregateOrder(t, x).ID)
	roles.claimed = claimedOrder.TaskID
	seed(roles.claimed,
		userBounce(roles.claimed, 1),
		historicalClaim(claimedOrder, times.claimLease, 2),
		core.Event{ID: 3, TaskID: roles.claimed, Kind: "merge.failed", At: times.at.Add(3 * time.Second), Payload: core.JSONPayload(map[string]any{"forge_error_category": "merge_conflict", "error": "activity marker merge failure"})},
	)

	bouncedOrder := claim(newAggregateOrder(t, x).ID)
	roles.bounced = bouncedOrder.TaskID
	seed(roles.bounced,
		historicalClaim(bouncedOrder, times.claimLease, 1),
		userBounce(roles.bounced, 2),
	)

	review := newAggregateTask(t, x, core.StageReview)
	roles.review = review.ID
	var jobs []core.Job
	var orders []core.WorkOrder
	for seat := 1; seat <= 2; seat++ {
		id := review.ID + "-review-1-" + strconv.Itoa(seat)
		jobs = append(jobs, core.Job{ID: id, TaskID: review.ID, Stage: core.StageReview, State: core.JobPending})
		orders = append(orders, core.WorkOrder{ID: id, JobID: id, TaskID: review.ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: seat, QueueEnteredAt: times.now, QueueDeadline: times.now.Add(24 * time.Hour)})
	}
	requireOK(t, CreateReviewRound(ctx, st, review.ID, jobs, orders))
	claimedSeat := claim(orders[0].ID)
	queuedSeat, err := st.GetWorkOrder(ctx, orders[1].ID)
	requireOK(t, err)
	seed(roles.review,
		historicalClaim(claimedSeat, times.claimLease, 1),
		core.Event{ID: 2, TaskID: review.ID, JobID: claimedSeat.JobID, Kind: "work_order.lease_renewed", At: times.at.Add(2 * time.Second), Payload: core.JSONPayload(map[string]any{"id": claimedSeat.ID, "lease_expires_at": times.renewedLease})},
		historicalClaim(queuedSeat, times.expiredLease, 3),
	)
	return roles
}

// activityMarkerRead is one entry point that reaches listActivityMarkers. A read
// without a horizon returns the zero time and hasHorizon false.
type activityMarkerRead struct {
	name       string
	hasHorizon bool
	read       func(ctx context.Context, st store.Backend, ids []string, now time.Time) ([]store.ActivityMarker, time.Time, error)
}

var activityMarkerReads = []activityMarkerRead{
	{name: "ListActivityMarkers", read: func(ctx context.Context, st store.Backend, _ []string, _ time.Time) ([]store.ActivityMarker, time.Time, error) {
		markers, err := st.ListActivityMarkers(ctx)
		return markers, time.Time{}, err
	}},
	{name: "ListActivityMarkersForTasks", read: func(ctx context.Context, st store.Backend, ids []string, _ time.Time) ([]store.ActivityMarker, time.Time, error) {
		markers, err := st.ListActivityMarkersForTasks(ctx, ids)
		return markers, time.Time{}, err
	}},
	{name: "ReadActivityInputs-workspace", hasHorizon: true, read: func(ctx context.Context, st store.Backend, _ []string, now time.Time) ([]store.ActivityMarker, time.Time, error) {
		inputs, err := st.ReadActivityInputs(ctx, nil, now)
		return inputs.Markers, inputs.ValidUntil, err
	}},
	{name: "ReadActivityInputs-tasks", hasHorizon: true, read: func(ctx context.Context, st store.Backend, ids []string, now time.Time) ([]store.ActivityMarker, time.Time, error) {
		inputs, err := st.ReadActivityInputs(ctx, ids, now)
		return inputs.Markers, inputs.ValidUntil, err
	}},
}

// roleMarkers returns the marker of each role's task, encoded with its task ID
// replaced and its event ID checked and cleared, so two fixtures' markers
// compare byte for byte.
func roleMarkers(t *testing.T, label string, roles activityMarkerRoles, markers []store.ActivityMarker) map[string]string {
	t.Helper()
	byTask := make(map[string]store.ActivityMarker, len(markers))
	for _, marker := range markers {
		byTask[marker.TaskID] = marker
	}
	encoded := make(map[string]string, 3)
	for name, id := range map[string]string{"claimed": roles.claimed, "bounced": roles.bounced, "review": roles.review} {
		marker, ok := byTask[id]
		if !ok {
			t.Fatalf("%s returned no marker for the %s task %s", label, name, id)
		}
		if marker.LastEventID != roles.lastEvents[id] {
			t.Fatalf("%s %s marker last event %d, want %d", label, name, marker.LastEventID, roles.lastEvents[id])
		}
		marker.LastEventID = 0
		payload, err := json.Marshal(marker)
		requireOK(t, err)
		encoded[name] = strings.ReplaceAll(string(payload), id, "<task>")
	}
	return encoded
}

// runActivityMarkerCompactReads pins every entry point that reaches
// listActivityMarkers over historical work_order.claimed events that embed
// snapshot bodies, on implement and review orders. The markers and the review
// lease horizon equal those of the same fixture with compact payloads, and the
// bytes the database sends under the read label stay below
// compactReadPayloadBound, so a return to the raw payload column fails
// (component-task-lifecycle, Compact event reads; component-persistence,
// Database read accounting).
func runActivityMarkerCompactReads(t *testing.T, x Fixture) {
	if x.SeedEvents == nil {
		t.Fatal("factory must provide SeedEvents for activity marker conformance")
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	at := seedTime()
	times := activityMarkerTimes{
		now: now, at: at,
		claimLease: at.Add(10 * time.Minute), renewedLease: at.Add(20 * time.Minute),
		expiredLease: now.Add(-time.Minute),
	}
	historical := seedActivityMarkerFixture(t, x, times, func(_ string, payload json.RawMessage) json.RawMessage { return payload })

	// A second workspace holds the same fixture with compact payloads, so each
	// whole-workspace read covers one fixture on the durable backends.
	compactWorkspace := x.Workspace + "-compact"
	compactFixture := x
	compactFixture.Workspace = compactWorkspace
	compactFixture.Context = store.WithWorkspace(x.Context, compactWorkspace)
	compactFixture.Config = &config.Config{Workspace: compactWorkspace, Repos: x.Config.Repos, Routing: x.Config.Routing}
	_, err := x.Backend.BootstrapWorkspaceConfig(compactFixture.Context, compactFixture.Config)
	requireOK(t, err)
	compact := seedActivityMarkerFixture(t, compactFixture, times, store.CompactEventPayload)

	// The fixture is not vacuous: the ledger holds the bodies and the read
	// ledger sees them when a read selects the raw column.
	drain := func() map[string]uint64 {
		if x.DrainReadBytes == nil {
			return nil
		}
		return x.DrainReadBytes()
	}
	drain()
	full, err := x.Backend.ListEvents(dbmetrics.WithReadLabel(x.Context, "activity-marker-compact-full"), historical.review)
	requireOK(t, err)
	bodies := 0
	for _, event := range full {
		if bytes.Contains(event.Payload, []byte(compactSnapshotMarker)) {
			bodies++
		}
	}
	if bodies != 2 {
		t.Fatalf("review ledger holds %d snapshot bodies, want 2", bodies)
	}
	if received := drain(); received != nil {
		if got := received["activity-marker-compact-full"]; got < 2*compactSnapshotBodyBytes {
			t.Fatalf("the raw read received %d bytes, want at least the %d seeded body bytes", got, 2*compactSnapshotBodyBytes)
		}
	}

	for _, entry := range activityMarkerReads {
		t.Run(entry.name, func(t *testing.T) {
			label := "activity-marker-compact-" + entry.name
			drain()
			markers, horizon, err := entry.read(dbmetrics.WithReadLabel(x.Context, label), x.Backend, historical.ids(), now)
			requireOK(t, err)
			if received := drain(); received != nil {
				got := received[label]
				if got == 0 || got >= compactReadPayloadBound {
					t.Fatalf("%s received %d bytes, want more than 0 and fewer than %d", entry.name, got, compactReadPayloadBound)
				}
				t.Logf("%s received %d bytes", entry.name, got)
			}
			compactMarkers, compactHorizon, err := entry.read(compactFixture.Context, x.Backend, compact.ids(), now)
			requireOK(t, err)

			got, want := roleMarkers(t, entry.name, historical, markers), roleMarkers(t, entry.name+" compact", compact, compactMarkers)
			for role := range want {
				if got[role] != want[role] {
					t.Fatalf("%s %s marker differs from the compact fixture:\nhistorical %s\ncompact    %s", entry.name, role, got[role], want[role])
				}
			}
			if !horizon.Equal(compactHorizon) {
				t.Fatalf("%s horizon %s, compact fixture %s", entry.name, horizon, compactHorizon)
			}
			if entry.hasHorizon && !horizon.Equal(times.claimLease) {
				t.Fatalf("%s horizon %s, want the historical claim's lease %s", entry.name, horizon, times.claimLease)
			}
			requireActivityMarkerFolds(t, entry.name, historical, markers, times)
		})
	}
}

// requireActivityMarkerFolds pins each marker field the fixture's historical
// events decide, so the comparison with the compact fixture is not vacuous.
func requireActivityMarkerFolds(t *testing.T, label string, roles activityMarkerRoles, markers []store.ActivityMarker, times activityMarkerTimes) {
	t.Helper()
	byTask := make(map[string]store.ActivityMarker, len(markers))
	for _, marker := range markers {
		byTask[marker.TaskID] = marker
	}
	// A historical implement claim clears the user bounce only through its
	// stage key, and a later bounce stays pending.
	claimed := byTask[roles.claimed]
	if claimed.UserChangesRequested {
		t.Fatalf("%s: the historical implement claim did not clear the user bounce", label)
	}
	if failure := claimed.ForgeFailure; failure == nil || failure.Category != "merge_conflict" || failure.Detail != "activity marker merge failure" {
		t.Fatalf("%s: forge failure = %+v, want the seeded merge failure", label, failure)
	}
	if !byTask[roles.bounced].UserChangesRequested {
		t.Fatalf("%s: the user bounce after the historical claim is not pending", label)
	}
	diagnostics := byTask[roles.review].ReviewDiagnostics
	if len(diagnostics) != 2 {
		t.Fatalf("%s: review diagnostics = %+v, want one per seat", label, diagnostics)
	}
	for _, diagnostic := range diagnostics {
		switch diagnostic.ReviewSeat {
		case 1:
			if diagnostic.Status != store.ReviewClaimedWithoutVerdict || !diagnostic.LeaseExpiresAt.Equal(times.renewedLease) || !diagnostic.ClaimedAt.Equal(times.at.Add(time.Second)) {
				t.Fatalf("%s: claimed seat diagnostic = %+v, want the renewed lease %s", label, diagnostic, times.renewedLease)
			}
		case 2:
			if diagnostic.Status != store.ReviewExpiredWithoutVerdict || !diagnostic.LeaseExpiresAt.Equal(times.expiredLease) {
				t.Fatalf("%s: queued seat diagnostic = %+v, want the historical claim's elapsed lease %s", label, diagnostic, times.expiredLease)
			}
		default:
			t.Fatalf("%s: unexpected review seat diagnostic %+v", label, diagnostic)
		}
	}
}
