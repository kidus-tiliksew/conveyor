package store

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/memlog"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
)

// The volatile backend's peer check read (component-persistence, Change
// notices; feature-quiet-idle-runtime, Peers). The volatile backend notifies
// no other daemon, so a peered daemon reads every workspace's marks here once
// per peer check interval.

var _ changes.PeerReader = (*volatileMemory)(nil)

// PeerMarks returns each workspace's log head position and the count and
// highest ID of its task events.
func (m *volatileMemory) PeerMarks(ctx context.Context) (map[string]changes.PeerMark, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	marks := map[string]changes.PeerMark{}
	if notifying, ok := m.log.(*logqueue.NotifyingLog); ok {
		if log, ok := notifying.Store.(*memlog.Store); ok {
			for workspace, position := range log.Positions() {
				mark := marks[workspace]
				mark.Log = int64(position)
				marks[workspace] = mark
			}
		}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for taskID, events := range m.events {
		task, ok := m.tasks[taskID]
		if taskID == "" || !ok || task.Workspace == "" || len(events) == 0 {
			continue
		}
		mark := marks[task.Workspace]
		for _, event := range events {
			mark.Events++
			if event.ID > mark.LastEvent {
				mark.LastEvent = event.ID
			}
		}
		marks[task.Workspace] = mark
	}
	return marks, nil
}
