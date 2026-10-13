package postgres

import (
	"errors"
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

// TestWorkerOwnershipMigrationPreservesLegacyOwnerlessIntegration upgrades a
// version-86 schema through migration 087. The ownerless legacy row keeps its
// nullable owner and stays listable, and its credential is refused at use
// (component-work-orders, Workers: Admission).
func TestWorkerOwnershipMigrationPreservesLegacyOwnerlessIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 86)
	workspace := "worker-owner-migration-" + core.NewTaskID()
	ctx := store.WithActor(store.WithWorkspace(t.Context(), workspace), store.SystemActor())
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
	workers, err := st.ListWorkers(ctx)
	if err != nil || len(workers) != 1 || workers[0].ID != "legacy-worker" || workers[0].OwnerUserID != "" {
		t.Fatalf("legacy worker listing=%+v err=%v", workers, err)
	}
	if _, err := st.AuthenticateWorker(ctx, credentialHash); !errors.Is(err, store.ErrWorkerUnauthorized) {
		t.Fatalf("ownerless legacy worker authenticated: %v", err)
	}
	if _, err := st.HeartbeatWorker(ctx, "legacy-worker", time.Now().UTC().Add(time.Minute), nil); !errors.Is(err, store.ErrWorkerUnauthorized) {
		t.Fatalf("ownerless legacy worker heartbeat: %v", err)
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
	// The concurrent starts began below the feature drop (task 261007-9d50e0)
	// and converge on one applied drop with the table gone.
	assertFeatureSchemaRetired(t, t.Context(), store.pool)
}
