package store

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// LifecycleAuditViolation is an observed event edge absent from the canonical
// §21.37 transition tables. Auditing is read-only: callers decide whether a
// discrepancy is historical corruption or requires a spec amendment.
type LifecycleAuditViolation struct {
	Space   core.LifecycleSpace `json:"space"`
	Entity  string              `json:"entity"`
	EventID int64               `json:"event_id"`
	From    string              `json:"from"`
	Command string              `json:"command"`
	To      string              `json:"to"`
	Reason  string              `json:"reason"`
}

// AuditLifecycleHistory folds task and work-order lifecycle events without
// mutating projections or history (spec §21.37). Pre-command task events are
// accepted only when their recorded edge exists in the canonical table.
func AuditLifecycleHistory(events []core.Event) []LifecycleAuditViolation {
	ordered := append([]core.Event(nil), events...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].At.Equal(ordered[j].At) {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].At.Before(ordered[j].At)
	})
	taskStates := map[string]core.TaskState{}
	workOrderStates := map[string]core.WorkOrderState{}
	var violations []LifecycleAuditViolation
	for _, event := range ordered {
		if event.Kind == "task.created" {
			var task core.Task
			if json.Unmarshal(event.Payload, &task) == nil && task.State != "" {
				taskStates[event.TaskID] = task.State
			}
			continue
		}
		if event.Kind == "task.state_changed" {
			var payload struct {
				From    core.TaskState   `json:"from"`
				To      core.TaskState   `json:"to"`
				Command core.TaskCommand `json:"command"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				violations = append(violations, auditViolation(core.TaskLifecycle, event.TaskID, event, "", "", "", "invalid state-change payload: "+err.Error()))
				continue
			}
			if current, ok := taskStates[event.TaskID]; ok && current != payload.From {
				violations = append(violations, auditViolation(core.TaskLifecycle, event.TaskID, event, string(current), string(payload.Command), string(payload.To), fmt.Sprintf("event records source %q, prior history resolves to %q", payload.From, current)))
			}
			if payload.Command != "" {
				to, err := core.TransitionTask(payload.From, payload.Command)
				if err != nil || to != payload.To {
					violations = append(violations, auditViolation(core.TaskLifecycle, event.TaskID, event, string(payload.From), string(payload.Command), string(payload.To), transitionReason(err, string(to), string(payload.To))))
				}
			} else if !taskEdgeExists(payload.From, payload.To) {
				violations = append(violations, auditViolation(core.TaskLifecycle, event.TaskID, event, string(payload.From), "<historical>", string(payload.To), "edge is absent from the canonical table"))
			}
			if payload.To != "" {
				taskStates[event.TaskID] = payload.To
			}
			continue
		}
		from := workOrderStates[event.JobID]
		command, relevant := workOrderEventCommand(event, from)
		if !relevant || event.JobID == "" {
			continue
		}
		to := workOrderStateFromEvent(event)
		if command != "" {
			expected, err := core.TransitionWorkOrder(from, command)
			if err != nil || (to != "" && expected != to) {
				violations = append(violations, auditViolation(core.WorkOrderLifecycle, event.JobID, event, string(from), string(command), string(to), transitionReason(err, string(expected), string(to))))
			} else {
				to = expected
			}
		} else if to != "" && to != from && !workOrderEdgeExists(from, to) {
			violations = append(violations, auditViolation(core.WorkOrderLifecycle, event.JobID, event, string(from), "<historical>", string(to), "edge is absent from the canonical table"))
		}
		if to != "" {
			workOrderStates[event.JobID] = to
		}
	}
	return violations
}

func taskEdgeExists(from, to core.TaskState) bool {
	for _, edge := range core.TaskTransitionAlternatives(from) {
		if edge.To == string(to) {
			return true
		}
	}
	return false
}
func workOrderEdgeExists(from, to core.WorkOrderState) bool {
	for _, edge := range core.WorkOrderTransitionAlternatives(from) {
		if edge.To == string(to) {
			return true
		}
	}
	return false
}

func workOrderEventCommand(event core.Event, from core.WorkOrderState) (core.WorkOrderCommand, bool) {
	switch event.Kind {
	case "work_order.created":
		return core.WorkOrderCmdCreate, true
	case "work_order.claimed":
		return core.WorkOrderCmdClaim, true
	case "work_order.lease_renewed":
		return core.WorkOrderCmdRenew, true
	case "work_order.released", "work_order.child_failed", "work_order.stalled":
		return core.WorkOrderCmdRelease, true
	case "work_order.preempted":
		return core.WorkOrderCmdPreempt, true
	case "work_order.expired":
		return core.WorkOrderCmdExpire, true
	case "work_order.timed_out":
		return core.WorkOrderCmdTimeout, true
	case "work_order.stale":
		return core.WorkOrderCmdMarkStale, true
	case "work_order.redispatched":
		// W14 is stale -> queued for never-claimed queue timeouts. Historical
		// queued recovery events were state-preserving metadata resets, so they
		// carry no lifecycle command (spec §3.3, §21.41).
		if from == core.WorkOrderQueued {
			return "", true
		}
		return core.WorkOrderCmdRedispatch, true
	case "work_order.recovered":
		// Historical queued recovery was a state-preserving metadata reset;
		// stale and timed-out recovery is W13.
		if from == core.WorkOrderQueued {
			return "", true
		}
		if from == core.WorkOrderClaimed || from == core.WorkOrderSubmitted {
			return core.WorkOrderCmdRedispatch, true
		}
		return core.WorkOrderCmdRecover, true
	case "work_order.cancelled":
		return core.WorkOrderCmdCancel, true
	case "work_order.updated":
		return "", true
	default:
		return "", false
	}
}

func workOrderStateFromEvent(event core.Event) core.WorkOrderState {
	var payload struct {
		State    core.WorkOrderState `json:"state"`
		NewState core.WorkOrderState `json:"new_state"`
	}
	if json.Unmarshal(event.Payload, &payload) != nil {
		return ""
	}
	if payload.NewState != "" {
		return payload.NewState
	}
	if payload.State != "" {
		return payload.State
	}
	return ""
}

func auditViolation(space core.LifecycleSpace, entity string, event core.Event, from, command, to, reason string) LifecycleAuditViolation {
	return LifecycleAuditViolation{Space: space, Entity: entity, EventID: event.ID, From: from, Command: command, To: to, Reason: reason}
}
func transitionReason(err error, got, want string) string {
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("command resolves to %q, event records %q", got, want)
}
