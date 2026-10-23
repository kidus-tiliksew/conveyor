package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
)

// NextActionOptions is the caller view the projection cannot read from the task.
type NextActionOptions struct {
	Now             time.Time
	Capabilities    map[core.Capability]bool
	CanChangePolicy bool
	LiveClaimants   *nextaction.ClaimantView
	// MarkersLoaded skips a second marker read when the caller already has them.
	MarkersLoaded bool
	Markers       []ActivityMarker
	// OrdersLoaded skips a second order read when the caller already has them.
	OrdersLoaded bool
	Orders       []core.WorkOrder
	// MergeReadiness returns the last-known readiness peek of a
	// merge_approval gate without a forge call, or nil when none is known
	// (DEC-64(3); DEC-61).
	MergeReadiness func(context.Context, core.Task) *nextaction.MergeReadiness
	// OpenTasksLoaded skips the dependency view's open-task read when the
	// caller already holds every open task in the workspace.
	OpenTasksLoaded bool
	OpenTasks       []core.Task
	// CompactOnly marks a caller that consumes only nextaction.CompactFrom.
	// The row-10 disposition binding feeds only details and choices, so its
	// per-order verification read is skipped; every compact field is
	// projected from the same input (feature-operator-next-action NA-3).
	CompactOnly bool
}

// ReadNextAction projects one task. Missing tasks return the store's not-found error.
func ReadNextAction(ctx context.Context, st Store, taskID string, opts NextActionOptions) (nextaction.NextAction, error) {
	task, err := st.GetTask(ctx, taskID)
	if err != nil {
		return nextaction.NextAction{}, err
	}
	projected, err := ReadNextActions(ctx, st, []core.Task{task}, opts)
	if err != nil {
		return nextaction.NextAction{}, err
	}
	return projected[task.ID], nil
}

// ReadNextActions projects each task from one batch of orders, markers, and proposals.
func ReadNextActions(ctx context.Context, st Store, tasks []core.Task, opts NextActionOptions) (map[string]nextaction.NextAction, error) {
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	}
	ids := make([]string, len(tasks))
	for i := range tasks {
		ids[i] = tasks[i].ID
	}
	var orders []core.WorkOrder
	var err error
	if opts.OrdersLoaded {
		orders = opts.Orders
	} else {
		orders, err = st.ListWorkOrdersForTasks(ctx, ids)
		if err != nil {
			return nil, err
		}
	}
	var markers []ActivityMarker
	if opts.MarkersLoaded {
		markers = opts.Markers
	} else {
		markers, err = st.ListActivityMarkersForTasks(ctx, ids)
		if err != nil {
			return nil, err
		}
	}
	proposals, err := st.ListPendingProposals(ctx)
	if err != nil {
		return nil, err
	}
	dependencies, err := readDependencyView(ctx, st, tasks, orders, opts)
	if err != nil {
		return nil, err
	}
	events, specs, err := readAssemblerHistory(ctx, st, tasks)
	if err != nil {
		return nil, err
	}
	waiting, err := ClaimWaitingProposals(ctx, st, orders)
	if err != nil {
		return nil, err
	}
	contexts, err := readContextProposals(ctx, st, tasks)
	if err != nil {
		return nil, err
	}
	origins, err := readMonitorOrigins(ctx, st, tasks, contexts)
	if err != nil {
		return nil, err
	}
	ordersByTask := map[string][]core.WorkOrder{}
	for _, order := range orders {
		ordersByTask[order.TaskID] = append(ordersByTask[order.TaskID], withDependencyBlockers(order, dependencies.blockers[order.TaskID]))
	}
	markerByTask := map[string]ActivityMarker{}
	for _, marker := range markers {
		markerByTask[marker.TaskID] = marker
	}
	out := make(map[string]nextaction.NextAction, len(tasks))
	for _, task := range tasks {
		input := assembleInput(ctx, task, ordersByTask[task.ID], markerByTask[task.ID], proposals, waiting[task.ID], events[task.ID], specs, opts)
		input.Blockers = dependencies.forward[task.ID]
		input.Blocks = dependencies.blocks[task.ID]
		input.ContextProposals = contexts[task.ID]
		if origin, ok := origins[task.ID]; ok {
			input.MonitorObservation = &nextaction.MonitorObservationRef{Kind: string(origin.Kind), SourceURL: origin.SourceURL, ObservedAt: origin.ObservedAt}
		}
		if input.Markers.CheckpointDecision && input.Markers.CheckpointClass == core.WorkOrderCheckpointClassAuthorityConflict {
			if input.CitedDocuments, err = readCitedDocuments(ctx, st, task.ID, input.Markers.Citations); err != nil {
				return nil, err
			}
		}
		action := nextaction.Project(input)
		// Row 10 reads its selected order's sealed checkpoint only when it
		// wins, so no other row ever carries the binding
		// (feature-operator-next-action NA-3, Disposition binding).
		if action.Kind == nextaction.KindVerificationDisposition && action.CurrentOrder != nil && !opts.CompactOnly {
			disposition, err := readDispositionInput(ctx, st, task, input.Orders, action.CurrentOrder.ID)
			if err != nil {
				return nil, err
			}
			input.Dispositions = map[string]nextaction.DispositionInput{action.CurrentOrder.ID: disposition}
			action = nextaction.Project(input)
		}
		out[task.ID] = action
	}
	return out, nil
}

