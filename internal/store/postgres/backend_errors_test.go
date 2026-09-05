package postgres

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestBackendConflictTranslation(t *testing.T) {
	for _, tc := range []struct {
		code, constraint string
		want             error
	}{
		{"23505", "jobs_pkey", store.ErrDispatchJobConflict},
		{"23505", "reference_documents_live_name_idx", store.ErrReferenceDocumentNameConflict},
		{"23505", "tasks_pkey", nil},
		{"23503", "jobs_pkey", nil},
	} {
		t.Run(tc.code+"/"+tc.constraint, func(t *testing.T) {
			original := fmt.Errorf("transaction: %w", &pgconn.PgError{Code: tc.code, ConstraintName: tc.constraint})
			got := translateBackendConflict(original)
			if tc.want == nil {
				if got != original {
					t.Fatalf("unrelated error was replaced: %v", got)
				}
				return
			}
			if !errors.Is(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			var driver *pgconn.PgError
			if errors.As(got, &driver) {
				t.Fatal("driver error escaped the backend")
			}
		})
	}
	if translateBackendConflict(nil) != nil {
		t.Fatal("nil error changed")
	}
}
