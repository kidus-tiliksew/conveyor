package postgres

import (
	"strings"
	"sync"
	"testing"
)

func TestStartupMigrationRejectsStoreNewerThanBinaryIntegration(t *testing.T) {
	store := newIdentityIntegrationStore(t, 0)
	if _, err := store.pool.Exec(t.Context(), `INSERT INTO conveyor_schema_migrations(version,name,checksum) VALUES(999,'999_future.sql','future')`); err != nil {
		t.Fatal(err)
	}
	err := Migrate(t.Context(), store.pool)
	if err == nil || !strings.Contains(err.Error(), "store schema version 999 is newer") || !strings.Contains(err.Error(), "install a Conveyor release") {
		t.Fatalf("error=%v", err)
	}
}

func TestConcurrentStartupMigrationConvergesIntegration(t *testing.T) {
	store := newIdentityIntegrationStore(t, 85)
	const migrationCallers = 8
	start := make(chan struct{})
	var wait sync.WaitGroup
	errors := make(chan error, migrationCallers)
	for range migrationCallers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			errors <- Migrate(t.Context(), store.pool)
		}()
	}
	close(start)
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := store.pool.QueryRow(t.Context(), `SELECT count(*) FROM conveyor_schema_migrations WHERE version=86`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("migration 086 rows=%d, want 1", count)
	}
}