// dependencyView is one reading of the workspace's dependency edges. Forward
// references fill blocked_by on a dependent; blocks lists, on a blocker, the
// open dependents whose latest queued order names it. Both come from the same
// ListDependencyBlockers result, the one that stamps blocking_task_ids on the
// dependents' orders, so the two sides always describe the same edges
// (feature-operator-next-action NA-3, Dependents; req-operator-next-action
// AC-2.1).
type dependencyView struct {
	blockers map[string]DependencyBlockers
	forward  map[string][]nextaction.BlockerRef
	blocks   map[string][]nextaction.BlockerRef
}

var openTaskStates = []core.TaskState{core.TaskClaiming, core.TaskQueued, core.TaskRunning, core.TaskAwaiting, core.TaskApproved, core.TaskParked}

// readDependencyView reads the workspace's open tasks before applying the
// requested subset, so a blocker projected alone, or inside a filtered page,
// still names dependents outside the request. Orders are read only for
// dependents the request did not already load.
func readDependencyView(ctx context.Context, st Store, tasks []core.Task, orders []core.WorkOrder, opts NextActionOptions) (dependencyView, error) {
	view := dependencyView{blockers: map[string]DependencyBlockers{}, forward: map[string][]nextaction.BlockerRef{}, blocks: map[string][]nextaction.BlockerRef{}}
	if len(tasks) == 0 {
		return view, nil
	}
	open := opts.OpenTasks
	var err error
	if !opts.OpenTasksLoaded {
		open, err = st.ListTasksFiltered(ctx, TaskFilter{States: openTaskStates})
		if err != nil {
			return view, err
		}
	}
	workspace, _ := WorkspaceFromContext(ctx)
	known := map[string]core.Task{}
	requested := map[string]bool{}
	ids := make([]string, 0, len(tasks)+len(open))
	var candidates []core.Task
	for _, task := range open {
		if (workspace != "" && task.Workspace != workspace) || core.TaskTerminal(task.State) {
			continue
		}
		known[task.ID] = task
		candidates = append(candidates, task)
		ids = append(ids, task.ID)
	}
	for _, task := range tasks {
		requested[task.ID] = true
		if _, listed := known[task.ID]; !listed {
			ids = append(ids, task.ID)
		}
		known[task.ID] = task
	}
	view.blockers, err = st.ListDependencyBlockers(ctx, ids)
	if err != nil {
		return view, err
	}
	ordersByTask := map[string][]core.WorkOrder{}
	for _, order := range orders {
		ordersByTask[order.TaskID] = append(ordersByTask[order.TaskID], order)
	}
	var dependents []core.Task
	var unloaded []string
	for _, task := range candidates {
		for _, blocker := range view.blockers[task.ID].BlockingTaskIDs {
			if requested[blocker] {
				dependents = append(dependents, task)
				if !requested[task.ID] {
					unloaded = append(unloaded, task.ID)
				}
				break
			}
		}
	}
	if len(unloaded) > 0 {
		more, err := st.ListWorkOrdersForTasks(ctx, unloaded)
		if err != nil {
			return view, err
		}
		for _, order := range more {
			ordersByTask[order.TaskID] = append(ordersByTask[order.TaskID], order)
		}
	}
	for _, dependent := range dependents {
		taskOrders := make([]core.WorkOrder, 0, len(ordersByTask[dependent.ID]))
		for _, order := range ordersByTask[dependent.ID] {
			taskOrders = append(taskOrders, withDependencyBlockers(order, view.blockers[dependent.ID]))
		}
		latest := nextaction.LatestQueued(taskOrders)
		if latest == nil {
			continue
		}
		for _, blocker := range latest.BlockingTaskIDs {
			if requested[blocker] {
				view.blocks[blocker] = append(view.blocks[blocker], nextaction.BlockerRef{TaskID: dependent.ID, Title: dependent.Title, State: string(dependent.State)})
			}
		}
	}
	for id := range view.blocks {
		refs := view.blocks[id]
		sort.Slice(refs, func(i, j int) bool { return refs[i].TaskID < refs[j].TaskID })
	}
	// Blockers outside the open set, merged or closed, are read in one batch
	// rather than one task read each.
	heads := map[string]TaskHead{}
	var missing []string
	for id := range requested {
		for _, blockerID := range view.blockers[id].BlockingTaskIDs {
			if _, ok := known[blockerID]; !ok && !slices.Contains(missing, blockerID) {
				missing = append(missing, blockerID)
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		heads, err = st.ListTaskHeads(ctx, missing)
		if err != nil {
			return view, err
		}
	}
	for id := range requested {
		blockerIDs := view.blockers[id].BlockingTaskIDs
		refs := make([]nextaction.BlockerRef, 0, len(blockerIDs))
		for _, blockerID := range blockerIDs {
			if blocker, ok := known[blockerID]; ok {
				refs = append(refs, nextaction.BlockerRef{TaskID: blocker.ID, Title: blocker.Title, State: string(blocker.State)})
				continue
			}
			head, ok := heads[blockerID]
			if !ok {
				return view, fmt.Errorf("%w: task %s", ErrNotFound, blockerID)
			}
			refs = append(refs, nextaction.BlockerRef{TaskID: head.ID, Title: head.Title, State: string(head.State)})
		}
		view.forward[id] = refs
	}
	return view, nil
}

// readCitedDocuments reads, for one task whose row-9 checkpoint has class
// authority_conflict, each cited document's current confirmed version and,
// for a System Design attached to the task, its pinned version folded from
// the task's context events. A missing, archived, or unconfirmed document is
// left out; any other read error fails the projection (component-work-orders,
// Next action; feature-operator-next-action NA-3).
func readCitedDocuments(ctx context.Context, st Store, taskID string, citations []core.WorkOrderAuthorityConflictCitation) (map[string]nextaction.CitedDocument, error) {
	ids := nextaction.CitedDocumentIDs(citations)
	if len(ids) == 0 {
		return nil, nil
	}
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		return nil, err
	}
	_, pins := ActiveTaskContextReferences(events)
	out := make(map[string]nextaction.CitedDocument, len(ids))
	for _, id := range ids {
		requirement, err := st.GetRequirement(ctx, id)
		if err == nil {
			if !requirement.Archived && requirement.CurrentVersion > 0 {
				out[id] = nextaction.CitedDocument{CurrentVersion: requirement.CurrentVersion}
			}
			continue
		}
		if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		design, err := st.GetSystemDesign(ctx, id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !design.Archived && design.CurrentVersion > 0 {
			out[id] = nextaction.CitedDocument{CurrentVersion: design.CurrentVersion, PinnedVersion: pins[id]}
		}
	}
	return out, nil
}

// readAssemblerHistory batches the history the assembler folds: the
// NextActionEventKinds events of parked, approved, and awaiting tasks, and
// the latest plan version of awaiting tasks. Other tasks read neither, as
// before batching (feature-operator-next-action, Input loading).
func readAssemblerHistory(ctx context.Context, st Store, tasks []core.Task) (map[string][]core.Event, map[string]int, error) {
	var eventIDs, specIDs []string
	for _, task := range tasks {
		if task.State == core.TaskParked || task.State == core.TaskApproved || task.State == core.TaskAwaiting {
			eventIDs = append(eventIDs, task.ID)
		}
		if task.State == core.TaskAwaiting {
			specIDs = append(specIDs, task.ID)
		}
	}
	events := map[string][]core.Event{}
	specs := map[string]int{}
	var err error
	if len(eventIDs) > 0 {
		if events, err = st.ListNextActionEvents(ctx, eventIDs); err != nil {
			return nil, nil, err
		}
	}
	if len(specIDs) > 0 {
		if specs, err = st.ListLatestSpecVersionNumbers(ctx, specIDs); err != nil {
			return nil, nil, err
		}
	}
	return events, specs, nil
}

// withDependencyBlockers returns an implement or verify order carrying the
// task's unmerged dependencies, which also make it unclaimable. Other stages
// return unchanged.
func withDependencyBlockers(order core.WorkOrder, blockers DependencyBlockers) core.WorkOrder {
	if order.Stage != core.StageImplement && order.Stage != core.StageVerify {
		return order
	}
	order.BlockingTaskIDs = append([]string(nil), blockers.BlockingTaskIDs...)
	order.UnsatisfiableTaskIDs = append([]string(nil), blockers.UnsatisfiableTaskIDs...)
	if len(order.BlockingTaskIDs) > 0 {
		order.Claimable = false
	}
	return order
}

// assembleInput builds one task's projection input from batched reads.
// events holds only NextActionEventKinds, which are every kind the folds
// below inspect, and specs the latest plan version of awaiting tasks.
func assembleInput(ctx context.Context, task core.Task, orders []core.WorkOrder, marker ActivityMarker, proposals []core.PendingProposal, waiting []ClaimBlockingProposal, events []core.Event, specs map[string]int, opts NextActionOptions) nextaction.Input {
	specVersion, found := 0, false
	if task.State == core.TaskAwaiting {
		specVersion, found = specs[task.ID]
	}
	for i := range orders {
		pendingRevision := task.State == core.TaskAwaiting && task.GateKind == core.GateKindPlanRevision
		ApplyRecoveryRefusal(&orders[i], task, orders, pendingRevision)
	}
	conflict := mergeConflictFromEvents(events, orders)
	input := nextaction.Input{
		Task: task, Orders: orders, Markers: markersFrom(marker, events, orders), Assignee: task.Assignee,
		ClaimWaiting: claimWaitingRefs(waiting), PendingProposals: pendingProposalRefs(proposals),
		MergeConflict: conflict, Recovery: AutomaticRecoveryFor(orders),
		Now: opts.Now, Capabilities: opts.Capabilities, LiveClaimants: opts.LiveClaimants,
		PullRequest: recordedPullRequest(events), BounceCount: recordedBounceCount(events),
	}
	if task.State == core.TaskAwaiting && task.GateKind == core.GateKindMergeApproval && opts.MergeReadiness != nil {
		input.MergeReadiness = opts.MergeReadiness(ctx, task)
	}
	if found {
		input.LatestSpec = &nextaction.SpecRef{Version: specVersion}
		input.GateSpecVersion = specVersion
	}
	return input
}

// AutomaticRecoveryFor projects the durable AR-3 ledger on each order: a
// scheduled next_automatic_recovery_at is the retry time of the order's next
// window-local attempt, and the exhaustion suppression reason marks the order
// the operator must recover. Orders with neither are absent.
func AutomaticRecoveryFor(orders []core.WorkOrder) map[string]nextaction.AutomaticRecovery {
	out := map[string]nextaction.AutomaticRecovery{}
	for _, order := range orders {
		exhausted := order.RetrySuppressed && order.RetrySuppressionReason == core.AutomaticRecoveryExhaustedReason
		if order.NextAutomaticRecoveryAt.IsZero() && !exhausted {
			continue
		}
		rec := nextaction.AutomaticRecovery{Exhausted: exhausted}
		if !order.NextAutomaticRecoveryAt.IsZero() {
			next := order.NextAutomaticRecoveryAt
			rec.NextAt = &next
			rec.Attempt = order.AutomaticRecoveryCount + 1
		}
		out[order.ID] = rec
	}
	return out
}

func markersFrom(marker ActivityMarker, events []core.Event, orders []core.WorkOrder) nextaction.Markers {
	out := nextaction.Markers{ParkCommand: parkCommand(events)}
	if marker.ForgeFailure != nil {
		out.ForgeFailure = true
	}
	if marker.ReviewRecovery != nil {
		out.ReviewRecovery = true
	}
	if marker.InterruptedReviewRecovery != nil {
		out.InterruptedReview = true
	}
	for _, order := range orders {
		if order.Checkpoint != nil && order.Checkpoint.Verification != nil {
			out.VerificationDisposition = true
		}
	}
	// The agent question reads the checkpoint of the order it selects, the
	// latest queued order, never an older order's, and only while that order
	// is still released at the checkpoint. An operator recovery keeps the
	// checkpoint for history but clears the released outcome
	// (component-work-orders, Next action; feature-operator-next-action NA-3
	// row 9).
	for i := len(orders) - 1; i >= 0; i-- {
		if orders[i].State != core.WorkOrderQueued {
			continue
		}
		if checkpoint := orders[i].Checkpoint; checkpoint != nil && checkpoint.DecisionRequest != "" && orders[i].LastAttemptOutcome == core.WorkOrderOutcomeReleased {
			out.CheckpointDecision = true
			out.Checkpoint = checkpoint.DecisionRequest
			out.CheckpointClass = checkpoint.Class
			out.Citations = append([]core.WorkOrderAuthorityConflictCitation(nil), checkpoint.Citations...)
		}
		break
	}
	return out
}

func parkCommand(events []core.Event) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != "task.state_changed" {
			continue
		}
		var payload struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(events[i].Payload, &payload) == nil && payload.Command != "" {
			return payload.Command
		}
	}
	return ""
}

