package pglog

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/jobindex"
)

// JobIndexSchema is the job index's DDL: one row per job stream, written in
// the transaction of each job append (component-durable-queue, The job
// index; req-quiet-idle-runtime AC-5.1). EnsureSchema runs it with the
// other log tables, and migration 155 declares it again before its fill.
const JobIndexSchema = `
CREATE TABLE IF NOT EXISTS event_log_job_index (
    workspace_id text NOT NULL,
    stream_id    text NOT NULL,
    kind         text NOT NULL,
    state        text NOT NULL,
    generation   integer NOT NULL,
    version      bigint NOT NULL,
    next_at      timestamptz,
    claimed_at   timestamptz,
    PRIMARY KEY (workspace_id, stream_id)
);
CREATE INDEX IF NOT EXISTS event_log_job_index_unfinished
    ON event_log_job_index (workspace_id, state)
    WHERE state IN ('available', 'scheduled', 'running');
`

const jobIndexColumns = `workspace_id, stream_id, kind, state, generation, version, next_at, claimed_at`

// upsertJobIndex never moves a row back to an older version.
const upsertJobIndex = `
ON CONFLICT (workspace_id, stream_id) DO UPDATE SET
    kind = EXCLUDED.kind, state = EXCLUDED.state, generation = EXCLUDED.generation,
    version = EXCLUDED.version, next_at = EXCLUDED.next_at, claimed_at = EXCLUDED.claimed_at
WHERE event_log_job_index.version <= EXCLUDED.version`

// writeJobIndex updates a job stream's row after an append inside tx. The
// caller holds the stream's head lock; observed is the head before the
// append and appended are the events it inserted.
func (s *Store) writeJobIndex(ctx context.Context, tx Executor, workspace string, stream eventlog.StreamID, observed eventlog.Version, appended []eventlog.Event) error {
	if !jobindex.IsJobStream(stream) {
		return nil
	}
	prev, ok, err := readJobIndexRow(ctx, tx, workspace, stream)
	if err != nil {
		return err
	}
	var prevRow *jobindex.Row
	if ok {
		prevRow = &prev
	}
	row, err := jobindex.Next(workspace, stream, prevRow, observed, appended, func() ([]eventlog.Event, error) {
		return readStream(ctx, tx, workspace, stream)
	})
	if err != nil {
		return fmt.Errorf("pglog: job index %s: %w", stream, err)
	}
	return putJobIndexRow(ctx, tx, row)
}

