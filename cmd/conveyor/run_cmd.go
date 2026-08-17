package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func runCmd() *cobra.Command {
	configPath := defaultLocalExecutionConfigPath()
	auto := false
	raw := false
	cmd := &cobra.Command{
		Use:   "run <task-id>",
		Short: "Explicitly claim and execute one task on this machine",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			input := cmd.InOrStdin()
			output := cmd.OutOrStdout()
			return runTaskWithPresentation(ctx, newClient(), args[0], configPath, input, output, auto, inputIsTerminal(input), outputIsTerminal(output), raw)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", configPath, "local execution configuration")
	cmd.Flags().BoolVar(&auto, "auto", false, "run every claimable stage without confirmation")
	cmd.Flags().BoolVar(&raw, "raw", false, "print the raw harness event stream")
	return cmd
}

const (
	runModeConfirmed = "confirmed-per-stage"
	runModeAuto      = "auto-chained"
)

var runGatePollInterval = 2 * time.Second

func runTask(ctx context.Context, c *client, taskID, configPath string, input io.Reader, output io.Writer, auto, terminal bool) error {
	return runTaskWithPresentation(ctx, c, taskID, configPath, input, output, auto, terminal, false, false)
}

func runTaskWithPresentation(ctx context.Context, c *client, taskID, configPath string, input io.Reader, output io.Writer, auto, inputTerminal, outputTerminal, raw bool) error {
	if strings.TrimSpace(c.token) == "" {
		return fmt.Errorf("CONVEYOR_API_TOKEN is required for task execution")
	}
	reader := bufio.NewReader(input)
	answers := newRunInputSource(reader)
	runStages := make([]core.Stage, 0, 2)
	attached := inputTerminal && outputTerminal
	var setup localExecutionSetup
	setupLoaded := false
	var lastStage core.Stage
	for {
		item, err := c.getTaskRunOrderContext(ctx, c.token, taskID)
		if err != nil {
			return err
		}
		if item == nil || item.Order.ID == "" {
			if !attached {
				if lastStage == core.StageSpec {
					_, err = fmt.Fprintf(output, "task %s reached the pending spec approval gate; operator approval is required\n", taskID)
					return err
				}
				_, err = fmt.Fprintf(output, "task %s has no claimable spec, implement, or review order\n", taskID)
				return err
			}
			if item == nil {
				_, err = fmt.Fprintf(output, "task %s has no claimable spec, implement, or review order\n", taskID)
				return err
			}
			if item.Task.State == core.TaskMerged || item.Task.State == core.TaskClosed || item.Task.State == core.TaskParked {
				return printFinalRunSummaryStyled(output, item.Task, runStages, outputTerminal)
			}
			if item.Gate == nil {
				if err = presentRunIdleStateStyled(output, item.Task, outputTerminal); err != nil {
					return err
				}
				select {
				case <-ctx.Done():
					return printRunSummaryStyled(output, item.Task, runStages, outputTerminal)
				case <-time.After(runGatePollInterval):
					continue
				}
			}
			decision, feedback, waitErr := waitAtTaskRunGate(ctx, answers, output, *item, outputTerminal)
			if waitErr != nil {
				return waitErr
			}
			switch decision {
			case runGateStop:
				return printRunSummaryStyled(output, item.Task, runStages, outputTerminal)
			case runGatePoll:
				continue
			case runGateApprove:
				err = c.approveTaskRunGateContext(ctx, c.token, *item)
			case runGateRequestChanges:
				err = c.requestTaskRunGateChangesContext(ctx, c.token, *item, feedback)
			}
			if err != nil {
				var response *workerHTTPError
				if errors.As(err, &response) && response.StatusCode == http.StatusConflict {
					_, _ = fmt.Fprintln(output, "Gate state changed; refreshing task state.")
					continue
				}
				return err
			}
			_, _ = fmt.Fprintln(output, "Gate decision recorded; refreshing task state.")
			continue
		}

		if !setupLoaded {
			setup, err = loadLocalExecutionSetup(configPath)
			if err != nil {
				_ = presentPendingRunOrderStyled(output, *item, outputTerminal)
				return err
			}
			setupLoaded = true
		}
		local := setup.Config
		selected, selectErr := selectLocalRunDispatch(*item, local)
		if selectErr != nil {
			_ = presentPendingRunOrderStyled(output, *item, outputTerminal)
			return localExecutionSetupRemedy(configPath, selectErr)
		}
		if err = presentRunOrderStyled(output, selected, local.Routing.Stages[string(selected.Order.Stage)].TimeoutText, outputTerminal); err != nil {
			return err
		}
		mode := runModeAuto
		if !auto {
			mode = runModeConfirmed
			if !inputTerminal {
				_, _ = fmt.Fprintf(output, "No work order was claimed because stdin is not a terminal.\nRun conveyor run %s --auto to proceed.\n", taskID)
				return fmt.Errorf("stage confirmation requires a terminal; use conveyor run %s --auto", taskID)
			}
			confirmed, confirmErr := confirmRunStageFromSourceStyled(ctx, answers, output, selected.Order.Stage, outputTerminal)
			if confirmErr != nil {
				return confirmErr
			}
			if !confirmed {
				return printRunSummaryStyled(output, selected.Task, runStages, outputTerminal)
			}
		}
		var presentation *runOutputPresentation
		if outputTerminal && !raw {
			presentation = &runOutputPresentation{output: output}
		}
		started := time.Now()
		runErr := runHarnessChildWithFirstActivityTimeoutAndRunModeAndPresentation(ctx, c, c.token, selected, setup.FirstActivityTimeout, mode, presentation)
		if outputTerminal && !raw {
			_ = presentRunStageSummary(output, selected.Order.Stage, time.Since(started), runErr)
		}
		if runErr != nil {
			return runErr
		}
		runStages = append(runStages, selected.Order.Stage)
		lastStage = selected.Order.Stage
	}
}

type runGateDecision int

const (
	runGatePoll runGateDecision = iota
	runGateApprove
	runGateRequestChanges
	runGateStop
)

func waitAtTaskRunGate(ctx context.Context, answers *runInputSource, output io.Writer, item workerservice.DispatchOrder, styled bool) (runGateDecision, string, error) {
	gate := item.Gate
	if gate == nil {
		return runGatePoll, "", nil
	}
	if err := presentTaskRunGateStyled(output, item.Task, *gate, styled); err != nil {
		return runGateStop, "", err
	}
	if !gate.CanOperate && !gate.CanRequestChanges {
		_, _ = fmt.Fprintln(output, "A maintainer or operator can resolve this gate; waiting without a claim.")
		select {
		case <-ctx.Done():
			return runGateStop, "", nil
		case <-time.After(runGatePollInterval):
			return runGatePoll, "", nil
		}
	}

	actions := "wait"
	if gate.CanOperate {
		actions = "approve/changes/wait"
	} else if gate.CanRequestChanges {
		actions = "changes/wait"
	}
	for {
		answer, polled, err := readRunPromptOrPoll(ctx, answers, output, fmt.Sprintf("Gate action [%s]: ", actions), styled, runGatePollInterval)
		if err != nil {
			return runGateStop, "", err
		}
		if polled {
			return runGatePoll, "", nil
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "":
			return runGateStop, "", nil
		case "wait":
			select {
			case <-ctx.Done():
				return runGateStop, "", nil
			case <-time.After(runGatePollInterval):
				return runGatePoll, "", nil
			}
		case "approve":
			if gate.CanOperate {
				return runGateApprove, "", nil
			}
		case "changes", "request-changes":
			if gate.CanOperate || gate.CanRequestChanges {
				feedback, feedbackErr := readRunPrompt(ctx, answers, output, "Feedback: ", styled)
				if feedbackErr != nil {
					return runGateStop, "", feedbackErr
				}
				if strings.TrimSpace(feedback) != "" {
					return runGateRequestChanges, strings.TrimSpace(feedback), nil
				}
				_, _ = fmt.Fprintln(output, "Feedback is required to request changes.")
				continue
			}
		}
		_, _ = fmt.Fprintf(output, "Type one of: %s. Approval requires the full word approve.\n", actions)
	}
}

type runInputResult struct {
	value string
	err   error
}

type runInputSource struct {
	results <-chan runInputResult
}

func newRunInputSource(reader *bufio.Reader) *runInputSource {
	results := make(chan runInputResult)
	go func() {
		defer close(results)
		for {
			value, err := reader.ReadString('\n')
			results <- runInputResult{value: value, err: err}
			if err != nil {
				return
			}
		}
	}()
	return &runInputSource{results: results}
}

func readRunPrompt(ctx context.Context, answers *runInputSource, output io.Writer, prompt string, styled bool) (string, error) {
	answer, _, err := readRunPromptOrPoll(ctx, answers, output, prompt, styled, 0)
	return answer, err
}

func readRunPromptOrPoll(ctx context.Context, answers *runInputSource, output io.Writer, prompt string, styled bool, poll time.Duration) (string, bool, error) {
	if styled {
		prompt = newCLIPalette(output).accent.Render(prompt)
	}
	if _, err := fmt.Fprint(output, prompt); err != nil {
		return "", false, err
	}
	var timer <-chan time.Time
	if poll > 0 {
		timer = time.After(poll)
	}
	select {
	case <-ctx.Done():
		_, _ = fmt.Fprintln(output)
		return "", false, nil
	case <-timer:
		return "", true, nil
	case value, ok := <-answers.results:
		if !ok {
			return "", false, nil
		}
		if value.err != nil && len(value.value) == 0 && value.err != io.EOF {
			return "", false, value.err
		}
		return value.value, false, nil
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

func outputIsTerminal(output io.Writer) bool {
	file, ok := output.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(file.Fd()))
}

func presentRunOrder(output io.Writer, item workerservice.DispatchOrder, timeout string) error {
	return presentRunOrderStyled(output, item, timeout, false)
}

func presentRunOrderStyled(output io.Writer, item workerservice.DispatchOrder, timeout string, styled bool) error {
	if err := presentPendingRunOrderStyled(output, item, styled); err != nil {
		return err
	}
	effort := item.Effort
	if effort == "" {
		effort = "default"
	}
	line := fmt.Sprintf("Execution: harness %s, model %s, effort %s, timeout %s", item.Harness.Name, item.Model, effort, timeout)
	if styled {
		line = newCLIPalette(output).muted.Render(line)
	}
	_, err := fmt.Fprintln(output, line)
	return err
}

func presentPendingRunOrder(output io.Writer, item workerservice.DispatchOrder) error {
	return presentPendingRunOrderStyled(output, item, false)
}

func presentPendingRunOrderStyled(output io.Writer, item workerservice.DispatchOrder, styled bool) error {
	if !styled {
		_, err := fmt.Fprintf(output, "Task %s: %s (state %s)\nNext: %s work order %s\n", item.Task.ID, item.Task.Title, item.Task.State, item.Order.Stage, item.Order.ID)
		return err
	}
	palette := newCLIPalette(output)
	if _, err := fmt.Fprintln(output, palette.title.Render(fmt.Sprintf("Task %s · %s", item.Task.ID, item.Task.Title))); err != nil {
		return err
	}
	_, err := fmt.Fprintln(output, palette.accent.Render(strings.ToUpper(string(item.Order.Stage)))+fmt.Sprintf("  order %s  state %s", item.Order.ID, item.Task.State))
	return err
}

func confirmRunStage(ctx context.Context, input *bufio.Reader, output io.Writer, stage core.Stage) (bool, error) {
	return confirmRunStageStyled(ctx, input, output, stage, false)
}

func confirmRunStageStyled(ctx context.Context, input *bufio.Reader, output io.Writer, stage core.Stage, styled bool) (bool, error) {
	return confirmRunStageFromSourceStyled(ctx, newRunInputSource(input), output, stage, styled)
}

func confirmRunStageFromSourceStyled(ctx context.Context, answers *runInputSource, output io.Writer, stage core.Stage, styled bool) (bool, error) {
	for {
		prompt := fmt.Sprintf("Proceed with %s? [y/N]: ", stage)
		if styled {
			prompt = newCLIPalette(output).accent.Render(prompt)
		}
		if _, err := fmt.Fprint(output, prompt); err != nil {
			return false, err
		}
		var answer string
		var err error
		select {
		case <-ctx.Done():
			_, _ = fmt.Fprintln(output)
			return false, nil
		case read, ok := <-answers.results:
			if !ok {
				_, _ = fmt.Fprintln(output)
				return false, nil
			}
			answer, err = read.value, read.err
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
	return printRunSummaryStyled(output, task, stages, false)
}

func printRunSummaryStyled(output io.Writer, task core.Task, stages []core.Stage, styled bool) error {
	ran := "none"
	if len(stages) > 0 {
		names := make([]string, len(stages))
		for index, stage := range stages {
			names[index] = string(stage)
		}
		ran = strings.Join(names, ", ")
	}
	message := fmt.Sprintf("Run stopped before claiming the next work order.\nRan: %s\nTask %s is currently %s.\nResume: conveyor run %s", ran, task.ID, task.State, task.ID)
	if styled {
		message = newCLIPalette(output).muted.Render(message)
	}
	_, err := fmt.Fprintln(output, message)
	return err
}

func presentTaskRunGateStyled(output io.Writer, task core.Task, gate workerservice.TaskRunGate, styled bool) error {
	rows := [][2]string{
		{"Task", fmt.Sprintf("%s · %s", task.ID, task.Title)},
		{"Waiting", gate.Label},
		{"Artifact", gate.Summary},
		{"Claim", "none (polling recorded factory state)"},
	}
	if gate.Rationale != "" {
		rows = append(rows, [2]string{"Rationale", gate.Rationale})
	}
	return renderCLIStatusRows(output, styled, rows...)
}

func presentRunIdleStateStyled(output io.Writer, task core.Task, styled bool) error {
	if err := renderCLIStatusRows(output, styled,
		[2]string{"Task", fmt.Sprintf("%s · %s", task.ID, task.Title)},
		[2]string{"State", string(task.State)},
	); err != nil {
		return err
	}
	_, err := fmt.Fprintln(output, "No claimable stage or human gate is pending.")
	return err
}

func printFinalRunSummaryStyled(output io.Writer, task core.Task, stages []core.Stage, styled bool) error {
	ran := "none"
	if len(stages) > 0 {
		names := make([]string, len(stages))
		for index, stage := range stages {
			names[index] = string(stage)
		}
		ran = strings.Join(names, ", ")
	}
	message := fmt.Sprintf("Task %s finished in state %s.\nRan: %s", task.ID, task.State, ran)
	if styled {
		message = newCLIPalette(output).success.Render(message)
	}
	_, err := fmt.Fprintln(output, message)
	return err
}

func presentRunStageSummary(output io.Writer, stage core.Stage, duration time.Duration, runErr error) error {
	palette := newCLIPalette(output)
	outcome := "completed"
	mark := palette.success.Render("✓")
	submitted := map[core.Stage]string{
		core.StageSpec:      "execution plan",
		core.StageImplement: "implementation for review",
		core.StageReview:    "review verdict",
	}[stage]
	if runErr != nil {
		outcome = "failed"
		mark = palette.warning.Render("!")
		submitted = "nothing"
	}
	_, err := fmt.Fprintf(output, "%s %s stage %s in %s · submitted: %s\n", mark, stage, outcome, duration.Round(time.Second), submitted)
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
