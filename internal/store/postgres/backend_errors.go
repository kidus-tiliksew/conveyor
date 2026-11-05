package postgres

import (
	"context"
	"errors"
	"fmt"
	"log"
	"reflect"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Driver conditions become store errors before they leave the backend. No
// pgx or pgconn type, SQLSTATE, driver message, or SQL text crosses the store
// boundary (component-persistence; DEC-38).
//
// boundaryDB wraps the pool for direct reads and writes, and boundaryTx wraps
// every transaction the store opens, so query, row-scan, and row-iteration
// errors are translated where the driver returns them. inTx also translates
// begin and commit errors and any callback error that still carries a driver
// type. Callback errors without one, such as domain sentinels and application
// errors, pass unchanged.

// uniqueConstraintErrors maps the unique constraints whose violation is a
// domain conflict. Callers that report an entity ID wrap the sentinel again.
var uniqueConstraintErrors = map[string]error{
	"jobs_pkey":                                 store.ErrDispatchJobConflict,
	"reference_documents_live_name_idx":         store.ErrReferenceDocumentNameConflict,
	"tasks_open_repo_branch_idx":                store.ErrTaskBranchConflict,
	"tasks_supersedes_unique":                   store.ErrTaskAlreadyStartedOver,
	"task_start_overs_workspace_id_task_id_key": store.ErrTaskAlreadyStartedOver,
	"requirements_workspace_id_slug_key":        store.ErrRequirementSlugConflict,
	"system_designs_pkey":                       store.ErrSystemDesignIDConflict,
	"system_designs_workspace_id_slug_key":      store.ErrSystemDesignSlugConflict,
	"decisions_pkey":                            store.ErrDecisionIDConflict,
	"decisions_confirmed_supersedes_key":        store.ErrDecisionSupersessionConflict,
	// A case- and space-normalized workspace name collision is the same
	// generic creation conflict as an ID collision (DEC-75(7)).
	"workspaces_name_lower_unique": store.ErrWorkspaceConflict,
}

// errBackendOperation is the generic PostgreSQL failure. It wraps
// store.ErrBackendOperation and carries nothing from the driver.
var errBackendOperation = fmt.Errorf("PostgreSQL %w", store.ErrBackendOperation)

// noRowsError is the store-owned form of the driver's no-row result. It
// matches store.ErrNotFound for callers outside the backend and still
// matches pgx.ErrNoRows for the package's own not-found mapping.
type noRowsError struct{}

func (noRowsError) Error() string { return store.ErrNotFound.Error() }

func (noRowsError) Is(target error) bool {
	return target == store.ErrNotFound || target == pgx.ErrNoRows
}

var errNoRows error = noRowsError{}

// translateBackendConflict translates an error that may mix application and
// driver failures, such as a transaction callback's result. An error that
// carries no driver type is returned unchanged.
func translateBackendConflict(err error) error {
	if err == nil || !carriesDriverError(err) {
		return err
	}
	return translateDriverError(err)
}

// translateDriverError translates an error returned by a driver call. Every
// condition without a store meaning becomes the generic backend error.
func translateDriverError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(noRowsError); ok {
		return err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return errNoRows
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			if sentinel, ok := uniqueConstraintErrors[pgErr.ConstraintName]; ok {
				return sentinel
			}
		case "40P01", "55P03":
			// Deadlock and lock-wait refusals: retry the whole command, as
			// SingleStore reports 1213 and 1205.
			return store.ErrRetryable
		}
	}
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	}
	// The driver detail stays in the server log; callers receive only the
	// store error.
	logDriverError(err)
	return errBackendOperation
}

// logDriverError writes the one server log line for an untranslated driver
// condition, in the form SingleStore's logDriverError uses. SQLSTATE is
// PostgreSQL's error code, so the line has no separate error number. The
// message is redacted and bounded; no SQL text or argument is added to it
// (component-persistence; req-security-boundaries REQ-2).
func logDriverError(err error) {
	state, message := driverDiagnosis(err)
	log.Printf("postgres: database operation failed: operation=%s sqlstate=%s message=%q", store.BackendOperation(storePackage), state, store.DriverLogMessage(message))
}

// storePackage is this package's import path, as runtime function names
// spell it.
var storePackage = reflect.TypeOf((*Store)(nil)).Elem().PkgPath()

// sqlStatePattern is the form of a SQLSTATE. A driver error's Code is an
// unchecked string, and only a real SQLSTATE reaches the unredacted field.
var sqlStatePattern = regexp.MustCompile(`^[0-9A-Z]{5}$`)

