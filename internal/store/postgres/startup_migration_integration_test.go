package postgres

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
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

func TestWorkerOwnershipMigrationPreservesLegacyOwnerlessIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 86)
	workspace := "worker-owner-migration-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), workspace)
	if _, err := st.BootstrapWorkspaceConfig(ctx, isolationConfig(workspace)); err != nil {
		t.Fatal(err)
	}
	credentialHash := "legacy-worker-hash-" + core.NewTaskID()
	if _, err := st.pool.Exec(ctx, `INSERT INTO workers(id,workspace_id,name,credential_hash,created_at) VALUES($1,$2,$3,$4,$5)`, "legacy-worker", workspace, "legacy", credentialHash, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, st.pool); err != nil {
		t.Fatal(err)
	}
	worker, err := st.AuthenticateWorker(ctx, credentialHash)
	if err != nil || worker.OwnerUserID != "" {
		t.Fatalf("legacy worker owner=%q err=%v", worker.OwnerUserID, err)
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
	if err := store.pool.QueryRow(t.Context(), `SELECT count(*) FROM conveyor_schema_migrations WHERE version=87`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("migration 087 rows=%d, want 1", count)
	}
}
