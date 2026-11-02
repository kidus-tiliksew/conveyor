package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// ClaimSessionSnapshotBytes is the design body every snapshot-bearing order
// of the claim session fixtures pins. A read that decodes even one snapshot
// reports at least this many bytes.
const ClaimSessionSnapshotBytes = 64 << 10

// ClaimSessionScaleOrders is the number of snapshot-bearing orders that
// RunClaimSessionAndActivityReadsAtScale adds beside its named, holder, and
// template orders: 200 orders of 64 KiB, about 13 MB that a workspace-wide
// or snapshot-bearing read would decode.
const ClaimSessionScaleOrders = 200

// ClaimSessionGovernance returns the governance snapshot the claim session
// fixtures pin: one design body of ClaimSessionSnapshotBytes.
func ClaimSessionGovernance() core.GovernanceSnapshot {
	return core.GovernanceSnapshot{
		Designs:                []core.GovernanceDesignContext{{ID: "component-claim-session-fixture", Title: "Claim session fixture", Category: "component", Version: 1, Content: strings.Repeat("g", ClaimSessionSnapshotBytes), Governs: []core.GovernedScope{}}},
		Decisions:              []core.Decision{},
		PendingDesignProposals: []core.PendingSystemDesignProposal{},
	}
}

// ClaimSessionRequirements returns the served-requirement snapshot the claim
// session fixtures pin.
func ClaimSessionRequirements() []core.ServedRequirementContext {
	return []core.ServedRequirementContext{{ID: "req-claim-session-fixture", Version: 1, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Check the session"}}}}
}

// LegacyClaimSessionAnswer is the answer the claim session check of
// Server.authorizeClaimantSession gave before it moved to
// store.ResolveClaimSession: it reads the named order through GetWorkOrder
// and, when the order is missing or its session differs, scans every order
// ListWorkOrders returns for another order holding the non-empty session. It
// returns nil when the named order holds the session. Tests compare the
// current answer with it over the same rows.
func LegacyClaimSessionAnswer(ctx context.Context, st store.Store, workOrderID, sessionID string) error {
	order, err := st.GetWorkOrder(ctx, workOrderID)
	if err != nil || order.SessionID == "" || order.SessionID != sessionID {
		orders, listErr := st.ListWorkOrders(ctx)
		if listErr != nil {
			return listErr
		}
		for _, candidate := range orders {
			if sessionID != "" && candidate.ID != workOrderID && candidate.SessionID == sessionID {
				return store.ErrWorkOrderClaimUnauthorized
			}
		}
		return store.ErrWorkOrderClaimLost
	}
	return nil
}

// WorkOrderReadRecorder wraps a store and records, for every work-order read
// it serves, the call, the rows returned, and the encoded bytes of the
// served-requirement and governance snapshots those rows carry. A durable
// backend fills a snapshot field only by decoding its selected column, so
// zero snapshot bytes means no read selected a snapshot body.
type WorkOrderReadRecorder struct {
	store.Store

	mu            sync.Mutex
	calls         map[string]int
	rows          int
	snapshotBytes int
}

// NewWorkOrderReadRecorder wraps st.
func NewWorkOrderReadRecorder(st store.Store) *WorkOrderReadRecorder {
	return &WorkOrderReadRecorder{Store: st, calls: map[string]int{}}
}

// Reset clears every count.
func (r *WorkOrderReadRecorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls, r.rows, r.snapshotBytes = map[string]int{}, 0, 0
}

// Calls returns the number of calls of one recorded method.
func (r *WorkOrderReadRecorder) Calls(method string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[method]
}

// Rows returns the number of work orders the recorded reads returned.
func (r *WorkOrderReadRecorder) Rows() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rows
}

// SnapshotBytes returns the encoded snapshot bytes the recorded reads
// returned.
func (r *WorkOrderReadRecorder) SnapshotBytes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshotBytes
}

func (r *WorkOrderReadRecorder) record(method string, orders []core.WorkOrder) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls[method]++
	r.rows += len(orders)
	r.snapshotBytes += encodedSnapshotBytes(orders)
}

func encodedSnapshotBytes(orders []core.WorkOrder) int {
	total := 0
	for _, order := range orders {
		if order.ServedRequirementSnapshot != nil {
			encoded, _ := json.Marshal(order.ServedRequirementSnapshot)
			total += len(encoded)
		}
		if order.GovernanceSnapshot != nil {
			encoded, _ := json.Marshal(order.GovernanceSnapshot)
			total += len(encoded)
		}
	}
	return total
}

