package singlestore

import (
	"context"
	"database/sql"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
)

// The peer check read on SingleStore (component-persistence, Change notices;
// feature-quiet-idle-runtime, Peers; req-quiet-idle-runtime AC-1.3, AC-2.5).
// SingleStore offers no change notification, so a peered daemon reads every
// workspace's marks in this one statement once per peer check interval.
//
// A workspace's log position advances under its head row lock in commit
// order, and its task-event count grows with every committed insert whatever
// the order in which AUTO_INCREMENT IDs commit, so a change committed after
// one read always moves a value the next read returns. The highest ID is
// read too, so a deletion paired with an insert still moves a value.
const peerMarksSQL = `SELECT workspace_id, 0 AS source, position AS n, 0 AS last_id FROM event_log_positions
UNION ALL
SELECT workspace_id, 1 AS source, COUNT(*) AS n, MAX(id) AS last_id FROM events WHERE task_id IS NOT NULL GROUP BY workspace_id`

var _ changes.PeerReader = (*Store)(nil)

// PeerMarks returns each workspace's log head position and the count and
// highest ID of its task events, in one statement. A failed read returns no
// marks.
func (s *Store) PeerMarks(ctx context.Context) (map[string]changes.PeerMark, error) {
	rows, err := s.db.QueryContext(ctx, peerMarksSQL)
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
