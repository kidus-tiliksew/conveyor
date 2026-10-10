package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func requireNoDriverType(t *testing.T, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		t.Fatalf("pgconn.PgError escaped the backend: %v", err)
	}
	if carriesDriverError(err) {
		t.Fatalf("a pgx-package error escaped the backend: %#v", err)
	}
}

func TestBackendConflictTranslation(t *testing.T) {
	for _, tc := range []struct {
		code, constraint string
		want             error
	}{
		{"23505", "jobs_pkey", store.ErrDispatchJobConflict},
		{"23505", "reference_documents_live_name_idx", store.ErrReferenceDocumentNameConflict},
		{"23505", "tasks_open_repo_branch_idx", store.ErrTaskBranchConflict},
		{"23505", "requirements_workspace_id_slug_key", store.ErrRequirementSlugConflict},
		{"23505", "system_designs_pkey", store.ErrSystemDesignIDConflict},
		{"23505", "system_designs_workspace_id_slug_key", store.ErrSystemDesignSlugConflict},
		{"23505", "decisions_pkey", store.ErrDecisionIDConflict},
		{"23505", "decisions_confirmed_supersedes_key", store.ErrDecisionSupersessionConflict},
		{"40P01", "", store.ErrRetryable},
		{"55P03", "", store.ErrRetryable},
		{"23505", "tasks_pkey", store.ErrBackendOperation},
		{"23503", "jobs_pkey", store.ErrBackendOperation},
		{"23514", "tasks_state_check", store.ErrBackendOperation},
		{"42P01", "", store.ErrBackendOperation},
	} {
		t.Run(tc.code+"/"+tc.constraint, func(t *testing.T) {
			driver := &pgconn.PgError{Code: tc.code, ConstraintName: tc.constraint, Message: "secret driver detail", Detail: "Key (email)=(owner@example.test) already exists."}
			for name, original := range map[string]error{"unwrapped": driver, "wrapped": fmt.Errorf("transaction: %w", driver), "joined": errors.Join(errors.New("callback"), driver)} {
				for translator, got := range map[string]error{"driver": translateDriverError(original), "mixed": translateBackendConflict(original)} {
					if !errors.Is(got, tc.want) {
						t.Fatalf("%s %s: got %v, want %v", name, translator, got, tc.want)
					}
					requireNoDriverType(t, got)
					for _, leaked := range []string{tc.code, "secret driver detail", "owner@example.test", "SQLSTATE"} {
						if strings.Contains(got.Error(), leaked) {
							t.Fatalf("%s %s: message %q leaks %q", name, translator, got, leaked)
						}
					}
				}
			}
		})
	}
	if translateBackendConflict(nil) != nil || translateDriverError(nil) != nil {
		t.Fatal("nil error changed")
	}
}

func TestUnmappedDriverErrorBecomesStoreError(t *testing.T) {
	generic := translateDriverError(errors.New("unable to encode 1099511627776 into binary format for int4"))
	if !errors.Is(generic, store.ErrBackendOperation) || generic.Error() != "PostgreSQL database operation failed" {
		t.Fatalf("driver-call failure=%v", generic)
	}
	requireNoDriverType(t, generic)
	commit := translateBackendConflict(fmt.Errorf("commit: %w", pgx.ErrTxCommitRollback))
	if !errors.Is(commit, store.ErrBackendOperation) || errors.Is(commit, pgx.ErrTxCommitRollback) {
		t.Fatalf("transaction sentinel=%v", commit)
	}
}

func TestNoRowsBecomesStoreNotFound(t *testing.T) {
	for _, original := range []error{pgx.ErrNoRows, fmt.Errorf("scan: %w", pgx.ErrNoRows)} {
		got := translateDriverError(original)
		if !errors.Is(got, store.ErrNotFound) || !errors.Is(got, pgx.ErrNoRows) {
			t.Fatalf("no-row translation=%v", got)
		}
		requireNoDriverType(t, got)
		if strings.Contains(got.Error(), "no rows in result set") {
			t.Fatalf("no-row message leaks the driver text: %q", got)
		}
		if mapped := notFound(got, "task %s", "task-1"); !errors.Is(mapped, store.ErrNotFound) || mapped.Error() != "task task-1: resource not found" {
			t.Fatalf("notFound over the translated no-row error=%v", mapped)
		}
		if translateBackendConflict(got) != got {
			t.Fatal("a translated no-row error was translated again")
		}
	}
}

