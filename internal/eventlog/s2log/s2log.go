// Package s2log implements the SingleStore event-log contract (DEC-38; component-durable-queue).
package s2log

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
)

// Executor is the database/sql operations a driver call needs. Both *sql.DB and
// *sql.Tx satisfy it.
type Executor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Schema is the driver's DDL. Every statement is idempotent so EnsureSchema
// can run at every startup.
const Schema = `
CREATE ROWSTORE TABLE IF NOT EXISTS event_log_positions (
 workspace_id VARCHAR(255) NOT NULL, position BIGINT NOT NULL DEFAULT 0,
 PRIMARY KEY (workspace_id), SHARD KEY (workspace_id)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
CREATE ROWSTORE TABLE IF NOT EXISTS event_log_streams (
 workspace_id VARCHAR(255) NOT NULL, stream_id VARCHAR(768) NOT NULL, version BIGINT NOT NULL DEFAULT 0,
 PRIMARY KEY (workspace_id, stream_id), SHARD KEY (workspace_id)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
CREATE ROWSTORE TABLE IF NOT EXISTS event_log (
 workspace_id VARCHAR(255) NOT NULL, position BIGINT NOT NULL,
 stream_id VARCHAR(768) NOT NULL, version BIGINT NOT NULL, kind VARCHAR(255) NOT NULL,
 actor_id VARCHAR(255) NOT NULL DEFAULT '', actor_role VARCHAR(255) NOT NULL DEFAULT '',
 payload JSON NOT NULL, at DATETIME(6) NOT NULL,
 PRIMARY KEY (workspace_id, position), UNIQUE KEY event_log_stream_version (workspace_id,stream_id,version),
 SHARD KEY (workspace_id)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;`

// EnsureSchema is called before numbered backend migrations. DDL commits implicitly.
func EnsureSchema(ctx context.Context, exec Executor) error {
	for _, statement := range strings.Split(Schema, ";") {
		if strings.TrimSpace(statement) == "" {
			continue
		}
		if _, err := exec.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("s2log: schema: %w", err)
		}
	}
	return nil
}

type txKey struct{}

// WithTx makes every driver call on ctx run inside tx. The caller owns the
// transaction; the driver neither commits nor rolls it back. This is how the
// store enqueues a job atomically with the rows that demand it.
func WithTx(ctx context.Context, tx *sql.Tx) context.Context {
	return context.WithValue(ctx, txKey{}, tx)
}

func txFrom(ctx context.Context) (*sql.Tx, bool) {
	tx, ok := ctx.Value(txKey{}).(*sql.Tx)
	return tx, ok
}

// Store is an eventlog.Store on one database pool.
type Store struct {
	pool *sql.DB
	now  func() time.Time
}

var _ eventlog.Store = (*Store)(nil)

func New(pool *sql.DB) *Store {
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
	tx, err := s.pool.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("s2log: begin: %w", err)
	}
	defer tx.Rollback()
	head, err := s.appendTx(ctx, tx, workspace, stream, expected, events)
	if err != nil {
		_ = tx.Rollback()
		return head, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("s2log: commit: %w", err)
	}
	return head, nil
}

// lockWorkspace takes the workspace's position row lock inside exec, which
// must be a transaction, and returns the current position. Every append in
// the workspace queues behind it until the transaction ends. Lock order is
// always workspace first, then streams.
func (s *Store) lockWorkspace(ctx context.Context, exec Executor, workspace string) (eventlog.Position, error) {
	if _, err := exec.ExecContext(ctx, `INSERT INTO event_log_positions (workspace_id,position) VALUES (?,0) ON DUPLICATE KEY UPDATE position=position`, workspace); err != nil {
		return 0, err
	}
	var position int64
	err := exec.QueryRowContext(ctx, `SELECT position FROM event_log_positions WHERE workspace_id=? FOR UPDATE`, workspace).Scan(&position)
	return eventlog.Position(position), err
}

func (s *Store) lockStream(ctx context.Context, exec Executor, workspace string, stream eventlog.StreamID) (eventlog.Version, error) {
	if _, err := exec.ExecContext(ctx, `INSERT INTO event_log_streams (workspace_id,stream_id,version) VALUES (?,?,0) ON DUPLICATE KEY UPDATE version=version`, workspace, string(stream)); err != nil {
		return 0, err
	}
	var head int64
	err := exec.QueryRowContext(ctx, `SELECT version FROM event_log_streams WHERE workspace_id=? AND stream_id=? FOR UPDATE`, workspace, string(stream)).Scan(&head)
	return eventlog.Version(head), err
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
		if _, err := tx.ExecContext(ctx, `
INSERT INTO event_log (workspace_id, position, stream_id, version, kind, actor_id, actor_role, payload, at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			workspace, position, string(stream), head, incoming.Kind, incoming.ActorID, incoming.ActorRole,
			[]byte(incoming.Payload), incoming.At); err != nil {
			return 0, fmt.Errorf("s2log: append event %d: %w", i, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE event_log_streams SET version = ? WHERE workspace_id = ? AND stream_id = ?`, head, workspace, string(stream)); err != nil {
		return 0, fmt.Errorf("s2log: advance stream head: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE event_log_positions SET position = ? WHERE workspace_id = ?`, position, workspace); err != nil {
		return 0, fmt.Errorf("s2log: advance workspace position: %w", err)
	}
	return eventlog.Version(head), nil
}

const selectColumns = `workspace_id, position, stream_id, version, kind, actor_id, actor_role, payload, at`

func (s *Store) Read(ctx context.Context, workspace string, stream eventlog.StreamID, after eventlog.Version, limit int) ([]eventlog.Event, error) {
	rows, err := s.executor(ctx).QueryContext(ctx, `
SELECT `+selectColumns+`
FROM event_log
WHERE workspace_id = ? AND stream_id = ? AND version > ?
ORDER BY version
LIMIT ?`, workspace, string(stream), int64(after), rowLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("s2log: read: %w", err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

func (s *Store) Head(ctx context.Context, workspace string, stream eventlog.StreamID) (eventlog.Version, error) {
	var head int64
	err := s.executor(ctx).QueryRowContext(ctx, `SELECT version FROM event_log_streams WHERE workspace_id = ? AND stream_id = ?`, workspace, string(stream)).Scan(&head)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("s2log: head: %w", err)
	}
	return eventlog.Version(head), nil
}

func (s *Store) Tail(ctx context.Context, workspace string, after eventlog.Position, limit int) ([]eventlog.Event, error) {
	rows, err := s.executor(ctx).QueryContext(ctx, `
SELECT `+selectColumns+`
FROM event_log
WHERE workspace_id = ? AND position > ?
ORDER BY position
LIMIT ?`, workspace, int64(after), rowLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("s2log: tail: %w", err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

func scanEvents(rows *sql.Rows) ([]eventlog.Event, error) {
	var out []eventlog.Event
	for rows.Next() {
		var e eventlog.Event
		var stream string
		var position, version int64
		var payload []byte
		if err := rows.Scan(&e.Workspace, &position, &stream, &version, &e.Kind, &e.ActorID, &e.ActorRole, &payload, &e.At); err != nil {
			return nil, fmt.Errorf("s2log: scan: %w", err)
		}
		e.Stream = eventlog.StreamID(stream)
		e.Position = eventlog.Position(position)
		e.Version = eventlog.Version(version)
		e.Payload = payload
		e.At = e.At.UTC()
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("s2log: rows: %w", err)
	}
	return out, nil
}

func rowLimit(limit int) int64 {
	if limit <= 0 {
		return 9223372036854775807
	}
	return int64(limit)
}
