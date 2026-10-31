package workorder

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// claimErrorStore fails every claim transaction with err and, when
// recordErr is set, every claim-failure record write too.
type claimErrorStore struct {
	store.Store
	err       error
	recordErr error
}

func (s claimErrorStore) ClaimWorkOrderCommand(context.Context, taskops.TaskLease, string, core.WorkOrderClaim) (core.WorkOrder, error) {
	return core.WorkOrder{}, s.err
}

func (s claimErrorStore) RecordWorkOrderClaimFailure(ctx context.Context, id, class string, at time.Time) (core.WorkOrderClaimFailure, error) {
	if s.recordErr != nil {
		return core.WorkOrderClaimFailure{}, s.recordErr
	}
	return s.Store.RecordWorkOrderClaimFailure(ctx, id, class, at)
}

// The service records only backend-operation claim failures, and a record
// write that fails never replaces the claim's error (component-work-orders,
// Claim admission).
func TestClaimRecordsOnlyBackendOperationFailures(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "claim-failure", Workspace: "demo", Repo: "conveyor", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: job.Stage}); err != nil {
		t.Fatal(err)
	}
	provider := func(context.Context) (*config.Config, error) {
		return &config.Config{Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Timeout: time.Hour}}}}, nil
	}
	claim := core.WorkOrderClaim{SessionID: "session", ClientToken: "secret", ClaimantID: core.TaskRunClaimantID("owner"), OwnerUserID: "owner"}
	count := func() int {
		t.Helper()
		records, err := st.ListWorkOrderClaimFailures(ctx, []string{job.ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(records) == 0 {
			return 0
		}
		return records[0].ConsecutiveFailures
	}

	refusal := errors.New("work order is assigned to another user")
	if _, err := (&Service{Store: claimErrorStore{Store: st, err: refusal}, ConfigProvider: provider}).Claim(ctx, job.ID, claim); !errors.Is(err, refusal) {
		t.Fatalf("refused claim error = %v", err)
	}
	if got := count(); got != 0 {
		t.Fatalf("a refusal recorded %d failures", got)
	}

	backend := fmt.Errorf("PostgreSQL database operation failed: %w", store.ErrBackendOperation)
	failing := &Service{Store: claimErrorStore{Store: st, err: backend}, ConfigProvider: provider}
	for n := 1; n <= 2; n++ {
		if _, err := failing.Claim(ctx, job.ID, claim); err != backend {
			t.Fatalf("backend claim %d error = %v, want the claim's own error", n, err)
		}
		if got := count(); got != n {
			t.Fatalf("after backend failure %d count = %d", n, got)
		}
	}

	unrecorded := &Service{Store: claimErrorStore{Store: st, err: backend, recordErr: errors.New("record write failed")}, ConfigProvider: provider}
	if _, err := unrecorded.Claim(ctx, job.ID, claim); err != backend {
		t.Fatalf("claim with a failed record write error = %v, want the claim's own error", err)
	}
	if got := count(); got != 2 {
		t.Fatalf("after a failed record write count = %d, want 2", got)
	}

	if _, err := (&Service{Store: st, ConfigProvider: provider}).Claim(ctx, job.ID, claim); err != nil {
		t.Fatalf("successful claim: %v", err)
	}
	if got := count(); got != 0 {
		t.Fatalf("after a successful claim count = %d, want the record deleted", got)
	}
}
