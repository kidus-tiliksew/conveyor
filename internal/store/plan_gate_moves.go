package store

import (
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// TaskCommandPlanStateMove returns the task.plan_state_changed event a
// lifecycle command's plan-state move appends, or nil when the command moves
// no plan state or the task already holds the target state. Every backend
// commits the event and the column in the command's own transaction
// (component-plan-state invariant 3; req-task-plan-state REQ-3).
func TaskCommandPlanStateMove(taskID string, current core.PlanState, move *taskops.PlanStateMove) (*core.Event, error) {
	if move == nil {
		return nil, nil
	}
	if !move.To.Valid() {
		return nil, Refusef("invalid plan_state %q", move.To)
	}
	return PlanStateMoveEvent(taskID, current, move.To, move.Cause), nil
}

// PlanStateMoveEvent is the task.plan_state_changed event of one move from
// current to the target state, or nil when the task already holds it
// (component-plan-state invariant 3).
func PlanStateMoveEvent(taskID string, current, to core.PlanState, cause core.PlanStateCause) *core.Event {
	if current == to {
		return nil
	}
	var from *core.PlanState
	if current != "" {
		value := current
		from = &value
	}
	event := PlanStateChangedEvent(taskID, from, to, cause, time.Time{})
	return &event
}
