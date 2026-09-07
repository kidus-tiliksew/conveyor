package singlestore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// sharedDatabase is the one isolated SingleStore database a test binary
// creates and migrates. Creating a distributed database costs seconds on
// SingleStore, so fixtures share it and reset its tables instead of creating
// one database each; every table but the migration ledger is emptied before
// a fixture is handed out. Tests in this package run serially.
var sharedDatabase struct {
	once   sync.Once
	err    error
	admin  *sql.DB
	cfg    *mysql.Config
	name   string
	tables []string
	store  *Store
}

func TestMain(m *testing.M) {
	code := m.Run()
	if sharedDatabase.admin != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if _, err := sharedDatabase.admin.ExecContext(ctx, "DROP DATABASE `"+sharedDatabase.name+"`"); err != nil {
			fmt.Fprintf(os.Stderr, "drop shared test database %s: %v\n", sharedDatabase.name, err)
			if code == 0 {
				code = 1
			}
		}
		cancel()
		if sharedDatabase.store != nil {
			sharedDatabase.store.Close()
		}
		sharedDatabase.admin.Close()
	}
	os.Exit(code)
}

func openSharedDatabase() error {
	raw := os.Getenv("CONVEYOR_TEST_SINGLESTORE_URL")
	cfg, err := connectionConfig(raw)
	if err != nil {
		return err
	}
	if !strings.HasSuffix(cfg.DBName, "_test") {
		return fmt.Errorf("SingleStore integration database must end in _test")
	}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return err
	}
	admin := sql.OpenDB(connector)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	name := fmt.Sprintf("conveyor_%d_test", time.Now().UnixNano())
	if _, err = admin.ExecContext(ctx, "CREATE DATABASE `"+name+"` PARTITIONS 2"); err != nil {
		admin.Close()
		return err
	}
	sharedDatabase.admin = admin
	sharedDatabase.name = name
	cfg.DBName = name
	sharedDatabase.cfg = cfg
	// One Store for the whole binary: Open migrates once, and its pool stays
	// warm so compiled query plans are reused across fixtures.
	st, err := Open(ctx, cfg.FormatDSN())
	if err != nil {
		return err
	}
	sharedDatabase.store = st
	rows, err := admin.QueryContext(ctx, "SELECT table_name FROM information_schema.tables WHERE table_schema=? AND table_name<>'conveyor_singlestore_migrations' ORDER BY table_name", name)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var table string
		if err = rows.Scan(&table); err != nil {
			return err
		}
		sharedDatabase.tables = append(sharedDatabase.tables, table)
	}
	return rows.Err()
}

// integrationStore returns the shared Store with every table empty and no
// forge encryption key configured. Tests that need to close a pool call
// openOwnedStore instead.
func integrationStore(t *testing.T) *Store {
	t.Helper()
	resetSharedDatabase(t)
	st := sharedDatabase.store
	st.ConfigureForgeTokenEncryptionKey(nil)
	return st
}

// openOwnedStore opens a private pool on the shared, emptied database.
func openOwnedStore(t *testing.T) *Store {
	t.Helper()
	resetSharedDatabase(t)
	st, err := Open(t.Context(), sharedDatabase.cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

func resetSharedDatabase(t *testing.T) {
	t.Helper()
	if os.Getenv("CONVEYOR_TEST_SINGLESTORE_URL") == "" {
		t.Skip("CONVEYOR_TEST_SINGLESTORE_URL is unset")
	}
	sharedDatabase.once.Do(func() { sharedDatabase.err = openSharedDatabase() })
	if sharedDatabase.err != nil {
		t.Fatal(sharedDatabase.err)
	}
	started := time.Now()
	// DELETE, not TRUNCATE: truncation is distributed DDL that costs hundreds
	// of milliseconds per table on SingleStore, while a compiled DELETE on an
	// empty rowstore table returns in a few milliseconds.
	for _, table := range sharedDatabase.tables {
		if _, err := sharedDatabase.admin.ExecContext(t.Context(), "DELETE FROM `"+sharedDatabase.name+"`.`"+table+"`"); err != nil {
			t.Fatalf("reset %s: %v", table, err)
		}
	}
	t.Logf("fixture reset %d tables in %s", len(sharedDatabase.tables), time.Since(started).Round(time.Millisecond))
}
func TestSingleStoreConformanceIntegration(t *testing.T) {
	storetest.RunAll(t, storetest.Factory{ProductionCapable: true,
		Capabilities: storetest.Capabilities{Identity: true, Membership: true, Tokens: true},
		New: func(t *testing.T, repos []config.Repo) storetest.Fixture {
			st := integrationStore(t)
			ws := "conformance-" + core.NewTaskID()
			ctx := store.WithWorkspace(t.Context(), ws)
			cfg := &config.Config{Workspace: ws, Repos: repos, Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Timeout: time.Hour}, "review": {Execution: config.ExecutionMCP, Timeout: time.Hour}}}}
			if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			return storetest.Fixture{Backend: st, Context: ctx, Workspace: ws, Config: cfg}
		}})
}
