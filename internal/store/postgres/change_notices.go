package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/pglog"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

// Change notices on PostgreSQL (component-persistence, Change notices;
// DEC-99(1), (2)). Every translating transaction carries a notice batch that
// is published only after Commit returns; with peers on, the batch is also
// sent as pg_notify inside the transaction, and one direct LISTEN
// connection republishes other daemons' notices on the hub.

// peerChannel is the LISTEN/NOTIFY channel daemons share.
const peerChannel = "conveyor_changes"

// noticeTx is a translating transaction that holds the notices it raises
// until it commits.
type noticeTx struct {
	pgx.Tx
	store *Store
	batch *changes.Batch
	// root is false for a savepoint, whose batch hands its notices to the
	// enclosing transaction's batch.
	root bool
}

func (s *Store) newNoticeTx(tx pgx.Tx) pgx.Tx {
	if tx == nil {
		return nil
	}
	return &noticeTx{Tx: tx, store: s, batch: s.changes.Begin(), root: true}
}

// Begin opens a savepoint whose notices join this transaction's on release.
func (t *noticeTx) Begin(ctx context.Context) (pgx.Tx, error) {
	sp, err := t.Tx.Begin(ctx)
	if err != nil {
		return nil, translateDriverError(err)
	}
	return &noticeTx{Tx: newBoundaryTx(sp), store: t.store, batch: t.batch.Savepoint()}, nil
}

// Commit sends the batch to peers inside the transaction when peers are on,
// commits, and publishes the batch only after the commit succeeded.
func (t *noticeTx) Commit(ctx context.Context) error {
	if t.root && t.store.peersEnabled() {
		// The listener hears this daemon's own notifications too and
		// republishes them; the duplicate coalesces on the hub. Nothing is
		// suppressed, because a payload does not identify its sender and a
		// peer's notice must arrive whatever happens to this transaction.
		for _, n := range t.batch.Notices() {
			if _, err := t.Tx.Exec(ctx, `SELECT pg_notify($1, $2)`, peerChannel, n.Payload()); err != nil {
				t.batch.Discard()
				_ = t.Tx.Rollback(ctx)
				return err
			}
		}
	}
	if err := t.Tx.Commit(ctx); err != nil {
		t.batch.Discard()
		return err
	}
	t.batch.Commit()
	return nil
}

// Rollback discards the batch.
func (t *noticeTx) Rollback(ctx context.Context) error {
	t.batch.Discard()
	return t.Tx.Rollback(ctx)
}

// noticeBatch returns the batch of the transaction exec runs on, if any.
func noticeBatch(exec any) *changes.Batch {
	switch tx := exec.(type) {
	case *noticeTx:
		return tx.batch
	case boundaryTx:
		return noticeBatch(tx.Tx)
	}
	return nil
}

// raiseTx adds n to the batch of the transaction exec runs on. A statement
// outside a store transaction raises nothing: every event writer runs in one.
func raiseTx(exec any, n changes.Notice) {
	noticeBatch(exec).Raise(n)
}

// raiseTaskNotice raises a task notice for an event inserted through exec.
func raiseTaskNotice(ctx context.Context, exec any, taskID string) {
	raiseTx(exec, changes.Notice{Workspace: workspace(ctx), Kind: changes.Task, TaskID: taskID})
}

// logTx binds tx to the event-log driver and its notice batch to the
// context, so a job appended inside tx raises its notice at tx's commit.
// It replaces pglog.WithTx everywhere in this package.
func logTx(ctx context.Context, tx pgx.Tx) context.Context {
	return changes.WithBatch(pglog.WithTx(ctx, tx), noticeBatch(tx))
}

// txLog is the store's Log(): the notifying driver, with an append that is
// not already inside a store transaction run inside one, so its notice takes
// the same commit path, including pg_notify for peers.
type txLog struct {
	*logqueue.NotifyingLog
	store *Store
}

func (l *txLog) Append(ctx context.Context, workspace string, stream eventlog.StreamID, expected eventlog.Version, events []eventlog.NewEvent) (eventlog.Version, error) {
	if _, ok := changes.BatchFrom(ctx); ok {
		return l.NotifyingLog.Append(ctx, workspace, stream, expected, events)
	}
	var head eventlog.Version
	err := l.store.inTx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
		var err error
		head, err = l.NotifyingLog.Append(logTx(ctx, tx), workspace, stream, expected, events)
		return err
	})
	return head, err
}

