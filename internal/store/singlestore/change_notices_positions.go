package singlestore

import (
	"context"
	"database/sql"
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
// A witness is needed only until its event is visible to everyone. Each
// writer, holding the workspace counter lock, deletes the earlier witnesses
// whose events its own read can see, so the table keeps only the latest
// position's witnesses and any whose events are still in flight. A peer
// treats an absent witness below a present one as visible: only a writer that
// saw its event removes it.

// eventWitness names one task event a transaction wrote.
type eventWitness struct {
	workspace string
	shard     string
	id        int64
}

// taskEventPositionPrune bounds the earlier witnesses one commit removes.
const taskEventPositionPrune = 1000

// observeTaskEventPreparation, when set by a fixture, receives the
// workspaces each commit preparation advanced, empty when the transaction
// wrote no task event.
var observeTaskEventPreparation atomic.Pointer[func(workspaces []string)]

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
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_event_positions(workspace_id,position) VALUES(?,1) ON DUPLICATE KEY UPDATE position=position+1`, ws); err != nil {
		return err
	}
	var position int64
	if err := tx.QueryRowContext(ctx, `SELECT position FROM task_event_positions WHERE workspace_id=?`, ws).Scan(&position); err != nil {
		return err
	}
	if err := pruneTaskEventWitnesses(ctx, tx, ws, position); err != nil {
		return err
	}
	marks := make([]string, 0, len(witnesses))
	args := make([]any, 0, 4*len(witnesses))
	for _, w := range witnesses {
		marks = append(marks, "(?,?,?,?)")
		args = append(args, ws, position, w.shard, w.id)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO task_event_witnesses(workspace_id,position,event_shard_key,event_id) VALUES `+strings.Join(marks, ","), args...)
	return err
}

// pruneTaskEventWitnesses deletes the earlier witnesses of ws whose events
// this transaction can read. A visible committed event stays visible to every
// later read, so no peer needs its witness again.
func pruneTaskEventWitnesses(ctx context.Context, tx *sql.Tx, ws string, position int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT w.position,w.event_shard_key,w.event_id FROM task_event_witnesses w
JOIN events_columnstore e ON e.event_shard_key=w.event_shard_key AND e.workspace_id=w.workspace_id AND e.id=w.event_id
WHERE w.workspace_id=? AND w.position<? LIMIT ?`, ws, position, taskEventPositionPrune)
	if err != nil {
		return err
	}
	var conditions []string
	args := []any{ws}
	for rows.Next() {
		var p, id int64
		var shard string
		if err = rows.Scan(&p, &shard, &id); err != nil {
			rows.Close()
			return err
		}
		conditions = append(conditions, "(position=? AND event_shard_key=? AND event_id=?)")
		args = append(args, p, shard, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(conditions) == 0 {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM task_event_witnesses WHERE workspace_id=? AND (`+strings.Join(conditions, " OR ")+`)`, args...)
	return err
}
