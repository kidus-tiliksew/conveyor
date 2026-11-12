package httpapi

import (
	"sort"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
)

// Plan cycles group a task's plan history into one timeline entry each
// (DEC-130; component-web-task-surfaces invariant 67; req-260809-a40c75
// AC-4.1, AC-4.2). The server folds them from the complete ledger by
// (at, id), so the dashboard never derives a plan version, an outcome, or a
// person from the bounded events page (DEC-130(2)).

// The plan-cycle wire vocabulary of component-web-task-surfaces invariant 67
// and the ledger kinds the fold reads.
const (
	planCycleOpenedByPlan             = "plan"
	planCycleOpenedByRevisionRequest  = "revision_request"
	planCycleOpenedByPlanningRequest  = "planning_request"
	planCycleOutcomeApproved          = "approved"
	planCycleOutcomeAwaitingDecision  = "awaiting_decision"
	planCycleOutcomePlanning          = "planning"
	planCycleOutcomeDeclined          = "declined"
	planCycleOutcomeNotNeeded         = "not_needed"
	planCycleOutcomeClosed            = "closed"
	planCycleOutcomeMerged            = "merged"
	planCycleStepRevisionRequested    = "revision_requested"
	planCycleStepPlanningRequested    = "planning_requested"
	planCycleStepRevisionApproved     = "revision_approved"
	planCycleStepRevisionDeclined     = "revision_declined"
	planCycleStepRevisionRejected     = "revision_rejected"
	planCycleStepPlanDrafted          = "plan_drafted"
	planCycleStepPlanFiled            = "plan_filed"
	planCycleStepPlanAttached         = "plan_attached"
	planCycleStepChangesRequested     = "changes_requested"
	planCycleStepPlanApproved         = "plan_approved"
	planCycleStepPlanSentBack         = "plan_sent_back"
	planCycleStepPlanNotNeeded        = "plan_not_needed"
	planCycleStepTaskStartedOver      = "task_started_over"
	planCycleStepTaskClosed           = "task_closed"
	planCycleStepTaskMerged           = "task_merged"
	planCycleEventPlanVersionCreated  = "spec.version_created"
	planCycleEventPlanVersionApproved = "spec.version_approved"
	planCycleEventRevisionRequested   = "work_order.plan_revision_requested"
	planCycleEventSetupChanged        = "task.setup.changed"
	planCycleEventTaskStateChanged    = "task.state_changed"
	planCycleInterventionEventPrefix  = "intervention."
)

// taskPlanCycle is one plan cycle of summaries.plan_cycles.
type taskPlanCycle struct {
	OpenedBy         string              `json:"opened_by"`
	ContestedVersion *int                `json:"contested_version"`
	FirstVersion     *int                `json:"first_version"`
	FinalVersion     *int                `json:"final_version"`
	Outcome          string              `json:"outcome"`
	Steps            []taskPlanCycleStep `json:"steps"`
}

// taskPlanCycleStep is one step of a plan cycle. EventID names the event that
// records the step, and InterventionID the intervention when one records it,
// so the timeline absorbs both (component-web-task-surfaces invariant 68).
type taskPlanCycleStep struct {
	Kind           string `json:"kind"`
	At             string `json:"at"`
	EventID        string `json:"event_id"`
	PlanVersion    *int   `json:"plan_version,omitempty"`
	UserID         string `json:"user_id,omitempty"`
	InterventionID *int64 `json:"intervention_id,omitempty"`
	Text           string `json:"text,omitempty"`
	Truncated      bool   `json:"truncated,omitempty"`
	// actorID is the recorded actor of a person's act whose user the
	// read resolves after the fold: an agent:<id> credential's owner
	// (req-security-boundaries AC-1.3).
	actorID string
}

// planCycleGate is what an open cycle waits on.
type planCycleGate int

const (
	// planCycleGateNone: the cycle waits on a new plan version.
	planCycleGateNone planCycleGate = iota
	// planCycleGatePlan: a plan version waits at the plan_approval gate.
	planCycleGatePlan
	// planCycleGateRevision: a revision or planning request waits on the
	// operator's plan-revision decision.
	planCycleGateRevision
)

