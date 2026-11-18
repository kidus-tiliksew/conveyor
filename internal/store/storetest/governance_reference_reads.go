package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

// governanceReferenceTimelineEvents and governanceReferenceTimelineBytes size
// the unrelated history each reference fixture task carries: more bytes than
// one ClaimSessionSnapshotBytes snapshot, so a whole-timeline read cannot pass
// the byte bound.
const (
	governanceReferenceTimelineEvents = 24
	governanceReferenceTimelineBytes  = 4 << 10
)

// governanceReferenceTask is one task whose document list the reference reads
// serve: a body larger than one snapshot, a design attachment history with an
// attachment, a later attachment, a removal, and a requirement attachment
// between them, and a long unrelated timeline. Its pinned order holds the
// live governance resolved at claim time, whose attached design carries a
// ClaimSessionSnapshotBytes body.
type governanceReferenceTask struct {
	task     core.Task
	pinned   core.WorkOrder
	unpinned core.WorkOrder
	designs  []string
}

// governanceReferenceCorpus creates the confirmed documents the reference
// fixtures attach and pin: three designs governing the fixture repository,
// one with a ClaimSessionSnapshotBytes body, a requirement, and a confirmed
// decision whose successor supersedes another.
func governanceReferenceCorpus(t *testing.T, x Fixture) (designs []string, requirement string) {
	t.Helper()
	ctx, st := x.Context, x.Backend
	suffix := strings.ToLower(core.NewTaskID())
	for i, name := range []string{"b-attached", "a-attached", "removed"} {
		id := "component-reference-reads-" + name + "-" + suffix
		body := "Reference fixture prose."
		if i == 0 {
			body = strings.Repeat("g", ClaimSessionSnapshotBytes)
		}
		document, version, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: id, Title: "Reference reads " + name, Category: "Component design"}, core.SystemDesignVersion{Content: "# Reference reads\n\n" + body + "\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - reference/" + name + "/**\n```\n", Origin: core.SystemDesignOriginOperator})
		requireOK(t, err)
		_, _, err = st.ConfirmSystemDesignVersion(ctx, document.ID, version.Version, 0)
		requireOK(t, err)
		designs = append(designs, id)
	}
	created, first, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-reference-reads-" + suffix, Title: "Reference reads"}, core.RequirementVersion{
		Content: "# Reference reads", Origin: core.RequirementOriginOperator,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "List the governing documents."}},
	})
	requireOK(t, err)
	_, _, err = st.ConfirmRequirementVersion(ctx, created.ID, first.Version)
	requireOK(t, err)
	retired, err := st.ProposeDecision(ctx, core.Decision{Statement: "Reference fixture decision that a successor retires.", Context: "Retired.", AlternativesRejected: "None.", Origin: core.DecisionOriginOperator})
	requireOK(t, err)
	_, err = st.ConfirmDecision(ctx, retired.ID)
	requireOK(t, err)
	successor, err := st.ProposeDecision(ctx, core.Decision{Statement: "Reference fixture decision\n\nwhose summary the list carries.", Context: "Current.", AlternativesRejected: "None.", Origin: core.DecisionOriginOperator, Supersedes: retired.ID})
	requireOK(t, err)
	_, err = st.ConfirmDecision(ctx, successor.ID)
	requireOK(t, err)
	return designs, created.ID
}

