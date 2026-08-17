package dispatch

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestPolicyOnlyDispatchCarriesTimeoutAndNoExecutionPins(t *testing.T) {
	cfg := &config.Config{
		WorkOrderQueueTimeout: time.Hour,
		Routing: config.Routing{Stages: map[string]config.StageRoute{
			"implement": {Execution: config.ExecutionMCP, TimeoutText: "3h", Model: "server-model", Harness: "server-harness", Effort: "high"},
		}},
	}
	order, err := BuildFutureWorkOrderRouting(cfg, core.Task{}, core.StageImplement)
	if err != nil {
		t.Fatal(err)
	}
	if order.ExecutionTimeoutText != "3h" {
		t.Fatalf("timeout=%q", order.ExecutionTimeoutText)
	}
	if order.RequiredModel != "" || order.RequiredHarness != "" || order.RequiredEffort != "" || order.RequiredHarnessConfig != nil {
		t.Fatalf("server execution pins survived: %+v", order)
	}
}

func TestPolicyOnlyReviewRoundFreezesSeatShapeNotExecution(t *testing.T) {
	cfg := &config.Config{
		WorkOrderQueueTimeout: time.Hour,
		Review:                config.ReviewPanel{Seats: []config.ReviewSeat{{Model: "one", Harness: "codex"}, {Model: "two", Harness: "claude", Effort: "high"}}},
	}
	_, orders, err := BuildReviewRound(cfg, core.Task{ID: "policy-review"}, config.StageRoute{TimeoutText: "45m"}, 1)
	if err != nil || len(orders) != 2 {
		t.Fatalf("orders=%+v err=%v", orders, err)
	}
	for _, order := range orders {
		if order.RequiredModel != "" || order.RequiredHarness != "" || order.RequiredEffort != "" || order.RequiredHarnessConfig != nil {
			t.Fatalf("seat %d carries server execution pins: %+v", order.ReviewSeat, order)
		}
	}
}

func TestFrozenPolicySurvivesWorkspacePolicyEdits(t *testing.T) {
	intake := &config.Config{
		MaxBounces:            3,
		WorkOrderQueueTimeout: time.Hour,
		Routing: config.Routing{Stages: map[string]config.StageRoute{
			"spec":      {Execution: config.ExecutionMCP, TimeoutText: "20m"},
			"implement": {Execution: config.ExecutionMCP, TimeoutText: "2h"},
			"review":    {Execution: config.ExecutionMCP, TimeoutText: "40m"},
		}},
		Review: config.ReviewPanel{Seats: []config.ReviewSeat{{}, {}}},
	}
	task := core.Task{ID: "frozen-policy", SetupContract: intake.FreezePolicy()}
	current := &config.Config{
		MaxBounces:            9,
		WorkOrderQueueTimeout: time.Hour,
		Routing: config.Routing{Stages: map[string]config.StageRoute{
			"spec":      {Execution: config.ExecutionMCP, TimeoutText: "50m"},
			"implement": {Execution: config.ExecutionMCP, TimeoutText: "8h"},
			"review":    {Execution: config.ExecutionMCP, TimeoutText: "3h"},
		}},
		Review: config.ReviewPanel{Seats: []config.ReviewSeat{{}}},
	}
	implement, err := BuildFutureWorkOrderRouting(current, task, core.StageImplement)
	if err != nil || implement.ExecutionTimeoutText != "2h" {
		t.Fatalf("implementation order=%+v err=%v", implement, err)
	}
	_, reviews, err := BuildReviewRound(current, task, current.Routing.Stages["review"], 1)
	if err != nil || len(reviews) != 2 || reviews[0].ExecutionTimeoutText != "40m" {
		t.Fatalf("review orders=%+v err=%v", reviews, err)
	}
	if frozen := current.WithSetup(task.SetupContract); frozen.MaxBounces != 3 {
		t.Fatalf("max_bounces=%d", frozen.MaxBounces)
	}
}
