package s2log

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/jobindex"
)

// JobIndexSchema is the job index's DDL: one row per job stream, written in
// the transaction of each job append (component-durable-queue, The job
// index; req-quiet-idle-runtime AC-5.1). EnsureSchema runs it with the
// other log tables, and migration 0030 declares it again before its fill.
const JobIndexSchema = `
CREATE ROWSTORE TABLE IF NOT EXISTS event_log_job_index (
 workspace_id VARCHAR(255) NOT NULL, stream_id VARCHAR(768) NOT NULL,
 kind VARCHAR(255) NOT NULL, state VARCHAR(32) NOT NULL, generation INT NOT NULL, version BIGINT NOT NULL,
 next_at DATETIME(6) NULL, claimed_at DATETIME(6) NULL,
 PRIMARY KEY (workspace_id, stream_id), KEY event_log_job_index_unfinished (workspace_id, state),
 SHARD KEY (workspace_id)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;`

const jobIndexColumns = `workspace_id, stream_id, kind, state, generation, version, next_at, claimed_at`

// upsertJobIndex never moves a row back to an older version. version is
// assigned last, so every earlier comparison sees the stored version.
const upsertJobIndex = `
ON DUPLICATE KEY UPDATE
 kind = IF(event_log_job_index.version <= VALUES(version), VALUES(kind), event_log_job_index.kind),
 state = IF(event_log_job_index.version <= VALUES(version), VALUES(state), event_log_job_index.state),
 generation = IF(event_log_job_index.version <= VALUES(version), VALUES(generation), event_log_job_index.generation),
 next_at = IF(event_log_job_index.version <= VALUES(version), VALUES(next_at), event_log_job_index.next_at),
 claimed_at = IF(event_log_job_index.version <= VALUES(version), VALUES(claimed_at), event_log_job_index.claimed_at),
 version = IF(event_log_job_index.version <= VALUES(version), VALUES(version), event_log_job_index.version)`

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
		return fmt.Errorf("s2log: job index %s: %w", stream, err)
	}
	return putJobIndexRows(ctx, tx, []jobindex.Row{row})
}

