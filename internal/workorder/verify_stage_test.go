package workorder

import (
	"context"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"strings"
	"testing"
	"time"
)

func TestSubmitRoutesFrozenVerifyAndRendersPinnedContext(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[enabled], func(t *testing.T) {
			ctx := store.WithWorkspace(t.Context(), "demo")
			st := store.NewMemory()
			cfg := &config.Config{Workspace: "demo", Repos: []config.Repo{{Name: "app", Base: "main"}}, Review: config.ReviewPanel{Seats: []config.ReviewSeat{{}}}, Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Execution: config.ExecutionMCP, TimeoutText: "1h"}, "review": {Execution: config.ExecutionMCP, TimeoutText: "1h"}, "verify": {Execution: config.ExecutionMCP, TimeoutText: "1h"}}}}
			cfg.Execution.VerifyStage = enabled
			task := core.Task{ID: "verify-submit", Workspace: "demo", Repo: "app", State: core.TaskRunning, NextStage: core.StageImplement, SetupContract: cfg.FreezePolicy(), CreatedAt: time.Now().UTC()}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			plan, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: task.ID, Content: "approved verification plan"})
			if err != nil {
				t.Fatal(err)
			}
			if err = st.ApproveSpecVersion(ctx, task.ID, plan.Version); err != nil {
				t.Fatal(err)
			}
			job := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
			if err := st.CreateJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, JobID: job.ID, TaskID: task.ID, Stage: job.Stage}); err != nil {
				t.Fatal(err)
			}
			if _, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{ClaimantID: "tester", SessionID: "implement-session", ClientToken: "implement-token", Lease: time.Minute}); err != nil {
				t.Fatal(err)
			}
			d := dispatch.New(st, cfg, nil)
			d.DisableMemoryQueueForTest()
			bundle, err := pack.Load("")
			if err != nil {
				t.Fatal(err)
			}
			service := &Service{Store: st, Dispatcher: d, Pack: bundle, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
			result, err := service.SubmitForReview(ctx, job.ID, "implement-session", "submitted-head")
			if err != nil {
				t.Fatal(err)
			}
			if enabled && result["next_stage"] != core.StageVerify {
				t.Fatalf("response: %+v", result)
			}
			if !enabled {
				if _, ok := result["next_stage"]; ok {
					t.Fatal("toggle-off response changed")
				}
			}
			if err := d.DispatchNow(ctx, task.ID); err != nil {
				t.Fatal(err)
			}
			if err := d.DispatchNow(ctx, task.ID); err != nil {
				t.Fatal(err)
			}
			orders, err := st.ListTaskWorkOrders(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, order := range orders {
				if order.Stage == core.StageVerify {
					count++
					if order.ID != task.ID+"-verify-1" || order.HeadSHA != "submitted-head" || order.ReviewSeat != 0 {
						t.Fatalf("verify order: %+v", order)
					}
					// Verification allows the implementer's session; DEC-11 stays review-only.
					claimed, err := service.Claim(ctx, order.ID, core.WorkOrderClaim{ClaimantID: "tester", SessionID: "implement-session", ClientToken: "implement-token", Lease: time.Minute})
					if err != nil {
						t.Fatal(err)
					}
					context, err := service.Get(ctx, claimed.ID, "implement-session")
					if err != nil {
						t.Fatal(err)
					}
					if context.ApprovedSpec == nil || context.ApprovedSpec.Content != plan.Content || !strings.Contains(context.RolePrompt, "submitted-head") || !strings.Contains(context.RolePrompt, "# Verification obligations") {
						t.Fatal("verify context is incomplete")
					}
					if context.AuthoritySource != "pinned" || context.GovernanceSnapshot == nil {
						t.Fatal("verify authority was not pinned")
					}
				}
			}
			if enabled && count != 1 || !enabled && count != 0 {
				t.Fatalf("verify count=%d", count)
			}
		})
	}
}
