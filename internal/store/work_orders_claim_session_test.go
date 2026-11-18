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

// TestRenewalDeliveryReadsAtScale renews pinned verify and review orders
// through the renew_work_order service path, DeliverOrder included, over 200
// snapshot-bearing orders on the volatile backend (component-work-orders
// invariants 23 and 24).
// The volatile backend's own timeline count proves that no renewal copies a
// whole task timeline, including through calls it makes on itself.
func TestRenewalDeliveryReadsAtScale(t *testing.T) {
	x := claimSessionScaleFixture(t)
	storetest.RunRenewalDeliveryReads(t, x, storetest.ClaimSessionScale{TimelineReads: func() int64 { return store.MemoryTimelineReads(x.Backend) }}, renewAndDeliverThroughService)
}

// renewAndDeliverThroughService is MCP renew_work_order's service path:
// renewal, then the response DeliverOrder builds.
func renewAndDeliverThroughService(st store.Store) func(context.Context, core.WorkOrderClaimIdentity, string, time.Duration) (core.WorkOrder, *core.GovernanceReferences, string, error) {
	service := &workorder.Service{Store: st}
	return func(ctx context.Context, claim core.WorkOrderClaimIdentity, workOrderID string, lease time.Duration) (core.WorkOrder, *core.GovernanceReferences, string, error) {
		order, err := service.RenewClaimWithLease(ctx, claim, workOrderID, lease)
		if err != nil {
			return order, nil, "", err
		}
		delivery := service.DeliverOrder(ctx, order)
		return delivery.WorkOrder, delivery.GovernanceReferences, delivery.GovernanceReferencesError, nil
	}
}