// newGovernanceReferenceTask creates a task of stage with the attachment
// history and timeline the reference reads fold, and returns its pinned and
// unpinned orders. A verify task holds one claimed verify order, beside
// another task's queued verify order; a review task holds a two-seat round
// with seat one claimed.
func newGovernanceReferenceTask(t *testing.T, x Fixture, stage core.Stage, designs []string, requirement, session string) governanceReferenceTask {
	t.Helper()
	ctx, st := x.Context, x.Backend
	id := fixtureTaskID()
	task := core.Task{ID: id, Workspace: x.Workspace, Repo: "conveyor", Title: id, Body: strings.Repeat("Task body the document list never reads. ", 2*ClaimSessionSnapshotBytes/41), BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, NextStage: stage, CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
	if stage == core.StageVerify {
		task.ReviewedHeadSHA = "submitted-head"
		task.SetupContract.VerifyStage = true
	}
	requireOK(t, st.CreateTask(ctx, task))
	for _, change := range []store.TaskContextChange{
		{Add: store.TaskContextInput{DesignIDs: []string{designs[0]}}},
		{Add: store.TaskContextInput{RequirementIDs: []string{requirement}, DesignIDs: []string{designs[2]}}},
		{Add: store.TaskContextInput{DesignIDs: []string{designs[1]}}},
		{Remove: store.TaskContextInput{DesignIDs: []string{designs[2]}}},
	} {
		_, err := st.UpdateTaskContext(ctx, id, change)
		requireOK(t, err)
	}
	filler := strings.Repeat("x", governanceReferenceTimelineBytes)
	for i := range governanceReferenceTimelineEvents {
		requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: id, Kind: "job.started", Payload: core.JSONPayload(map[string]any{"n": i, "filler": filler}), At: time.Now().UTC()}))
	}
	governance, err := store.GovernanceForTask(ctx, st, id, task.Repo)
	requireOK(t, err)
	claim := core.WorkOrderClaim{ClaimantID: core.TaskRunClaimantID("claim-session"), SessionID: session, ClientToken: session, Lease: time.Hour, ExecutionTimeout: time.Hour, Requirements: ClaimSessionRequirements(), Governance: &governance}
	fixture := governanceReferenceTask{task: task, designs: designs}
	switch stage {
	case core.StageVerify:
		job := core.Job{ID: id + "-verify-1", TaskID: id, Stage: stage, State: core.JobPending}
		order := core.WorkOrder{ID: job.ID, TaskID: id, JobID: job.ID, Stage: stage, State: core.WorkOrderQueued, HeadSHA: task.ReviewedHeadSHA, QueueEnteredAt: task.CreatedAt, QueueDeadline: task.CreatedAt.Add(time.Hour), CreatedAt: task.CreatedAt}
		created, err := CreateStageWorkOrder(ctx, st, job, order)
		requireOK(t, err)
		if !created {
			t.Fatal("reference fixture order was not created")
		}
		fixture.pinned, err = ClaimWorkOrder(ctx, st, order.ID, claim)
		requireOK(t, err)
		// Another task's queued verify order holds no pin.
		fixture.unpinned = newAggregateOrder(t, x, core.StageVerify)
	case core.StageReview:
		jobs, orders := reviewRound(id, 1)
		requireOK(t, CreateReviewRound(ctx, st, id, jobs, orders))
		fixture.pinned, err = ClaimWorkOrder(ctx, st, orders[0].ID, claim)
		requireOK(t, err)
		fixture.unpinned = orders[1]
	default:
		t.Fatalf("reference fixture stage %s", stage)
	}
	stored, err := st.GetWorkOrder(ctx, fixture.pinned.ID)
	requireOK(t, err)
	if stored.GovernanceSnapshot == nil || len(stored.GovernanceSnapshot.Designs) < 2 || len(stored.GovernanceSnapshot.Decisions) < 2 {
		t.Fatalf("the %s claim pinned no governance with the fixture's designs and decisions", stage)
	}
	return fixture
}

// legacyGovernanceReferences is the list DeliverOrder built before it read
// only references: the full order's pin, the full task's repository, and the
// whole compact timeline's design provenance.
func legacyGovernanceReferences(t *testing.T, ctx context.Context, st store.Store, orderID string) (core.GovernanceReferences, core.WorkOrder) {
	t.Helper()
	order, err := st.GetWorkOrder(ctx, orderID)
	requireOK(t, err)
	if order.GovernanceSnapshot == nil {
		t.Fatalf("order %s holds no pin", orderID)
	}
	task, err := st.GetTask(ctx, order.TaskID)
	requireOK(t, err)
	events, err := st.ListCompactEvents(ctx, order.TaskID)
	requireOK(t, err)
	return core.ProjectGovernanceReferences(*order.GovernanceSnapshot, task.Repo, store.TaskDesignProvenance(events), nil), order
}

// sortedReferencePin orders a reference pin by ID, because element order is
// not part of GetWorkOrderGovernancePin's contract.
func sortedReferencePin(pin core.GovernanceSnapshot) core.GovernanceSnapshot {
	sort.SliceStable(pin.Designs, func(i, j int) bool { return pin.Designs[i].ID < pin.Designs[j].ID })
	sort.SliceStable(pin.Decisions, func(i, j int) bool { return pin.Decisions[i].ID < pin.Decisions[j].ID })
	return pin
}

