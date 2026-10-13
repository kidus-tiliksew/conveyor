package dispatch_test

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres"
	"github.com/kidus-tiliksew/conveyor/internal/testsupport/stalemintfixture"
)

// Real-dispatch stage-mint cases on the volatile and PostgreSQL backends; the
// SingleStore run lives with that backend (component-work-orders; DEC-62).
func TestStaleMintScenarioMemory(t *testing.T) {
	stalemintfixture.Run(t, func(t *testing.T) store.Backend {
		b := store.NewVolatileBackend()
		t.Cleanup(b.Close)
		return b
	})
}

func TestStaleMintScenarioPostgresIntegration(t *testing.T) {
	raw := os.Getenv("CONVEYOR_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("missing PostgreSQL evidence: CONVEYOR_TEST_DATABASE_URL is unset")
	}
	parsed, err := url.Parse(raw)
	if err != nil || !strings.HasSuffix(strings.TrimPrefix(parsed.Path, "/"), "_test") {
		t.Fatal("stale-mint PostgreSQL requires a disposable database ending in _test")
	}
	stalemintfixture.Run(t, func(t *testing.T) store.Backend { return schemaBackend(t, raw) })
}

// schemaBackend opens PostgreSQL in a fresh schema dropped when the test ends.
func schemaBackend(t *testing.T, raw string) store.Backend {
	t.Helper()
	admin, err := pgxpool.New(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	schema := "stalemint_" + strings.ReplaceAll(core.NewTaskID(), "-", "_")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(t.Context(), "CREATE SCHEMA "+quoted); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		defer admin.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	b, err := postgres.Open(store.WithActor(t.Context(), store.SystemActor()), u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	return b
}