// planCycleFold is the state of derivePlanCycles over the ordered ledger.
type planCycleFold struct {
	cycles []taskPlanCycle
	// open is the index of the cycle that has not settled, or -1.
	open int
	gate planCycleGate
	// pendingVersion is the plan version at the plan_approval gate.
	pendingVersion int
	interventions  []core.Intervention
	used           []bool
	// setupChanges are the ledger positions of task.setup.changed events
	// that record a plan_state change, with the consumed flag.
	setupChanges []int
	setupUsed    []bool
	ordered      []core.Event
	// awaited are the gate decisions whose step a version approval, a
	// plan-state move, or a task state change recorded before the decision's
	// intervention event.
	awaited []planCycleAwaitedDecision
}

// planCycleAwaitedDecision is a plan-gate decision step recorded before the
// decision's intervention event. The dispatcher applies a plan-gate decision
// before the API records its intervention, so on every backend the version
// approval, the plan-state move, and the task state change precede the
// intervention event (component-web-task-surfaces invariants 67 and 68). The
// intervention event that follows enriches this step instead of adding one.
type planCycleAwaitedDecision struct {
	cycle, step int
	action      core.InterventionAction
	// reason is the reason code the decision must carry, or "" for any.
	reason string
}

// derivePlanCycles folds summaries.plan_cycles from the complete ledger in
// (at, id) order and the task's complete interventions
// (component-web-task-surfaces invariants 25 and 67). A plan version's step
// kind comes from its recorded origin, a plan-state step from the move's
// cause, and a decision from its intervention. No step takes its kind or its
// person from an event's actor: a self-claimed agent session records the
// operator's account as actor (DEC-130 context).
func derivePlanCycles(events []core.Event, interventions []core.Intervention) []taskPlanCycle {
	fold := &planCycleFold{open: -1, ordered: sortedTaskEvents(events)}
	fold.interventions = sortedInterventions(interventions)
	fold.used = make([]bool, len(fold.interventions))
	for i, event := range fold.ordered {
		if event.Kind == planCycleEventSetupChanged {
			if _, ok := decodeSummaryPayload(event.Payload)["plan_state"].(map[string]any); ok {
				fold.setupChanges = append(fold.setupChanges, i)
			}
		}
	}
	fold.setupUsed = make([]bool, len(fold.setupChanges))
	for i := range fold.ordered {
		fold.apply(i)
	}
	if fold.open >= 0 {
		cycle := &fold.cycles[fold.open]
		switch fold.gate {
		case planCycleGatePlan, planCycleGateRevision:
			cycle.Outcome = planCycleOutcomeAwaitingDecision
		default:
			cycle.Outcome = planCycleOutcomePlanning
		}
	}
	if fold.cycles == nil {
		return []taskPlanCycle{}
	}
	return fold.cycles
}

// sortedInterventions orders a copy of the interventions by (at, id).
func sortedInterventions(interventions []core.Intervention) []core.Intervention {
	sorted := append([]core.Intervention(nil), interventions...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].At.Equal(sorted[j].At) {
			return sorted[i].At.Before(sorted[j].At)
		}
		return sorted[i].ID < sorted[j].ID
	})
	return sorted
}

func (f *planCycleFold) apply(index int) {
	event := f.ordered[index]
	payload := decodeSummaryPayload(event.Payload)
	f.expireAwaited(event, payload)
	switch {
	case event.Kind == planCycleEventPlanVersionCreated:
		f.planVersion(event, payload)
	case event.Kind == planCycleEventRevisionRequested:
		f.request(event, payload)
	case event.Kind == planCycleEventPlanVersionApproved:
		// A version approval records the approval step: one under a policy
		// that approves the plan on submission, and one an operator's
		// approval writes before its intervention event, which then names
		// itself on this step. An intervention recorded first has already
		// settled the cycle.
		if version, ok := positiveVersion(payload, "version"); ok && f.gate == planCycleGatePlan && f.open >= 0 && version == f.pendingVersion {
			f.await(core.InterventionApprove, "")
			f.settle(planCycleOutcomeApproved, f.step(planCycleStepPlanApproved, event, &version))
		}
	case event.Kind == core.EventTaskPlanStateChanged:
		f.planStateMove(index, event, payload)
	case event.Kind == planCycleEventTaskStateChanged:
		f.taskStateChange(event, payload)
	case strings.HasPrefix(event.Kind, planCycleInterventionEventPrefix):
		f.decision(event, payload)
	}
}

