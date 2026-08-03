package postgres

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestNotFoundKeepsEntityFirstOperatorWordingAndClassification(t *testing.T) {
	err := notFound(pgx.ErrNoRows, "task %s", "task-123")
	if !errors.Is(err, store.ErrNotFound) || !strings.HasPrefix(err.Error(), "task task-123 not found") {
		t.Fatalf("not-found error=%q", err)
	}
}
