package store

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// Stored deadlines and the deadline notices that announce them
// (component-persistence, Deadlines; DEC-99(4); req-quiet-idle-runtime
// AC-2.4). The deadline alarm of component-durable-queue sleeps until the
// earliest stored deadline and wakes early on a deadline notice.

// DeadlineReader reads the earliest stored deadline of the context's
// workspace.
type DeadlineReader interface {
	// NextDeadline returns the earliest deadline strictly after after, and
	// false when there is none. It writes nothing.
	NextDeadline(ctx context.Context, after time.Time) (time.Time, bool, error)
}

// WorkOrderDeadlines are the deadlines a work order's state records: the
// lease of a claimed order, the queue deadline of a queued order not yet
// started and not blocked, the execution deadline of a queued or claimed
// order, and the automatic-recovery time of any live order. NextDeadline
// reads the same set in every backend.
func WorkOrderDeadlines(order core.WorkOrder) []time.Time {
	var deadlines []time.Time
	consider := func(at time.Time) {
		if !at.IsZero() {
			deadlines = append(deadlines, at)
		}
	}
	switch order.State {
	case core.WorkOrderClaimed:
		consider(order.LeaseExpiresAt)
		consider(order.ExecutionDeadline)
		consider(order.NextAutomaticRecoveryAt)
	case core.WorkOrderQueued:
		if order.ExecutionStartedAt.IsZero() && order.QueueBlockedAt.IsZero() {
			consider(order.QueueDeadline)
		}
		consider(order.ExecutionDeadline)
		consider(order.NextAutomaticRecoveryAt)
	case core.WorkOrderStale, core.WorkOrderTimedOut:
		consider(order.NextAutomaticRecoveryAt)
	}
	return deadlines
}

// WorkOrderDeadline is the earliest of WorkOrderDeadlines.
func WorkOrderDeadline(order core.WorkOrder) (time.Time, bool) {
	var earliest time.Time
	for _, at := range WorkOrderDeadlines(order) {
		if earliest.IsZero() || at.Before(earliest) {
			earliest = at
		}
	}
	return earliest, !earliest.IsZero()
}

// PublicationReenqueueDeadline is a failed review or GitHub issue
// publication's next automatic re-enqueue: one recovery window after the
// last one (feature-bounded-automatic-recovery AR-5). A row never
// re-enqueued is due at once and has no stored time.
func PublicationReenqueueDeadline(failed bool, reenqueuedAt time.Time, bound int, window time.Duration) (time.Time, bool) {
	if !failed || reenqueuedAt.IsZero() || bound == 0 || window <= 0 {
		return time.Time{}, false
	}
	return reenqueuedAt.Add(window), true
}

// deadlineFields reads the deadline fields of an event payload that carries
// them without a work-order state, such as work_order.lease_renewed.
type deadlineFields struct {
	LeaseExpiresAt          *time.Time `json:"lease_expires_at"`
	QueueDeadline           *time.Time `json:"queue_deadline"`
	ExecutionDeadline       *time.Time `json:"execution_deadline"`
	NextAutomaticRecoveryAt *time.Time `json:"next_automatic_recovery_at"`
}

// EventDeadlineNotice maps an inserted event to the deadline notice it
// raises. Every write that records a work-order deadline appends a
// work_order.* event in its transaction, so the event insert is the one
// place each backend raises deadline notices:
//
//   - a work_order.* event whose payload is a work order raises the order's
//     earliest deadline, or the event time when the order entered a
//     recoverable stop with no automatic-recovery time, because the next
//     order clock run schedules its recovery;
//   - a work_order.* event that carries deadline fields without a state
//     raises the earliest of them;
//   - a review or GitHub issue publication failure, a workspace
//     configuration update, and a requirement or System Design confirmation
//     raise the event time, so the alarm fires at once;
//   - a merge.check_pending event raises the deadline its payload records
//     (req-quiet-idle-runtime AC-2.4), a failed read's included.
//
// The time is in-process only; a peer receives the notice without it.
func EventDeadlineNotice(workspace, kind string, payload json.RawMessage, at time.Time) (changes.Notice, bool) {
	notice := func(when time.Time) (changes.Notice, bool) {
		if when.IsZero() {
			when = time.Now().UTC()
		}
		return changes.Notice{Workspace: workspace, Kind: changes.Deadline, At: when}, true
	}
	switch kind {
	case "review.publication_failed", "github_issue.publication_failed",
		"config.updated", "requirement.version_confirmed", "system_design.version_confirmed":
		return notice(at)
	case "verification." + VerificationStageChunk:
		// A staged chunk's upload expires VerificationUploadLifetime after its
		// first chunk; the first chunk's event raises that time, and
		// NextDeadline returns each staging chunk's exact expiry.
		if at.IsZero() {
			at = time.Now().UTC()
		}
		return notice(at.Add(VerificationUploadLifetime))
	case EventCheckPending:
		var recorded struct {
			Deadline time.Time `json:"deadline"`
		}
		if json.Unmarshal(payload, &recorded) != nil || recorded.Deadline.IsZero() {
			return changes.Notice{}, false
		}
		return notice(recorded.Deadline)
	}
	if strings.HasPrefix(kind, "work_order.") && verificationClaimMayHaveEnded(kind, payload) {
		// A verify order that lost its claim may leave a running or pending
		// verification attempt; the next fire reconciles the claim loss
		// (component-durable-queue, The deadline alarm).
		return notice(at)
	}
	if !strings.HasPrefix(kind, "work_order.") || len(payload) == 0 {
		return changes.Notice{}, false
	}
	var order core.WorkOrder
	if err := json.Unmarshal(payload, &order); err == nil && order.State != "" {
		if order.NextAutomaticRecoveryAt.IsZero() && RecoverableStop(order, at) && order.State != core.WorkOrderClaimed {
			return notice(at)
		}
		if deadline, ok := WorkOrderDeadline(order); ok {
			return notice(deadline)
		}
		return changes.Notice{}, false
	}
	var fields deadlineFields
	if err := json.Unmarshal(payload, &fields); err != nil {
		return changes.Notice{}, false
	}
	var earliest time.Time
	for _, at := range []*time.Time{fields.LeaseExpiresAt, fields.QueueDeadline, fields.ExecutionDeadline, fields.NextAutomaticRecoveryAt} {
		if at != nil && !at.IsZero() && (earliest.IsZero() || at.Before(earliest)) {
			earliest = *at
		}
	}
	if earliest.IsZero() {
		return changes.Notice{}, false
	}
	return notice(earliest)
}