// claimWaitingRefs carries the task's claim-wait selection to row 8, so the
// next action waits on a proposal exactly when a verify or review claim is
// withheld (req-operator-next-action AC-1.7; req-260810-70ce2f AC-1.3).
func claimWaitingRefs(waiting []ClaimBlockingProposal) []nextaction.ProposalRef {
	var out []nextaction.ProposalRef
	for _, item := range waiting {
		out = append(out, nextaction.ProposalRef{Tier: item.Tier, ID: item.ID, Version: item.Version})
	}
	return out
}

// readContextProposals reads the batch's pending task-context proposals with
// their stored metadata in one ListTaskContextProposals call: scoped to the one
// task for a single read, otherwise to the bound workspace. A terminal task's
// records are skipped (feature-operator-next-action NA-3, Context-proposal
// attention; req-intake-and-triage AC-7.3).
func readContextProposals(ctx context.Context, st Store, tasks []core.Task) (map[string][]nextaction.ContextProposalRef, error) {
	open := map[string]bool{}
	for _, task := range tasks {
		if !core.TaskTerminal(task.State) {
			open[task.ID] = true
		}
	}
	out := map[string][]nextaction.ContextProposalRef{}
	if len(open) == 0 {
		return out, nil
	}
	scope := ""
	if len(tasks) == 1 {
		scope = tasks[0].ID
	}
	records, err := st.ListTaskContextProposals(ctx, scope, core.TaskContextProposalProposed)
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		if !open[record.TaskID] || record.State != core.TaskContextProposalProposed {
			continue
		}
		out[record.TaskID] = append(out[record.TaskID], nextaction.ContextProposalRef{
			Kind: string(record.TargetKind), TargetID: record.TargetID, TargetTitle: record.TargetTitle,
			Source: string(record.Source), Justification: record.Justification, ProposedBy: record.ProposedBy, CreatedAt: record.CreatedAt,
		})
	}
	return out, nil
}

