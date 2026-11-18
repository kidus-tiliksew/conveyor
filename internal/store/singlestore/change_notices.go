package singlestore

import (
	"context"
	"database/sql"
	"errors"
	"sync"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/s2log"
)

// Change notices on SingleStore (component-persistence, Change notices;
// DEC-99(1), (3)). A store transaction registers a batch for its *sql.Tx;
// event writes and job appends inside it raise into that batch, which is
// published only after Commit returns. SingleStore has no change
// notification, so a peered daemon runs the peer check on PeerMarks instead.

type txNoticeState struct {
	batch *changes.Batch
	// taskEvents holds, per workspace, the task events tx wrote. Commit
	// preparation advances each workspace's position once and records these
	// events as the position's witnesses (change_notices_positions.go).
	taskEvents map[string][]eventWitness
}

// txBatches maps each open store transaction to its notice state.
var txBatches sync.Map // *sql.Tx -> *txNoticeState

// beginNotices registers tx's batch; finishNotices publishes it after a
// commit or drops it.
func (s *Store) beginNotices(tx *sql.Tx) {
	txBatches.Store(tx, &txNoticeState{batch: s.changes.Begin(), taskEvents: make(map[string][]eventWitness)})
}

func finishNotices(tx *sql.Tx, committed bool) {
	value, ok := txBatches.LoadAndDelete(tx)
	if !ok {
		return
	}
	batch := value.(*txNoticeState).batch
	if committed {
		batch.Commit()
		return
	}
	batch.Discard()
}

func txBatch(tx *sql.Tx) *changes.Batch {
	if value, ok := txBatches.Load(tx); ok {
		return value.(*txNoticeState).batch
	}
	return nil
}

// errTaskEventOutsideNotices refuses a task event written in a transaction
// that registered no notice state: its commit could not advance the
// workspace's task-event position, so a peer would never notice it
// (component-persistence invariant 42).
var errTaskEventOutsideNotices = errors.New("task event written outside a store transaction that advances task_event_positions")

// markTaskEvent records a committed-to-be task event of tx for commit
// preparation. A transaction without notice state cannot take a task event.
func markTaskEvent(tx *sql.Tx, w eventWitness) error {
	value, ok := txBatches.Load(tx)
	if !ok {
		return errTaskEventOutsideNotices
	}
	state := value.(*txNoticeState)
	state.taskEvents[w.workspace] = append(state.taskEvents[w.workspace], w)
	return nil
}

// raiseTx adds n to tx's batch. Every event writer runs in a registered
// store transaction.
func raiseTx(tx *sql.Tx, n changes.Notice) {
	txBatch(tx).Raise(n)
}

// logTx binds tx to the event-log driver and its batch to the context, so
// a job appended inside tx raises its notice at tx's commit. It replaces
// s2log.WithTx everywhere in this package.
func logTx(ctx context.Context, tx *sql.Tx) context.Context {
	return changes.WithBatch(s2log.WithTx(ctx, tx), txBatch(tx))
}

// Changes is the hub this store publishes committed notices on.
func (s *Store) Changes() *changes.Hub { return s.changes }

// EnablePeers reports false: SingleStore offers no change notification, so
// a peered daemon runs one peer check every 30 seconds on PeerMarks.
func (s *Store) EnablePeers(context.Context) bool { return false }
