package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
	"github.com/spf13/cobra"
)

func runCmd() *cobra.Command {
	configPath := strings.TrimSpace(os.Getenv("CONVEYOR_CONFIG"))
	if configPath == "" {
		configPath = "conveyor.yaml"
	}
	cmd := &cobra.Command{
		Use:   "run <task-id>",
		Short: "Explicitly claim and execute one task on this machine",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runTask(ctx, newClient(), args[0], configPath, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&configPath, "config", configPath, "local execution configuration")
	return cmd
}

func runTask(ctx context.Context, c *client, taskID, configPath string, output interface{ Write([]byte) (int, error) }) error {
	if strings.TrimSpace(c.token) == "" {
		return fmt.Errorf("CONVEYOR_API_TOKEN is required for task execution")
	}
	local, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load local execution config: %w", err)
	}
	if err = validateWorkerConfig(workerservice.WorkerConfig{WorkspaceDocument: local.WorkspaceDocument()}); err != nil {
		return fmt.Errorf("invalid local execution config: %w", err)
	}
	firstActivityTimeout := local.Execution.FirstActivityTimeout
	if firstActivityTimeout <= 0 {
		firstActivityTimeout, _ = time.ParseDuration(local.Execution.FirstActivityTimeoutText)
	}
	for {
		item, getErr := c.getTaskRunOrderContext(ctx, c.token, taskID)
		if getErr != nil {
			return getErr
		}
		if item == nil {
			_, err = fmt.Fprintf(output, "task %s has no claimable implement or review order\n", taskID)
			return err
		}
		selected, selectErr := selectLocalRunDispatch(*item, local)
		if selectErr != nil {
			return selectErr
		}
		if runErr := runHarnessChildWithFirstActivityTimeout(ctx, c, c.token, selected, firstActivityTimeout); runErr != nil {
			return runErr
		}
	}
}

func selectLocalRunDispatch(item workerservice.DispatchOrder, local *config.Config) (workerservice.DispatchOrder, error) {
	stage := string(item.Order.Stage)
	route, ok := local.Routing.Stages[stage]
	if !ok || route.Execution != config.ExecutionMCP {
		return item, fmt.Errorf("local execution config has no MCP route for %s", stage)
	}
	harnessName, model, effort := route.Harness, local.EffectiveModel(stage), route.Effort
	if item.Order.Stage == core.StageReview && item.Order.ReviewSeat > 0 && item.Order.ReviewSeat <= len(local.Review.Seats) {
		seat := local.Review.Seats[item.Order.ReviewSeat-1]
		if seat.Harness != "" {
			harnessName = seat.Harness
		}
		if seat.Model != "" {
			model = seat.Model
		}
		if seat.Effort != "" {
			effort = seat.Effort
		}
	}
	var harness config.Harness
	for _, candidate := range local.Harnesses {
		if candidate.Name == harnessName {
			harness = candidate
			break
		}
	}
	if harness.Name == "" {
		return item, fmt.Errorf("local execution config has no harness %q for %s", harnessName, stage)
	}
	item.Harness = harness
	item.Model = model
	item.Effort = effort
	item.EffortArgv = append([]string(nil), harness.EffortArgs[effort]...)
	item.HarnessSelection = "local"
	item.Dispatch = "run"
	item.Auth = "user"
	return item, nil
}
