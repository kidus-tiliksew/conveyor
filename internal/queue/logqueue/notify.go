package logqueue

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
)

// NotifyingLog is an eventlog.Store that raises a job change notice after
// each committed append that makes a job claimable or reschedules it
// (DEC-99(1); component-durable-queue, Job change notices).
type NotifyingLog struct {
	eventlog.Store
	hub *changes.Hub
}

// Notify wraps log so job appends raise notices on hub. Inside a store
// transaction, whose context carries a changes.Batch, the notice waits for
// the commit; otherwise the driver has committed and the notice is
// published at once.
func Notify(log eventlog.Store, hub *changes.Hub) *NotifyingLog {
	return &NotifyingLog{Store: log, hub: hub}
}

// Changes is the hub the log raises notices on.
func (l *NotifyingLog) Changes() *changes.Hub { return l.hub }

// Append appends and, on success, raises the job notice when it applies.
func (l *NotifyingLog) Append(ctx context.Context, workspace string, stream eventlog.StreamID, expected eventlog.Version, events []eventlog.NewEvent) (eventlog.Version, error) {
	head, err := l.Store.Append(ctx, workspace, stream, expected, events)
	if err != nil {
		return head, err
	}
	if RaisesJobNotice(stream, events) {
		changes.Raise(ctx, l.hub, changes.Notice{Workspace: workspace, Kind: changes.Job})
	}
	return head, nil
}

// RaisesJobNotice reports whether an append to stream wakes the runtime:
// a job stream event that enqueues, fails, snoozes, or rescues a job.
func RaisesJobNotice(stream eventlog.StreamID, events []eventlog.NewEvent) bool {
	if stream.Type() != StreamType {
		return false
	}
	for _, event := range events {
		switch event.Kind {
		case KindEnqueued, KindFailed, KindSnoozed, KindRescued:
			return true
		}
	}
	return false
}
