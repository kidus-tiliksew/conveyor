// Package pglog is the PostgreSQL eventlog driver. It is the reference
// durable driver and the only place in the log core that speaks Postgres.
//
// Serialization is two row locks taken in a fixed order: the workspace's
// position row, then the stream's head row. Holding the workspace row until
// commit means positions become visible in order, so a tailer that has seen
// position N can trust that every position below N is already committed.
package pglog

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
)

// Executor is the subset of pgx a driver call needs. Both *pgxpool.Pool and
// pgx.Tx satisfy it.
type Executor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Schema is the driver's DDL. Every statement is idempotent so EnsureSchema
// can run at every startup.
const Schema = `
CREATE TABLE IF NOT EXISTS event_log_positions (
    workspace_id text PRIMARY KEY,
    position     bigint NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS event_log_streams (
    workspace_id text NOT NULL,
    stream_id    text NOT NULL,
    version      bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (workspace_id, stream_id)
);
CREATE TABLE IF NOT EXISTS event_log (
    workspace_id    text NOT NULL,
    position        bigint NOT NULL,
    stream_id       text NOT NULL,
    version         bigint NOT NULL,
    kind            text NOT NULL,
    actor_id        text NOT NULL DEFAULT '',
    actor_role      text NOT NULL DEFAULT '',
    payload         jsonb NOT NULL DEFAULT '{}'::jsonb,
    at              timestamptz NOT NULL,
    PRIMARY KEY (workspace_id, position),
    UNIQUE (workspace_id, stream_id, version)
);
`

// EnsureSchema creates the driver's tables when they are missing.
func EnsureSchema(ctx context.Context, exec Executor) error {
	if _, err := exec.Exec(ctx, Schema); err != nil {
		return fmt.Errorf("pglog: ensure schema: %w", err)
	}
	return nil
}

type txKey struct{}

// WithTx makes every driver call on ctx run inside tx. The caller owns the
// transaction; the driver neither commits nor rolls it back. This is how the
// store enqueues a job atomically with the rows that demand it.
func WithTx(ctx context.Context, tx pgx.Tx) context.Context {
	return context.WithValue(ctx, txKey{}, tx)
}

func txFrom(ctx context.Context) (pgx.Tx, bool) {
	tx, ok := ctx.Value(txKey{}).(pgx.Tx)
	return tx, ok
}

// Store is an eventlog.Store on one pgx pool.
type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

var _ eventlog.Store = (*Store)(nil)

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, now: time.Now}
}

// WithClock replaces the timestamp source; tests use it for determinism.
func (s *Store) WithClock(now func() time.Time) *Store {
	s.now = now
	return s
}

func (s *Store) executor(ctx context.Context) Executor {
	if tx, ok := txFrom(ctx); ok {
		return tx
	}
	return s.pool
}

func (s *Store) Append(ctx context.Context, workspace string, stream eventlog.StreamID, expected eventlog.Version, events []eventlog.NewEvent) (eventlog.Version, error) {
	if err := eventlog.ValidateAppend(workspace, stream, events); err != nil {
		return 0, err
	}
	if tx, ok := txFrom(ctx); ok {
		return s.appendTx(ctx, tx, workspace, stream, expected, events)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("pglog: begin: %w", err)
	}
	head, err := s.appendTx(ctx, tx, workspace, stream, expected, events)
	if err != nil {
		_ = tx.Rollback(ctx)
		return head, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("pglog: commit: %w", err)
	}
	return head, nil
}

// lockWorkspace takes the workspace's position row lock inside exec, which
// must be a transaction, and returns the current position. Every append in
// the workspace queues behind it until the transaction ends. Lock order is
// always workspace first, then streams.
func (s *Store) lockWorkspace(ctx context.Context, exec Executor, workspace string) (eventlog.Position, error) {
	var position int64
	if err := exec.QueryRow(ctx, `
INSERT INTO event_log_positions (workspace_id, position) VALUES ($1, 0)
ON CONFLICT (workspace_id) DO UPDATE SET position = event_log_positions.position
RETURNING position`, workspace).Scan(&position); err != nil {
		return 0, fmt.Errorf("pglog: lock workspace position: %w", err)
	}
	return eventlog.Position(position), nil
}

