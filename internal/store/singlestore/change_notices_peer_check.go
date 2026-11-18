package singlestore

import (
	"context"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

// The peer check read on SingleStore (component-persistence, Change notices,
// invariants 27 and 42; component-runtime invariant 36; feature-quiet-idle-
// runtime, Peers; req-quiet-idle-runtime AC-1.3, AC-2.5). SingleStore offers
// no change notification, so a peered daemon reads every workspace's marks in
// this one statement once per peer check interval.
//
// The statement returns each workspace's log position, its task-event
// position, and, for every task-event witness above the position this store
// last accepted for the workspace, whether the witnessed event row is
// visible to this read. While no workspace changed, the witness branch
// selects no row, so the read looks up no event.
const (
	peerMarksPositionsSQL = `SELECT workspace_id, 0 AS source, position AS n, 1 AS visible FROM event_log_positions
UNION ALL
SELECT workspace_id, 1 AS source, position AS n, 1 AS visible FROM task_event_positions
UNION ALL
`
	peerMarksWitnessSQL = `SELECT w.workspace_id, 2 AS source, w.position AS n, CASE WHEN e.id IS NULL THEN 0 ELSE 1 END AS visible
FROM task_event_witnesses w
LEFT JOIN events_columnstore e ON e.event_shard_key = w.event_shard_key AND e.workspace_id = w.workspace_id AND e.id = w.event_id
WHERE w.position > `
)

// peerMarksStatement builds the one statement for the accepted positions.
// Each accepted workspace bounds its witnesses from below; any other
// workspace reads all of its witnesses, which writers keep few.
func peerMarksStatement(accepted map[string]int64) (string, []any) {
	if len(accepted) == 0 {
		return peerMarksPositionsSQL + peerMarksWitnessSQL + "0", nil
	}
	workspaces := make([]string, 0, len(accepted))
	for ws := range accepted {
		workspaces = append(workspaces, ws)
	}
	sort.Strings(workspaces)
	var floor strings.Builder
	args := make([]any, 0, 2*len(workspaces))
	floor.WriteString("CASE w.workspace_id")
	for _, ws := range workspaces {
		floor.WriteString(" WHEN ? THEN ?")
		args = append(args, ws, accepted[ws])
	}
	floor.WriteString(" ELSE 0 END")
	return peerMarksPositionsSQL + peerMarksWitnessSQL + floor.String(), args
}

// observePeerWitnessRows, when set by a fixture, receives the number of
// witness rows each successful PeerMarks statement returned.
var observePeerWitnessRows atomic.Pointer[func(rows int)]

var _ changes.PeerReader = (*Store)(nil)

// PeerMarks returns each workspace's log position and its accepted
// task-event position: the highest position whose witnesses, and every
// earlier one still recorded, join a visible event row. A position whose
// event is not yet visible leaves the mark where it was, so the next
// successful read accepts it without another task event. A failed read
// returns no marks and keeps the accepted positions.
func (s *Store) PeerMarks(ctx context.Context) (map[string]changes.PeerMark, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "PeerMarks")
	s.peerMu.Lock()
	defer s.peerMu.Unlock()
	query, args := peerMarksStatement(s.peerAccepted)
	rows, err := backendRows(ctx, s.db, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	marks := map[string]changes.PeerMark{}
	counters := map[string]int64{}
	witnesses := map[string]map[int64]bool{}
	witnessRows := 0
	for rows.Next() {
		var workspace string
		var source, visible int
		var n int64
		if err = rows.Scan(&workspace, &source, &n, &visible); err != nil {
			return nil, translateBackendConflict(err)
		}
		switch source {
		case 0:
			mark := marks[workspace]
			mark.Log = n
			marks[workspace] = mark
		case 1:
			counters[workspace] = n
			if _, ok := marks[workspace]; !ok {
				marks[workspace] = changes.PeerMark{}
			}
		default:
			witnessRows++
			positions := witnesses[workspace]
			if positions == nil {
				positions = map[int64]bool{}
				witnesses[workspace] = positions
			}
			seen, ok := positions[n]
			positions[n] = (seen || !ok) && visible == 1
		}
	}
	if err = rows.Err(); err != nil {
		return nil, translateBackendConflict(err)
	}
	accepted := make(map[string]int64, len(counters))
	for workspace, counter := range counters {
		position := acceptTaskEventPosition(s.peerAccepted[workspace], counter, witnesses[workspace])
		accepted[workspace] = position
		mark := marks[workspace]
		mark.Events = position
		marks[workspace] = mark
	}
	s.peerAccepted = accepted
	if hook := observePeerWitnessRows.Load(); hook != nil {
		(*hook)(witnessRows)
	}
	return marks, nil
}

// acceptTaskEventPosition walks the witnessed positions above floor up to the
// counter in commit order and stops at the first one with an invisible
// event. It accepts only a position that has a witness row: a missing one
// above floor was removed by a writer that saw its event, but the counter's
// own position always keeps its witnesses until the next commit. A position
// without any witness at all (a counter from before witnesses existed, or a
// read that saw the counter before its witnesses) is never accepted.
func acceptTaskEventPosition(floor, counter int64, witnessed map[int64]bool) int64 {
	if counter < floor {
		// The counter never moves back; a smaller one is a different
		// database. Start over.
		floor = 0
	}
	positions := make([]int64, 0, len(witnessed))
	for position := range witnessed {
		if position > floor && position <= counter {
			positions = append(positions, position)
		}
	}
	sort.Slice(positions, func(i, j int) bool { return positions[i] < positions[j] })
	accepted := floor
	for _, position := range positions {
		if !witnessed[position] {
			break
		}
		accepted = position
	}
	return accepted
}