// driverDiagnosis selects the SQLSTATE and message to log. From a server error
// it takes only the primary message, never the detail, hint, query, or context
// fields, which can hold row values and SQL text.
func driverDiagnosis(err error) (state, message string) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if sqlStatePattern.MatchString(pgErr.Code) {
			state = pgErr.Code
		}
		return state, pgErr.Message
	}
	return "", withoutQuotedQueryValues(err.Error())
}

var (
	// quotedQueryValues matches the pgx client text that repeats a bound
	// argument (`failed to encode args[0]: unable to encode "value" into ...`)
	// or the SQL of a batched statement (`error building query SELECT ...`).
	quotedQueryValues = regexp.MustCompile(`(?s)(?:error building query |(?:failed|unable) to encode ).*`)
	argumentPosition  = regexp.MustCompile(`args\[\d+\]`)
)

// withoutQuotedQueryValues replaces the text quotedQueryValues matches with a
// fixed statement that keeps only the argument's position.
func withoutQuotedQueryValues(message string) string {
	found := quotedQueryValues.FindStringIndex(message)
	if found == nil {
		return message
	}
	omitted := "failed to encode query argument"
	if position := argumentPosition.FindString(message[found[0]:]); position != "" {
		omitted += " " + position
	}
	return message[:found[0]] + omitted + ": value omitted"
}

// carriesDriverError reports whether any error in the chain is declared in
// a pgx package, or is one of pgx's transaction sentinels.
func carriesDriverError(err error) bool {
	if errors.Is(err, pgx.ErrTxClosed) || errors.Is(err, pgx.ErrTxCommitRollback) {
		return true
	}
	pending := []error{err}
	for len(pending) > 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if current == nil {
			continue
		}
		if driverType(reflect.TypeOf(current)) {
			return true
		}
		switch unwrapped := current.(type) {
		case interface{ Unwrap() []error }:
			pending = append(pending, unwrapped.Unwrap()...)
		case interface{ Unwrap() error }:
			pending = append(pending, unwrapped.Unwrap())
		}
	}
	return false
}

func driverType(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return strings.HasPrefix(t.PkgPath(), "github.com/jackc/")
}

// boundaryQuerier is the subset of pgx query methods both the pool and a
// transaction provide.
type boundaryQuerier interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// boundaryDB translates every driver error a direct pool query returns.
type boundaryDB struct {
	q boundaryQuerier
	// use, when set, runs before each statement; the store's reopens a
	// closed peer listener on the next request (component-persistence).
	use func()
}

func (b boundaryDB) used() {
	if b.use != nil {
		b.use()
	}
}

func (b boundaryDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	b.used()
	tag, err := b.q.Exec(ctx, sql, args...)
	return tag, translateDriverError(err)
}

func (b boundaryDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	b.used()
	rows, err := b.q.Query(ctx, sql, args...)
	if err != nil {
		if rows != nil {
			rows.Close()
		}
		return nil, translateDriverError(err)
	}
	return boundaryRows{rows}, nil
}

func (b boundaryDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	b.used()
	return boundaryRow{b.q.QueryRow(ctx, sql, args...)}
}

// boundaryTx is a transaction whose query, scan, iteration, and commit
// errors are translated. Every other method is the driver's.
type boundaryTx struct{ pgx.Tx }

func newBoundaryTx(tx pgx.Tx) pgx.Tx {
	if tx == nil {
		return nil
	}
	if _, ok := tx.(boundaryTx); ok {
		return tx
	}
	return boundaryTx{tx}
}

func (b boundaryTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return boundaryDB{q: b.Tx}.Exec(ctx, sql, args...)
}

func (b boundaryTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return boundaryDB{q: b.Tx}.Query(ctx, sql, args...)
}

func (b boundaryTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return boundaryDB{q: b.Tx}.QueryRow(ctx, sql, args...)
}

func (b boundaryTx) Commit(ctx context.Context) error {
	return translateDriverError(b.Tx.Commit(ctx))
}

type boundaryRow struct{ row pgx.Row }

func (r boundaryRow) Scan(dest ...any) error {
	return translateDriverError(r.row.Scan(dest...))
}

type boundaryRows struct{ pgx.Rows }

func (r boundaryRows) Scan(dest ...any) error {
	return translateDriverError(r.Rows.Scan(dest...))
}

func (r boundaryRows) Err() error {
	return translateDriverError(r.Rows.Err())
}
