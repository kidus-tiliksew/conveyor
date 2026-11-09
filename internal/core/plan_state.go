package core

import "regexp"

// PlanState is a task's plan state (component-plan-state invariant 1;
// req-task-plan-state REQ-1; DEC-122(5)).
type PlanState string

const (
	PlanNotRequired      PlanState = "not_required"
	PlanNeeded           PlanState = "needed"
	PlanAwaitingApproval PlanState = "awaiting_approval"
	PlanCompleted        PlanState = "completed"
)

// PlanStates lists the four plan states in their documented order.
func PlanStates() []PlanState {
	return []PlanState{PlanNotRequired, PlanNeeded, PlanAwaitingApproval, PlanCompleted}
}

// Valid reports whether s is one of the four plan states.
func (s PlanState) Valid() bool {
	switch s {
	case PlanNotRequired, PlanNeeded, PlanAwaitingApproval, PlanCompleted:
		return true
	}
	return false
}

// PlanOrigin names the act that recorded a plan version
// (component-plan-state, Wire fields).
type PlanOrigin string

const (
	PlanOriginPlanOrder PlanOrigin = "plan_order"
	PlanOriginIntake    PlanOrigin = "intake"
	PlanOriginOperator  PlanOrigin = "operator"
)

// Valid reports whether o is one of the three plan origins.
func (o PlanOrigin) Valid() bool {
	switch o {
	case PlanOriginPlanOrder, PlanOriginIntake, PlanOriginOperator:
		return true
	}
	return false
}

// EventTaskPlanStateChanged records one plan-state move with its from, to,
// and cause (component-plan-state invariant 3).
const EventTaskPlanStateChanged = "task.plan_state_changed"

// PlanStateCause names the act that moved a plan state
// (component-plan-state invariant 3).
type PlanStateCause string

const (
	PlanCauseIntake                    PlanStateCause = "intake"
	PlanCausePlanSubmitted             PlanStateCause = "plan_submitted"
	PlanCausePlanApproved              PlanStateCause = "plan_approved"
	PlanCausePlanRedirected            PlanStateCause = "plan_redirected"
	PlanCausePlanRevisionApproved      PlanStateCause = "plan_revision_approved"
	PlanCausePlanningRequestedApproved PlanStateCause = "planning_requested_approved"
	PlanCauseOperator                  PlanStateCause = "operator"
	PlanCausePlanAttached              PlanStateCause = "plan_attached"
)

// PlanStateCauses lists the eight causes of component-plan-state invariant 3.
func PlanStateCauses() []PlanStateCause {
	return []PlanStateCause{
		PlanCauseIntake, PlanCausePlanSubmitted, PlanCausePlanApproved, PlanCausePlanRedirected,
		PlanCausePlanRevisionApproved, PlanCausePlanningRequestedApproved, PlanCauseOperator, PlanCausePlanAttached,
	}
}

// PlanStateChange is the task.plan_state_changed payload. From is nil for the
// intake move (component-plan-state invariant 3).
type PlanStateChange struct {
	From  *PlanState     `json:"from"`
	To    PlanState      `json:"to"`
	Cause PlanStateCause `json:"cause"`
}

// IntakePlan is an execution plan filed with a task. The creating transaction
// records it as plan version 1 with origin intake and, when Approved, records
// the filer's approval (component-plan-state invariant 2; req-task-plan-state
// AC-2.2, AC-2.3, AC-2.5).
type IntakePlan struct {
	Content    string
	BaseCommit string
	Approved   bool
}

// IntakePlanVersion is the plan version an intake plan becomes.
const IntakePlanVersion = 1

// IntakePlanApprovalReasonCode is the reason code of the approve intervention
// recorded for a plan its filer approved at intake.
const IntakePlanApprovalReasonCode = "intake-plan-approved"

var fullCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ValidPlanBaseCommit reports whether commit is a full commit ID: 40
// lowercase hex characters (component-plan-state invariant 2;
// req-task-plan-state AC-2.5).
func ValidPlanBaseCommit(commit string) bool {
	return fullCommitPattern.MatchString(commit)
}
