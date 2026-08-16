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
