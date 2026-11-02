package singlestore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

const lockSchema = "CREATE ROWSTORE TABLE IF NOT EXISTS conveyor_locks (`key` VARCHAR(64) NOT NULL, PRIMARY KEY (`key`), SHARD KEY (`key`))"

// lockReturnObserver records when lockKey returns. Tests set it; production
// callers leave it unset.
type lockReturnObserver struct{ at time.Time }

type lockReturnKey struct{}

func observeLockReturn(ctx context.Context) (context.Context, *lockReturnObserver) {
	obs := &lockReturnObserver{}
	return context.WithValue(ctx, lockReturnKey{}, obs), obs
}

func noteLockReturn(ctx context.Context) {
	if obs, ok := ctx.Value(lockReturnKey{}).(*lockReturnObserver); ok {
		obs.at = time.Now()
	}
}

// taskTxCommitObserver is a test-only hook; production callers leave it unset.
type taskTxCommitObserver struct {
	at    time.Time
	calls atomic.Int32
	hook  func()
}

type taskTxCommitKey struct{}

func observeTaskTxCommit(ctx context.Context, hook func()) (context.Context, *taskTxCommitObserver) {
	obs := &taskTxCommitObserver{hook: hook}
	return context.WithValue(ctx, taskTxCommitKey{}, obs), obs
}

func noteTaskTxCommit(ctx context.Context) {
	if obs, ok := ctx.Value(taskTxCommitKey{}).(*taskTxCommitObserver); ok {
		obs.at = time.Now()
		obs.calls.Add(1)
		if obs.hook != nil {
			obs.hook()
		}
	}
}

// lockKey serializes one key until the caller transaction ends. Hashing gives
// arbitrary caller keys a fixed width with no collation-dependent equivalence.
func lockKey(ctx context.Context, tx *sql.Tx, key string) error {
	defer noteLockReturn(ctx)
	key = lockRowKey(key)
	if err := upsertLockRow(ctx, tx, key); err != nil {
		return err
	}
	var held string
	return tx.QueryRowContext(ctx, "SELECT `key` FROM conveyor_locks WHERE `key`=? FOR UPDATE", key).Scan(&held)
}

// claimKey locks key like lockKey and returns the holder recorded on its lock
// row. A commit spanning partitions can release the lock row before rows on
// other partitions are visible, so the next holder may miss the rows its
// predecessor wrote. The holder commits on the locked row itself, atomically
// with the release, so a unique rule reads it instead of trusting only a count
// over other partitions.
func claimKey(ctx context.Context, tx *sql.Tx, key string) (string, error) {
	defer noteLockReturn(ctx)
	key = lockRowKey(key)
	if err := upsertLockRow(ctx, tx, key); err != nil {
		return "", err
	}
	var holder string
	err := tx.QueryRowContext(ctx, "SELECT holder FROM conveyor_locks WHERE `key`=? FOR UPDATE", key).Scan(&holder)
	return holder, err
}

// setClaim records holder on a lock row the transaction claimed; "" releases it.
func setClaim(ctx context.Context, tx *sql.Tx, key, holder string) error {
	_, err := tx.ExecContext(ctx, "UPDATE conveyor_locks SET holder=? WHERE `key`=?", holder, lockRowKey(key))
	return err
}

// claimedRecord is the record a claim row's holder names: the last record a
// decision under the claim appended (component-persistence, SingleStore
// claimed records). Table is "events" or "monitor_activity". Shard is the
// event's shard key; "" means the claim's own task. Note is an optional value
// the decision reads back, such as a review summary version.
type claimedRecord struct {
	Table string
	ID    int64
	Shard string
	Note  string
}

const (
	claimedEventsTable          = "events"
	claimedMonitorActivityTable = "monitor_activity"
)

func (r claimedRecord) holder() string {
	return r.Table + ":" + strconv.FormatInt(r.ID, 10) + ":" + r.Shard + ":" + r.Note
}

