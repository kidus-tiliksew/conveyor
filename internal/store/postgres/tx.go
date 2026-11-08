package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// begin opens a translating transaction on the pool.
func (s *Store) begin(ctx context.Context) (pgx.Tx, error) {
	s.notePeerUse()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, translateDriverError(err)
	}
	return s.newNoticeTx(newBoundaryTx(tx)), nil
}

// beginOn opens a translating transaction on a pinned connection.
func (s *Store) beginOn(ctx context.Context, conn *pgxpool.Conn) (pgx.Tx, error) {
	s.notePeerUse()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, translateDriverError(err)
	}
	return s.newNoticeTx(newBoundaryTx(tx)), nil
}

// beginInTaskSession opens a translating transaction on the connection that
// holds the caller's WithTaskSideEffectLock when the context carries one, and
// on the pool otherwise. PostgreSQL advisory locks are re-entrant within one
// session, so a transaction on the held connection may take the same
// task-operation key. A transaction on another pooled session would wait for
// its own caller's session lock and never acquire it (component-persistence
// invariant 6). The held connection stays owned by withAdvisoryLock, which
// unlocks and releases it exactly once after the callback returns.
func (s *Store) beginInTaskSession(ctx context.Context) (pgx.Tx, error) {
	if conn, ok := ctx.Value(sideEffectConnKey{}).(*pgxpool.Conn); ok {
		return s.beginOn(ctx, conn)
	}
	return s.begin(ctx)
}

// beginWith opens a translating transaction with explicit options.
func (s *Store) beginWith(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	s.notePeerUse()
	tx, err := s.pool.BeginTx(ctx, options)
	if err != nil {
		return nil, translateDriverError(err)
	}
	return s.newNoticeTx(newBoundaryTx(tx)), nil
}