func (r *WorkOrderReadRecorder) GetWorkOrder(ctx context.Context, id string) (core.WorkOrder, error) {
	order, err := r.Store.GetWorkOrder(ctx, id)
	var rows []core.WorkOrder
	if err == nil {
		rows = []core.WorkOrder{order}
	}
	r.record("GetWorkOrder", rows)
	return order, err
}

func (r *WorkOrderReadRecorder) GetWorkOrderWithoutSnapshots(ctx context.Context, id string) (core.WorkOrder, error) {
	order, err := r.Store.GetWorkOrderWithoutSnapshots(ctx, id)
	var rows []core.WorkOrder
	if err == nil {
		rows = []core.WorkOrder{order}
	}
	r.record("GetWorkOrderWithoutSnapshots", rows)
	return order, err
}

func (r *WorkOrderReadRecorder) OtherWorkOrderHoldsSession(ctx context.Context, workOrderID, sessionID string) (bool, error) {
	held, err := r.Store.OtherWorkOrderHoldsSession(ctx, workOrderID, sessionID)
	r.record("OtherWorkOrderHoldsSession", nil)
	return held, err
}

func (r *WorkOrderReadRecorder) ListWorkOrders(ctx context.Context) ([]core.WorkOrder, error) {
	orders, err := r.Store.ListWorkOrders(ctx)
	r.record("ListWorkOrders", orders)
	return orders, err
}

func (r *WorkOrderReadRecorder) ListLiveWorkOrders(ctx context.Context) ([]core.WorkOrder, error) {
	orders, err := r.Store.ListLiveWorkOrders(ctx)
	r.record("ListLiveWorkOrders", orders)
	return orders, err
}

func (r *WorkOrderReadRecorder) ListWorkOrdersForTasks(ctx context.Context, taskIDs []string) ([]core.WorkOrder, error) {
	orders, err := r.Store.ListWorkOrdersForTasks(ctx, taskIDs)
	r.record("ListWorkOrdersForTasks", orders)
	return orders, err
}

func (r *WorkOrderReadRecorder) ListTaskWorkOrders(ctx context.Context, taskID string) ([]core.WorkOrder, error) {
	orders, err := r.Store.ListTaskWorkOrders(ctx, taskID)
	r.record("ListTaskWorkOrders", orders)
	return orders, err
}

func (r *WorkOrderReadRecorder) ListTaskWorkOrdersStored(ctx context.Context, taskID string) ([]core.WorkOrder, error) {
	orders, err := r.Store.ListTaskWorkOrdersStored(ctx, taskID)
	r.record("ListTaskWorkOrdersStored", orders)
	return orders, err
}

func (r *WorkOrderReadRecorder) ListTaskWorkOrdersSnapshot(ctx context.Context, taskID string) ([]core.WorkOrder, error) {
	orders, err := r.Store.ListTaskWorkOrdersSnapshot(ctx, taskID)
	r.record("ListTaskWorkOrdersSnapshot", orders)
	return orders, err
}

func (r *WorkOrderReadRecorder) ReadActivityInputs(ctx context.Context, taskIDs []string, now time.Time) (store.ActivityInputs, error) {
	inputs, err := r.Store.ReadActivityInputs(ctx, taskIDs, now)
	r.record("ReadActivityInputs", inputs.Orders)
	return inputs, err
}

// claimSessionOrder creates one verify order and claims it under session with
// both snapshots pinned, or leaves it queued when session is empty.
func claimSessionOrder(t *testing.T, x Fixture, session string, lease time.Duration) core.WorkOrder {
	t.Helper()
	order := newAggregateOrder(t, x, core.StageVerify)
	if session == "" {
		return order
	}
	governance := ClaimSessionGovernance()
	claimed, err := ClaimWorkOrder(x.Context, x.Backend, order.ID, core.WorkOrderClaim{ClaimantID: core.TaskRunClaimantID("claim-session"), SessionID: session, ClientToken: session, Lease: lease, ExecutionTimeout: time.Hour, Requirements: ClaimSessionRequirements(), Governance: &governance})
	requireOK(t, err)
	return claimed
}

func sameClaimSessionAnswer(got, want error) bool {
	if want == nil {
		return got == nil
	}
	return errors.Is(got, want)
}