// runGovernanceReferenceReads proves the three reads a claim, renewal, or
// refresh response's document list makes on every backend: the pinned
// governance reference projection, the task repository, and the design
// attachment history (component-work-orders invariants 19, 20, 23, and 24).
func runGovernanceReferenceReads(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	designs, requirement := governanceReferenceCorpus(t, x)
	suffix := core.NewTaskID()
	verify := newGovernanceReferenceTask(t, x, core.StageVerify, designs, requirement, "reference-verify-"+suffix)
	review := newGovernanceReferenceTask(t, x, core.StageReview, designs, requirement, "reference-review-"+suffix)

	foreignWorkspace := x.Workspace + "-reference-foreign"
	foreignCtx := store.WithWorkspace(ctx, foreignWorkspace)
	_, err := st.BootstrapWorkspaceConfig(foreignCtx, &config.Config{Workspace: foreignWorkspace, Repos: x.Config.Repos})
	requireOK(t, err)

	for _, fixture := range []governanceReferenceTask{verify, review} {
		stage := string(fixture.pinned.Stage)
		before, err := st.GetWorkOrder(ctx, fixture.pinned.ID)
		requireOK(t, err)
		pin, err := st.GetWorkOrderGovernancePin(ctx, fixture.pinned.ID)
		requireOK(t, err)
		if pin == nil {
			t.Fatalf("%s pin read returned no pin", stage)
		}
		// The projection carries the reference fields of every pinned
		// document and no design body.
		requireSameJSON(t, stage+" pin", sortedReferencePin(*pin), sortedReferencePin(core.GovernanceReferencePin(*before.GovernanceSnapshot)))
		for _, design := range pin.Designs {
			if design.Content != "" || len(design.Governs) != 0 || design.Category != "" {
				t.Fatalf("%s pin carries more than references for %s", stage, design.ID)
			}
		}
		encoded, err := json.Marshal(pin)
		requireOK(t, err)
		if len(encoded) >= ClaimSessionSnapshotBytes {
			t.Fatalf("%s pin projection is %d bytes, want fewer than the %d-byte design body", stage, len(encoded), ClaimSessionSnapshotBytes)
		}
		// The delivered list is the list the full reads produced.
		want, _ := legacyGovernanceReferences(t, ctx, st, fixture.pinned.ID)
		repository, err := st.GetTaskRepository(ctx, fixture.task.ID)
		requireOK(t, err)
		events, err := st.ListTaskDesignContextEvents(ctx, fixture.task.ID)
		requireOK(t, err)
		requireSameJSON(t, stage+" references", core.ProjectGovernanceReferences(*pin, repository, store.TaskDesignProvenance(events), nil), want)
		if len(want.Designs) < 2 || want.Designs[0].ID != designs[0] || want.Designs[1].ID != designs[1] || want.Designs[0].Group != core.GovernanceDesignAttached {
			t.Fatalf("%s references do not list the attached designs in attachment order: %+v", stage, want.Designs)
		}
		// Reading changes no stored pin.
		after, err := st.GetWorkOrder(ctx, fixture.pinned.ID)
		requireOK(t, err)
		if after.GovernanceSnapshotDigest != before.GovernanceSnapshotDigest || !reflect.DeepEqual(after.GovernanceSnapshot, before.GovernanceSnapshot) {
			t.Fatalf("%s pin read changed the stored pin", stage)
		}
		// An order without a pin answers nil; a missing or foreign order is
		// not found.
		if unpinned, err := st.GetWorkOrderGovernancePin(ctx, fixture.unpinned.ID); err != nil || unpinned != nil {
			t.Fatalf("%s unpinned order = %+v, %v; want no pin", stage, unpinned, err)
		}
		for _, missing := range []struct {
			ctx context.Context
			id  string
		}{{ctx, "missing-" + suffix}, {foreignCtx, fixture.pinned.ID}} {
			if _, err := st.GetWorkOrderGovernancePin(missing.ctx, missing.id); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("%s pin read of %s = %v, want not found", stage, missing.id, err)
			}
		}
		if _, err := st.GetTaskRepository(foreignCtx, fixture.task.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("foreign task repository read = %v, want not found", err)
		}
		if foreign, err := st.ListTaskDesignContextEvents(foreignCtx, fixture.task.ID); err != nil || len(foreign) != 0 {
			t.Fatalf("foreign design events = %d, %v; want none", len(foreign), err)
		}
	}

	// The repository read answers the task's repository and GetTask's
	// not-found answer.
	task, err := st.GetTask(ctx, verify.task.ID)
	requireOK(t, err)
	if repository, err := st.GetTaskRepository(ctx, verify.task.ID); err != nil || repository != task.Repo {
		t.Fatalf("task repository = %q, %v; want %q", repository, err, task.Repo)
	}
	if _, err := st.GetTaskRepository(ctx, "missing-"+suffix); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing task repository read = %v, want not found", err)
	}

	// The design read returns exactly the compact ledger's design attachment
	// events, in ledger order, and no requirement or unrelated event.
	events, err := st.ListTaskDesignContextEvents(ctx, verify.task.ID)
	requireOK(t, err)
	ledger, err := st.ListCompactEvents(ctx, verify.task.ID)
	requireOK(t, err)
	var want []core.Event
	requirementEvents := 0
	for _, event := range ledger {
		switch event.Kind {
		case store.TaskContextDesignAdded, store.TaskContextDesignRemoved:
			want = append(want, event)
		case store.TaskContextRequirementAdded:
			requirementEvents++
		}
	}
	if len(want) != 4 || requirementEvents == 0 || len(events) != len(want) {
		t.Fatalf("design read returned %d events; the ledger holds %d design and %d requirement attachment events", len(events), len(want), requirementEvents)
	}
	for i := range want {
		if events[i].ID != want[i].ID || events[i].Kind != want[i].Kind || string(events[i].Payload) != string(want[i].Payload) {
			t.Fatalf("design event %d = %+v, want %+v", i, events[i], want[i])
		}
	}
	if got, full := store.TaskDesignProvenance(events), store.TaskDesignProvenance(ledger); !reflect.DeepEqual(got, full) || len(got.Attached) != 2 {
		t.Fatalf("design provenance %+v, want the full ledger's %+v with two attached designs", got, full)
	}
}

