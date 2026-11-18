package singlestore

import (
	"context"
	"fmt"
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
// position, every task-event witness above the position this store last
// accepted for the workspace, and, again, each of those witnesses whose
// event row is visible to this read. The visible branch is an inner join
// driven by the witness rows: SingleStore builds its hash table on the few
// witness rows and probes events_columnstore through the unique hash key
// events_task_event_idx (a "join index" filter), which FORCE INDEX keeps the
// planner from replacing with a repartition of the event table. While no
// workspace changed, the witness filter selects no row, so the probe skips
// every on-disk segment. SingleStore still reads the table's unflushed
// in-memory segment for the probe. (A left join builds the hash table on the
// whole event table instead.)
const (
	peerMarksPositionsSQL = `SELECT workspace_id, 0 AS source, position AS n FROM event_log_positions
UNION ALL
SELECT workspace_id, 1 AS source, position AS n FROM task_event_positions
UNION ALL
SELECT w.workspace_id, 2 AS source, w.position AS n FROM task_event_witnesses w WHERE w.position > %[1]s
UNION ALL
SELECT w.workspace_id, 3 AS source, w.position AS n FROM task_event_witnesses w
JOIN events_columnstore e FORCE INDEX (events_task_event_idx) ON e.event_shard_key = w.event_shard_key AND e.workspace_id = w.workspace_id AND e.id = w.event_id
WHERE w.position > %[1]s`
)

// peerMarksStatement builds the one statement for the accepted positions.
// Each accepted workspace bounds its witnesses from below; any other
// workspace reads all of its witnesses, which writers keep few. Both
// witness branches take the same bound.
func peerMarksStatement(accepted map[string]int64) (string, []any) {
	if len(accepted) == 0 {
		return fmt.Sprintf(peerMarksPositionsSQL, "0"), nil
	}
	workspaces := make([]string, 0, len(accepted))
	for ws := range accepted {
		workspaces = append(workspaces, ws)
	}
	sort.Strings(workspaces)
	var floor strings.Builder
	bound := make([]any, 0, 2*len(workspaces))
	floor.WriteString("CASE w.workspace_id")
	for _, ws := range workspaces {
		floor.WriteString(" WHEN ? THEN ?")
		bound = append(bound, ws, accepted[ws])
	}
	floor.WriteString(" ELSE 0 END")
	return fmt.Sprintf(peerMarksPositionsSQL, floor.String()), append(bound, bound...)
}

// observePeerWitnessRows, when set by a fixture, receives the number of
// rows both witness branches of each successful PeerMarks statement
// returned.
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
	// witnessed[workspace][position] counts the position's witnesses and,
	// separately, those whose event row this read saw.
	type tally struct{ witnesses, visible int }
	witnessed := map[string]map[int64]*tally{}
	witnessRows := 0
	for rows.Next() {
		var workspace string
		var source int
		var n int64
		if err = rows.Scan(&workspace, &source, &n); err != nil {
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
			positions := witnessed[workspace]
			if positions == nil {
				positions = map[int64]*tally{}
				witnessed[workspace] = positions
			}
			t := positions[n]
			if t == nil {
				t = &tally{}
				positions[n] = t
			}
			if source == 2 {
				t.witnesses++
			} else {
				t.visible++
			}
		}
	}
	if err = rows.Err(); err != nil {
		return nil, translateBackendConflict(err)
	}
	accepted := make(map[string]int64, len(counters))
	for workspace, counter := range counters {
		visible := map[int64]bool{}
		for position, t := range witnessed[workspace] {
			// A position whose witness rows this read did not return at
			// all is unknown, never visible.
			visible[position] = t.witnesses > 0 && t.visible >= t.witnesses
		}
		position := acceptTaskEventPosition(s.peerAccepted[workspace], counter, visible)
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