// runClaimSessionReads proves the two snapshot-free reads of the claim
// session check and the answers ResolveClaimSession derives from them on
// every backend (component-work-orders, Snapshot-free fold reads).
func runClaimSessionReads(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	suffix := core.NewTaskID()
	named := claimSessionOrder(t, x, "named-"+suffix, time.Hour)
	holder := claimSessionOrder(t, x, "holder-"+suffix, time.Hour)
	expired := claimSessionOrder(t, x, "expired-"+suffix, time.Millisecond)
	queued := claimSessionOrder(t, x, "", 0)

	// Another workspace's order holds its own session.
	foreignWorkspace := x.Workspace + "-claim-session-foreign"
	foreignCtx := store.WithWorkspace(ctx, foreignWorkspace)
	_, err := st.BootstrapWorkspaceConfig(foreignCtx, &config.Config{Workspace: foreignWorkspace, Repos: x.Config.Repos})
	requireOK(t, err)
	foreign := claimSessionOrder(t, Fixture{Backend: st, Context: foreignCtx, Workspace: foreignWorkspace, Config: x.Config}, "foreign-"+suffix, time.Hour)
	time.Sleep(20 * time.Millisecond) // the expired order's lease passes

	holds := func(ctx context.Context, workOrderID, sessionID string) bool {
		t.Helper()
		held, err := st.OtherWorkOrderHoldsSession(ctx, workOrderID, sessionID)
		requireOK(t, err)
		return held
	}
	for _, test := range []struct {
		name               string
		ctx                context.Context
		workOrder, session string
		want               bool
	}{
		{"no other holder", ctx, named.ID, "nobody-" + suffix, false},
		{"another order holds the session", ctx, named.ID, holder.SessionID, true},
		{"a missing order and another holder", ctx, "missing-" + suffix, holder.SessionID, true},
		{"only the named order holds the session", ctx, named.ID, named.SessionID, false},
		{"empty session", ctx, named.ID, "", false},
		{"empty session on a queued order", ctx, queued.ID, "", false},
		{"only another workspace's order holds the session", ctx, named.ID, foreign.SessionID, false},
		{"this workspace's holder is foreign to the other workspace", foreignCtx, foreign.ID, holder.SessionID, false},
		{"the other workspace's own holder", foreignCtx, "missing-" + suffix, foreign.SessionID, true},
		{"the holder's lease expired", ctx, named.ID, expired.SessionID, false},
	} {
		if got := holds(test.ctx, test.workOrder, test.session); got != test.want {
			t.Fatalf("OtherWorkOrderHoldsSession %s (%s, %q) = %v, want %v", test.name, test.workOrder, test.session, got, test.want)
		}
	}

	// GetWorkOrderWithoutSnapshots is GetWorkOrder with both snapshots and
	// the assignee cleared, with the same not-found answer.
	for _, id := range []string{named.ID, holder.ID, expired.ID, queued.ID} {
		full, err := st.GetWorkOrder(ctx, id)
		requireOK(t, err)
		got, err := st.GetWorkOrderWithoutSnapshots(ctx, id)
		requireOK(t, err)
		if id != queued.ID && (full.GovernanceSnapshot == nil || len(full.GovernanceSnapshot.Designs[0].Content) != ClaimSessionSnapshotBytes) {
			t.Fatalf("GetWorkOrder(%s) lost the pinned governance snapshot", id)
		}
		if n := encodedSnapshotBytes([]core.WorkOrder{got}); n != 0 {
			t.Fatalf("GetWorkOrderWithoutSnapshots(%s) decoded %d snapshot bytes, want 0", id, n)
		}
		full.Assignee = nil
		requireSameOrders(t, "GetWorkOrderWithoutSnapshots("+id+")", []core.WorkOrder{got}, withoutSnapshots([]core.WorkOrder{full}))
	}
	if expiredRead, err := st.GetWorkOrderWithoutSnapshots(ctx, expired.ID); err != nil || expiredRead.SessionID != "" || expiredRead.State != core.WorkOrderQueued {
		t.Fatalf("expired claim read = state %s session %q err %v, want the projected queued order without a session", expiredRead.State, expiredRead.SessionID, err)
	}
	for _, missing := range []struct {
		ctx context.Context
		id  string
	}{{ctx, "missing-" + suffix}, {ctx, foreign.ID}, {foreignCtx, named.ID}} {
		_, fullErr := st.GetWorkOrder(missing.ctx, missing.id)
		_, err := st.GetWorkOrderWithoutSnapshots(missing.ctx, missing.id)
		if !errors.Is(err, store.ErrNotFound) || !errors.Is(fullErr, store.ErrNotFound) {
			t.Fatalf("GetWorkOrderWithoutSnapshots(%s) err=%v, GetWorkOrder err=%v, want both not found", missing.id, err, fullErr)
		}
	}

	// ResolveClaimSession answers as the former ListWorkOrders scan did. The
	// other workspace's holder is compared with the durable scan's answer,
	// which never read another workspace's rows.
	for _, test := range []struct {
		name               string
		workOrder, session string
		want               error
		legacy             bool
	}{
		{"own session", named.ID, named.SessionID, nil, true},
		{"mismatched session", named.ID, "nobody-" + suffix, store.ErrWorkOrderClaimLost, true},
		{"foreign session", named.ID, holder.SessionID, store.ErrWorkOrderClaimUnauthorized, true},
		{"missing order", "missing-" + suffix, "nobody-" + suffix, store.ErrWorkOrderClaimLost, true},
		{"missing order with a held session", "missing-" + suffix, holder.SessionID, store.ErrWorkOrderClaimUnauthorized, true},
		{"empty session", named.ID, "", store.ErrWorkOrderClaimLost, true},
		{"expired own claim", expired.ID, expired.SessionID, store.ErrWorkOrderClaimLost, true},
		{"another workspace's session", named.ID, foreign.SessionID, store.ErrWorkOrderClaimLost, false},
	} {
		if test.legacy {
			if legacy := LegacyClaimSessionAnswer(ctx, st, test.workOrder, test.session); !sameClaimSessionAnswer(legacy, test.want) {
				t.Fatalf("legacy answer for %s = %v, want %v", test.name, legacy, test.want)
			}
		}
		order, err := store.ResolveClaimSession(ctx, st, test.workOrder, test.session)
		if !sameClaimSessionAnswer(err, test.want) {
			t.Fatalf("ResolveClaimSession %s = %v, want %v", test.name, err, test.want)
		}
		if test.want == nil && (order.ID != test.workOrder || order.SessionID != test.session || order.ClaimantID != named.ClaimantID || encodedSnapshotBytes([]core.WorkOrder{order}) != 0) {
			t.Fatalf("ResolveClaimSession %s returned %+v", test.name, order)
		}
	}
}

