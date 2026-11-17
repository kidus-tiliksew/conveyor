package workorder

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// Every read of a work order states the workspace's progress report interval
// as the configuration holds it at that read, in whole seconds, and 0 when no
// interval applies. A later read of the same claimed order sees a change, and
// the order itself stores nothing (component-work-orders invariant 67;
// req-delegated-execution AC-6.1, AC-6.2).
func TestWorkOrderContextStatesLiveProgressReportInterval(t *testing.T) {
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []core.Stage{core.StageSpec, core.StageImplement, core.StageReview, core.StageVerify} {
		t.Run(string(stage), func(t *testing.T) {
			ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "test")
			st := store.NewMemory()
			task := core.Task{ID: "interval-task", Workspace: "test", Repo: "app", State: core.TaskRunning, NextStage: stage, CreatedAt: time.Now()}
			if stage == core.StageVerify {
				task.SetupContract.VerifyStage = true
				task.ReviewedHeadSHA = strings.Repeat("b", 40)
			}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			job := core.Job{ID: "interval-" + string(stage) + "-1", TaskID: task.ID, Stage: stage, State: core.JobPending}
			if err := st.CreateJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: stage}
			if stage == core.StageReview || stage == core.StageVerify {
				order.ServedRequirementSnapshot = []core.ServedRequirementContext{}
				order.GovernanceSnapshot = &core.GovernanceSnapshot{Designs: []core.GovernanceDesignContext{}, Decisions: []core.Decision{}}
				order.ReviewRound, order.ReviewSeat, order.BaselineSHA, order.HeadSHA = 1, 1, strings.Repeat("a", 40), strings.Repeat("b", 40)
			}
			if err := storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
				t.Fatal(err)
			}
			if _, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "interval-session", ClientToken: "token", Lease: time.Minute}); err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			cfg := &config.Config{}
			svc := &Service{Store: st, Pack: bundle, ConfigProvider: func(context.Context) (*config.Config, error) {
				mu.Lock()
				defer mu.Unlock()
				return cfg, nil
			}}
			for _, tc := range []struct {
				text string
				want int64
			}{
				{"", 600},
				{"5m", 300},
				{"90s", 90},
				{"0", 0},
				{"60m", 3600},
			} {
				mu.Lock()
				cfg = &config.Config{Execution: config.ExecutionPolicy{ProgressReportIntervalText: tc.text}}
				mu.Unlock()
				result, err := svc.Get(ctx, job.ID, "interval-session")
				if err != nil {
					t.Fatal(err)
				}
				if result.ProgressReportIntervalSeconds != tc.want {
					t.Fatalf("interval %q: progress_report_interval_seconds = %d, want %d", tc.text, result.ProgressReportIntervalSeconds, tc.want)
				}
				wire, err := json.Marshal(result.Outbound())
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(wire, &fields); err != nil {
					t.Fatal(err)
				}
				if got := string(fields["progress_report_interval_seconds"]); got != strconv.FormatInt(tc.want, 10) {
					t.Fatalf("interval %q: delivered field = %q, want %d", tc.text, got, tc.want)
				}
			}
			stored, err := st.GetWorkOrder(ctx, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			wire, err := json.Marshal(stored)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(wire), "progress_report_interval") {
				t.Fatalf("stored order carries the interval: %s", wire)
			}
		})
	}
}

// A service without a configuration provider states the 10-minute default.
func TestWorkOrderContextDefaultsProgressReportIntervalWithoutConfiguration(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "test")
	st := store.NewMemory()
	task := core.Task{ID: "interval-default", Workspace: "test", Repo: "app", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "interval-default-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement}); err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "default-session", ClientToken: "token", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Service{Store: st, Pack: bundle}).Get(ctx, job.ID, "default-session")
	if err != nil {
		t.Fatal(err)
	}
	if result.ProgressReportIntervalSeconds != 600 {
		t.Fatalf("progress_report_interval_seconds = %d, want 600", result.ProgressReportIntervalSeconds)
	}
}