// step builds a step recorded by event.
func (f *planCycleFold) step(kind string, event core.Event, version *int) taskPlanCycleStep {
	step := taskPlanCycleStep{Kind: kind, At: formatTaskEventTime(event.At), EventID: formatTaskEventID(event.ID)}
	if version != nil {
		value := *version
		step.PlanVersion = &value
	}
	return step
}

func (f *planCycleFold) add(step taskPlanCycleStep) {
	cycle := &f.cycles[f.open]
	cycle.Steps = append(cycle.Steps, step)
}

// settle adds the closing step and settles the open cycle.
func (f *planCycleFold) settle(outcome string, step taskPlanCycleStep) {
	f.add(step)
	f.cycles[f.open].Outcome = outcome
	f.open, f.gate, f.pendingVersion = -1, planCycleGateNone, 0
}

func (f *planCycleFold) openCycle(openedBy string, contested *int) {
	f.cycles = append(f.cycles, taskPlanCycle{OpenedBy: openedBy, ContestedVersion: contested, Steps: []taskPlanCycleStep{}})
	f.open, f.gate, f.pendingVersion = len(f.cycles)-1, planCycleGateNone, 0
}

// planVersion records a plan version. Its kind follows the recorded origin:
// plan_order, intake, or operator (component-plan-state invariant 2). A
// version whose payload names no origin predates the field, when only a plan
// order recorded versions.
func (f *planCycleFold) planVersion(event core.Event, payload summaryPayload) {
	version, ok := positiveVersion(payload, "version")
	if !ok {
		return
	}
	kind := planCycleStepPlanDrafted
	origin, _ := payload.str("origin")
	switch core.PlanOrigin(origin) {
	case core.PlanOriginIntake:
		kind = planCycleStepPlanFiled
	case core.PlanOriginOperator:
		kind = planCycleStepPlanAttached
	}
	if f.open < 0 {
		f.openCycle(planCycleOpenedByPlan, nil)
	}
	step := f.step(kind, event, &version)
	if kind == planCycleStepPlanAttached {
		// Attaching a plan is an operator act (component-plan-state invariant
		// 5): its kind comes from the origin, and the recorded actor of that
		// act names the person. The attach reason reads as the comment.
		step.actorID = event.ActorID
		reason, _ := payload.str("reason")
		step.Text, step.Truncated = truncateSummaryText(jsTrim(reason))
	}
	f.add(step)
	cycle := &f.cycles[f.open]
	if cycle.FirstVersion == nil {
		first := version
		cycle.FirstVersion = &first
	}
	final := version
	cycle.FinalVersion = &final
	f.gate, f.pendingVersion = planCycleGatePlan, version
}

// request opens a cycle for an agent's revision or planning request
// (req-260809-a40c75 AC-1.3). A request without a rationale, or with a plan
// version that is not a positive integer, is not decidable and opens nothing
// (planRevisionRequestFrom).
func (f *planCycleFold) request(event core.Event, payload summaryPayload) {
	request := planRevisionRequestFrom(event, payload)
	if request == nil {
		return
	}
	var contested *int
	kind, openedBy := planCycleStepPlanningRequested, planCycleOpenedByPlanningRequest
	if request.PlanVersion != nil {
		value := int(*request.PlanVersion)
		contested = &value
		kind, openedBy = planCycleStepRevisionRequested, planCycleOpenedByRevisionRequest
	}
	if f.open >= 0 {
		// A cycle still open when the next request arrives keeps the outcome
		// it reached; the request starts its own cycle.
		cycle := &f.cycles[f.open]
		cycle.Outcome = planCycleOutcomePlanning
		if f.gate != planCycleGateNone {
			cycle.Outcome = planCycleOutcomeAwaitingDecision
		}
	}
	f.openCycle(openedBy, contested)
	step := f.step(kind, event, contested)
	step.Text, step.Truncated = request.Rationale, request.Truncated
	f.add(step)
	f.gate = planCycleGateRevision
}