// ClaimSessionScale configures RunClaimSessionAndActivityReadsAtScale for one
// backend.
type ClaimSessionScale struct {
	// Seed adds count snapshot-bearing orders to the bound workspace by
	// copying the queued template order and then writing governance and
	// served, the encoded snapshots, into every order of the workspace. A
	// durable backend seeds with set-based SQL; nil seeds through the public
	// store API, for the volatile backend.
	Seed func(t *testing.T, ctx context.Context, template core.WorkOrder, count int, governance, served []byte)
	// DrainReadBytes drains the backend's read ledger and returns the bytes
	// received per read label. Durable backends provide it; nil skips the
	// byte bounds.
	DrainReadBytes func() map[string]uint64
}

// RenewFunc is workorder.Service.RenewClaimWithLease over the given store.
// storetest cannot import workorder, because workorder tests import
// storetest, so each test binary supplies it.
type RenewFunc func(st store.Store) func(ctx context.Context, claim core.WorkOrderClaimIdentity, workOrderID string, lease time.Duration) (core.WorkOrder, error)

// seedClaimSessionScale adds ClaimSessionScaleOrders snapshot-bearing orders
// to the bound workspace beside the queued template order.
func seedClaimSessionScale(t *testing.T, x Fixture, scale ClaimSessionScale, template core.WorkOrder, suffix string) {
	t.Helper()
	if scale.Seed != nil {
		governanceJSON, err := json.Marshal(ClaimSessionGovernance())
		requireOK(t, err)
		servedJSON, err := json.Marshal(ClaimSessionRequirements())
		requireOK(t, err)
		scale.Seed(t, x.Context, template, ClaimSessionScaleOrders, governanceJSON, servedJSON)
	} else {
		for i := range ClaimSessionScaleOrders {
			claimSessionOrder(t, x, fmt.Sprintf("seed-%s-%d", suffix, i), time.Hour)
		}
	}
	full, err := x.Backend.ListWorkOrders(x.Context)
	requireOK(t, err)
	if len(full) < ClaimSessionScaleOrders+2 || encodedSnapshotBytes(full) < ClaimSessionScaleOrders*ClaimSessionSnapshotBytes {
		t.Fatalf("fixture holds %d orders and %d snapshot bytes, want at least %d orders and %d bytes", len(full), encodedSnapshotBytes(full), ClaimSessionScaleOrders+2, ClaimSessionScaleOrders*ClaimSessionSnapshotBytes)
	}
}

