package singlestore

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
)

// Task-event positions and their witnesses (component-persistence, Change
// notices, invariants 27 and 42; component-runtime invariant 36;
// req-quiet-idle-runtime AC-2.5).
//
// Every transaction that commits a task event advances its workspace's row
// in the rowstore table task_event_positions once, just before commit. The
// counter row and the event rows live on different partitions (the counter
// is sharded by workspace, an event by its task), so a peer can read an
// advanced counter before the event rows are visible to it. The same
// transaction therefore records the identity of every task event it counts
// in task_event_witnesses, a rowstore table sharded like the counter. A peer
// accepts a position only once every witness of it joins a visible event row
// (PeerMarks). Commit order comes from the counter row lock, never from event
// IDs or times.
//
// A witness is needed only until its event is visible to everyone. A writer
// removes earlier witnesses only at a prune position, every
// taskEventPruneCadence-th position of a workspace, while it holds the
// workspace counter lock and before it records its own witnesses. It deletes
// at most taskEventPositionPrune witnesses whose events its own read can see,
// oldest first, and keeps every witness whose event is still in flight. A
// peer treats an absent witness below a present one as visible: only a
// writer that saw its event removes it. The counter's own position keeps its
// witnesses until a later commit.
//
// The ordinary commit sends two statements per written workspace beyond its
// event writes: the counter update, which returns the new position through
// LAST_INSERT_ID(expr) in its OK packet, and the witness insert. Only a
// workspace's first commit, whose update matches no row, adds the counter
// insert, and a first commit whose insert loses to a concurrent one adds the
// update again (advanceTaskEventCounter). The ordinary path never reads
// events_columnstore. (SingleStore
// leaves the last insert ID at 0 on the ON DUPLICATE KEY UPDATE branch of an
// upsert, so the counter is not an upsert:
// TestSingleStoreTaskEventPositionResultIntegration.) A prune position adds
// one delete of fixed shape that returns no rows, so it adds no row to the
// read account of the request that commits (component-persistence invariant
// 31).
//
// The cadence counts positions, which advance only in committed
// transactions, so no timer, peer read, or process state drives the prune
// and a rollback rolls the cadence back with the counter
// (req-quiet-idle-runtime AC-1.3; component-runtime invariant 36).
//
// Retained witnesses per workspace: let W be the rows before a prune
// position's commit, V the witnesses below it whose events that commit can
// read, and E its own events. After the commit W' = W - min(V,
// taskEventPositionPrune) + E, and every other commit adds its event count.
// While every event is visible at the next prune position and no position
// carries more than B witnesses, with 64B <= taskEventPositionPrune, the
// table never holds more than taskEventPruneCadence*B rows of the
// workspace: 64 for one event per commit. A witness of an invisible event
// stays however long its event takes, so no constant bounds every workload.

// eventWitness names one task event a transaction wrote.
type eventWitness struct {
	workspace string
	shard     string
	id        int64
}

const (
	// taskEventPruneCadence is the position interval of witness pruning: a
	// commit prunes only when its new position is a multiple of it.
	taskEventPruneCadence = 64
	// taskEventPositionPrune bounds the earlier witnesses one prune removes.
	taskEventPositionPrune = 1000
)

// taskEventPositionUpdateSQL advances an existing workspace counter, takes
// its row lock, and returns the new position as the statement's last insert
// ID. taskEventPositionInsertSQL creates the counter of a workspace whose
// update matched no row.
const (
	taskEventPositionUpdateSQL = `UPDATE task_event_positions SET position=LAST_INSERT_ID(position+1) WHERE workspace_id=?`
	taskEventPositionInsertSQL = `INSERT INTO task_event_positions(workspace_id,position) VALUES(?,1)`
)

// taskEventPruneSQL deletes, in one statement, up to the limit of earlier
// witnesses of a workspace whose event rows this transaction can read,
// oldest first. The derived selection joins each witness to its event
// through events_task_event_idx on the exact key, so the inner join filters
// visibility before the limit and invisible witnesses never crowd out
// visible ones. The delete matches each selected key exactly, never a
// position range. Its text does not change with the number of witnesses, and
// it returns no rows.
const taskEventPruneSQL = `DELETE w FROM task_event_witnesses w JOIN (SELECT x.workspace_id,x.position,x.event_shard_key,x.event_id FROM task_event_witnesses x
JOIN events_columnstore e FORCE INDEX (events_task_event_idx) ON e.event_shard_key=x.event_shard_key AND e.workspace_id=x.workspace_id AND e.id=x.event_id
WHERE x.workspace_id=? AND x.position<? ORDER BY x.position,x.event_shard_key,x.event_id LIMIT ?) c
ON w.workspace_id=c.workspace_id AND w.position=c.position AND w.event_shard_key=c.event_shard_key AND w.event_id=c.event_id
WHERE w.workspace_id=?`

// isTaskEventPrunePosition reports whether the commit that takes position
// prunes earlier witnesses.
func isTaskEventPrunePosition(position int64) bool {
	return position > 0 && position%taskEventPruneCadence == 0
}

// observeTaskEventPreparation, when set by a fixture, receives the
// workspaces each commit preparation advanced, empty when the transaction
// wrote no task event.
var observeTaskEventPreparation atomic.Pointer[func(workspaces []string)]

