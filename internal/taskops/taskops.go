// Package taskops is Conveyor's serialized lifecycle command plane. Callers
// issue a closed canonical command; only the plane can mint the capability
// required by lifecycle store mutators (spec §§3.3-3.4, §21.38).
package taskops

import (
	"context"
	"fmt"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// TaskLease proves that a lifecycle write was admitted through Plane.Perform.
// Its fields and constructor are intentionally private so code outside this
// package cannot forge permission to mutate a task projection (spec §21.38).
type TaskLease struct {
	taskID  string
	command string
	seal    *leaseSeal
}

type leaseSeal struct{}

// ValidFor lets store implementations reject a zero, copied-for-another-task,
// or otherwise invalid capability without exposing a constructor.
func (l TaskLease) ValidFor(taskID string) bool {
	return l.seal != nil && l.taskID == taskID
}

// ValidForCommand additionally binds a capability to one canonical lifecycle
// command, preventing a callback admitted for one operation from invoking a
// different guarded writer.
func (l TaskLease) ValidForCommand(taskID, command string) bool {
	return l.ValidFor(taskID) && l.command == command
}

// ExecuteWorkOrder is the typed admission boundary for store-specific
// work-order payloads. The command-bound lease exists only for the duration of
// apply and cannot be forged by callers outside this package.
func ExecuteWorkOrder[T any](ctx context.Context, backend Backend, taskID string, command core.WorkOrderCommand, apply func(TaskLease) (T, error)) (T, error) {
	var zero T
	if backend == nil {
		return zero, fmt.Errorf("taskops plane requires a backend")
	}
	if taskID == "" || command == "" {
		return zero, fmt.Errorf("taskops work-order task id and command are required")
	}
	if apply == nil {
		return zero, fmt.Errorf("taskops work-order command handler is required")
	}
	return apply(TaskLease{taskID: taskID, command: string(command), seal: &leaseSeal{}})
}

// Command is the typed task-lifecycle command envelope. Kind is restricted by
// the canonical machine; stage fields are projections carried by the commands
// that advance, bounce, redirect, or recover the pipeline.
type Command struct {
	Kind          core.TaskCommand
	NextStage     core.Stage
	RecoveryStage core.Stage
	ProjectStages bool
}

// WorkOrderMetadataCommand admits updates to progress/cost fields that do not
// move the canonical lifecycle state.
const WorkOrderMetadataCommand core.WorkOrderCommand = "order.metadata"

// Outcome is the durable projection after a command commits.
type Outcome struct {
	Task     core.Task
	Command  core.TaskCommand
	Enqueued bool
}

// Backend is the narrow capability-protected mutation surface implemented by
// both durable Postgres and the in-memory test double.
type Backend interface {
	ApplyTaskCommand(context.Context, TaskLease, string, Command) (core.Task, error)
	CancelTaskCommand(context.Context, TaskLease, core.Intervention) (core.Task, error)
	AcceptReviewDecisionCommand(context.Context, TaskLease, core.ReviewDecision) error
	ClaimWorkOrderCommand(context.Context, TaskLease, string, core.WorkOrderClaim) (core.WorkOrder, error)
	ListElapsedWorkOrderTaskIDs(context.Context, time.Time) ([]string, error)
	ApplyWorkOrderClock(context.Context, TaskLease, string, time.Time) (int, error)
}

func (p *Plane) ClaimWorkOrder(ctx context.Context, taskID, orderID string, claim core.WorkOrderClaim) (core.WorkOrder, error) {
	if p == nil || p.backend == nil {
		return core.WorkOrder{}, fmt.Errorf("taskops plane requires a backend")
	}
	if taskID == "" || orderID == "" {
		return core.WorkOrder{}, fmt.Errorf("task and work-order ids are required")
	}
	return p.backend.ClaimWorkOrderCommand(ctx, TaskLease{taskID: taskID, command: string(core.WorkOrderCmdClaim), seal: &leaseSeal{}}, orderID, claim)
}

type durableBackend interface{ IsDurable() bool }

func (p *Plane) Cancel(ctx context.Context, intervention core.Intervention) (core.Task, error) {
	if p == nil || p.backend == nil {
		return core.Task{}, fmt.Errorf("taskops plane requires a backend")
	}
	if intervention.TaskID == "" {
		return core.Task{}, fmt.Errorf("taskops task id is required")
	}
	return p.backend.CancelTaskCommand(ctx, TaskLease{taskID: intervention.TaskID, seal: &leaseSeal{}}, intervention)
}

func (p *Plane) AcceptReviewDecision(ctx context.Context, decision core.ReviewDecision) error {
	if p == nil || p.backend == nil {
		return fmt.Errorf("taskops plane requires a backend")
	}
	if decision.TaskID == "" {
		return fmt.Errorf("taskops task id is required")
	}
	return p.backend.AcceptReviewDecisionCommand(ctx, TaskLease{taskID: decision.TaskID, seal: &leaseSeal{}}, decision)
}

// TickOrderClock sends deadline commands for every task with elapsed work-order
// clocks. Candidate discovery is a pure read; each task's transitions execute
// under the same unforgeable, workspace-qualified command-plane lease.
func (p *Plane) TickOrderClock(ctx context.Context, now time.Time) (int, error) {
	if p == nil || p.backend == nil {
		return 0, fmt.Errorf("taskops plane requires a backend")
	}
	taskIDs, err := p.backend.ListElapsedWorkOrderTaskIDs(ctx, now)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, taskID := range taskIDs {
		count, applyErr := p.backend.ApplyWorkOrderClock(ctx, TaskLease{taskID: taskID, seal: &leaseSeal{}}, taskID, now)
		if applyErr != nil {
			return total, applyErr
		}
		total += count
	}
	return total, nil
}

type Plane struct{ backend Backend }

func New(backend Backend) *Plane { return &Plane{backend: backend} }

// Perform executes one canonical command against one task. Serialization,
// machine validation, projection/event atomicity, and durable enqueue are
// completed by the backend in the same protected write span.
func (p *Plane) Perform(ctx context.Context, taskID string, cmd Command) (Outcome, error) {
	if p == nil || p.backend == nil {
		return Outcome{}, fmt.Errorf("taskops plane requires a backend")
	}
	if taskID == "" {
		return Outcome{}, fmt.Errorf("taskops task id is required")
	}
	if cmd.Kind == "" {
		return Outcome{}, fmt.Errorf("taskops command is required")
	}
	task, err := p.backend.ApplyTaskCommand(ctx, TaskLease{taskID: taskID, seal: &leaseSeal{}}, taskID, cmd)
	if err != nil {
		return Outcome{}, err
	}
	durable, _ := p.backend.(durableBackend)
	return Outcome{Task: task, Command: cmd.Kind, Enqueued: task.State == core.TaskQueued && durable != nil && durable.IsDurable()}, nil
}