// RunClaimSessionAndActivityReadsAtScale seeds ClaimSessionScaleOrders orders
// that each pin a ClaimSessionSnapshotBytes governance snapshot, wraps the
// store in a WorkOrderReadRecorder, and requires the claim session check and
// the whole-workspace activity reads to make no ListWorkOrders call, decode
// zero snapshot bytes, and answer as before over the same rows. With
// DrainReadBytes it also bounds the bytes each read receives from the
// database, measured under a test read label (component-work-orders,
// Snapshot-free fold reads; component-persistence, Board activity validation
// reads).
func RunClaimSessionAndActivityReadsAtScale(t *testing.T, x Fixture, scale ClaimSessionScale) {
	ctx := x.Context
	suffix := core.NewTaskID()
	named := claimSessionOrder(t, x, "named-"+suffix, time.Hour)
	holder := claimSessionOrder(t, x, "holder-"+suffix, time.Hour)
	template := claimSessionOrder(t, x, "", 0)
	seedClaimSessionScale(t, x, scale, template, suffix)
	drain := func() map[string]uint64 {
		if scale.DrainReadBytes == nil {
			return nil
		}
		return scale.DrainReadBytes()
	}
	if scale.DrainReadBytes != nil {
		// The counter sees the snapshot bodies whenever a read selects them.
		drain()
		if _, err := x.Backend.ListWorkOrders(dbmetrics.WithReadLabel(ctx, "claim-session-scale-full")); err != nil {
			t.Fatal(err)
		}
		if got := drain()["claim-session-scale-full"]; got < ClaimSessionScaleOrders*ClaimSessionSnapshotBytes {
			t.Fatalf("the full read received %d bytes, want at least the %d seeded snapshot bytes", got, ClaimSessionScaleOrders*ClaimSessionSnapshotBytes)
		}
	}

	recorder := NewWorkOrderReadRecorder(x.Backend)
	for _, test := range []struct {
		name, workOrder, session string
		want                     error
	}{
		{"own", named.ID, named.SessionID, nil},
		{"mismatched", named.ID, "nobody-" + suffix, store.ErrWorkOrderClaimLost},
		{"missing", "missing-" + suffix, "nobody-" + suffix, store.ErrWorkOrderClaimLost},
		{"foreign", named.ID, holder.SessionID, store.ErrWorkOrderClaimUnauthorized},
		{"missing-foreign", "missing-" + suffix, holder.SessionID, store.ErrWorkOrderClaimUnauthorized},
		{"empty", named.ID, "", store.ErrWorkOrderClaimLost},
	} {
		legacy := LegacyClaimSessionAnswer(ctx, x.Backend, test.workOrder, test.session)
		if !sameClaimSessionAnswer(legacy, test.want) {
			t.Fatalf("%s: legacy answer %v, want %v", test.name, legacy, test.want)
		}
		recorder.Reset()
		drain()
		label := "claim-session-scale-" + test.name
		_, err := store.ResolveClaimSession(dbmetrics.WithReadLabel(ctx, label), recorder, test.workOrder, test.session)
		if !sameClaimSessionAnswer(err, legacy) {
			t.Fatalf("%s: ResolveClaimSession = %v, want the legacy answer %v", test.name, err, legacy)
		}
		if recorder.Calls("ListWorkOrders") != 0 || recorder.SnapshotBytes() != 0 || recorder.Rows() > 1 {
			t.Fatalf("%s: ListWorkOrders calls %d, snapshot bytes %d, rows %d; want 0, 0, at most the named order", test.name, recorder.Calls("ListWorkOrders"), recorder.SnapshotBytes(), recorder.Rows())
		}
		if test.session == "" && (recorder.Calls("GetWorkOrderWithoutSnapshots") != 0 || recorder.Calls("OtherWorkOrderHoldsSession") != 0) {
			t.Fatalf("empty session read the store")
		}
		if bytes := drain(); bytes != nil {
			got := bytes[label]
			if got >= ClaimSessionSnapshotBytes || (test.session != "" && got == 0) {
				t.Fatalf("%s: the session check received %d bytes, want more than 0 and fewer than one %d-byte snapshot", test.name, got, ClaimSessionSnapshotBytes)
			}
			t.Logf("%s: session check received %d bytes", test.name, got)
		}
	}

	// The whole-workspace activity reads decode no snapshot byte and return
	// the full read's orders without snapshots and the task-scoped read's
	// markers.
	bound := uint64(ClaimSessionScaleOrders * ClaimSessionSnapshotBytes / 8)
	full, err := x.Backend.ListWorkOrders(ctx)
	requireOK(t, err)
	now := time.Now().UTC()
	recorder.Reset()
	drain()
	inputs, err := recorder.ReadActivityInputs(dbmetrics.WithReadLabel(ctx, "claim-session-scale-inputs"), nil, now)
	requireOK(t, err)
	if bytes := drain(); bytes != nil {
		if got := bytes["claim-session-scale-inputs"]; got == 0 || got >= bound {
			t.Fatalf("ReadActivityInputs(nil) received %d bytes, want more than 0 and fewer than %d", got, bound)
		} else {
			t.Logf("ReadActivityInputs(nil) received %d bytes", got)
		}
	}
	if recorder.SnapshotBytes() != 0 || len(inputs.Orders) != len(full) {
		t.Fatalf("ReadActivityInputs(nil) returned %d orders with %d snapshot bytes, want %d orders and 0 bytes", len(inputs.Orders), recorder.SnapshotBytes(), len(full))
	}
	requireSameOrders(t, "ReadActivityInputs(nil).Orders", inputs.Orders, withoutSnapshots(full))
	taskIDs := make([]string, 0, len(inputs.Markers))
	for _, marker := range inputs.Markers {
		taskIDs = append(taskIDs, marker.TaskID)
	}
	scoped, err := x.Backend.ReadActivityInputs(ctx, taskIDs, now)
	requireOK(t, err)
	requireSameMarkers(t, "ReadActivityInputs(nil).Markers", inputs.Markers, scoped.Markers)
	if !inputs.ValidUntil.Equal(scoped.ValidUntil) {
		t.Fatalf("whole-workspace horizon %s, task-scoped horizon %s", inputs.ValidUntil, scoped.ValidUntil)
	}
	drain()
	markers, err := recorder.ListActivityMarkers(dbmetrics.WithReadLabel(ctx, "claim-session-scale-markers"))
	requireOK(t, err)
	if bytes := drain(); bytes != nil {
		if got := bytes["claim-session-scale-markers"]; got == 0 || got >= bound {
			t.Fatalf("ListActivityMarkers received %d bytes, want more than 0 and fewer than %d", got, bound)
		} else {
			t.Logf("ListActivityMarkers received %d bytes", got)
		}
	}
	if recorder.Calls("ListWorkOrders") != 0 {
		t.Fatalf("activity reads called ListWorkOrders %d times", recorder.Calls("ListWorkOrders"))
	}
	requireSameMarkers(t, "ListActivityMarkers", markers, inputs.Markers)
}

