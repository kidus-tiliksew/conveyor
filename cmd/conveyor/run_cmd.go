package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
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
	configPath := defaultLocalExecutionConfigPath()
	auto := false
	cmd := &cobra.Command{
		Use:   "run <task-id>",
		Short: "Explicitly claim and execute one task on this machine",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			input := cmd.InOrStdin()
			return runTask(ctx, newClient(), args[0], configPath, input, cmd.OutOrStdout(), auto, inputIsTerminal(input))
		},
	}
	cmd.Flags().StringVar(&configPath, "config", configPath, "local execution configuration")
	cmd.Flags().BoolVar(&auto, "auto", false, "run every claimable stage without confirmation")
	return cmd
}

const (
	runModeConfirmed = "confirmed-per-stage"
	runModeAuto      = "auto-chained"
)

func runTask(ctx context.Context, c *client, taskID, configPath string, input io.Reader, output io.Writer, auto, terminal bool) error {
	if strings.TrimSpace(c.token) == "" {
		return fmt.Errorf("CONVEYOR_API_TOKEN is required for task execution")
	}
	item, err := c.getTaskRunOrderContext(ctx, c.token, taskID)
	if err != nil {
		return err
	}
	if item == nil {
		_, err = fmt.Fprintf(output, "task %s has no claimable spec, implement, or review order\n", taskID)
		return err
	}
	setup, err := loadLocalExecutionSetup(configPath)
	if err != nil {
		_ = presentPendingRunOrder(output, *item)
		return err
	}
	local := setup.Config
	reader := bufio.NewReader(input)
	runStages := make([]core.Stage, 0, 2)
	for {
		selected, selectErr := selectLocalRunDispatch(*item, local)
		if selectErr != nil {
			_ = presentPendingRunOrder(output, *item)
			return localExecutionSetupRemedy(configPath, selectErr)
		}
		if err = presentRunOrder(output, selected, local.Routing.Stages[string(selected.Order.Stage)].TimeoutText); err != nil {
			return err
		}
		mode := runModeAuto
		if !auto {
			mode = runModeConfirmed
			if !terminal {
				_, _ = fmt.Fprintf(output, "No work order was claimed because stdin is not a terminal.\nRun conveyor run %s --auto to proceed.\n", taskID)
				return fmt.Errorf("stage confirmation requires a terminal; use conveyor run %s --auto", taskID)
			}
			confirmed, confirmErr := confirmRunStage(ctx, reader, output, selected.Order.Stage)
			if confirmErr != nil {
				return confirmErr
			}
			if !confirmed {
				return printRunSummary(output, selected.Task, runStages)
			}
		}
		if runErr := runHarnessChildWithFirstActivityTimeoutAndRunMode(ctx, c, c.token, selected, setup.FirstActivityTimeout, mode); runErr != nil {
			return runErr
		}
		runStages = append(runStages, selected.Order.Stage)
		item, err = c.getTaskRunOrderContext(ctx, c.token, taskID)
		if err != nil {
			return err
		}
		if item == nil {
			if selected.Order.Stage == core.StageSpec {
				_, err = fmt.Fprintf(output, "task %s reached the pending spec approval gate; operator approval is required\n", taskID)
				return err
			}
			_, err = fmt.Fprintf(output, "task %s has no claimable spec, implement, or review order\n", taskID)
			return err
		}
	}
}

func localExecutionSetupRemedy(configPath string, err error) error {
	return fmt.Errorf("%w; recreate it with `%s --config %s`", err, localExecutionSetupCommand, configPath)
}

func inputIsTerminal(input io.Reader) bool {
	file, ok := input.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func presentRunOrder(output io.Writer, item workerservice.DispatchOrder, timeout string) error {
	if err := presentPendingRunOrder(output, item); err != nil {
		return err
	}
	effort := item.Effort
	if effort == "" {
		effort = "default"
	}
	_, err := fmt.Fprintf(output, "Execution: harness %s, model %s, effort %s, timeout %s\n", item.Harness.Name, item.Model, effort, timeout)
	return err
}

func presentPendingRunOrder(output io.Writer, item workerservice.DispatchOrder) error {
	_, err := fmt.Fprintf(output, "Task %s: %s (state %s)\nNext: %s work order %s\n", item.Task.ID, item.Task.Title, item.Task.State, item.Order.Stage, item.Order.ID)
	return err
}

func confirmRunStage(ctx context.Context, input *bufio.Reader, output io.Writer, stage core.Stage) (bool, error) {
	for {
		if _, err := fmt.Fprintf(output, "Proceed with %s? [y/N]: ", stage); err != nil {
			return false, err
		}
		type readResult struct {
			answer string
			err    error
		}
		result := make(chan readResult, 1)
		go func() {
			answer, err := input.ReadString('\n')
			result <- readResult{answer: answer, err: err}
		}()
		var answer string
		var err error
		select {
		case <-ctx.Done():
			_, _ = fmt.Fprintln(output)
			return false, nil
		case read := <-result:
			answer, err = read.answer, read.err
		}
		if err != nil && len(answer) == 0 {
			if err == io.EOF {
				_, _ = fmt.Fprintln(output)
				return false, nil
			}
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "y", "yes":
			return true, nil
		case "", "n", "no":
			return false, nil
		default:
			if _, err = fmt.Fprintln(output, "Please answer yes or no."); err != nil {
				return false, err
			}
		}
	}
}

func printRunSummary(output io.Writer, task core.Task, stages []core.Stage) error {
	ran := "none"
	if len(stages) > 0 {
		names := make([]string, len(stages))
		for index, stage := range stages {
			names[index] = string(stage)
		}
		ran = strings.Join(names, ", ")
	}
	_, err := fmt.Fprintf(output, "Run stopped before claiming the next work order.\nRan: %s\nTask %s is currently %s.\nResume: conveyor run %s\n", ran, task.ID, task.State, task.ID)
	return err
}

func configuredFirstActivityTimeout(local *config.Config) (time.Duration, error) {
	if local.Execution.FirstActivityTimeout > 0 {
		return local.Execution.FirstActivityTimeout, nil
	}
	parsed, err := time.ParseDuration(local.Execution.FirstActivityTimeoutText)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("execution.first_activity_timeout must be a positive duration")
	}
	return parsed, nil
}

func selectLocalRunDispatch(item workerservice.DispatchOrder, local *config.Config) (workerservice.DispatchOrder, error) {
	item, err := selectLocalExecutionDispatch(item, local)
	if err != nil {
		return item, err
	}
	item.Dispatch = "run"
	item.Auth = "user"
	return item, nil
}

func selectLocalWorkerDispatch(item workerservice.DispatchOrder, local *config.Config, log io.Writer) (workerservice.DispatchOrder, error) {
	selected, err := selectLocalExecutionDispatch(item, local)
	if err != nil {
		return item, err
	}
	if item.Order.RequiredModel != "" || item.Order.RequiredHarnessConfig != nil || item.Order.RequiredHarness != "" || item.Order.RequiredEffort != "" {
		_, _ = fmt.Fprintf(log, "worker order %s: ignoring server-pinned execution fields; using local setup harness=%s model=%s effort=%s\n", item.Order.ID, selected.Harness.Name, selected.Model, selected.Effort)
	}
	selected.Dispatch = "worker"
	selected.Auth = "byoa"
	return selected, nil
}

func selectLocalExecutionDispatch(item workerservice.DispatchOrder, local *config.Config) (workerservice.DispatchOrder, error) {
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
	return item, nil
}