// readMonitorOrigins reads, in one call, the originating observations of the
// batch's monitor-filed tasks that carry a pending context proposal; no other
// task needs one (feature-operator-next-action NA-3, Monitor origin).
func readMonitorOrigins(ctx context.Context, st Store, tasks []core.Task, contexts map[string][]nextaction.ContextProposalRef) (map[string]monitor.ObservationRecord, error) {
	var ids []string
	for _, task := range tasks {
		if strings.HasPrefix(task.Source, "monitor:") && len(contexts[task.ID]) > 0 {
			ids = append(ids, task.ID)
		}
	}
	if len(ids) == 0 {
		return map[string]monitor.ObservationRecord{}, nil
	}
	return st.MonitorOriginsForTasks(ctx, ids)
}

// pendingProposalRefs is the workspace's pending set for agent-question
// presentation; the projection keeps only cited requirement and System Design
// versions.
func pendingProposalRefs(proposals []core.PendingProposal) []nextaction.ProposalRef {
	out := make([]nextaction.ProposalRef, 0, len(proposals))
	for _, item := range proposals {
		out = append(out, nextaction.ProposalRef{Tier: item.Tier, ID: item.ID, Version: item.Version})
	}
	return out
}

func revisionRationale(events []core.Event) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != "work_order.plan_revision_requested" {
			continue
		}
		var request struct {
			Rationale string `json:"rationale"`
		}
		if json.Unmarshal(events[i].Payload, &request) == nil {
			return request.Rationale
		}
	}
	return ""
}