func parseClaimedRecord(holder string) (claimedRecord, error) {
	parts := strings.SplitN(holder, ":", 4)
	if len(parts) != 4 || (parts[0] != claimedEventsTable && parts[0] != claimedMonitorActivityTable) {
		return claimedRecord{}, fmt.Errorf("claim holder %q names no record", holder)
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || id <= 0 {
		return claimedRecord{}, fmt.Errorf("claim holder %q names no record", holder)
	}
	return claimedRecord{Table: parts[0], ID: id, Shard: parts[2], Note: parts[3]}, nil
}

// Claimed-record visibility polling: the first delay doubles up to the
// largest, and the wait gives up after the limit. Tests shorten the limit.
const (
	claimedRecordFirstDelay = time.Millisecond
	claimedRecordMaxDelay   = 50 * time.Millisecond
)

var claimedRecordWaitLimit = 10 * time.Second

// claimedRecordWaitObserver counts visibility checks that found the claimed
// record missing. Tests set it; production callers leave it unset.
type claimedRecordWaitObserver struct{ waits chan struct{} }

type claimedRecordWaitKey struct{}

func observeClaimedRecordWait(ctx context.Context) (context.Context, *claimedRecordWaitObserver) {
	obs := &claimedRecordWaitObserver{waits: make(chan struct{}, 1)}
	return context.WithValue(ctx, claimedRecordWaitKey{}, obs), obs
}

func noteClaimedRecordWait(ctx context.Context) {
	if obs, ok := ctx.Value(claimedRecordWaitKey{}).(*claimedRecordWaitObserver); ok {
		select {
		case obs.waits <- struct{}{}:
		default:
		}
	}
}

// awaitClaimedRecord locks key like claimKey and returns the record its holder
// names once that record is visible to this transaction. A commit that spans
// partitions can release the lock row before the predecessor's record is
// visible; waiting for the named record closes that gap. Each holder waited
// for its own predecessor before it wrote, so every earlier record under the
// claim is visible too. found is false when no decision recorded one yet; the
// caller then reads records written before the claim existed as before.
// After claimedRecordWaitLimit the call returns store.ErrRetryable.
func awaitClaimedRecord(ctx context.Context, tx *sql.Tx, key, workspace, taskShard string) (record claimedRecord, found bool, err error) {
	holder, err := claimKey(ctx, tx, key)
	if err != nil || holder == "" {
		return claimedRecord{}, false, err
	}
	if record, err = parseClaimedRecord(holder); err != nil {
		return claimedRecord{}, false, err
	}
	query, args := "SELECT COUNT(*) FROM monitor_activity WHERE workspace_id=? AND id=?", []any{workspace, record.ID}
	if record.Table == claimedEventsTable {
		shard := record.Shard
		if shard == "" {
			shard = taskShard
		}
		query, args = "SELECT COUNT(*) FROM events WHERE workspace_id=? AND event_shard_key=? AND id=?", []any{workspace, shard, record.ID}
	}
	deadline := time.Now().Add(claimedRecordWaitLimit)
	delay := claimedRecordFirstDelay
	for {
		var visible int
		if err = tx.QueryRowContext(ctx, query, args...).Scan(&visible); err != nil {
			return claimedRecord{}, false, err
		}
		if visible > 0 {
			return record, true, nil
		}
		noteClaimedRecordWait(ctx)
		if !time.Now().Before(deadline) {
			return claimedRecord{}, false, fmt.Errorf("%w: claimed %s record %d is not visible", store.ErrRetryable, record.Table, record.ID)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return claimedRecord{}, false, ctx.Err()
		case <-timer.C:
		}
		delay = min(2*delay, claimedRecordMaxDelay)
	}
}

// recordClaimedRecord names record on the claim row awaitClaimedRecord locked,
// in the transaction that appended it, so the name commits with the release.
func recordClaimedRecord(ctx context.Context, tx *sql.Tx, key string, record claimedRecord) error {
	return setClaim(ctx, tx, key, record.holder())
}

// lastInsertedRecord returns the claimedRecord of the row this transaction's
// connection inserted last into table. A task event keeps the empty shard;
// a workspace event carries its generated shard key.
func lastInsertedRecord(ctx context.Context, tx *sql.Tx, table, workspace string, taskEvent bool) (claimedRecord, error) {
	record := claimedRecord{Table: table}
	if err := tx.QueryRowContext(ctx, `SELECT LAST_INSERT_ID()`).Scan(&record.ID); err != nil {
		return claimedRecord{}, err
	}
	if table == claimedEventsTable && !taskEvent {
		if err := tx.QueryRowContext(ctx, `SELECT event_shard_key FROM events WHERE workspace_id=? AND id=?`, workspace, record.ID).Scan(&record.Shard); err != nil {
			return claimedRecord{}, err
		}
	}
	return record, nil
}

// recordLastInserted names this transaction's last insert into table, a
// workspace event or a monitor_activity row, on claim key.
func recordLastInserted(ctx context.Context, tx *sql.Tx, key, table string) error {
	record, err := lastInsertedRecord(ctx, tx, table, documentWorkspace(ctx), false)
	if err != nil {
		return err
	}
	return recordClaimedRecord(ctx, tx, key, record)
}

func lockRowKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func upsertLockRow(ctx context.Context, tx *sql.Tx, key string) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO conveyor_locks (`key`) VALUES (?) ON DUPLICATE KEY UPDATE `key`=`key`", key)
	return err
}

