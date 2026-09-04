package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// begin opens a transaction on the pool.
func (s *Store) begin(ctx context.Context) (pgx.Tx, error) {
	return s.pool.Begin(ctx)
}

// beginOn opens a transaction on a pinned connection.
func (s *Store) beginOn(ctx context.Context, conn *pgxpool.Conn) (pgx.Tx, error) {
	return conn.Begin(ctx)
}
