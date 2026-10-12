package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
)

// NextActionOptions is the caller view the projection cannot read from the task.
type NextActionOptions struct {
	Now             time.Time
	Capabilities    map[core.Capability]bool
	CanChangePolicy bool
	LiveClaimants   *nextaction.ClaimantView
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
	orders, err := st.ListWorkOrdersForTasks(ctx, ids)
	if err != nil {
		return nil, err
	}
	markers, err := st.ListActivityMarkersForTasks(ctx, ids)
	if err != nil {
		return nil, err
	}
	proposals, err := st.ListPendingProposals(ctx)
	if err != nil {
		return nil, err
	}
	ordersByTask := map[string][]core.WorkOrder{}
	for _, order := range orders {
		ordersByTask[order.TaskID] = append(ordersByTask[order.TaskID], order)
	}
	markerByTask := map[string]ActivityMarker{}
	for _, marker := range markers {
		markerByTask[marker.TaskID] = marker
	}
	out := make(map[string]nextaction.NextAction, len(tasks))
	for _, task := range tasks {
		input, err := assembleInput(ctx, st, task, ordersByTask[task.ID], markerByTask[task.ID], proposals, opts)
		if err != nil {
			return nil, err
		}
		out[task.ID] = nextaction.Project(input)
	}
	return out, nil
}

func assembleInput(ctx context.Context, st Store, task core.Task, orders []core.WorkOrder, marker ActivityMarker, proposals []core.PendingProposal, opts NextActionOptions) (nextaction.Input, error) {
	var events []core.Event
	if task.State == core.TaskParked || task.State == core.TaskApproved || task.State == core.TaskAwaiting {
		var err error
		events, err = st.ListEvents(ctx, task.ID)
		if err != nil {
			return nextaction.Input{}, err
		}
	}
	var spec core.SpecVersion
	var found bool
	if task.State == core.TaskAwaiting {
		var err error
		spec, found, err = st.GetLatestSpecVersion(ctx, task.ID)
		if err != nil {
			return nextaction.Input{}, err
		}
	}
	blockers, err := blockerRefs(ctx, st, task.ID)
	if err != nil {
		return nextaction.Input{}, err
	}
	for i := range orders {
		pendingRevision := task.State == core.TaskAwaiting && task.GateKind == core.GateKindPlanRevision
		ApplyRecoveryRefusal(&orders[i], task, orders, pendingRevision)
	}
	conflict := mergeConflictFromEvents(events, orders)
	input := nextaction.Input{
		Task: task, Orders: orders, Markers: markersFrom(marker, events, orders),
		ClaimWaiting: claimWaiting(proposals, orders, task.ID),
		Blockers:     blockers, MergeConflict: conflict, Recovery: map[string]nextaction.AutomaticRecovery{},
		Now: opts.Now, Capabilities: opts.Capabilities, LiveClaimants: opts.LiveClaimants,
	}
	if found {
		input.LatestSpec = &nextaction.SpecRef{Version: spec.Version}
		input.GateSpecVersion = spec.Version
	}
	return input, nil
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
		if order.Checkpoint != nil && order.Checkpoint.DecisionRequest != "" {
			out.CheckpointDecision = true
		}
		if order.Checkpoint != nil && order.Checkpoint.Verification != nil {
			out.VerificationDisposition = true
		}
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

func claimWaiting(proposals []core.PendingProposal, orders []core.WorkOrder, taskID string) []nextaction.ProposalRef {
	refs := proposalRefs(proposals, taskID)
	if pendingAuthorityTaskIDs(orders, proposals)[taskID] {
		return refs
	}
	var context []nextaction.ProposalRef
	for _, ref := range refs {
		if ref.Tier == "task_context" {
			context = append(context, ref)
		}
	}
	return context
}

func proposalRefs(proposals []core.PendingProposal, taskID string) []nextaction.ProposalRef {
	var out []nextaction.ProposalRef
	for _, item := range proposals {
		if item.OriginType == "task" && item.OriginID == taskID {
			out = append(out, nextaction.ProposalRef{Tier: item.Tier, ID: item.ID, Version: item.Version})
		}
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

func blockerRefs(ctx context.Context, st Store, taskID string) ([]nextaction.BlockerRef, error) {
	ids, err := st.ListBlockingTaskIDs(ctx, taskID)
	if err != nil {
		return nil, err
	}
	out := make([]nextaction.BlockerRef, 0, len(ids))
	for _, id := range ids {
		blocker, err := st.GetTask(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, nextaction.BlockerRef{TaskID: blocker.ID, Title: blocker.Title, State: string(blocker.State)})
	}
	return out, nil
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