// recordedPullRequest is the number of the latest recorded
// pull_request.opened event, or zero when none records one. The catalogue
// names the pull request only from this record, never from an issue number
// (feature-operator-next-action NA-4).
func recordedPullRequest(events []core.Event) int {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != "pull_request.opened" {
			continue
		}
		var payload struct {
			Number int `json:"number"`
		}
		if json.Unmarshal(events[i].Payload, &payload) == nil && payload.Number > 0 {
			return payload.Number
		}
	}
	return 0
}

// recordedBounceCount is the bounce count recorded by the latest
// pipeline.bounce_limit event, the window that produced the bounce gate, or
// zero when none is recorded.
func recordedBounceCount(events []core.Event) int {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != "pipeline.bounce_limit" {
			continue
		}
		var payload struct {
			Window int `json:"window"`
		}
		if json.Unmarshal(events[i].Payload, &payload) == nil && payload.Window > 0 {
			return payload.Window
		}
		return 0
	}
	return 0
}

// readDispositionInput reads the verification records of the verify order
// row 10 selected and binds its sealed checkpoint. A store that cannot read
// verification records yields a refusal, never a fabricated binding; a read
// failure stays an error.
func readDispositionInput(ctx context.Context, st Store, task core.Task, orders []core.WorkOrder, orderID string) (nextaction.DispositionInput, error) {
	var order core.WorkOrder
	for _, candidate := range orders {
		if candidate.ID == orderID {
			order = candidate
		}
	}
	if order.Checkpoint == nil || order.Checkpoint.Verification == nil {
		return nextaction.DispositionInput{Refusal: fmt.Sprintf("verify order %s has no sealed verification checkpoint to dispose", orderID)}, nil
	}
	reader, ok := st.(VerificationReviewReader)
	if !ok {
		return nextaction.DispositionInput{Refusal: "verification records are not readable from this store"}, nil
	}
	state, err := reader.ReadVerificationReview(ctx, task.ID, order.ID)
	if err != nil {
		return nextaction.DispositionInput{}, err
	}
	// The binding checks the order's served-requirement and governance pins,
	// which list projections may omit, so it reads the full stored order
	// returned with the verification records.
	if state.Order.ID != order.ID {
		return nextaction.DispositionInput{Refusal: fmt.Sprintf("verify order %s is not readable with its verification records", orderID)}, nil
	}
	return verificationDispositionBinding(task, state.Order, state.Rows), nil
}

