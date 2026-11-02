package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

func claimSessionScaleFixture(t *testing.T) storetest.Fixture {
	t.Helper()
	st := store.NewVolatileBackend()
	workspace := "claim-session-scale"
	ctx := store.WithActor(store.WithWorkspace(t.Context(), workspace), store.SystemActor())
	cfg := &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}
	if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	return storetest.Fixture{Backend: st, Context: ctx, Workspace: workspace, Config: cfg}
}

// TestClaimSessionAndActivityReadsAtScale runs the claim session and
// whole-workspace activity reads over 200 snapshot-bearing orders on the
// volatile backend, seeded through the public store API
// (component-work-orders, Snapshot-free fold reads).
func TestClaimSessionAndActivityReadsAtScale(t *testing.T) {
	storetest.RunClaimSessionAndActivityReadsAtScale(t, claimSessionScaleFixture(t), storetest.ClaimSessionScale{})
}

// TestRenewalReadsAtScale renews through workorder.Service.RenewClaimWithLease
// over 200 snapshot-bearing orders on the volatile backend
// (component-work-orders, Snapshot-free fold reads).
func TestRenewalReadsAtScale(t *testing.T) {
	storetest.RunRenewalReads(t, claimSessionScaleFixture(t), storetest.ClaimSessionScale{}, renewThroughService)
}

// renewThroughService is the renewal route MCP renew_work_order calls.
func renewThroughService(st store.Store) func(ctx context.Context, claim core.WorkOrderClaimIdentity, workOrderID string, lease time.Duration) (core.WorkOrder, error) {
	return (&workorder.Service{Store: st}).RenewClaimWithLease
}