func readStream(ctx context.Context, exec Executor, workspace string, stream eventlog.StreamID) ([]eventlog.Event, error) {
	rows, err := exec.Query(ctx, `
SELECT `+selectColumns+`
FROM event_log
WHERE workspace_id = $1 AND stream_id = $2
ORDER BY version`, workspace, string(stream))
	if err != nil {
		return nil, fmt.Errorf("pglog: read stream: %w", err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

func putJobIndexRow(ctx context.Context, exec Executor, row jobindex.Row) error {
	if _, err := exec.Exec(ctx, `
INSERT INTO event_log_job_index (`+jobIndexColumns+`)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`+upsertJobIndex,
		row.Workspace, string(row.Stream), row.Kind, string(row.State), row.Generation, int64(row.Version),
		nullTime(row.NextAt), nullTime(row.ClaimedAt)); err != nil {
		return fmt.Errorf("pglog: write job index %s: %w", row.Stream, err)
	}
	return nil
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func readJobIndexRow(ctx context.Context, exec Executor, workspace string, stream eventlog.StreamID) (jobindex.Row, bool, error) {
	rows, err := exec.Query(ctx, `SELECT `+jobIndexColumns+` FROM event_log_job_index WHERE workspace_id = $1 AND stream_id = $2`, workspace, string(stream))
	if err != nil {
		return jobindex.Row{}, false, fmt.Errorf("pglog: read job index: %w", err)
	}
	defer rows.Close()
	out, err := scanJobIndexRows(rows)
	if err != nil || len(out) == 0 {
		return jobindex.Row{}, false, err
	}
	return out[0], true, nil
}

func scanJobIndexRows(rows pgx.Rows) ([]jobindex.Row, error) {
	var out []jobindex.Row
	for rows.Next() {
		var row jobindex.Row
		var stream, state string
		var version int64
		var nextAt, claimedAt *time.Time
		if err := rows.Scan(&row.Workspace, &stream, &row.Kind, &state, &row.Generation, &version, &nextAt, &claimedAt); err != nil {
			return nil, fmt.Errorf("pglog: scan job index: %w", err)
		}
		row.Stream = eventlog.StreamID(stream)
		row.State = jobindex.State(state)
		row.Version = eventlog.Version(version)
		if nextAt != nil {
			row.NextAt = nextAt.UTC()
		}
		if claimedAt != nil {
			row.ClaimedAt = claimedAt.UTC()
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pglog: job index rows: %w", err)
	}
	return out, nil
}

// UnfinishedJobs implements jobindex.Reader. Outside a caller's transaction
// the position and the rows are read in one repeatable-read, read-only
// transaction, so the rows reflect exactly the events at or below the
// position.
func (s *Store) UnfinishedJobs(ctx context.Context, workspace string) (eventlog.Position, []jobindex.Row, error) {
	if tx, ok := txFrom(ctx); ok {
		return unfinishedJobs(ctx, tx, workspace)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return 0, nil, fmt.Errorf("pglog: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck -- read-only
	position, rows, err := unfinishedJobs(ctx, tx, workspace)
	if err != nil {
		return 0, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, nil, fmt.Errorf("pglog: commit: %w", err)
	}
	return position, rows, nil
}

func unfinishedJobs(ctx context.Context, exec Executor, workspace string) (eventlog.Position, []jobindex.Row, error) {
	var position int64
	err := exec.QueryRow(ctx, `SELECT position FROM event_log_positions WHERE workspace_id = $1`, workspace).Scan(&position)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, fmt.Errorf("pglog: read position: %w", err)
	}
	rows, err := exec.Query(ctx, `
SELECT `+jobIndexColumns+`
FROM event_log_job_index
WHERE workspace_id = $1 AND state IN ('available', 'scheduled', 'running')
ORDER BY stream_id`, workspace)
	if err != nil {
		return 0, nil, fmt.Errorf("pglog: read unfinished jobs: %w", err)
	}
	defer rows.Close()
	out, err := scanJobIndexRows(rows)
	return eventlog.Position(position), out, err
}

// JobIndexRow implements jobindex.Reader.
func (s *Store) JobIndexRow(ctx context.Context, workspace string, stream eventlog.StreamID) (jobindex.Row, bool, error) {
	return readJobIndexRow(ctx, s.executor(ctx), workspace, stream)
}

// RepairJobIndex implements jobindex.Reader. The database compares each job
// stream's head in event_log_streams with its row's version, and only the
// mismatched stream IDs reach the daemon; no event is read for a stream
// whose row matches. Each mismatched stream is rebuilt under the workspace
// and stream locks that appends take, in the same order.
func (s *Store) RepairJobIndex(ctx context.Context, workspace string) (int, error) {
	rows, err := s.executor(ctx).Query(ctx, `
SELECT s.stream_id
FROM event_log_streams s
LEFT JOIN event_log_job_index i ON i.workspace_id = s.workspace_id AND i.stream_id = s.stream_id
WHERE s.workspace_id = $1 AND s.stream_id LIKE 'job/%' AND s.version > 0
  AND (i.version IS NULL OR i.version <> s.version)
ORDER BY s.stream_id`, workspace)
	if err != nil {
		return 0, fmt.Errorf("pglog: find stale job index rows: %w", err)
	}
	var streams []eventlog.StreamID
	for rows.Next() {
		var stream string
		if err := rows.Scan(&stream); err != nil {
			rows.Close()
			return 0, fmt.Errorf("pglog: scan stale job index row: %w", err)
		}
		streams = append(streams, eventlog.StreamID(stream))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("pglog: stale job index rows: %w", err)
	}
	for i, stream := range streams {
		if err := s.repairStream(ctx, workspace, stream); err != nil {
			return i, err
		}
	}
	return len(streams), nil
}

func (s *Store) repairStream(ctx context.Context, workspace string, stream eventlog.StreamID) error {
	run := func(tx Executor) error {
		if _, err := s.lockWorkspace(ctx, tx, workspace); err != nil {
			return err
		}
		if _, err := s.lockStream(ctx, tx, workspace, stream); err != nil {
			return err
		}
		events, err := readStream(ctx, tx, workspace, stream)
		if err != nil {
			return err
		}
		job, err := jobindex.Fold(workspace, stream, events)
		if err != nil {
			return fmt.Errorf("pglog: repair job index %s: %w", stream, err)
		}
		return putJobIndexRow(ctx, tx, jobindex.RowOf(job))
	}
	if tx, ok := txFrom(ctx); ok {
		return run(tx)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pglog: begin: %w", err)
	}
	if err := run(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("pglog: commit: %w", err)
	}
	return nil
}

// fillBatch is how many rows one fill statement upserts.
const fillBatch = 1000

// FillJobIndex folds every job stream of every workspace once and upserts
// its row, never moving a stored row back to an older version. Migration
// 155 runs it inside the migration transaction; a rerun writes the same
// rows. It reads event_log and rewrites no event. It reports the number of
// streams it folded.
func FillJobIndex(ctx context.Context, exec Executor) (int, error) {
	rows, err := exec.Query(ctx, `
SELECT `+selectColumns+`
FROM event_log
WHERE stream_id LIKE 'job/%'
ORDER BY workspace_id, stream_id, version`)
	if err != nil {
		return 0, fmt.Errorf("pglog: fill job index: %w", err)
	}
	var out []jobindex.Row
	var job jobindex.Job
	open := false
	for rows.Next() {
		var e eventlog.Event
		var stream string
		var position, version int64
		var payload []byte
		if err := rows.Scan(&e.Workspace, &position, &stream, &version, &e.Kind, &e.ActorID, &e.ActorRole, &payload, &e.At); err != nil {
			rows.Close()
			return 0, fmt.Errorf("pglog: fill job index: scan: %w", err)
		}
		e.Stream, e.Position, e.Version, e.Payload, e.At = eventlog.StreamID(stream), eventlog.Position(position), eventlog.Version(version), payload, e.At.UTC()
		if !open || job.Workspace != e.Workspace || job.Stream != e.Stream {
			if open {
				out = append(out, jobindex.RowOf(job))
			}
			job, open = jobindex.NewJob(e.Workspace, e.Stream), true
		}
		if err := job.Apply(e); err != nil {
			rows.Close()
			return 0, fmt.Errorf("pglog: fill job index: %w", err)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("pglog: fill job index: %w", err)
	}
	if open {
		out = append(out, jobindex.RowOf(job))
	}
	for start := 0; start < len(out); start += fillBatch {
		end := min(start+fillBatch, len(out))
		if err := putJobIndexRows(ctx, exec, out[start:end]); err != nil {
			return 0, err
		}
	}
	return len(out), nil
}

func putJobIndexRows(ctx context.Context, exec Executor, rows []jobindex.Row) error {
	n := len(rows)
	workspaces, streams, kinds, states := make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	generations, versions := make([]int32, n), make([]int64, n)
	nextAts, claimedAts := make([]*time.Time, n), make([]*time.Time, n)
	for i, row := range rows {
		workspaces[i], streams[i], kinds[i], states[i] = row.Workspace, string(row.Stream), row.Kind, string(row.State)
		generations[i], versions[i] = int32(row.Generation), int64(row.Version)
		nextAts[i], claimedAts[i] = nullTime(row.NextAt), nullTime(row.ClaimedAt)
	}
	if _, err := exec.Exec(ctx, `
INSERT INTO event_log_job_index (`+jobIndexColumns+`)
SELECT * FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::integer[], $6::bigint[], $7::timestamptz[], $8::timestamptz[])`+upsertJobIndex,
		workspaces, streams, kinds, states, generations, versions, nextAts, claimedAts); err != nil {
		return fmt.Errorf("pglog: fill job index: write: %w", err)
	}
	return nil
}
