package store

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// PlanStateRequest is the plan-state half of a task intake request after the
// route has refused malformed input (component-plan-state invariant 2).
type PlanStateRequest struct {
	PlanState    *core.PlanState
	SpecApproval *bool
	Level        core.EscalationLevel
	HasPlan      bool
	PlanApproved bool
}

// ResolveIntakePlanState applies component-plan-state invariant 1. A plan
// filed at intake sets awaiting_approval, or completed when its filer approved
// it, and no level decides it. Without a plan the order is the request's
// plan_state, then the legacy spec_approval, then the legacy level, then the
// workspace default, which is needed when unset (req-task-plan-state AC-2.1,
// AC-2.2, AC-2.3; DEC-122(5), (6)).
func ResolveIntakePlanState(request PlanStateRequest, workspaceDefault core.PlanState) core.PlanState {
	if request.HasPlan {
		if request.PlanApproved {
			return core.PlanCompleted
		}
		return core.PlanAwaitingApproval
	}
	if request.PlanState != nil {
		return *request.PlanState
	}
	if request.SpecApproval != nil {
		return PlanStateFromSpecApproval(*request.SpecApproval)
	}
	if request.Level != "" {
		return PlanStateFromLevel(request.Level)
	}
	if workspaceDefault.Valid() {
		return workspaceDefault
	}
	return core.PlanNeeded
}

// PlanStateFromSpecApproval maps the legacy spec_approval alias: true is
// needed and false is not_required (component-plan-state invariant 1).
func PlanStateFromSpecApproval(specApproval bool) core.PlanState {
	if specApproval {
		return core.PlanNeeded
	}
	return core.PlanNotRequired
}

// PlanStateFromLevel maps a legacy level: L0 and L1 are not_required, L2 and
// L3 are needed (component-plan-state invariant 1).
func PlanStateFromLevel(level core.EscalationLevel) core.PlanState {
	if level == core.L2 || level == core.L3 {
		return core.PlanNeeded
	}
	return core.PlanNotRequired
}

// SpecApprovalBridge is the value written to the retained tasks.spec_approval
// column until triage and plan submission read plan_state: true for needed
// and awaiting_approval, so a completed or not_required task goes to
// implementation and an awaiting_approval task is planned again rather than
// skipping a person's approval. No new code reads the column
// (component-plan-state invariant 1).
func SpecApprovalBridge(state core.PlanState) bool {
	return state == core.PlanNeeded || state == core.PlanAwaitingApproval
}

// LegacyTaskPlanState derives the plan state of a task that a caller built
// without one from its legacy policy: spec_approval at policy version 1, else
// its level, mirroring the upgrade fill (component-plan-state invariant 8).
func LegacyTaskPlanState(task core.Task) core.PlanState {
	if task.PolicyVersion > 0 {
		return PlanStateFromSpecApproval(task.SpecApproval)
	}
	return PlanStateFromLevel(task.Level)
}

// PrepareTaskPlanState fixes a task's plan state before a creating
// transaction writes it. An intake plan sets awaiting_approval or completed,
// and a stated plan state writes SpecApprovalBridge to the retained
// spec_approval column. A task a caller built without a plan state takes the
// state its legacy policy routes by and keeps its spec_approval, so the
// column and the state agree. It refuses an invalid state and an intake plan
// with an invalid base commit (component-plan-state invariants 1 and 2).
func PrepareTaskPlanState(task *core.Task) error {
	if task.IntakePlan != nil {
		if strings.TrimSpace(task.IntakePlan.Content) == "" {
			return Refusef("intake plan is empty")
		}
		if task.IntakePlan.BaseCommit != "" && !core.ValidPlanBaseCommit(task.IntakePlan.BaseCommit) {
			return Refusef("plan_base_commit must be a full commit ID of 40 lowercase hex characters")
		}
		task.PlanState = core.PlanAwaitingApproval
		if task.IntakePlan.Approved {
			task.PlanState = core.PlanCompleted
		}
	}
	if task.PlanState == "" {
		task.PlanState = LegacyTaskPlanState(*task)
		return nil
	}
	if !task.PlanState.Valid() {
		return Refusef("invalid plan_state %q", task.PlanState)
	}
	task.SpecApproval = SpecApprovalBridge(task.PlanState)
	return nil
}

// PlanStateChangedEvent is the task.plan_state_changed event of one move
// (component-plan-state invariant 3). A nil from is the intake move.
func PlanStateChangedEvent(taskID string, from *core.PlanState, to core.PlanState, cause core.PlanStateCause, at time.Time) core.Event {
	return core.Event{TaskID: taskID, Kind: core.EventTaskPlanStateChanged, At: at,
		Payload: core.JSONPayload(core.PlanStateChange{From: from, To: to, Cause: cause})}
}