// await records that the step the open cycle adds next is a gate decision
// whose intervention event has not been folded yet. Call it before the step is
// added.
func (f *planCycleFold) await(action core.InterventionAction, reason string) {
	f.awaited = append(f.awaited, planCycleAwaitedDecision{
		cycle: f.open, step: len(f.cycles[f.open].Steps), action: action, reason: reason,
	})
}

// expireAwaited ends the wait for decisions that the ledger has moved past.
// Every awaited decision is applied before the API records its intervention,
// within the same request, so the wait ends at the next intervention event,
// the next plan version or request, an operator's plan-state move, and any
// task state change that does not leave the human gate. Only the gate
// decision itself leaves that gate, so a later merge approval or review
// redirect never reaches an earlier plan step.
func (f *planCycleFold) expireAwaited(event core.Event, payload summaryPayload) {
	if len(f.awaited) == 0 {
		return
	}
	switch event.Kind {
	case planCycleEventPlanVersionCreated, planCycleEventRevisionRequested:
	case core.EventTaskPlanStateChanged:
		if cause, _ := payload.str("cause"); core.PlanStateCause(cause) != core.PlanCauseOperator {
			return
		}
	case planCycleEventTaskStateChanged:
		if from, _ := payload.str("from"); core.TaskState(from) == core.TaskAwaiting {
			return
		}
	default:
		// decision consumes the wait of an intervention event.
		return
	}
	f.awaited = f.awaited[:0]
}

// decision applies an operator decision to the open cycle. The intervention
// event joins its intervention record by the fields both record: the action,
// the time, the actor, the reason code, and the comment. A decision whose
// step an earlier event already recorded names itself on that step. A
// decision outside the plan gates, such as a merge approval or a review-round
// redirect, stays out of every cycle and keeps its own timeline entry.
func (f *planCycleFold) decision(event core.Event, payload summaryPayload) {
	action := core.InterventionAction(strings.TrimPrefix(event.Kind, planCycleInterventionEventPrefix))
	reason, _ := payload.str("reason_code")
	comment, _ := payload.str("comment")
	awaited := f.awaited
	f.awaited = nil
	for _, wait := range awaited {
		if wait.action != action || (wait.reason != "" && wait.reason != reason) {
			continue
		}
		f.join(&f.cycles[wait.cycle].Steps[wait.step], event, action, reason, comment)
		return
	}
	if f.open < 0 {
		return
	}
	var kind, outcome string
	var version *int
	switch f.gate {
	case planCycleGateRevision:
		switch {
		case action == core.InterventionRedirect && reason == dispatch.PlanRevisionApprovedReasonCode:
			kind = planCycleStepRevisionApproved
		case action == core.InterventionRedirect && reason == dispatch.PlanRevisionDeclinedReasonCode:
			kind, outcome = planCycleStepRevisionDeclined, planCycleOutcomeDeclined
		case action == core.InterventionReject:
			kind, outcome = planCycleStepRevisionRejected, planCycleOutcomeClosed
		}
		version = f.cycles[f.open].ContestedVersion
	case planCycleGatePlan:
		pending := f.pendingVersion
		version = &pending
		switch action {
		case core.InterventionApprove:
			kind, outcome = planCycleStepPlanApproved, planCycleOutcomeApproved
		case core.InterventionRedirect:
			kind = planCycleStepChangesRequested
		case core.InterventionReject:
			kind, outcome, version = planCycleStepTaskClosed, planCycleOutcomeClosed, nil
		}
	}
	if action == core.InterventionCancel {
		kind, outcome, version = planCycleStepTaskClosed, planCycleOutcomeClosed, nil
	}
	if kind == "" {
		return
	}
	step := f.step(kind, event, version)
	f.join(&step, event, action, reason, comment)
	if outcome != "" {
		f.settle(outcome, step)
		return
	}
	f.add(step)
	f.gate, f.pendingVersion = planCycleGateNone, 0
}

