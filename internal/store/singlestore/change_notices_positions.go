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
// The ordinary commit sends three statements per written workspace beyond
// its event writes: the counter upsert, the read of the new position, and
// the witness insert. It never reads events_columnstore. The upsert cannot
// return the position itself: on SingleStore, LAST_INSERT_ID(expr) in the
// ON DUPLICATE KEY UPDATE branch leaves the statement's last insert ID at 0
// (TestSingleStoreTaskEventPositionResultIntegration). A prune position adds
// one bounded candidate read and, when that read finds a visible witness,
// one delete of fixed shape.
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

// taskEventPositionSQL advances a workspace's counter and takes its row
// lock; taskEventPositionReadSQL reads the new position under that lock.
const (
	taskEventPositionSQL     = `INSERT INTO task_event_positions(workspace_id,position) VALUES(?,1) ON DUPLICATE KEY UPDATE position=position+1`
	taskEventPositionReadSQL = `SELECT position FROM task_event_positions WHERE workspace_id=?`
)

// taskEventPruneCandidatesSQL selects the oldest earlier witnesses whose
// event rows this transaction can read. The inner join filters visibility
// before the limit, so invisible witnesses never crowd out visible ones.
const taskEventPruneCandidatesSQL = `SELECT w.position,w.event_shard_key,w.event_id FROM task_event_witnesses w
JOIN events_columnstore e FORCE INDEX (events_task_event_idx) ON e.event_shard_key=w.event_shard_key AND e.workspace_id=w.workspace_id AND e.id=w.event_id
WHERE w.workspace_id=? AND w.position<? ORDER BY w.position,w.event_shard_key,w.event_id LIMIT ?`

// taskEventPruneDeleteSQL deletes exact witness keys. It always carries
// taskEventPositionPrune key slots, so its text and parameter count do not
// grow with the number of witnesses pruned.
var taskEventPruneDeleteSQL = `DELETE FROM task_event_witnesses WHERE workspace_id=? AND (position,event_shard_key,event_id) IN (` +
	strings.TrimSuffix(strings.Repeat("(?,?,?),", taskEventPositionPrune), ",") + `)`

// isTaskEventPrunePosition reports whether the commit that takes position
// prunes earlier witnesses.
func isTaskEventPrunePosition(position int64) bool {
	return position > 0 && position%taskEventPruneCadence == 0
}

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
	if _, err := tx.ExecContext(ctx, taskEventPositionSQL, ws); err != nil {
		return err
	}
	var position int64
	if err := tx.QueryRowContext(ctx, taskEventPositionReadSQL, ws).Scan(&position); err != nil {
		return err
	}
	if position <= 0 {
		return fmt.Errorf("task event position of workspace %q: the counter holds position %d after its advance", ws, position)
	}
	if isTaskEventPrunePosition(position) {
		if err := pruneTaskEventWitnesses(ctx, tx, ws, position); err != nil {
			return err
		}
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

// pruneTaskEventWitnesses deletes up to taskEventPositionPrune witnesses of ws
// below position whose events this transaction can read, oldest first. A
// visible committed event stays visible to every later read, so no peer
// needs its witness again. It deletes exact keys, never a position range: a
// range can hold the witness of an event that is still invisible.
func pruneTaskEventWitnesses(ctx context.Context, tx *sql.Tx, ws string, position int64) error {
	rows, err := tx.QueryContext(ctx, taskEventPruneCandidatesSQL, ws, position, taskEventPositionPrune)
	if err != nil {
		return err
	}
	keys := make([]any, 0, 3*taskEventPositionPrune)
	for rows.Next() {
		var p, id int64
		var shard string
		if err = rows.Scan(&p, &shard, &id); err != nil {
			rows.Close()
			return err
		}
		keys = append(keys, p, shard, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(keys) == 0 {
		return err
	}
	_, err = tx.ExecContext(ctx, taskEventPruneDeleteSQL, taskEventPruneDeleteArgs(ws, keys)...)
	return err
}

// taskEventPruneDeleteArgs fills every key slot of taskEventPruneDeleteSQL:
// the selected keys, then the first key again for each unused slot. A
// repeated key matches the same row, so padding deletes nothing more.
func taskEventPruneDeleteArgs(ws string, keys []any) []any {
	args := make([]any, 0, 1+3*taskEventPositionPrune)
	args = append(args, ws)
	args = append(args, keys...)
	for len(args) < cap(args) {
		args = append(args, keys[0], keys[1], keys[2])
	}
	return args
}
