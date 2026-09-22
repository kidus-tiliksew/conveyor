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
	"github.com/kidus-tiliksew/conveyor/internal/testsupport/vk10fixture"
)

func TestVK10PostgresScenarioIntegration(t *testing.T) {
	raw := os.Getenv("CONVEYOR_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("missing PostgreSQL evidence: CONVEYOR_TEST_DATABASE_URL is unset")
	}
	parsed, err := url.Parse(raw)
	if err != nil || !strings.HasSuffix(strings.TrimPrefix(parsed.Path, "/"), "_test") {
		t.Fatal("VK-10 PostgreSQL requires a disposable database ending in _test")
	}
	vk10fixture.Run(t, func(t *testing.T) store.Backend {
		admin, err := pgxpool.New(t.Context(), raw)
		if err != nil {
			t.Fatal(err)
		}
		schema := "vk10_" + strings.ReplaceAll(core.NewTaskID(), "-", "_")
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
		b, err := postgres.Open(t.Context(), u.String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(b.Close)
		return b
	})
}
