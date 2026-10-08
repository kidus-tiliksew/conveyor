package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// begin opens a translating transaction on the pool.
func (s *Store) begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, translateDriverError(err)
	}
	return newBoundaryTx(tx), nil
}

// beginOn opens a translating transaction on a pinned connection.
func (s *Store) beginOn(ctx context.Context, conn *pgxpool.Conn) (pgx.Tx, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, translateDriverError(err)
	}
	return newBoundaryTx(tx), nil
}

// beginWith opens a translating transaction with explicit options.
func (s *Store) beginWith(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	tx, err := s.pool.BeginTx(ctx, options)
	if err != nil {
		return nil, translateDriverError(err)
	}
	return newBoundaryTx(tx), nil
}