// join names on step the decision that the intervention event records: the
// intervention, its person, and its comment. An event whose intervention
// record is missing leaves the step without a person; nothing is invented.
func (f *planCycleFold) join(step *taskPlanCycleStep, event core.Event, action core.InterventionAction, reason, comment string) {
	if intervention, ok := f.intervention(event, action, reason, comment); ok {
		id := intervention.ID
		step.InterventionID = &id
		step.UserID, step.actorID = interventionPerson(intervention)
	}
	step.Text, step.Truncated = truncateSummaryText(jsTrim(comment))
}

// intervention returns the first unused intervention the event records.
func (f *planCycleFold) intervention(event core.Event, action core.InterventionAction, reason, comment string) (core.Intervention, bool) {
	for i, intervention := range f.interventions {
		if f.used[i] || intervention.Action != action || !intervention.At.Equal(event.At) ||
			intervention.ReasonCode != reason || intervention.Comment != comment || intervention.ActorID != event.ActorID {
			continue
		}
		f.used[i] = true
		return intervention, true
	}
	return core.Intervention{}, false
}

// interventionPerson is the person an intervention records: a human
// user:<id> actor, or, after the read resolves it, the user who owns a
// retired agent credential (interventionUserId in run-by.tsx).
func interventionPerson(intervention core.Intervention) (string, string) {
	if owner := strings.TrimSpace(intervention.ActorOwnerUserID); owner != "" {
		return owner, ""
	}
	if intervention.ActorRole != core.ActorHuman {
		return "", ""
	}
	return humanUserID(intervention.ActorID), ""
}

// humanUserID is the user ID of a user:<id> actor, and "" for any other.
func humanUserID(actorID string) string {
	id, ok := strings.CutPrefix(actorID, "user:")
	if !ok {
		return ""
	}
	return strings.TrimSpace(id)
}

// planStateMove reads a task.plan_state_changed move by its cause
// (component-plan-state invariant 3). An operator's move at the plan gate to
// needed sends the plan back and keeps the cycle planning; a move to
// not_required ends it. Every other cause repeats an act a version or a
// decision already recorded, and adds a step only when neither did. That step
// waits for the decision's intervention event, which the API records after
// the move.
func (f *planCycleFold) planStateMove(index int, event core.Event, payload summaryPayload) {
	if f.open < 0 {
		return
	}
	cause, _ := payload.str("cause")
	to, _ := payload.str("to")
	switch core.PlanStateCause(cause) {
	case core.PlanCauseOperator:
		var kind, outcome string
		switch {
		case core.PlanState(to) == core.PlanNotRequired:
			kind, outcome = planCycleStepPlanNotNeeded, planCycleOutcomeNotNeeded
		case core.PlanState(to) == core.PlanNeeded && f.gate == planCycleGatePlan:
			kind = planCycleStepPlanSentBack
		default:
			return
		}
		step := f.step(kind, event, nil)
		if f.gate == planCycleGatePlan {
			version := f.pendingVersion
			step.PlanVersion = &version
		}
		from, _ := payload.str("from")
		if actor, reason, ok := f.setupChange(index, from, to); ok {
			step.UserID = humanUserID(actor)
			if step.UserID == "" {
				step.actorID = actor
			}
			step.Text, step.Truncated = truncateSummaryText(jsTrim(reason))
		}
		if outcome != "" {
			f.settle(outcome, step)
			return
		}
		f.add(step)
		f.gate, f.pendingVersion = planCycleGateNone, 0
	case core.PlanCausePlanRedirected:
		if f.gate == planCycleGatePlan {
			version := f.pendingVersion
			f.await(core.InterventionRedirect, "")
			f.add(f.step(planCycleStepChangesRequested, event, &version))
			f.gate, f.pendingVersion = planCycleGateNone, 0
		}
	case core.PlanCausePlanApproved:
		if f.gate == planCycleGatePlan {
			version := f.pendingVersion
			f.await(core.InterventionApprove, "")
			f.settle(planCycleOutcomeApproved, f.step(planCycleStepPlanApproved, event, &version))
		}
	case core.PlanCausePlanRevisionApproved, core.PlanCausePlanningRequestedApproved:
		if f.gate == planCycleGateRevision {
			f.await(core.InterventionRedirect, dispatch.PlanRevisionApprovedReasonCode)
			f.add(f.step(planCycleStepRevisionApproved, event, f.cycles[f.open].ContestedVersion))
			f.gate = planCycleGateNone
		}
	}
}