// ApplyRecoveryRefusal is the one refusal fold shared by claim projection and next action.
func ApplyRecoveryRefusal(order *core.WorkOrder, task core.Task, taskOrders []core.WorkOrder, pendingPlanRevision bool) {
	if order.State != core.WorkOrderQueued && order.State != core.WorkOrderStale && order.State != core.WorkOrderTimedOut {
		return
	}
	if pendingPlanRevision {
		order.RecoveryRefusal = fmt.Sprintf("work order %s cannot be recovered while task %s awaits the pending plan-revision decision; use the plan-revision decision gate", order.ID, task.ID)
	} else if err := WorkOrderRecoveryRefusal(task, *order, taskOrders); err != nil {
		order.RecoveryRefusal = err.Error()
	}
	if order.RecoveryRefusal != "" && order.State == core.WorkOrderQueued {
		order.Claimable = false
	}
}

// MergeConflictState is the episode fold shared by dispatch and next action.
type MergeConflictState struct {
	Open            bool
	Failures        int
	NextRetryAt     time.Time
	Exhausted       bool
	RecoveryBlocked bool
}

// MergeConflictDispatchState folds one conflict episode.
func MergeConflictDispatchState(events []core.Event, approvedHead, newHead string, orders []core.WorkOrder) MergeConflictState {
	var state MergeConflictState
	type episode struct {
		ApprovedHead string `json:"approved_head"`
		NewHead      string `json:"new_head"`
	}
	same := func(payload json.RawMessage) bool {
		var candidate episode
		return json.Unmarshal(payload, &candidate) == nil && candidate.ApprovedHead == approvedHead && candidate.NewHead == newHead
	}
	inside := false
	dispatched := map[string]bool{}
	for _, event := range events {
		switch event.Kind {
		case "merge.conflict_cleared":
			inside, state = false, MergeConflictState{}
		case "merge.blocked":
			inside = same(event.Payload)
			state = MergeConflictState{Open: inside}
		case "merge.conflict_fix_dispatched":
			if inside && same(event.Payload) {
				var payload struct {
					WorkOrderID string `json:"work_order_id"`
				}
				if json.Unmarshal(event.Payload, &payload) == nil && payload.WorkOrderID != "" {
					dispatched[payload.WorkOrderID] = true
				}
				state.NextRetryAt = time.Time{}
			}
		case "merge.conflict_dispatch_failed":
			if inside && same(event.Payload) {
				var payload struct {
					NextRetryAt time.Time `json:"next_retry_at"`
				}
				if json.Unmarshal(event.Payload, &payload) == nil {
					state.Failures++
					state.NextRetryAt = payload.NextRetryAt
				}
			}
		case "merge.conflict_dispatch_exhausted":
			if inside && same(event.Payload) {
				state.Exhausted = true
			}
		case "merge.conflict_recovery_blocked":
			if inside && same(event.Payload) {
				state.RecoveryBlocked = true
			}
		}
	}
	for _, order := range orders {
		if dispatched[order.ID] && terminalConflictFix(order) {
			state.Failures++
		}
	}
	return state
}

