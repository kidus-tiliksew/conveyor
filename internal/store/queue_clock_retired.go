package store

import (
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// The queue clock is retired. An order nobody has claimed since it entered the
// queue has no deadline: it stays queued and claimable in its queue position,
// no clock marks it stale, and automatic recovery never applies to it. The
// stored queue_deadline column stays as inert compatibility data, and nothing
// reads it to change an order's state, refuse a claim, select clock work,
// schedule recovery, or choose a next action (req-task-lifecycle-and-queue
// REQ-9, AC-9.4, AC-9.5; DEC-117(2); component-work-orders invariant 5).

// LegacyStaleNormalizationReason is the reason the order clock records on the
// one work_order.redispatched event that returns a stored stale order to
// queued.
const LegacyStaleNormalizationReason = "retired queue clock: stale order returned to the queue"

// NormalizeLegacyStaleWorkOrder is core.NormalizeLegacyStaleWorkOrder, the
// one rule every backend's clock, claim, and projection apply.
func NormalizeLegacyStaleWorkOrder(order core.WorkOrder, now time.Time) core.WorkOrder {
	return core.NormalizeLegacyStaleWorkOrder(order, now)
}

// LegacyStaleNormalizedEvent is the one audit event of a stale order's return
// to queued. It is a system act of the order clock and counts no recovery, so
// it carries no automatic recovery number or window.
func LegacyStaleNormalizedEvent(order core.WorkOrder, now time.Time) core.Event {
	return core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.redispatched", At: now, Payload: core.JSONPayload(map[string]any{
		"work_order_id": order.ID, "prior_state": core.WorkOrderStale, "new_state": core.WorkOrderQueued,
		"command": core.WorkOrderCmdRedispatch, "reason": LegacyStaleNormalizationReason,
	})}
}
