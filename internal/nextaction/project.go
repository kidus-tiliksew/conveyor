package nextaction

import (
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

type built struct {
	actor, kind string
	judgment    bool
	reason      string
	stall       string
	release     string
	gate        string
	order       *core.WorkOrder
	deadline    *time.Time
	blocked     []BlockerRef
	proposals   []ProposalRef
	checkpoint  string
	class       string
	citations   []core.WorkOrderAuthorityConflictCitation
	variant     string
}

type rule struct {
	match func(Input) bool
	build func(Input) built
}

// Project applies the 18 precedence rows. The first match wins. Every later
// match is recorded in details.secondary so clients do not recompute it.
func Project(in Input) NextAction {
	if in.Now.IsZero() {
		in.Now = time.Now().UTC()
	}
	rows := rules()
	var winner *built
	var secondary []Secondary
	for i := range rows {
		if !rows[i].match(in) {
			continue
		}
		built := rows[i].build(in)
		if winner == nil {
			winner = &built
			continue
		}
		secondary = append(secondary, Secondary{Kind: built.kind, ReasonCode: built.reason})
	}
	if winner == nil {
		winner = &built{actor: ActorNone, kind: KindTerminal}
	}
	return finish(in, *winner, secondary)
}

func rules() []rule {
	return []rule{
		{match: terminalTask, build: func(Input) built { return built{actor: ActorNone, kind: KindTerminal} }},
		{match: parkedTriage, build: func(Input) built {
			return built{actor: ActorOperator, kind: KindParked, judgment: true, reason: "triage.park"}
		}},
		{match: parkedFailFinal, build: func(in Input) built {
			return built{actor: ActorOperator, kind: KindDispatchExhausted, reason: "dispatch.fail_final", order: currentOrder(in)}
		}},
		{match: awaitingGate, build: gateBuild},
		{match: conflictExhausted, build: func(in Input) built {
			return built{actor: ActorOperator, kind: KindMergeConflict, reason: "merge_conflict_exhausted", order: currentOrder(in)}
		}},
		{match: conflictRetrying, build: func(in Input) built {
			return built{actor: ActorFactory, kind: KindRetrying, reason: "merge_conflict_retrying", deadline: timePtr(in.MergeConflict.NextRetryAt)}
		}},
		{match: gateOnApproved, build: func(in Input) built {
			return built{actor: ActorOperator, kind: KindMergeExecution, judgment: true, order: currentOrder(in)}
		}},
		{match: gateOffApproved, build: func(Input) built { return built{actor: ActorFactory, kind: KindMerging} }},
		{match: claimWaiting, build: func(in Input) built {
			return built{actor: ActorOperator, kind: KindDocumentProposal, judgment: true, proposals: append([]ProposalRef(nil), in.ClaimWaiting...)}
		}},
		{match: agentQuestion, build: agentQuestionBuild},
		{match: verificationDisposition, build: func(in Input) built {
			return built{actor: ActorOperator, kind: KindVerificationDisposition, judgment: true, deadline: in.Markers.VerificationHoldDeadline, order: verifyOrder(in.Orders)}
		}},
		{match: forgeFailure, build: func(Input) built { return built{actor: ActorOperator, kind: KindForgeFailure} }},
		{match: reviewRecovery, build: func(Input) built { return built{actor: ActorOperator, kind: KindReviewRecovery} }},
		{match: dependencyUnsatisfiable, build: func(in Input) built {
			hit := Stall(in.Task, in.Orders, in.Recovery, in.LiveClaimants, in.Now)
			return built{actor: ActorOperator, kind: KindDependencyUnsatisfiable, judgment: true, reason: KindDependencyUnsatisfiable, order: &hit.Order, blocked: blockersFor(in, hit.Order)}
		}},
		{match: stalled, build: func(in Input) built {
			hit := Stall(in.Task, in.Orders, in.Recovery, in.LiveClaimants, in.Now)
			return built{actor: ActorOperator, kind: KindStalled, stall: hit.Reason, reason: hit.Reason, order: &hit.Order, deadline: queueDeadline(hit.Order)}
		}},
		{match: blocked, build: func(in Input) built {
			order := latestBlocking(in.Orders)
			return built{actor: ActorDependency, kind: KindBlocked, order: order, blocked: blockersFor(in, *order)}
		}},
		{match: recoveryScheduled, build: func(in Input) built {
			order := scheduledOrder(in)
			return built{actor: ActorFactory, kind: KindRetrying, reason: "automatic_recovery", order: order, deadline: recoveryNext(in, order)}
		}},
		{match: claimedOrder, build: func(in Input) built {
			order := latestClaimed(in.Orders)
			return built{actor: ActorAgent, kind: KindWorking, order: order, deadline: executionDeadline(*order)}
		}},
		{match: queuedClaimable, build: func(in Input) built {
			order := latestClaimable(in.Orders)
			return built{actor: ActorAgent, kind: KindQueued, order: order, deadline: queueDeadline(*order)}
		}},
		{match: factoryWorking, build: func(in Input) built {
			return built{actor: ActorFactory, kind: KindWorking, order: currentOrder(in)}
		}},
	}
}

func terminalTask(in Input) bool {
	return in.Task.State == core.TaskMerged || in.Task.State == core.TaskClosed
}

func parkedTriage(in Input) bool {
	return in.Task.State == core.TaskParked && in.Markers.ParkCommand != "dispatch.fail_final"
}

func parkedFailFinal(in Input) bool {
	return in.Task.State == core.TaskParked && in.Markers.ParkCommand == "dispatch.fail_final"
}

func awaitingGate(in Input) bool {
	return in.Task.State == core.TaskAwaiting
}

func gateBuild(in Input) built {
	kind := in.Task.GateKind
	if kind == "" {
		kind = core.GateKindJobFailure
	}
	judgment := kind != core.GateKindJobFailure
	return built{actor: ActorOperator, kind: string(kind), judgment: judgment, gate: string(kind), reason: string(kind)}
}

func conflictExhausted(in Input) bool {
	return in.Task.State == core.TaskApproved && in.Task.MergeApproval && in.MergeConflict.Open && in.MergeConflict.Exhausted
}

func conflictRetrying(in Input) bool {
	if in.Task.State != core.TaskApproved || !in.Task.MergeApproval || !in.MergeConflict.Open || in.MergeConflict.Exhausted {
		return false
	}
	return !liveConflictFix(in.Orders)
}

func gateOnApproved(in Input) bool {
	return in.Task.State == core.TaskApproved && in.Task.MergeApproval
}

func gateOffApproved(in Input) bool {
	return in.Task.State == core.TaskApproved && !in.Task.MergeApproval
}

func claimWaiting(in Input) bool { return len(in.ClaimWaiting) > 0 }

func agentQuestion(in Input) bool {
	return in.Markers.CheckpointDecision && latestQueued(in.Orders) != nil
}

// agentQuestionBuild presents the latest queued order's checkpoint. An
// authority_conflict class selects the catalogue variant by whether any cited
// document has a pending proposal; other classes keep the base entry
// (req-operator-next-action AC-1.6; req-260820-394cac AC-3.2).
func agentQuestionBuild(in Input) built {
	row := built{
		actor: ActorOperator, kind: KindAgentQuestion, judgment: true, release: ReleaseCheckpoint,
		checkpoint: in.Markers.Checkpoint, class: in.Markers.CheckpointClass,
		citations: append([]core.WorkOrderAuthorityConflictCitation(nil), in.Markers.Citations...),
		order:     latestQueued(in.Orders),
	}
	if row.class != core.WorkOrderCheckpointClassAuthorityConflict {
		return row
	}
	row.proposals = CitedProposals(in.Markers.Citations, in.PendingProposals)
	row.variant = AgentQuestionRevisionNeeded
	if len(row.proposals) > 0 {
		row.variant = AgentQuestionCitedRevisions
	}
	return row
}

func verificationDisposition(in Input) bool {
	return in.Markers.VerificationDisposition || (in.Markers.VerificationHoldDeadline != nil && verifyOrder(in.Orders) != nil)
}

func forgeFailure(in Input) bool { return in.Markers.ForgeFailure }

func reviewRecovery(in Input) bool {
	return in.Markers.ReviewRecovery || in.Markers.InterruptedReview
}

func dependencyUnsatisfiable(in Input) bool {
	hit := Stall(in.Task, in.Orders, in.Recovery, in.LiveClaimants, in.Now)
	return hit != nil && hit.DependencyUnsatisfiable
}

func stalled(in Input) bool {
	hit := Stall(in.Task, in.Orders, in.Recovery, in.LiveClaimants, in.Now)
	return hit != nil && hit.Reason != ""
}

func blocked(in Input) bool { return latestBlocking(in.Orders) != nil }

func recoveryScheduled(in Input) bool { return scheduledOrder(in) != nil }

func claimedOrder(in Input) bool { return latestClaimed(in.Orders) != nil }

func queuedClaimable(in Input) bool { return latestClaimable(in.Orders) != nil }

func factoryWorking(in Input) bool {
	return in.Task.State == core.TaskClaiming || in.Task.State == core.TaskRunning || in.Task.State == core.TaskQueued
}

func finish(in Input, row built, secondary []Secondary) NextAction {
	entry, _ := LookupVariant(row.kind, row.stall, row.release, row.gate, row.variant)
	vars := varsFor(in, row)
	summary := render(entry.Summary, vars)
	if row.kind == KindRetrying {
		summary = summaryWithRetry(summary, vars)
	}
	specs := entry.Choices
	switch {
	case row.kind == KindDocumentProposal:
		specs = withheldProposalChoices(row.proposals)
	case row.kind == KindAgentQuestion && len(row.proposals) > 0:
		specs = append(citedProposalChoices(row.proposals), entry.Choices...)
	}
	choices, refusals := selectChoices(in, specs, row.order)
	if in.Task.GateKind == core.GateKindPlanRevision {
		if text := core.NextActionPlainApproveRefusal(in.Task.GateKind); text != "" {
			refusals = append(refusals, Refusal{Choice: "approve", Text: text})
		}
	}
	na := NextAction{
		Label: entry.Label, Actor: row.actor, Kind: row.kind, Judgment: row.judgment,
		Summary: summary, Question: render(entry.Question, vars), Choices: choices,
		IfIgnored: ifIgnored(entry, vars, row), Deadline: row.deadline, BlockedBy: row.blocked,
		CurrentOrder: orderView(row.order), Attempt: attemptView(in, row.order),
		Details: Details{
			GateKind: row.gate, ReasonCode: row.reason, StallReason: row.stall, ReleaseOutcome: row.release,
			OrderIDs: orderIDs(row.order), Checkpoint: row.checkpoint, CheckpointClass: row.class,
			Citations: row.citations, PendingProposals: row.proposals,
			Secondary: secondary, Refusals: refusals,
		},
	}
	if in.MergeReadiness != nil && in.MergeReadiness.Summary != "" {
		na.Details.MergeReadiness = in.MergeReadiness.Summary
	}
	if na.Choices == nil {
		na.Choices = []Choice{}
	}
	if na.Label == "" {
		na.Label = row.kind
	}
	return na
}

func ifIgnored(entry Entry, vars Vars, row built) string {
	text := render(entry.IfIgnored, vars)
	if row.kind == KindRetrying && vars.RetryAt != "" {
		return summaryWithRetry(text, vars)
	}
	return text
}

func varsFor(in Input, row built) Vars {
	vars := Vars{Title: in.Task.Title, PR: in.PullRequest, Stage: string(in.Task.NextStage), Branch: in.Task.Branch, Base: in.Task.BaseBranch, State: string(in.Task.State), Next: string(in.Task.NextStage)}
	if in.LatestSpec != nil {
		vars.Version = in.LatestSpec.Version
	}
	if row.order != nil {
		vars.OrderID = row.order.ID
		vars.Stage = string(row.order.Stage)
		vars.Attempts = row.order.AutomaticRetryCount
	}
	if in.MergeConflict.NextRetryAt.After(in.Now) {
		vars.RetryAt = in.MergeConflict.NextRetryAt.UTC().Format(time.RFC3339)
	}
	if row.order != nil {
		if next := recoveryNext(in, row.order); next != nil {
			vars.RetryAt = next.UTC().Format(time.RFC3339)
			if attempt := in.Recovery[row.order.ID].Attempt; attempt > 0 {
				vars.Attempts = attempt
			}
		}
	}
	for i, blocker := range in.Blockers {
		if i > 0 {
			vars.Blockers += ", "
		}
		vars.Blockers += blocker.Title
	}
	return vars
}

func orderView(order *core.WorkOrder) *OrderView {
	if order == nil {
		return nil
	}
	return &OrderView{
		ID: order.ID, Stage: string(order.Stage), State: string(order.State),
		QueueDeadline: timePtr(order.QueueDeadline), ExecutionDeadline: timePtr(order.ExecutionDeadline),
		LeaseExpiresAt: timePtr(order.LeaseExpiresAt), RecoveryRefusal: order.RecoveryRefusal,
	}
}

func attemptView(in Input, order *core.WorkOrder) *AttemptView {
	if order == nil || order.State != core.WorkOrderClaimed {
		return nil
	}
	user := ""
	if in.Assignee != nil {
		user = in.Assignee.UserID
	}
	elapsed := int64(0)
	if !order.ExecutionStartedAt.IsZero() {
		elapsed = int64(in.Now.Sub(order.ExecutionStartedAt).Seconds())
		if elapsed < 0 {
			elapsed = 0
		}
	}
	return &AttemptView{
		Harness: order.Agent, Model: order.Model, User: user, ClaimedBy: order.ClaimantID,
		SessionID: order.SessionID, AttemptID: order.AttemptID,
		ClaimedAt: timePtr(order.ExecutionStartedAt), LastCheckIn: timePtr(order.LastAgentActivityAt),
		LeaseExpiresAt: timePtr(order.LeaseExpiresAt), Progress: order.Progress, ElapsedSeconds: elapsed,
	}
}

func orderIDs(order *core.WorkOrder) []string {
	if order == nil {
		return nil
	}
	return []string{order.ID}
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	utc := t.UTC()
	return &utc
}

func queueDeadline(order core.WorkOrder) *time.Time { return timePtr(order.QueueDeadline) }

func executionDeadline(order core.WorkOrder) *time.Time { return timePtr(order.ExecutionDeadline) }

func liveConflictFix(orders []core.WorkOrder) bool {
	for _, order := range orders {
		if order.ReasonCode != ConflictFixReason {
			continue
		}
		if order.State == core.WorkOrderQueued || order.State == core.WorkOrderClaimed {
			return true
		}
	}
	return false
}

func latestQueued(orders []core.WorkOrder) *core.WorkOrder {
	for i := len(orders) - 1; i >= 0; i-- {
		if orders[i].State == core.WorkOrderQueued {
			order := orders[i]
			return &order
		}
	}
	return nil
}

func latestClaimed(orders []core.WorkOrder) *core.WorkOrder {
	for i := len(orders) - 1; i >= 0; i-- {
		if orders[i].State == core.WorkOrderClaimed {
			order := orders[i]
			return &order
		}
	}
	return nil
}

func latestClaimable(orders []core.WorkOrder) *core.WorkOrder {
	for i := len(orders) - 1; i >= 0; i-- {
		order := orders[i]
		if order.State == core.WorkOrderQueued && order.Claimable && order.RecoveryRefusal == "" && len(order.BlockingTaskIDs) == 0 {
			return &order
		}
	}
	return nil
}

func latestBlocking(orders []core.WorkOrder) *core.WorkOrder {
	for i := len(orders) - 1; i >= 0; i-- {
		if orders[i].State == core.WorkOrderQueued && len(orders[i].BlockingTaskIDs) > 0 {
			order := orders[i]
			return &order
		}
	}
	return nil
}

func verifyOrder(orders []core.WorkOrder) *core.WorkOrder {
	for i := len(orders) - 1; i >= 0; i-- {
		if orders[i].Stage == core.StageVerify && (orders[i].State == core.WorkOrderQueued || orders[i].State == core.WorkOrderClaimed) {
			order := orders[i]
			return &order
		}
	}
	return nil
}

func currentOrder(in Input) *core.WorkOrder {
	if order := latestClaimed(in.Orders); order != nil {
		return order
	}
	return latestQueued(in.Orders)
}

func scheduledOrder(in Input) *core.WorkOrder {
	for i := len(in.Orders) - 1; i >= 0; i-- {
		order := in.Orders[i]
		rec := in.Recovery[order.ID]
		if rec.NextAt != nil && rec.NextAt.After(in.Now) {
			return &order
		}
	}
	return nil
}

func recoveryNext(in Input, order *core.WorkOrder) *time.Time {
	if order == nil {
		return nil
	}
	rec := in.Recovery[order.ID]
	if rec.NextAt == nil {
		return nil
	}
	return rec.NextAt
}

func blockersFor(in Input, order core.WorkOrder) []BlockerRef {
	if len(in.Blockers) > 0 {
		return append([]BlockerRef(nil), in.Blockers...)
	}
	var out []BlockerRef
	for _, id := range order.BlockingTaskIDs {
		out = append(out, BlockerRef{TaskID: id})
	}
	for _, id := range order.UnsatisfiableTaskIDs {
		out = append(out, BlockerRef{TaskID: id, State: "closed"})
	}
	return out
}