func requireSameMarkers(t *testing.T, label string, got, want []store.ActivityMarker) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	requireOK(t, err)
	wantJSON, err := json.Marshal(want)
	requireOK(t, err)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("%s differs:\ngot  %s\nwant %s", label, gotJSON, wantJSON)
	}
}

// RenewWorkerClaimCommand records the order a renewal command returns, so a
// refusal that returned a snapshot-bearing order would count.
func (r *WorkOrderReadRecorder) RenewWorkerClaimCommand(ctx context.Context, lease taskops.TaskLease, workOrderID string, claim core.WorkOrderClaimIdentity, duration time.Duration) (core.WorkOrder, error) {
	order, err := r.Store.RenewWorkerClaimCommand(ctx, lease, workOrderID, claim, duration)
	var rows []core.WorkOrder
	if order.ID != "" {
		rows = []core.WorkOrder{order}
	}
	r.record("RenewWorkerClaimCommand", rows)
	return order, err
}

// RunRenewalReads renews through renew, workorder.Service.RenewClaimWithLease,
// over a WorkOrderReadRecorder in a workspace that holds
// ClaimSessionScaleOrders snapshot-bearing orders. Every refusal must return
// the error the renewal returned before (claim lost, not found, released at a
// checkpoint, preempted, or cancelled), decode zero snapshot bytes, and make
// no ListWorkOrders call. With DrainReadBytes each refusal must also receive
// fewer bytes than one snapshot from the database, which bounds the command's
// own reads. A renewal of the own live claim must extend the lease without
// passing the fixed execution deadline, keep the attempt, persist the lease,
// and keep both snapshots; a renewal of the same claim's completed order
// returns that order with its snapshots (component-work-orders, Snapshot-free
// fold reads; req-delegated-execution AC-1.1).
func RunRenewalReads(t *testing.T, x Fixture, scale ClaimSessionScale, renew RenewFunc) {
	st, ctx := x.Backend, x.Context
	suffix := core.NewTaskID()
	claimant := core.TaskRunClaimantID("claim-session")
	live := claimSessionOrder(t, x, "live-"+suffix, time.Hour)
	holder := claimSessionOrder(t, x, "holder-"+suffix, time.Hour)
	expired := claimSessionOrder(t, x, "expired-"+suffix, time.Millisecond)
	released := claimSessionOrder(t, x, "released-"+suffix, time.Hour)
	_, err := ReleaseWorkerClaim(ctx, st, released.ID, "", core.WorkOrderRelease{SessionID: released.SessionID, Reason: "fixture released", Cause: core.WorkOrderReleaseCauseOperatorAction, Outcome: core.WorkOrderOutcomeReleased})
	requireOK(t, err)
	checkpoint := claimSessionOrder(t, x, "checkpoint-"+suffix, time.Hour)
	_, err = ReleaseWorkerClaim(ctx, st, checkpoint.ID, "", core.WorkOrderRelease{SessionID: checkpoint.SessionID, Reason: core.WorkOrderReleaseReasonOperatorCheckpointReached, Cause: core.WorkOrderReleaseCauseOperatorAction, Outcome: core.WorkOrderOutcomeReleased})
	requireOK(t, err)
	preempted := claimSessionOrder(t, x, "preempted-"+suffix, time.Hour)
	_, err = taskops.ExecuteWorkOrder(ctx, st, preempted.TaskID, core.WorkOrderCmdPreempt, func(lease taskops.TaskLease) (store.WorkOrderPreemptResult, error) {
		return st.PreemptWorkOrderCommand(ctx, lease, store.WorkOrderPreemptRequest{WorkOrderID: preempted.ID, RequestID: "preempt-" + suffix, Reason: "fixture preempt"})
	})
	requireOK(t, err)
	cancelled := claimSessionOrder(t, x, "cancelled-"+suffix, time.Hour)
	_, err = taskops.New(st).Cancel(ctx, core.Intervention{TaskID: cancelled.TaskID, Action: core.InterventionCancel, ReasonCode: "obsolete"})
	requireOK(t, err)
	completed := claimSessionOrder(t, x, "completed-"+suffix, time.Hour)
	completed.State = core.WorkOrderCompleted
	requireOK(t, UpdateWorkOrder(ctx, st, completed, core.WorkOrderCmdSubmitVerification))
	seedClaimSessionScale(t, x, scale, claimSessionOrder(t, x, "", 0), suffix)
	time.Sleep(20 * time.Millisecond) // the expired order's lease passes

	drain := func() map[string]uint64 {
		if scale.DrainReadBytes == nil {
			return nil
		}
		return scale.DrainReadBytes()
	}
	recorder := NewWorkOrderReadRecorder(st)
	renewThrough := renew(recorder)
	identity := func(session string) core.WorkOrderClaimIdentity {
		return core.WorkOrderClaimIdentity{ClaimantID: claimant, SessionID: session}
	}
	for _, test := range []struct {
		name      string
		workOrder string
		claim     core.WorkOrderClaimIdentity
		want      error
		message   string
	}{
		{name: "mismatched", workOrder: live.ID, claim: identity("nobody-" + suffix), want: store.ErrWorkOrderClaimLost},
		{name: "missing", workOrder: "missing-" + suffix, claim: identity(live.SessionID), want: store.ErrNotFound},
		{name: "foreign", workOrder: live.ID, claim: identity(holder.SessionID), want: store.ErrWorkOrderClaimLost},
		{name: "other-claimant", workOrder: live.ID, claim: core.WorkOrderClaimIdentity{ClaimantID: core.TaskRunClaimantID("other"), SessionID: live.SessionID}, want: store.ErrWorkOrderClaimLost},
		{name: "empty", workOrder: live.ID, claim: identity(""), message: "session_id is required"},
		{name: "expired", workOrder: expired.ID, claim: identity(expired.SessionID), want: store.ErrWorkOrderClaimLost},
		{name: "released", workOrder: released.ID, claim: identity(released.SessionID), want: store.ErrWorkOrderClaimLost},
		{name: "checkpoint", workOrder: checkpoint.ID, claim: identity(checkpoint.SessionID), want: store.ErrWorkOrderReleasedAtCheckpoint},
		{name: "preempted", workOrder: preempted.ID, claim: identity(preempted.SessionID), want: store.ErrWorkOrderPreempted},
		{name: "cancelled", workOrder: cancelled.ID, claim: identity(cancelled.SessionID), want: store.ErrWorkOrderCancelled},
	} {
		recorder.Reset()
		drain()
		label := "renewal-refusal-" + test.name
		order, err := renewThrough(dbmetrics.WithReadLabel(ctx, label), test.claim, test.workOrder, time.Hour)
		switch {
		case test.message != "":
			if err == nil || err.Error() != test.message {
				t.Fatalf("%s renewal = %v, want %q", test.name, err, test.message)
			}
		case !errors.Is(err, test.want):
			t.Fatalf("%s renewal = %v, want %v", test.name, err, test.want)
		}
		if order.ID != "" {
			t.Errorf("%s renewal returned order %s with its refusal", test.name, order.ID)
		}
		if recorder.Calls("ListWorkOrders") != 0 || recorder.SnapshotBytes() != 0 {
			t.Errorf("%s renewal made %d ListWorkOrders calls and decoded %d snapshot bytes, want 0 and 0", test.name, recorder.Calls("ListWorkOrders"), recorder.SnapshotBytes())
		}
		if bytes := drain(); bytes != nil {
			got := bytes[label]
			if got >= ClaimSessionSnapshotBytes {
				t.Errorf("%s renewal received %d bytes from the database, want fewer than one %d-byte snapshot", test.name, got, ClaimSessionSnapshotBytes)
			}
			t.Logf("%s renewal refusal received %d bytes", test.name, got)
		}
	}

	// The same claim's completed order is acknowledged with the stored order.
	acknowledged, err := renewThrough(ctx, identity(completed.SessionID), completed.ID, time.Hour)
	requireOK(t, err)
	if acknowledged.ID != completed.ID || acknowledged.State != core.WorkOrderCompleted || acknowledged.GovernanceSnapshot == nil || len(acknowledged.GovernanceSnapshot.Designs[0].Content) != ClaimSessionSnapshotBytes {
		t.Fatalf("completed-order renewal returned %s in state %s, want the completed order with its snapshots", acknowledged.ID, acknowledged.State)
	}

	// The own live claim renews: the lease grows up to the fixed execution
	// deadline, the attempt stays, the lease persists, and both snapshots stay.
	before, err := st.GetWorkOrder(ctx, live.ID)
	requireOK(t, err)
	renewed, err := renewThrough(ctx, identity(live.SessionID), live.ID, 10*time.Hour)
	requireOK(t, err)
	if !renewed.ExecutionDeadline.Equal(before.ExecutionDeadline) || renewed.ExecutionDeadline.IsZero() || !renewed.LeaseExpiresAt.Equal(before.ExecutionDeadline) || renewed.AttemptID != before.AttemptID || renewed.State != core.WorkOrderClaimed {
		t.Fatalf("renewal = lease %s deadline %s attempt %s state %s; want the lease capped at the fixed deadline %s and attempt %s", renewed.LeaseExpiresAt, renewed.ExecutionDeadline, renewed.AttemptID, renewed.State, before.ExecutionDeadline, before.AttemptID)
	}
	short, err := renewThrough(ctx, identity(live.SessionID), live.ID, time.Minute)
	requireOK(t, err)
	if !short.LeaseExpiresAt.Before(before.ExecutionDeadline) || !short.ExecutionDeadline.Equal(before.ExecutionDeadline) {
		t.Fatalf("one-minute renewal = lease %s deadline %s, want a lease before the fixed deadline %s", short.LeaseExpiresAt, short.ExecutionDeadline, before.ExecutionDeadline)
	}
	stored, err := st.GetWorkOrder(ctx, live.ID)
	requireOK(t, err)
	if !stored.LeaseExpiresAt.Equal(short.LeaseExpiresAt) || !stored.ExecutionDeadline.Equal(before.ExecutionDeadline) || stored.SessionID != live.SessionID {
		t.Fatalf("stored renewal = lease %s deadline %s session %q, want lease %s deadline %s", stored.LeaseExpiresAt, stored.ExecutionDeadline, stored.SessionID, short.LeaseExpiresAt, before.ExecutionDeadline)
	}
	if stored.GovernanceSnapshot == nil || len(stored.GovernanceSnapshot.Designs[0].Content) != ClaimSessionSnapshotBytes || len(stored.ServedRequirementSnapshot) != 1 {
		t.Fatal("renewal lost the order's pinned snapshots")
	}
	if short.GovernanceSnapshot == nil || len(short.GovernanceSnapshot.Designs[0].Content) != ClaimSessionSnapshotBytes {
		t.Fatal("the renewed order no longer carries its pinned snapshots")
	}
}