// claimEndingWorkOrderKinds are the work-order events that can end a claim:
// release, expiry, cancellation, preemption, staleness, timeout,
// retirement, stall, child failure, and redispatch.
var claimEndingWorkOrderKinds = map[string]bool{
	"work_order.released": true, "work_order.expired": true, "work_order.cancelled": true,
	"work_order.preempted": true, "work_order.stale": true, "work_order.timed_out": true,
	"work_order.retired": true, "work_order.stalled": true, "work_order.child_failed": true,
	"work_order.redispatched": true,
}

// verificationClaimMayHaveEnded reports whether a work-order event may leave
// a verification attempt running or pending without its claim: its payload
// is a verify order that has had an attempt and is no longer claimed,
// whatever the event kind, or it is a claim-ending event whose payload names
// no stage, as the verification supersession cancel does.
func verificationClaimMayHaveEnded(kind string, payload json.RawMessage) bool {
	var order struct {
		Stage         core.Stage          `json:"stage"`
		State         core.WorkOrderState `json:"state"`
		LastAttemptID string              `json:"last_attempt_id"`
	}
	if len(payload) != 0 && json.Unmarshal(payload, &order) != nil {
		return claimEndingWorkOrderKinds[kind]
	}
	if order.Stage == "" {
		return claimEndingWorkOrderKinds[kind]
	}
	return order.Stage == core.StageVerify && order.State != "" && order.State != core.WorkOrderClaimed && order.LastAttemptID != ""
}

// DependencyResumeDeadlineNotice is raised when a dependency unblock
// restarts paused queue clocks, which appends no event
// (component-task-lifecycle, Dependency commands).
func DependencyResumeDeadlineNotice(workspace string, at time.Time) changes.Notice {
	return changes.Notice{Workspace: workspace, Kind: changes.Deadline, At: at}
}

// raiseEventDeadlineLocked raises the deadline notice of an event the
// volatile backend appended. The caller holds the mutex.
func (m *memory) raiseEventDeadlineLocked(workspace string, event core.Event) {
	if n, ok := EventDeadlineNotice(workspace, event.Kind, event.Payload, event.At); ok {
		m.raiseLocked(n)
	}
}

// NextDeadline is the volatile backend's DeadlineReader.
func (m *memory) NextDeadline(ctx context.Context, after time.Time) (time.Time, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	selected, _ := WorkspaceFromContext(ctx)
	var earliest time.Time
	consider := func(at time.Time, ok bool) {
		if ok && at.After(after) && (earliest.IsZero() || at.Before(earliest)) {
			earliest = at
		}
	}
	inWorkspace := func(taskID string) (core.Task, bool) {
		task, ok := m.tasks[taskID]
		return task, ok && (selected == "" || task.Workspace == selected)
	}
	for taskID := range m.tasks {
		// A deadline on a task at another state or behind the merge gate is one
		// a fire couldn't apply (component-durable-queue invariants 30, 34).
		if task, ok := inWorkspace(taskID); ok && task.State == core.TaskApproved && !task.MergeApproval && task.CheckStatusDeadline != nil {
			consider(*task.CheckStatusDeadline, true)
		}
	}
	for _, order := range m.workOrders {
		if _, ok := inWorkspace(order.TaskID); ok {
			for _, at := range WorkOrderDeadlines(order) {
				consider(at, true)
			}
		}
	}
	for taskID, lifecycle := range m.github {
		if task, ok := inWorkspace(taskID); ok {
			policy, _ := m.recoveryPolicyLocked(task.Workspace)
			consider(PublicationReenqueueDeadline(lifecycle.State == core.GitHubPublicationFailed, lifecycle.AutomaticReenqueueAt, policy.AutomaticRecoveries, policy.Window))
		}
	}
	for _, publication := range m.publications {
		if task, ok := inWorkspace(publication.TaskID); ok {
			policy, _ := m.recoveryPolicyLocked(task.Workspace)
			consider(PublicationReenqueueDeadline(publication.State == core.ReviewPublicationFailed, publication.AutomaticReenqueueAt, policy.AutomaticRecoveries, policy.Window))
		}
	}
	for _, delivery := range m.verificationDeliveries {
		if (selected != "" && delivery.WorkspaceID != selected) || delivery.State != "failed" || delivery.NextAttemptAt == nil {
			continue
		}
		consider(*delivery.NextAttemptAt, true)
	}
	for _, row := range m.verificationRows {
		if row.Table != "verification_upload_chunks" || row.State != "staging" || row.ExpiresAt == nil {
			continue
		}
		if _, ok := inWorkspace(row.TaskID); ok {
			consider(*row.ExpiresAt, true)
		}
	}
	return earliest, !earliest.IsZero(), nil
}