// lockStream takes the stream's head row lock inside exec, which must be a
// transaction, and returns the current head. Call lockWorkspace first.
func (s *Store) lockStream(ctx context.Context, exec Executor, workspace string, stream eventlog.StreamID) (eventlog.Version, error) {
	var head int64
	if err := exec.QueryRow(ctx, `
INSERT INTO event_log_streams (workspace_id, stream_id, version) VALUES ($1, $2, 0)
ON CONFLICT (workspace_id, stream_id) DO UPDATE SET version = event_log_streams.version
RETURNING version`, workspace, string(stream)).Scan(&head); err != nil {
		return 0, fmt.Errorf("pglog: lock stream head: %w", err)
	}
	return eventlog.Version(head), nil
}

func (s *Store) appendTx(ctx context.Context, tx Executor, workspace string, stream eventlog.StreamID, expected eventlog.Version, events []eventlog.NewEvent) (eventlog.Version, error) {
	// Lock order: workspace position row, then stream head row. The upsert
	// with a no-op update both creates the row on first use and takes the
	// row lock, so concurrent appends queue here.
	wsPosition, err := s.lockWorkspace(ctx, tx, workspace)
	if err != nil {
		return 0, err
	}
	position := int64(wsPosition)
	current, err := s.lockStream(ctx, tx, workspace, stream)
	if err != nil {
		return 0, err
	}
	head := int64(current)
	if expected != eventlog.ExpectAny && expected != current {
		return current, &eventlog.VersionConflictError{Workspace: workspace, Stream: stream, Expected: expected, Actual: current}
	}
	now := s.now().UTC()
	for i, incoming := range events {
		incoming = eventlog.Normalize(incoming, now)
		head++
		position++
		if _, err := tx.Exec(ctx, `
INSERT INTO event_log (workspace_id, position, stream_id, version, kind, actor_id, actor_role, payload, at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			workspace, position, string(stream), head, incoming.Kind, incoming.ActorID, incoming.ActorRole,
			[]byte(incoming.Payload), incoming.At); err != nil {
			return 0, fmt.Errorf("pglog: append event %d: %w", i, err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE event_log_streams SET version = $3 WHERE workspace_id = $1 AND stream_id = $2`, workspace, string(stream), head); err != nil {
		return 0, fmt.Errorf("pglog: advance stream head: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE event_log_positions SET position = $2 WHERE workspace_id = $1`, workspace, position); err != nil {
		return 0, fmt.Errorf("pglog: advance workspace position: %w", err)
	}
	return eventlog.Version(head), nil
}

const selectColumns = `workspace_id, position, stream_id, version, kind, actor_id, actor_role, payload, at`

func (s *Store) Read(ctx context.Context, workspace string, stream eventlog.StreamID, after eventlog.Version, limit int) ([]eventlog.Event, error) {
	rows, err := s.executor(ctx).Query(ctx, `
SELECT `+selectColumns+`
FROM event_log
WHERE workspace_id = $1 AND stream_id = $2 AND version > $3
ORDER BY version
LIMIT NULLIF($4, 0)`, workspace, string(stream), int64(after), limit)
	if err != nil {
		return nil, fmt.Errorf("pglog: read: %w", err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

func (s *Store) Head(ctx context.Context, workspace string, stream eventlog.StreamID) (eventlog.Version, error) {
	var head int64
	err := s.executor(ctx).QueryRow(ctx, `SELECT version FROM event_log_streams WHERE workspace_id = $1 AND stream_id = $2`, workspace, string(stream)).Scan(&head)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("pglog: head: %w", err)
	}
	return eventlog.Version(head), nil
}

func (s *Store) Tail(ctx context.Context, workspace string, after eventlog.Position, limit int) ([]eventlog.Event, error) {
	rows, err := s.executor(ctx).Query(ctx, `
SELECT `+selectColumns+`
FROM event_log
WHERE workspace_id = $1 AND position > $2
ORDER BY position
LIMIT NULLIF($3, 0)`, workspace, int64(after), limit)
	if err != nil {
		return nil, fmt.Errorf("pglog: tail: %w", err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

func scanEvents(rows pgx.Rows) ([]eventlog.Event, error) {
	var out []eventlog.Event
	for rows.Next() {
		var e eventlog.Event
		var stream string
		var position, version int64
		var payload []byte
		if err := rows.Scan(&e.Workspace, &position, &stream, &version, &e.Kind, &e.ActorID, &e.ActorRole, &payload, &e.At); err != nil {
			return nil, fmt.Errorf("pglog: scan: %w", err)
		}
		e.Stream = eventlog.StreamID(stream)
		e.Position = eventlog.Position(position)
		e.Version = eventlog.Version(version)
		e.Payload = payload
		e.At = e.At.UTC()
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pglog: rows: %w", err)
	}
	return out, nil
}