// sessionLock reserves a connection until release. database/sql rolls the
// transaction back on context cancellation; the callback must obey its context.
func (s *Store) sessionLock(ctx context.Context, key string) (func() error, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, translateBackendConflict(err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		conn.Close()
		return nil, translateBackendConflict(err)
	}
	if err = lockKey(ctx, tx, key); err != nil {
		tx.Rollback()
		conn.Close()
		return nil, translateBackendConflict(err)
	}
	var once sync.Once
	var releaseErr error
	release := func() error {
		once.Do(func() {
			err := tx.Rollback()
			if err != nil && err != sql.ErrTxDone {
				releaseErr = translateBackendConflict(err)
			}
			if err = conn.Close(); releaseErr == nil {
				releaseErr = err
			}
		})
		return releaseErr
	}
	stop := context.AfterFunc(ctx, func() { _ = release() })
	return func() error { stop(); return release() }, nil
}

// taskLockOwner is private to this backend and valid only for the callback's
// lifetime. An escaped context cannot bypass locking after the callback exits.
// DEC-38, component-persistence: commands and callbacks share one identity.
type taskLockOwnerKey struct{}
type taskLockOwner struct {
	store           *Store
	workspace, task string
	tx              *sql.Tx
	active          atomic.Bool
	parent          *taskLockOwner
}

func (s *Store) ownsTaskLock(ctx context.Context, ws, task string) bool {
	owner, _ := ctx.Value(taskLockOwnerKey{}).(*taskLockOwner)
	for ; owner != nil; owner = owner.parent {
		if owner.store == s && owner.workspace == ws && owner.task == task && owner.active.Load() {
			return true
		}
	}
	return false
}
func (s *Store) lockOnTaskSession(ctx context.Context, ws, task, key string) error {
	if s.ownsTaskLock(ctx, ws, key) {
		return nil
	}
	owner, _ := ctx.Value(taskLockOwnerKey{}).(*taskLockOwner)
	for ; owner != nil; owner = owner.parent {
		if owner.store == s && owner.workspace == ws && owner.task == task && owner.active.Load() {
			if owner.tx == nil {
				return fmt.Errorf("task lock session has no transaction")
			}
			return lockKey(ctx, owner.tx, "task-operation:"+ws+":"+key)
		}
	}
	return fmt.Errorf("task lock session required")
}

func (s *Store) lockTaskOperation(ctx context.Context, tx *sql.Tx, ws, task string) error {
	if s.ownsTaskLock(ctx, ws, task) {
		return nil
	}
	return lockKey(ctx, tx, "task-operation:"+ws+":"+task)
}
func (s *Store) WithTaskSideEffectLock(ctx context.Context, taskID string, fn func(context.Context) error) error {
	ws, err := workspace(ctx)
	if err != nil {
		return err
	}
	if s.ownsTaskLock(ctx, ws, taskID) {
		return fn(ctx)
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return translateBackendConflict(err)
	}
	defer conn.Close()
	// Acquisition obeys cancellation. Once acquired the transaction survives
	// cancellation until fn returns, so other commands cannot overlap cleanup.
	tx, err := conn.BeginTx(context.WithoutCancel(ctx), nil)
	if err != nil {
		return translateBackendConflict(err)
	}
	defer tx.Rollback()
	if err = s.lockTaskOperation(ctx, tx, ws, taskID); err != nil {
		return translateBackendConflict(err)
	}
	parent, _ := ctx.Value(taskLockOwnerKey{}).(*taskLockOwner)
	owner := &taskLockOwner{store: s, workspace: ws, task: taskID, tx: tx, parent: parent}
	owner.active.Store(true)
	defer owner.active.Store(false)
	return fn(context.WithValue(ctx, taskLockOwnerKey{}, owner))
}