// observeTaskEventCounterMiss, when set by a fixture, runs after a counter
// update matched no row and before the counter insert, so a fixture can make
// two first commits of a workspace race on that insert.
var observeTaskEventCounterMiss atomic.Pointer[func(ws string)]

// observeTaskEventCounterRecovery, when set by a fixture, receives the
// server's error for a counter insert that lost to a concurrent first commit,
// before the transaction runs the counter update again.
var observeTaskEventCounterRecovery atomic.Pointer[func(ws string, err error)]

// commitTx is the only commit of a store transaction that registered notice
// state (change_notices_positions_test.go, TestTaskEventCommitSites): withTx,
// RunVerificationDelivery, the per-key transactions of a verification
// delivery pass, and the dismissal archive repair. It advances every
// workspace the transaction wrote a task event in, then commits. The caller
// publishes or drops the notices with finishNotices.
func commitTx(ctx context.Context, tx *sql.Tx) error {
	if err := prepareTaskEventPositions(ctx, tx); err != nil {
		return translateBackendConflict(err)
	}
	return translateBackendConflict(tx.Commit())
}

// prepareTaskEventPositions advances each written workspace's position in
// sorted order, so concurrent transactions take the counter locks in one
// order.
func prepareTaskEventPositions(ctx context.Context, tx *sql.Tx) error {
	value, ok := txBatches.Load(tx)
	if !ok {
		return nil
	}
	state := value.(*txNoticeState)
	workspaces := make([]string, 0, len(state.taskEvents))
	for ws := range state.taskEvents {
		workspaces = append(workspaces, ws)
	}
	sort.Strings(workspaces)
	for _, ws := range workspaces {
		if err := advanceTaskEventPosition(ctx, tx, ws, state.taskEvents[ws]); err != nil {
			return err
		}
	}
	if hook := observeTaskEventPreparation.Load(); hook != nil {
		(*hook)(workspaces)
	}
	return nil
}

func advanceTaskEventPosition(ctx context.Context, tx *sql.Tx, ws string, witnesses []eventWitness) error {
	position, err := advanceTaskEventCounter(ctx, tx, ws)
	if err != nil {
		return err
	}
	if isTaskEventPrunePosition(position) {
		if _, err := tx.ExecContext(ctx, taskEventPruneSQL, ws, position, taskEventPositionPrune, ws); err != nil {
			return err
		}
	}
	marks := make([]string, 0, len(witnesses))
	args := make([]any, 0, 4*len(witnesses))
	for _, w := range witnesses {
		marks = append(marks, "(?,?,?,?)")
		args = append(args, ws, position, w.shard, w.id)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO task_event_witnesses(workspace_id,position,event_shard_key,event_id) VALUES `+strings.Join(marks, ","), args...)
	return err
}

// advanceTaskEventCounter advances ws's counter and returns the new
// position. An existing counter takes one update. A workspace without one
// takes the update and an insert at position 1. Two transactions that both
// find no row race on that insert: the loser's insert waits on the winner's
// row and fails with a PRIMARY duplicate key once the winner commits. The
// winner's row then exists, so the loser runs the update again in the same
// transaction and takes the next position under that row's lock. No caller
// sees the race, and the transaction is never replayed. The duplicate key is
// not a lock timeout or a deadlock, so it is not the retryable store error
// (component-persistence, Errors, invariant 12); a real lock timeout or
// deadlock in any of these statements still is.
func advanceTaskEventCounter(ctx context.Context, tx *sql.Tx, ws string) (int64, error) {
	position, found, err := updateTaskEventCounter(ctx, tx, ws)
	if err != nil || found {
		return position, err
	}
	if hook := observeTaskEventCounterMiss.Load(); hook != nil {
		(*hook)(ws)
	}
	_, err = tx.ExecContext(ctx, taskEventPositionInsertSQL, ws)
	if err == nil {
		return 1, nil
	}
	if !isDuplicateKey(err, "PRIMARY") {
		return 0, err
	}
	if hook := observeTaskEventCounterRecovery.Load(); hook != nil {
		(*hook)(ws, err)
	}
	position, found, err = updateTaskEventCounter(ctx, tx, ws)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, fmt.Errorf("task event position of workspace %q: the counter update matched no row after a concurrent first commit created the counter", ws)
	}
	return position, nil
}

// updateTaskEventCounter runs the counter update. It reports the new
// position, which an existing counter row makes at least 2, or found false
// when no counter row exists.
func updateTaskEventCounter(ctx context.Context, tx *sql.Tx, ws string) (position int64, found bool, err error) {
	result, err := tx.ExecContext(ctx, taskEventPositionUpdateSQL, ws)
	if err != nil {
		return 0, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, false, err
	}
	switch affected {
	case 0:
		return 0, false, nil
	case 1:
		position, err = result.LastInsertId()
		if err != nil {
			return 0, false, err
		}
		if position < 2 {
			return 0, false, fmt.Errorf("task event position of workspace %q: the counter update returned position %d", ws, position)
		}
		return position, true, nil
	default:
		return 0, false, fmt.Errorf("task event position of workspace %q: the counter update matched %d rows", ws, affected)
	}
}