// IntakePlanStateEvent is the intake move every creating transaction records
// (component-task-lifecycle invariant 20).
func IntakePlanStateEvent(task core.Task) core.Event {
	return PlanStateChangedEvent(task.ID, nil, task.PlanState, core.PlanCauseIntake, task.CreatedAt)
}

// IntakePlanSpecVersion is plan version 1 of a task filed with a plan, with
// origin intake and the filed base commit (component-plan-state invariant 2).
func IntakePlanSpecVersion(task core.Task) core.SpecVersion {
	spec := core.SpecVersion{
		TaskID: task.ID, Version: core.IntakePlanVersion, Content: strings.TrimSpace(task.IntakePlan.Content),
		Acceptance: json.RawMessage(`[]`), Decomposition: json.RawMessage(`[]`),
		Origin: core.PlanOriginIntake, CreatedAt: task.CreatedAt,
	}
	if task.IntakePlan.BaseCommit != "" {
		commit := task.IntakePlan.BaseCommit
		spec.BaseCommit = &commit
	}
	if task.IntakePlan.Approved {
		spec.Approved = true
		spec.ApprovedAt = task.CreatedAt
	}
	return spec
}

// IntakePlanVersionCreatedEvent is the spec.version_created event of an
// intake plan.
func IntakePlanVersionCreatedEvent(spec core.SpecVersion) core.Event {
	return core.Event{TaskID: spec.TaskID, Kind: "spec.version_created", At: spec.CreatedAt,
		Payload: core.JSONPayload(map[string]any{"version": spec.Version, "acceptance_count": spec.AcceptanceCount, "origin": spec.Origin})}
}

// IntakePlanApproval is the approve intervention a filer's intake approval
// records under the credential's person (component-plan-state invariant 2;
// req-task-plan-state AC-2.3; req-review-gates-evidence AC-1.2).
func IntakePlanApproval(task core.Task, actor Actor) core.Intervention {
	return core.Intervention{
		TaskID: task.ID, ActorID: actor.ID, ActorRole: actor.Role, Action: core.InterventionApprove,
		ReasonCode: core.IntakePlanApprovalReasonCode, At: task.CreatedAt,
	}
}

// InterventionEvent is the intervention.<action> event an intervention
// appends on every backend.
func InterventionEvent(intervention core.Intervention) core.Event {
	return core.Event{
		TaskID: intervention.TaskID, JobID: intervention.JobID, Kind: "intervention." + string(intervention.Action),
		ActorID: intervention.ActorID, ActorRole: intervention.ActorRole, At: intervention.At,
		Payload: core.JSONPayload(map[string]any{"reason_code": intervention.ReasonCode, "comment": intervention.Comment}),
	}
}

// IntakePlanApprovedEvent is the spec.version_approved event of an intake
// approval.
func IntakePlanApprovedEvent(spec core.SpecVersion) core.Event {
	return core.Event{TaskID: spec.TaskID, Kind: "spec.version_approved", At: spec.CreatedAt,
		Payload: core.JSONPayload(map[string]int{"version": spec.Version})}
}

// IntakePlanApprover returns the person an intake approval records: the
// creating credential's user actor. An approval without one is refused, so a
// plan reaches completed at intake only under a person's credential
// (req-task-plan-state AC-2.3; req-review-gates-evidence REQ-1).
func IntakePlanApprover(actor Actor) (Actor, error) {
	if actor.Role != core.ActorUser || strings.TrimSpace(strings.TrimPrefix(actor.ID, "user:")) == "" {
		return Actor{}, Refusef("an intake plan approval requires a person's credential")
	}
	return actor, nil
}

// PrepareSpecVersionOrigin defaults a recorded plan version's origin to
// plan_order and refuses an unknown origin or a base commit that is not a
// full commit ID (component-plan-state, Wire fields; req-task-plan-state
// AC-2.5).
func PrepareSpecVersionOrigin(spec *core.SpecVersion) error {
	if spec.Origin == "" {
		spec.Origin = core.PlanOriginPlanOrder
	}
	if !spec.Origin.Valid() {
		return Refusef("invalid plan origin %q", spec.Origin)
	}
	if spec.BaseCommit != nil && !core.ValidPlanBaseCommit(*spec.BaseCommit) {
		return Refusef("plan base_commit must be a full commit ID of 40 lowercase hex characters")
	}
	return nil
}
