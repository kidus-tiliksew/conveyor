package singlestore

import (
	"context"
	"database/sql"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/s2log"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
)

// backendRow, backendRows, and backendExec are the translating paths for a
// statement outside withTx. Every error they hand back, including a later
// Scan, row iteration, or RowsAffected error, has crossed
// translateBackendConflict, so no driver type or message leaves a store method
// (component-persistence; DEC-38). A workspace-scoped document read goes
// through documentRow and its siblings, which add the workspace check.
// TestPackageQueriesUseTranslatingPaths keeps direct statements behind them.
func backendRow(ctx context.Context, db s2log.Executor, query string, args ...any) documentScanner {
	return documentResult{documentScanner: db.QueryRowContext(ctx, query, args...)}
}

func backendRows(ctx context.Context, db s2log.Executor, query string, args ...any) (*documentResultSet, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, translateBackendConflict(err)
	}
	return &documentResultSet{rows}, nil
}

func backendExec(ctx context.Context, db s2log.Executor, query string, args ...any) (sql.Result, error) {
	result, err := db.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, translateBackendConflict(err)
	}
	return translatedResult{result}, nil
}

// translatedResult carries a statement's counts across the same boundary as
// the statement's own error.
type translatedResult struct{ sql.Result }

func (r translatedResult) LastInsertId() (int64, error) {
	id, err := r.Result.LastInsertId()
	return id, translateBackendConflict(err)
}

func (r translatedResult) RowsAffected() (int64, error) {
	affected, err := r.Result.RowsAffected()
	return affected, translateBackendConflict(err)
}

// loadJob folds a job's stream from the event log outside any transaction. The
// log driver returns driver errors unchanged, so this store method translates
// them (component-persistence; DEC-38).
func (s *Store) loadJob(ctx context.Context, ws string, stream eventlog.StreamID) (logqueue.Job, error) {
	job, err := logqueue.Load(ctx, s.log, ws, stream)
	return job, translateBackendConflict(err)
}
