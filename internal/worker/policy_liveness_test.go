package worker

import (
	"context"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestServiceabilityUsesWorkerLivenessNotHarnessKnowledge(t *testing.T) {
	ctx := store.WithWorkspace(context.Background(), "demo")
	st := store.NewMemory()
	now := time.Now().UTC()
	if err := st.CreateWorker(ctx, core.Worker{ID: "live", Workspace: "demo", LeaseExpiresAt: now.Add(time.Hour), LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: st}
	service.Now = func() time.Time { return now }
	cfg := &config.Config{Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Harness: "missing-server-registry"}}}}
	status := service.Serviceability(ctx, cfg)
	if !status.WorkerExpected || !status.Available || status.Reason != "" {
		t.Fatalf("liveness serviceability=%+v", status)
	}
}