// Changes is the hub this store publishes committed notices on.
func (s *Store) Changes() *changes.Hub { return s.changes }

// EnablePeers turns on pg_notify for every notice-raising transaction and
// opens the LISTEN connection. It reports true: PostgreSQL notifies peers.
func (s *Store) EnablePeers(ctx context.Context) bool {
	if s.peers.enabled.Swap(true) {
		return true
	}
	if err := s.peers.connect(ctx, s); err != nil {
		s.peers.logf("postgres: change listener: %v; it reconnects on the next request", err)
	}
	return true
}

func (s *Store) peersEnabled() bool { return s.peers != nil && s.peers.enabled.Load() }

// notePeerUse is called on every store operation that reaches the database.
// A closed listener reconnects here, never on a timer (req-quiet-idle-runtime
// AC-1.4).
func (s *Store) notePeerUse() {
	if s.peers == nil || !s.peers.enabled.Load() || s.peers.state.Load() != listenerDown {
		return
	}
	if !s.peers.state.CompareAndSwap(listenerDown, listenerConnecting) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.peers.dial(ctx, s); err != nil {
			s.peers.state.Store(listenerDown)
			s.peers.logf("postgres: change listener reconnect: %v", err)
			return
		}
		// Notices sent while nobody listened are recovered by one catch-up
		// of every workspace.
		for _, kind := range []changes.Kind{changes.Job, changes.Task, changes.Deadline, changes.Workspace} {
			s.changes.Publish(changes.Notice{Kind: kind})
		}
	}()
}

const (
	listenerDown int32 = iota
	listenerConnecting
	listenerUp
)

// peerListener owns the one direct, unpooled LISTEN connection.
type peerListener struct {
	enabled atomic.Bool
	state   atomic.Int32
	mu      sync.Mutex
	conn    *pgx.Conn
	cancel  context.CancelFunc
	closed  bool
	done    chan struct{}
	logf    func(string, ...any)
}

func newPeerListener(logf func(string, ...any)) *peerListener {
	return &peerListener{logf: logf}
}

// connect is the first dial, made by EnablePeers.
func (p *peerListener) connect(ctx context.Context, s *Store) error {
	p.state.Store(listenerConnecting)
	if err := p.dial(ctx, s); err != nil {
		p.state.Store(listenerDown)
		return err
	}
	return nil
}

// listenerApplicationName names the connection in pg_stat_activity.
func listenerApplicationName(base string) string {
	if base == "" {
		base = "conveyord"
	}
	return base + ":changes-listener"
}

func (p *peerListener) dial(ctx context.Context, s *Store) error {
	config := s.pool.Config().ConnConfig.Copy()
	config.RuntimeParams["application_name"] = listenerApplicationName(config.RuntimeParams["application_name"])
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return fmt.Errorf("connect: %w", translateDriverError(err))
	}
	if _, err := conn.Exec(ctx, "LISTEN "+peerChannel); err != nil {
		_ = conn.Close(context.Background())
		return fmt.Errorf("listen: %w", translateDriverError(err))
	}
	listenCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		cancel()
		_ = conn.Close(context.Background())
		return errors.New("store closed")
	}
	p.conn, p.cancel, p.done = conn, cancel, done
	p.state.Store(listenerUp)
	p.mu.Unlock()
	go p.listen(listenCtx, conn, s.changes, done)
	return nil
}

func (p *peerListener) listen(ctx context.Context, conn *pgx.Conn, hub *changes.Hub, done chan struct{}) {
	defer close(done)
	for {
		notification, err := conn.WaitForNotification(ctx)
		if err != nil {
			if ctx.Err() == nil {
				p.logf("postgres: change listener closed; it reconnects on the next request")
			}
			_ = conn.Close(context.Background())
			p.mu.Lock()
			if p.conn == conn {
				p.conn, p.cancel = nil, nil
				if !p.closed {
					p.state.Store(listenerDown)
				}
			}
			p.mu.Unlock()
			return
		}
		if n, ok := changes.ParsePayload(notification.Payload); ok {
			hub.Publish(n)
		}
	}
}

// close stops the listener for good.
func (p *peerListener) close() {
	p.mu.Lock()
	p.closed = true
	cancel, done := p.cancel, p.done
	p.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// listening reports whether the LISTEN connection is up, for tests.
func (p *peerListener) listening() bool { return p.state.Load() == listenerUp }