func terminalConflictFix(order core.WorkOrder) bool {
	switch order.State {
	case core.WorkOrderCancelled, core.WorkOrderStale, core.WorkOrderTimedOut:
		return true
	case core.WorkOrderQueued, core.WorkOrderCompleted:
		return order.RetrySuppressed || order.LastAttemptOutcome != "" || order.LastFailureMessage != ""
	default:
		return false
	}
}

func mergeConflictFromEvents(events []core.Event, orders []core.WorkOrder) nextaction.MergeConflict {
	var approved, fresh string
	open := false
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != "merge.blocked" {
			continue
		}
		var payload struct {
			ApprovedHead string `json:"approved_head"`
			NewHead      string `json:"new_head"`
		}
		if json.Unmarshal(events[i].Payload, &payload) == nil {
			approved, fresh, open = payload.ApprovedHead, payload.NewHead, true
		}
		break
	}
	if !open {
		return nextaction.MergeConflict{}
	}
	state := MergeConflictDispatchState(events, approved, fresh, orders)
	return nextaction.MergeConflict{Open: true, Exhausted: state.Exhausted || state.Failures >= 3, NextRetryAt: state.NextRetryAt}
}

// FilterFactoryActs keeps system recovery events inside the window, newest first.
func FilterFactoryActs(events []core.Event, since time.Time, limit int) []nextaction.Act {
	if limit <= 0 {
		limit = 50
	}
	kinds := map[string]string{
		"work_order.recovery_scheduled": "recovery_scheduled",
		"work_order.recovered":          "recovered",
		"merge.conflict_fix_dispatched": "conflict_fix_dispatched",
		"task.retry_accepted":           "retry_accepted",
	}
	var acts []nextaction.Act
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		kind, ok := kinds[event.Kind]
		if !ok || event.ActorRole != core.ActorSystem || event.At.Before(since) {
			continue
		}
		acts = append(acts, nextaction.Act{ID: event.ID, TaskID: event.TaskID, Kind: kind, At: event.At, Automatic: true})
		if len(acts) == limit {
			break
		}
	}
	return acts
}

