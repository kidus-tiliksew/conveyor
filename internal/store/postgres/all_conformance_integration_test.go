package postgres

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// conformanceSchema is the one migrated schema the conformance run shares.
// Applying every migration costs about half a second per fixture locally and
// several seconds on a slow CI runner, and RunAll asks for 86 fixtures, so the
// schema is migrated once per test binary and every table but the migration
// ledger is truncated before each fixture instead. The conformance subtests
// run serially, and the other integration tests keep their own schemas.
var conformanceSchema struct {
	once   sync.Once
	err    error
	admin  *pgxpool.Pool
	pool   *pgxpool.Pool
	name   string
	tables string
	org    string
	store  *Store
}

func TestMain(m *testing.M) {
	code := m.Run()
	if conformanceSchema.admin != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if conformanceSchema.pool != nil {
			conformanceSchema.pool.Close()
		}
		if _, err := conformanceSchema.admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{conformanceSchema.name}.Sanitize()+" CASCADE"); err != nil {
			fmt.Fprintf(os.Stderr, "drop conformance schema %s: %v\n", conformanceSchema.name, err)
			if code == 0 {
				code = 1
			}
		}
		cancel()
		conformanceSchema.admin.Close()
	}
	os.Exit(code)
}

func openConformanceSchema(databaseURL string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return err
	}
	name := "conformance_" + strings.ReplaceAll(core.NewTaskID(), "-", "_")
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{name}.Sanitize()); err != nil {
		admin.Close()
		return err
	}
	conformanceSchema.admin = admin
	conformanceSchema.name = name
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return err
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = name
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return err
	}
	conformanceSchema.pool = pool
	if err = migrateControlPlaneToVersion(ctx, pool, 0); err != nil {
		return fmt.Errorf("migrate conformance schema: %w", err)
	}
	// orgs is excluded: migration 081 seeds the singleton deployment row and a
	// trigger forbids deleting or re-keying it, so the reset restores the seeded
	// row's other columns instead of truncating the table.
	rows, err := admin.Query(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema=$1 AND table_type='BASE TABLE' AND table_name NOT IN ('conveyor_schema_migrations','orgs') ORDER BY table_name`, name)
	if err != nil {
		return err
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var table string
		if err = rows.Scan(&table); err != nil {
			return err
		}
		tables = append(tables, pgx.Identifier{name, table}.Sanitize())
	}
	if err = rows.Err(); err != nil {
		return err
	}
	// Truncation must reproduce a freshly migrated schema, so refuse a
	// migration set that seeds rows into an empty database.
	var seeded []string
	for _, table := range tables {
		var count int
		if err = admin.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			seeded = append(seeded, fmt.Sprintf("%s=%d", table, count))
		}
	}
	if len(seeded) > 0 {
		return fmt.Errorf("fresh schema holds seeded rows: %s", strings.Join(seeded, ", "))
	}
	conformanceSchema.tables = strings.Join(tables, ", ")
	var orgCount int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM orgs`).Scan(&orgCount); err != nil {
		return err
	}
	if orgCount != 1 {
		return fmt.Errorf("fresh schema holds %d organization rows, want the one seeded by migration 081", orgCount)
	}
	if err = pool.QueryRow(ctx, `SELECT row_to_json(orgs)::text FROM orgs`).Scan(&conformanceSchema.org); err != nil {
		return err
	}
	conformanceSchema.store = newStore(pool)
	return nil
}

// resetConformanceSchema empties every table but the ledger and the singleton
// organization, whose seeded columns it restores.
func resetConformanceSchema(ctx context.Context) error {
	if _, err := conformanceSchema.admin.Exec(ctx, "TRUNCATE "+conformanceSchema.tables+" RESTART IDENTITY CASCADE"); err != nil {
		return err
	}
	_, err := conformanceSchema.pool.Exec(ctx, `UPDATE orgs SET name = seeded.name, created_at = seeded.created_at FROM json_populate_record(NULL::orgs, $1::json) AS seeded`, conformanceSchema.org)
	return err
}

// conformanceStore returns the shared store with every table empty and no
// forge encryption key configured.
func conformanceStore(t *testing.T) *Store {
	t.Helper()
	databaseURL := integrationDatabaseURL(t)
	conformanceSchema.once.Do(func() { conformanceSchema.err = openConformanceSchema(databaseURL) })
	if conformanceSchema.err != nil {
		t.Fatal(conformanceSchema.err)
	}
	started := time.Now()
	if err := resetConformanceSchema(t.Context()); err != nil {
		t.Fatalf("reset conformance schema: %v", err)
	}
	st := conformanceSchema.store
	st.ConfigureForgeTokenEncryptionKey(nil)
	t.Logf("fixture reset in %s", time.Since(started).Round(time.Millisecond))
	return st
}

func TestPostgresConformanceIntegration(t *testing.T) {
	integrationDatabaseURL(t)
	storetest.RunAll(t, storetest.Factory{
		ProductionCapable: true,
		Capabilities:      storetest.Capabilities{Identity: true, Membership: true, Tokens: true},
		New: func(t *testing.T, repos []config.Repo) storetest.Fixture {
			t.Helper()
			st := conformanceStore(t)
			workspace := "conformance-" + core.NewTaskID()
			ctx := store.WithWorkspace(t.Context(), workspace)
			cfg := &config.Config{Workspace: workspace, Repos: repos, Routing: config.Routing{Stages: map[string]config.StageRoute{
				"implement": {Timeout: time.Hour}, "review": {Execution: config.ExecutionMCP, Timeout: time.Hour},
			}}}
			if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			return storetest.Fixture{Backend: st, Context: ctx, Workspace: workspace, Config: cfg, SeedLegacy: postgresConformanceLegacySeed(st, ctx, workspace)}
		},
	})
}
