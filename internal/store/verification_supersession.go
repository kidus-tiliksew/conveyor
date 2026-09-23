package store

import (
	"context"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"time"
)

func (m *memory) supersedeVerificationLocked(ctx context.Context, taskID, head string) {
	now := time.Now().UTC()
	for id, old := range m.workOrders {
		if old.TaskID != taskID {
			continue
		}
		order, changed := core.SupersedeVerificationOrder(old, head, now)
		if !changed {
			continue
		}
		m.workOrders[id] = order
		if job, index, ok := m.findJobLocked(old.JobID); ok {
			job.State, job.EndedAt = core.JobFailed, now
			m.jobs[taskID][index] = job
		}
		m.appendEventLocked(ctx, core.Event{TaskID: taskID, JobID: old.JobID, Kind: "work_order.cancelled", Payload: core.JSONPayload(map[string]any{"work_order_id": id, "session_id": old.SessionID, "attempt_id": old.AttemptID, "reason": "superseded verification head", "head_sha": head}), At: now})
	}
}