// TaskNeedsAttention is the operator-actor predicate.
func TaskNeedsAttention(action nextaction.NextAction) bool {
	return action.Actor == nextaction.ActorOperator
}

// CountOperatorAttention is the inbox and badge count. It uses the same predicate as caller attention.
func CountOperatorAttention(ctx context.Context, st Store) (int, error) {
	tasks, err := st.ListTasksFiltered(ctx, TaskFilter{})
	if err != nil {
		return 0, err
	}
	workspace, _ := WorkspaceFromContext(ctx)
	candidates := make([]core.Task, 0, len(tasks))
	for _, task := range tasks {
		if workspace != "" && task.Workspace != workspace {
			continue
		}
		if core.TaskTerminal(task.State) || core.BlueprintAnchor(task) {
			continue
		}
		candidates = append(candidates, task)
	}
	page, err := PageOperatorAttention(ctx, st, candidates, 0, len(candidates))
	if err != nil {
		return 0, err
	}
	return page.Total, nil
}

// PageOperatorAttention keeps candidate order, then keeps only operator-actor tasks.
func PageOperatorAttention(ctx context.Context, st Store, candidates []core.Task, offset, limit int) (TaskPage, error) {
	projected, err := ReadNextActions(ctx, st, candidates, NextActionOptions{})
	if err != nil {
		return TaskPage{}, err
	}
	attention := make([]core.Task, 0, len(candidates))
	for _, task := range candidates {
		if TaskNeedsAttention(projected[task.ID]) {
			attention = append(attention, task)
		}
	}
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = len(attention)
	}
	start := min(offset, len(attention))
	end := min(start+limit, len(attention))
	return TaskPage{Tasks: append([]core.Task(nil), attention[start:end]...), Total: len(attention)}, nil
}
