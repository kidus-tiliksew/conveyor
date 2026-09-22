package singlestore

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
	s := integrationStore(t)
	ws := "verification-read-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), ws)
	cfg := &config.Config{Workspace: ws, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Timeout: time.Hour}, "review": {Execution: config.ExecutionMCP, Timeout: time.Hour}}}}
	if _, err := s.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	storetest.RunVerificationReadCorruption(t, storetest.Fixture{Backend: s, Context: ctx, Workspace: ws, Config: cfg}, func(ctx context.Context, id string, body []byte) error {
		_, err := s.db.ExecContext(ctx, `UPDATE verification_evidence SET body=? WHERE workspace_id=? AND id=?`, body, ws, id)
		return err
	})
}
