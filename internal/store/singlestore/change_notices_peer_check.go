package singlestore

import (
	"context"
	"database/sql"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

// The peer check read on SingleStore (component-persistence, Change notices;
// feature-quiet-idle-runtime, Peers; req-quiet-idle-runtime AC-1.3, AC-2.5).
// SingleStore offers no change notification, so a peered daemon reads every
// workspace's marks in this one statement once per peer check interval.
//
// Both positions advance under a workspace row lock in commit order.
const peerMarksSQL = `SELECT workspace_id, 0 AS source, position AS n, 0 AS last_id FROM event_log_positions
UNION ALL
SELECT workspace_id, 1 AS source, position AS n, 0 AS last_id FROM task_event_positions`

var _ changes.PeerReader = (*Store)(nil)

// PeerMarks returns each workspace's log and task-event positions.
func (s *Store) PeerMarks(ctx context.Context) (map[string]changes.PeerMark, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "PeerMarks")
	rows, err := backendRows(ctx, s.db, peerMarksSQL)
	if err != nil {
		return nil, translateBackendConflict(err)
	}
	defer rows.Close()
	marks := map[string]changes.PeerMark{}
	for rows.Next() {
		var workspace string
		var source int
		var n int64
		var lastID sql.NullInt64
		if err = rows.Scan(&workspace, &source, &n, &lastID); err != nil {
			return nil, translateBackendConflict(err)
		}
		mark := marks[workspace]
		if source == 0 {
			mark.Log = n
		} else {
			mark.Events = n
			mark.LastEvent = lastID.Int64
		}
		marks[workspace] = mark
	}
	if err = rows.Err(); err != nil {
		return nil, translateBackendConflict(err)
	}
	return marks, nil
}
