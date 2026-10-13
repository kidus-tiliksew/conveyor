package singlestore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
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