func TestCancellationAndApplicationErrorsKeepTheirMeaning(t *testing.T) {
	for _, tc := range []struct {
		original error
		want     error
	}{
		{fmt.Errorf("query: %w", context.Canceled), context.Canceled},
		{fmt.Errorf("timeout: %w", context.DeadlineExceeded), context.DeadlineExceeded},
		{errors.Join(&pgconn.PgError{Code: "57014"}, context.Canceled), context.Canceled},
	} {
		got := translateDriverError(tc.original)
		if got != tc.want {
			t.Fatalf("translate %v = %v, want %v", tc.original, got, tc.want)
		}
	}
	application := fmt.Errorf("planning session %s is %s", "ps-1", "closed")
	if got := translateBackendConflict(application); got != application {
		t.Fatalf("application error replaced: %v", got)
	}
	domain := fmt.Errorf("%w: decision DEC-9 is dismissed", store.ErrDecisionSupersessionConflict)
	if got := translateBackendConflict(domain); got != domain {
		t.Fatalf("domain error replaced: %v", got)
	}
	cancelled := fmt.Errorf("callback: %w", context.Canceled)
	if got := translateBackendConflict(cancelled); got != cancelled {
		t.Fatalf("application cancellation replaced: %v", got)
	}
}

// fakeQuerier returns one driver error from every path.
type fakeQuerier struct{ err error }

func (f fakeQuerier) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, f.err
}

func (f fakeQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return fakeRows{err: f.err}, nil
}

func (f fakeQuerier) QueryRow(context.Context, string, ...any) pgx.Row { return fakeRow{f.err} }

type fakeRow struct{ err error }

func (r fakeRow) Scan(...any) error { return r.err }

type fakeRows struct {
	pgx.Rows
	err error
}

func (r fakeRows) Next() bool        { return false }
func (r fakeRows) Close()            {}
func (r fakeRows) Err() error        { return r.err }
func (r fakeRows) Scan(...any) error { return r.err }

type fakeTx struct {
	pgx.Tx
	err error
}

func (f fakeTx) Commit(context.Context) error { return f.err }

func TestBoundaryTranslatesEveryQueryPath(t *testing.T) {
	driver := fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: "23503", Message: "insert or update violates foreign key constraint"})
	db := boundaryDB{fakeQuerier{err: driver}}
	ctx := store.WithActor(t.Context(), store.SystemActor())
	_, execErr := db.Exec(ctx, "UPDATE")
	rows, queryErr := db.Query(ctx, "SELECT")
	if queryErr != nil {
		t.Fatalf("query returned %v before iteration", queryErr)
	}
	rowsErr, scanErr := rows.Err(), rows.Scan()
	rowErr := db.QueryRow(ctx, "SELECT").Scan()
	commitErr := newBoundaryTx(fakeTx{err: driver}).Commit(ctx)
	for name, err := range map[string]error{"exec": execErr, "rows.Err": rowsErr, "rows.Scan": scanErr, "row.Scan": rowErr, "commit": commitErr} {
		if !errors.Is(err, store.ErrBackendOperation) {
			t.Fatalf("%s=%v, want ErrBackendOperation", name, err)
		}
		requireNoDriverType(t, err)
	}
	if noRow := (boundaryDB{fakeQuerier{err: pgx.ErrNoRows}}).QueryRow(ctx, "SELECT").Scan(); !errors.Is(noRow, store.ErrNotFound) || !errors.Is(noRow, pgx.ErrNoRows) {
		t.Fatalf("row no-row=%v", noRow)
	}
	tx := newBoundaryTx(fakeTx{})
	if newBoundaryTx(tx) != tx {
		t.Fatal("a boundary transaction was wrapped twice")
	}
}

// TestPackageQueriesUseTranslatingBoundary keeps every direct query behind
// boundaryDB and every transaction behind boundaryTx (component-persistence).
func TestPackageQueriesUseTranslatingBoundary(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	rawQuery := regexp.MustCompile(`\.pool\.(Exec|Query|QueryRow|SendBatch|CopyFrom)\(|db\.New\([a-z]*\.?pool\)`)
	rawBegin := regexp.MustCompile(`s\.pool\.(Begin|BeginTx)\(`)
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for number, line := range strings.Split(string(body), "\n") {
			if rawQuery.MatchString(line) {
				t.Errorf("%s:%d queries the raw pool: %s", file, number+1, strings.TrimSpace(line))
			}
			if file != "tx.go" && rawBegin.MatchString(line) {
				t.Errorf("%s:%d begins a transaction outside tx.go: %s", file, number+1, strings.TrimSpace(line))
			}
		}
	}
}