// setupChange returns the actor and reason of the task.setup.changed event
// that records the same operator plan_state change, the nearest unused one
// with the same from and to. The act's own payload names its actor.
func (f *planCycleFold) setupChange(index int, from, to string) (string, string, bool) {
	best := -1
	for i, position := range f.setupChanges {
		if f.setupUsed[i] {
			continue
		}
		payload := decodeSummaryPayload(f.ordered[position].Payload)
		move, _ := payload["plan_state"].(map[string]any)
		moveFrom, _ := move["from"].(string)
		moveTo, _ := move["to"].(string)
		if moveFrom != from || moveTo != to {
			continue
		}
		if best < 0 || absInt(position-index) < absInt(f.setupChanges[best]-index) {
			best = i
		}
	}
	if best < 0 {
		return "", "", false
	}
	f.setupUsed[best] = true
	payload := decodeSummaryPayload(f.ordered[f.setupChanges[best]].Payload)
	actor, _ := payload.str("actor")
	reason, _ := payload.str("reason")
	return actor, reason, true
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

// taskStateChange closes a cycle still open when the task ends: a merge
// takes outcome merged, and a close or a start-over takes outcome closed.
func (f *planCycleFold) taskStateChange(event core.Event, payload summaryPayload) {
	if f.open < 0 {
		return
	}
	to, _ := payload.str("to")
	command, _ := payload.str("command")
	switch core.TaskState(to) {
	case core.TaskMerged:
		f.settle(planCycleOutcomeMerged, f.step(planCycleStepTaskMerged, event, nil))
	case core.TaskClosed:
		kind := planCycleStepTaskClosed
		switch core.TaskCommand(command) {
		case core.TaskStartOver:
			kind = planCycleStepTaskStartedOver
		case core.TaskInterventionReject:
			// A reject at a gate closes the task before the API records the
			// reject intervention, which then names itself on this step.
			if f.gate != planCycleGateNone {
				f.await(core.InterventionReject, "")
			}
		}
		f.settle(planCycleOutcomeClosed, f.step(kind, event, nil))
	}
}

// positiveVersion reads a positive integer plan version.
func positiveVersion(payload summaryPayload, key string) (int, bool) {
	number, ok := payload.num(key)
	if !ok || !jsIsInteger(number) || number <= 0 || number > 1<<31 {
		return 0, false
	}
	return int(number), true
}

// planCycleActorIDs lists the actors whose owner the read resolves.
func planCycleActorIDs(cycles []taskPlanCycle) []string {
	var actorIDs []string
	for _, cycle := range cycles {
		for _, step := range cycle.Steps {
			if step.actorID != "" {
				actorIDs = append(actorIDs, step.actorID)
			}
		}
	}
	return actorIDs
}

// resolvePlanCyclePeople sets each step's person from its recorded actor: a
// user:<id> actor's user, or an agent:<id> credential's owner. An actor that
// resolves to no person leaves the step without one; nothing is invented.
func resolvePlanCyclePeople(cycles []taskPlanCycle, owners map[string]string) {
	for c := range cycles {
		for s := range cycles[c].Steps {
			step := &cycles[c].Steps[s]
			if step.actorID == "" {
				continue
			}
			if step.UserID == "" {
				step.UserID = humanUserID(step.actorID)
			}
			if step.UserID == "" {
				step.UserID = owners[step.actorID]
			}
			step.actorID = ""
		}
	}
}