func readStream(ctx context.Context, exec Executor, workspace string, stream eventlog.StreamID) ([]eventlog.Event, error) {
	rows, err := exec.QueryContext(ctx, `
SELECT `+selectColumns+`
FROM event_log
WHERE workspace_id = ? AND stream_id = ?
ORDER BY version`, workspace, string(stream))
	if err != nil {
		return nil, fmt.Errorf("s2log: read stream: %w", err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

func putJobIndexRows(ctx context.Context, exec Executor, rows []jobindex.Row) error {
	if len(rows) == 0 {
		return nil
	}
	values := make([]string, 0, len(rows))
	args := make([]any, 0, len(rows)*8)
	for _, row := range rows {
		values = append(values, "(?, ?, ?, ?, ?, ?, ?, ?)")
		args = append(args, row.Workspace, string(row.Stream), row.Kind, string(row.State), row.Generation, int64(row.Version),
			nullTime(row.NextAt), nullTime(row.ClaimedAt))
	}
	if _, err := exec.ExecContext(ctx, `
INSERT INTO event_log_job_index (`+jobIndexColumns+`)
VALUES `+strings.Join(values, ", ")+upsertJobIndex, args...); err != nil {
		return fmt.Errorf("s2log: write job index: %w", err)
	}
	return nil
}

func nullTime(t time.Time) sql.NullTime {
	return sql.NullTime{Time: t, Valid: !t.IsZero()}
}

func readJobIndexRow(ctx context.Context, exec Executor, workspace string, stream eventlog.StreamID) (jobindex.Row, bool, error) {
	rows, err := exec.QueryContext(ctx, `SELECT `+jobIndexColumns+` FROM event_log_job_index WHERE workspace_id = ? AND stream_id = ?`, workspace, string(stream))
	if err != nil {
		return jobindex.Row{}, false, fmt.Errorf("s2log: read job index: %w", err)
	}
	defer rows.Close()
	out, err := scanJobIndexRows(rows)
	if err != nil || len(out) == 0 {
		return jobindex.Row{}, false, err
	}
	return out[0], true, nil
}

func scanJobIndexRows(rows *sql.Rows) ([]jobindex.Row, error) {
	var out []jobindex.Row
	for rows.Next() {
		var row jobindex.Row
		var stream, state string
		var version int64
		var nextAt, claimedAt sql.NullTime
		if err := rows.Scan(&row.Workspace, &stream, &row.Kind, &state, &row.Generation, &version, &nextAt, &claimedAt); err != nil {
			return nil, fmt.Errorf("s2log: scan job index: %w", err)
		}
		row.Stream = eventlog.StreamID(stream)
		row.State = jobindex.State(state)
		row.Version = eventlog.Version(version)
		if nextAt.Valid {
			row.NextAt = nextAt.Time.UTC()
		}
		if claimedAt.Valid {
			row.ClaimedAt = claimedAt.Time.UTC()
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("s2log: job index rows: %w", err)
	}
	return out, nil
}

// UnfinishedJobs implements jobindex.Reader. The position is read before
// the rows, so every event at or below it is reflected in them; the
// runtime skips events a row already reflects.
func (s *Store) UnfinishedJobs(ctx context.Context, workspace string) (eventlog.Position, []jobindex.Row, error) {
	exec := s.executor(ctx)
	var position int64
	err := exec.QueryRowContext(ctx, `SELECT position FROM event_log_positions WHERE workspace_id = ?`, workspace).Scan(&position)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, nil, fmt.Errorf("s2log: read position: %w", err)
	}
	rows, err := exec.QueryContext(ctx, `
SELECT `+jobIndexColumns+`
FROM event_log_job_index
WHERE workspace_id = ? AND state IN ('available', 'scheduled', 'running')
ORDER BY stream_id`, workspace)
	if err != nil {
		return 0, nil, fmt.Errorf("s2log: read unfinished jobs: %w", err)
	}
	defer rows.Close()
	out, err := scanJobIndexRows(rows)
	return eventlog.Position(position), out, err
}

// JobIndexRow implements jobindex.Reader.
func (s *Store) JobIndexRow(ctx context.Context, workspace string, stream eventlog.StreamID) (jobindex.Row, bool, error) {
	return readJobIndexRow(ctx, s.executor(ctx), workspace, stream)
}

// ReadGeneration implements jobindex.Reader. The first statement finds the
// last job.enqueued at or below version; the second returns only the events
// from there through version.
func (s *Store) ReadGeneration(ctx context.Context, workspace string, stream eventlog.StreamID, version eventlog.Version) ([]eventlog.Event, error) {
	exec := s.executor(ctx)
	from := int64(1)
	err := exec.QueryRowContext(ctx, `
SELECT version
FROM event_log
WHERE workspace_id = ? AND stream_id = ? AND version <= ? AND kind = ?
ORDER BY version DESC
LIMIT 1`, workspace, string(stream), int64(version), jobindex.KindEnqueued).Scan(&from)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("s2log: find current generation: %w", err)
	}
	rows, err := exec.QueryContext(ctx, `
SELECT `+selectColumns+`
FROM event_log
WHERE workspace_id = ? AND stream_id = ? AND version >= ? AND version <= ?
ORDER BY version`, workspace, string(stream), from, int64(version))
	if err != nil {
		return nil, fmt.Errorf("s2log: read current generation: %w", err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

// RepairJobIndex implements jobindex.Reader. The database compares each job
// stream's head in event_log_streams with its row's version, and only the
// mismatched stream IDs reach the daemon. Each is rebuilt under the
// workspace and stream locks that appends take, in the same order.
func (s *Store) RepairJobIndex(ctx context.Context, workspace string) (int, error) {
	rows, err := s.executor(ctx).QueryContext(ctx, `
SELECT s.stream_id
FROM event_log_streams s
LEFT JOIN event_log_job_index i ON i.workspace_id = s.workspace_id AND i.stream_id = s.stream_id
WHERE s.workspace_id = ? AND s.stream_id LIKE 'job/%' AND s.version > 0
  AND (i.version IS NULL OR i.version <> s.version)
ORDER BY s.stream_id`, workspace)
	if err != nil {
		return 0, fmt.Errorf("s2log: find stale job index rows: %w", err)
	}
	var streams []eventlog.StreamID
	for rows.Next() {
		var stream string
		if err := rows.Scan(&stream); err != nil {
			rows.Close()
			return 0, fmt.Errorf("s2log: scan stale job index row: %w", err)
		}
		streams = append(streams, eventlog.StreamID(stream))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("s2log: stale job index rows: %w", err)
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
			return fmt.Errorf("s2log: repair job index %s: %w", stream, err)
		}
		return putJobIndexRows(ctx, tx, []jobindex.Row{jobindex.RowOf(job)})
	}
	if tx, ok := txFrom(ctx); ok {
		return run(tx)
	}
	tx, err := s.pool.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("s2log: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck -- commit below owns the outcome
	if err := run(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("s2log: commit: %w", err)
	}
	return nil
}

// fillBatch is how many rows one fill statement upserts.
const fillBatch = 200

// FillJobIndex folds every job stream of every workspace once and upserts
// its row, never moving a stored row back to an older version. Migration
// 0030 runs it; a failed fill leaves 0030 unrecorded, and the rerun at the
// next start writes the same rows. It reads event_log and rewrites no
// event. It reports the number of streams it folded.
func FillJobIndex(ctx context.Context, exec Executor) (int, error) {
	return fillJobIndex(ctx, exec, "")
}

// fillJobIndex fills every workspace, or only workspace when it is set; the
// integration test fills its own workspace on the shared test database.
func fillJobIndex(ctx context.Context, exec Executor, workspace string) (int, error) {
	rows, err := exec.QueryContext(ctx, `
SELECT `+selectColumns+`
FROM event_log
WHERE stream_id LIKE 'job/%' AND (? = '' OR workspace_id = ?)
ORDER BY workspace_id, stream_id, version`, workspace, workspace)
	if err != nil {
		return 0, fmt.Errorf("s2log: fill job index: %w", err)
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
			return 0, fmt.Errorf("s2log: fill job index: scan: %w", err)
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
			return 0, fmt.Errorf("s2log: fill job index: %w", err)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("s2log: fill job index: %w", err)
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