// RenewDeliveryFunc is MCP renew_work_order's service path over the given
// store: workorder.Service.RenewClaimWithLease and then
// workorder.Service.DeliverOrder. It returns the delivered order, its
// document list, and its governance_references_error. storetest cannot import
// workorder, so each test binary supplies it.
type RenewDeliveryFunc func(st store.Store) func(ctx context.Context, claim core.WorkOrderClaimIdentity, workOrderID string, lease time.Duration) (core.WorkOrder, *core.GovernanceReferences, string, error)

// RunRenewalDeliveryReads renews a pinned verify order and a pinned review
// seat through deliver over a WorkOrderReadRecorder, in a workspace that also
// holds ClaimSessionScaleOrders snapshot-bearing orders, and requires each
// successful renewal response to list exactly the documents the former full
// reads listed while decoding zero snapshot bytes and reading no full order,
// no task body, and no whole timeline. With DrainReadBytes, the whole
// renewal, delivery included, must receive fewer bytes than one snapshot,
// although the task body, its timeline, and the pinned design body each
// exceed that. The same claim's completed order keeps the acknowledgement
// exception: it answers with its own snapshot and reads no pin
// (component-work-orders invariants 14, 20, 23, and 24).
func RunRenewalDeliveryReads(t *testing.T, x Fixture, scale ClaimSessionScale, deliver RenewDeliveryFunc) {
	st, ctx := x.Backend, x.Context
	suffix := core.NewTaskID()
	// A durable seed pins its snapshots on every order of the workspace, so
	// it runs before the fixture orders pin their own governance.
	claimSessionOrder(t, x, "holder-"+suffix, time.Hour)
	seedClaimSessionScale(t, x, scale, claimSessionOrder(t, x, "", 0), suffix)
	designs, requirement := governanceReferenceCorpus(t, x)
	verify := newGovernanceReferenceTask(t, x, core.StageVerify, designs, requirement, "delivery-verify-"+suffix)
	review := newGovernanceReferenceTask(t, x, core.StageReview, designs, requirement, "delivery-review-"+suffix)
	drain := func() map[string]uint64 {
		if scale.DrainReadBytes == nil {
			return nil
		}
		return scale.DrainReadBytes()
	}
	recorder := NewWorkOrderReadRecorder(st)
	renew := deliver(recorder)
	identity := func(session string) core.WorkOrderClaimIdentity {
		return core.WorkOrderClaimIdentity{ClaimantID: core.TaskRunClaimantID("claim-session"), SessionID: session}
	}
	for _, fixture := range []governanceReferenceTask{verify, review} {
		stage := string(fixture.pinned.Stage)
		want, before := legacyGovernanceReferences(t, ctx, st, fixture.pinned.ID)
		_ = drain()
		recorder.Reset()
		label := "renewal-delivery-" + stage
		order, references, referencesError, err := renew(dbmetrics.WithReadLabel(ctx, label), identity(fixture.pinned.SessionID), fixture.pinned.ID, time.Hour)
		requireOK(t, err)
		if referencesError != "" || references == nil {
			t.Fatalf("%s renewal delivered no list: %q", stage, referencesError)
		}
		requireSameJSON(t, stage+" renewal references", *references, want)
		if len(want.Designs) < 2 || want.Designs[0].ID != designs[0] || want.Designs[1].ID != designs[1] || want.Designs[0].Group != core.GovernanceDesignAttached {
			t.Fatalf("%s renewal does not list the attached designs in attachment order: %+v", stage, want.Designs)
		}
		if order.ID != fixture.pinned.ID || order.State != core.WorkOrderClaimed || encodedSnapshotBytes([]core.WorkOrder{order}) != 0 {
			t.Fatalf("%s renewal answered %s in state %s with %d snapshot bytes", stage, order.ID, order.State, encodedSnapshotBytes([]core.WorkOrder{order}))
		}
		if recorder.SnapshotBytes() != 0 {
			t.Fatalf("%s renewal decoded %d snapshot bytes, want 0", stage, recorder.SnapshotBytes())
		}
		for _, method := range []string{"GetWorkOrder", "GetTask", "ListCompactEvents", "ListEvents", "ListTaskContextEvents", "ListWorkOrders"} {
			if n := recorder.Calls(method); n != 0 {
				t.Fatalf("%s renewal called %s %d times, want none", stage, method, n)
			}
		}
		for _, method := range []string{"GetWorkOrderGovernancePin", "GetTaskRepository", "ListTaskDesignContextEvents"} {
			if n := recorder.Calls(method); n != 1 {
				t.Fatalf("%s renewal called %s %d times, want once", stage, method, n)
			}
		}
		if bytes := drain(); bytes != nil {
			got := bytes[label]
			if got == 0 || got >= ClaimSessionSnapshotBytes {
				t.Fatalf("%s renewal and delivery received %d bytes, want more than 0 and fewer than one %d-byte snapshot", stage, got, ClaimSessionSnapshotBytes)
			}
			t.Logf("%s renewal and delivery received %d bytes", stage, got)
		}
		after, err := st.GetWorkOrder(ctx, fixture.pinned.ID)
		requireOK(t, err)
		if after.GovernanceSnapshotDigest != before.GovernanceSnapshotDigest || !reflect.DeepEqual(after.GovernanceSnapshot, before.GovernanceSnapshot) {
			t.Fatalf("%s renewal changed the stored pin", stage)
		}
	}

	// A refused renewal delivers nothing and reads no pin.
	recorder.Reset()
	if _, references, _, err := renew(ctx, identity("nobody-"+suffix), verify.pinned.ID, time.Hour); !errors.Is(err, store.ErrWorkOrderClaimLost) || references != nil || recorder.Calls("GetWorkOrderGovernancePin") != 0 {
		t.Fatalf("mismatched renewal = %v with list %v, want a lost claim and no list", err, references)
	}

	// The same claim's completed order is acknowledged with the stored order,
	// and its list comes from that order's own snapshot.
	want, completed := legacyGovernanceReferences(t, ctx, st, verify.pinned.ID)
	completed.State = core.WorkOrderCompleted
	requireOK(t, UpdateWorkOrder(ctx, st, completed, core.WorkOrderCmdSubmitVerification))
	recorder.Reset()
	acknowledged, references, referencesError, err := renew(ctx, identity(verify.pinned.SessionID), verify.pinned.ID, time.Hour)
	requireOK(t, err)
	if acknowledged.State != core.WorkOrderCompleted || acknowledged.GovernanceSnapshot == nil || referencesError != "" || references == nil {
		t.Fatalf("completed-order renewal = state %s, snapshot %v, list error %q", acknowledged.State, acknowledged.GovernanceSnapshot != nil, referencesError)
	}
	requireSameJSON(t, "acknowledgement references", *references, want)
	if recorder.Calls("GetWorkOrderGovernancePin") != 0 {
		t.Fatal("the acknowledgement read the pin again instead of using its own order")
	}
}
