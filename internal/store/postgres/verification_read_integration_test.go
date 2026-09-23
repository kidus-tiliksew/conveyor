package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestVerificationReadCorruptionIntegration(t *testing.T) {
	s := conformanceStore(t)
	ws := "verification-read-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), ws)
	cfg := &config.Config{Workspace: ws, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Timeout: time.Hour}, "review": {Execution: config.ExecutionMCP, Timeout: time.Hour}}}}
	if _, err := s.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	storetest.RunVerificationReadCorruption(t, storetest.Fixture{Backend: s, Context: ctx, Workspace: ws, Config: cfg}, func(ctx context.Context, id string, body []byte) error {
		// Simulate retained storage corruption below the application boundary.
		// The immutable guard correctly rejects normal updates, so this fixture
		// changes only its isolated database transaction's trigger mode.
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if _, err = tx.Exec(ctx, "SET LOCAL session_replication_role = replica"); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE verification_evidence SET body=$3 WHERE workspace_id=$1 AND id=$2`, ws, id, body); err != nil {
			return err
		}
		return tx.Commit(ctx)
	})
}
