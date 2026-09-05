package storetest

import (
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func runWorkers(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	now := time.Now().UTC().Truncate(time.Microsecond)
	pair := core.WorkerPairing{TokenHash: "conformance-pair", Workspace: x.Workspace, CreatedAt: now, ExpiresAt: now.Add(time.Minute)}
	requireOK(t, st.CreateWorkerPairing(ctx, pair))
	consumed, err := st.ConsumeWorkerPairing(ctx, pair.TokenHash, now)
	requireOK(t, err)
	if consumed.Workspace != x.Workspace || consumed.ConsumedAt.IsZero() {
		t.Fatal("pairing was not consumed")
	}
	if _, err := st.ConsumeWorkerPairing(ctx, pair.TokenHash, now); !errors.Is(err, store.ErrPairingInvalid) {
		t.Fatalf("reused pairing error=%v", err)
	}
	pair.TokenHash = "expired-pair"
	pair.ExpiresAt = now.Add(-time.Second)
	requireOK(t, st.CreateWorkerPairing(ctx, pair))
	if _, err := st.ConsumeWorkerPairing(ctx, pair.TokenHash, now); !errors.Is(err, store.ErrPairingInvalid) {
		t.Fatalf("expired pairing error=%v", err)
	}
	worker := core.Worker{ID: "worker-" + core.NewTaskID(), Workspace: x.Workspace, Name: "Conformance", CredentialHash: "conformance-worker", CreatedAt: now}
	requireOK(t, st.CreateWorker(ctx, worker))
	auth, err := st.AuthenticateWorker(ctx, worker.CredentialHash)
	requireOK(t, err)
	if auth.ID != worker.ID {
		t.Fatal("worker credential identifies another worker")
	}
	before, err := st.ListEvents(ctx, "")
	requireOK(t, err)
	beat, err := st.HeartbeatWorker(ctx, worker.ID, now.Add(time.Minute), []core.HarnessProbe{{Harness: "codex", Healthy: true, CheckedAt: now}})
	requireOK(t, err)
	if !beat.LeaseExpiresAt.Equal(now.Add(time.Minute)) || len(beat.Probes) != 1 || !beat.Probes[0].Healthy {
		t.Fatal("heartbeat did not persist lease and probe")
	}
	after, err := st.ListEvents(ctx, "")
	requireOK(t, err)
	if len(after) != len(before) {
		t.Fatal("heartbeat appended a lifecycle event")
	}
	workers, err := st.ListWorkers(ctx)
	requireOK(t, err)
	if len(workers) != 1 || workers[0].ID != worker.ID {
		t.Fatal("worker listing differs")
	}
	failures, err := st.ListHarnessModelFailures(ctx)
	requireOK(t, err)
	if len(failures) != 0 {
		t.Fatal("healthy worker has model failure")
	}
	requireOK(t, st.RevokeWorker(ctx, worker.ID))
	if _, err := st.AuthenticateWorker(ctx, worker.CredentialHash); !errors.Is(err, store.ErrWorkerUnauthorized) {
		t.Fatalf("revoked worker error=%v", err)
	}
}
