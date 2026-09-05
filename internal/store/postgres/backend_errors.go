package postgres

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Driver conditions become store errors before the transaction returns to a
// caller (component-persistence; DEC-36).
func translateBackendConflict(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return err
	}
	switch pgErr.ConstraintName {
	case "jobs_pkey":
		return store.ErrDispatchJobConflict
	case "reference_documents_live_name_idx":
		return store.ErrReferenceDocumentNameConflict
	default:
		return err
	}
}
