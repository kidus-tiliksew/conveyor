package store_test

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// TestClaimSessionAndActivityReadsAtScale runs the claim session and
// whole-workspace activity reads over 200 snapshot-bearing orders on the
// volatile backend, seeded through the public store API
// (component-work-orders, Snapshot-free fold reads).
func TestClaimSessionAndActivityReadsAtScale(t *testing.T) {
	st := store.NewVolatileBackend()
	workspace := "claim-session-scale"
	ctx := store.WithActor(store.WithWorkspace(t.Context(), workspace), store.SystemActor())
	cfg := &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}
	if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	storetest.RunClaimSessionAndActivityReadsAtScale(t, storetest.Fixture{Backend: st, Context: ctx, Workspace: workspace, Config: cfg}, storetest.ClaimSessionScale{})
}
