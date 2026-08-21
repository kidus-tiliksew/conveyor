package main

import (
	"bufio"
	"bytes"
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
	setupName := ""
	auto := false
	raw := false
	cmd := &cobra.Command{
		Use:   "run <task-id>",
		Short: "Explicitly claim and execute one task on this machine",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resolvedConfig, err := resolveLocalExecutionConfigPath(cmd, configPath)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			input := cmd.InOrStdin()
			output := cmd.OutOrStdout()
			return runTaskWithPresentationAndSetup(ctx, newClient(), args[0], resolvedConfig.Path, setupName, input, output, auto, inputIsTerminal(input), outputIsTerminal(output), raw)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", configPath, "local execution configuration")
	cmd.Flags().StringVar(&setupName, "setup", "", "named local execution setup (does not change the persisted default)")
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
	return runTaskWithPresentationAndSetup(ctx, c, taskID, configPath, "", input, output, auto, inputTerminal, outputTerminal, raw)
}

func runTaskWithPresentationAndSetup(ctx context.Context, c *client, taskID, configPath, setupName string, input io.Reader, output io.Writer, auto, inputTerminal, outputTerminal, raw bool) error {
	if strings.TrimSpace(c.token) == "" {
		return fmt.Errorf("CONVEYOR_API_TOKEN is required for task execution")
	}
	if err := c.preflightForgeToken(ctx, c.token); err != nil {
		return err
	}
	reader := bufio.NewReader(input)
	answers := newRunInputSource(reader)
	runStages := make([]core.Stage, 0, 2)
	attached := inputTerminal && outputTerminal
	interactiveTUI := attached && !raw
	var lastTUIStage runTUIStage
	var setup localExecutionSetup
	setupLoaded := false
	var lastStage core.Stage
	// One persistent program owns the terminal for the whole attached run;
	// every attached-path print below must route through it while it lives.
	var app *runTUIController
	stopApp := func() {
		if app != nil {
			_ = app.Stop()
			app = nil
		}
	}
	defer stopApp()
	ensureApp := func(task core.Task) *runTUIController {
		if app == nil {
			var tuiInput io.Reader
			if _, ok := input.(*os.File); ok {
				tuiInput = input
			} else {
				tuiInput = reader
			}
			app = startRunTUI(ctx, tuiInput, output, runTUIStage{task: task}, nil, strings.TrimRight(c.base, "/")+"/tasks/"+task.ID)
		}
		return app
	}
	for {
		item, err := c.getTaskRunOrderContext(ctx, c.token, taskID)
		if err != nil {
			stopApp()
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
				stopApp()
				_, err = fmt.Fprintf(output, "task %s has no claimable spec, implement, or review order\n", taskID)
				return err
			}
			if item.Task.State == core.TaskMerged || item.Task.State == core.TaskClosed || item.Task.State == core.TaskParked {
				stopApp()
				return printFinalRunSummaryStyled(output, item.Task, runStages, outputTerminal)
			}
			if item.Gate == nil && (!interactiveTUI || len(item.PendingProposals) == 0) {
				if interactiveTUI {
					ensureApp(item.Task).Idle("Waiting for the factory — no claimable stage or pending gate")
				} else if err = presentRunIdleStateStyled(output, item.Task, outputTerminal); err != nil {
					return err
				}
				select {
				case <-ctx.Done():
					stopApp()
					return printRunSummaryStyled(output, item.Task, runStages, outputTerminal)
				case <-time.After(runGatePollInterval):
					continue
				}
			}
			var decision runGateDecision
			var feedback string
			var waitErr error
			if interactiveTUI {
				decision, feedback, waitErr = waitAtTaskRunGateAttached(ctx, c, ensureApp(item.Task), *item, lastTUIStage)
			} else {
				decision, feedback, waitErr = waitAtTaskRunGate(ctx, answers, output, *item, outputTerminal)
			}
			if waitErr != nil {
				stopApp()
				return waitErr
			}
			switch decision {
			case runGateStop:
				stopApp()
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
					if app != nil {
						app.Notice("Gate state changed; refreshing task state.")
					} else {
						_, _ = fmt.Fprintln(output, "Gate state changed; refreshing task state.")
					}
					continue
				}
				stopApp()
				return err
			}
			if app != nil {
				app.Notice("Gate decision recorded; refreshing task state.")
			} else {
				_, _ = fmt.Fprintln(output, "Gate decision recorded; refreshing task state.")
			}
			continue
		}

		if !setupLoaded {
			setup, err = loadNamedLocalExecutionSetup(configPath, setupName)
			if err != nil {
				stopApp()
				_ = presentPendingRunOrderStyled(output, *item, outputTerminal)
				return err
			}
			if strings.TrimSpace(setupName) != "" {
				if err = probeLocalExecutionConfig(ctx, setup.Config); err != nil {
					stopApp()
					_ = presentPendingRunOrderStyled(output, *item, outputTerminal)
					return fmt.Errorf("setup %q failed pre-claim harness probe: %w", setupName, err)
				}
			}
			setupLoaded = true
		}
		local := setup.Config
		selected, selectErr := selectLocalRunDispatch(*item, local)
		if selectErr != nil {
			stopApp()
			_ = presentPendingRunOrderStyled(output, *item, outputTerminal)
			var capacity *reviewSeatCapacityError
			if errors.As(selectErr, &capacity) {
				_, err = fmt.Fprintf(output, "Review round requires seat %d, but setup %q configures %d seat(s); the order was left queued and nothing was claimed.\n", capacity.Required, local.DefaultSetup, capacity.Configured)
				return err
			}
			return localExecutionSetupRemedy(configPath, selectErr)
		}
		stageTimeout := local.Routing.Stages[string(selected.Order.Stage)].TimeoutText
		if !interactiveTUI {
			if err = presentRunOrderStyled(output, selected, stageTimeout, outputTerminal); err != nil {
				return err
			}
		}
		mode := runModeAuto
		if !auto {
			mode = runModeConfirmed
			if !inputTerminal {
				_, _ = fmt.Fprintf(output, "No work order was claimed because stdin is not a terminal.\nRun conveyor run %s --auto to proceed.\n", taskID)
				return fmt.Errorf("stage confirmation requires a terminal; use conveyor run %s --auto", taskID)
			}
			var confirmed bool
			var confirmErr error
			if interactiveTUI {
				confirmed, confirmErr = ensureApp(selected.Task).Confirm(ctx, selected.Order.Stage, runOrderPreviewLines(selected, stageTimeout))
			} else {
				confirmed, confirmErr = confirmRunStageFromSourceStyled(ctx, answers, output, selected.Order.Stage, outputTerminal)
			}
			if confirmErr != nil {
				stopApp()
				return confirmErr
			}
			if !confirmed {
				stopApp()
				return printRunSummaryStyled(output, selected.Task, runStages, outputTerminal)
			}
		}
		started := time.Now()
		lastTUIStage = runTUIStage{task: selected.Task, stage: selected.Order.Stage, harness: selected.Harness.Name, model: selected.Model, started: started}
		stageCtx, cancelStage := context.WithCancel(ctx)
		presentation := &runOutputPresentation{output: output}
		var childStderr io.Writer
		if interactiveTUI {
			stageApp := ensureApp(selected.Task)
			stageApp.StartStage(lastTUIStage)
			presentation = &runOutputPresentation{output: stageApp, notice: stageApp.Notice, presentEvents: true}
			childStderr = stageApp.Stderr()
			go func() {
				select {
				case <-stageApp.interrupt:
					cancelStage()
				case <-stageCtx.Done():
				}
			}()
		}
		runChild := func() error {
			return runHarnessChildWithFirstActivityTimeoutAndRunModeAndPresentationAndStderr(stageCtx, c, c.token, selected, setup.FirstActivityTimeout, mode, presentation, childStderr)
		}
		var runErr error
		if interactiveTUI {
			runErr = runStageWithTaskProposalPolling(stageCtx, c, c.token, selected.Task.ID, selected.PendingProposals, ensureApp(selected.Task), runChild)
		} else {
			runErr = runChild()
		}
		cancelStage()
		summary := renderRunStageSummary(selected.Order.Stage, time.Since(started), runErr)
		if app != nil {
			app.EndStage(summary)
		} else if outputTerminal && !raw {
			_, _ = fmt.Fprintln(output, summary)
		}
		if runErr != nil {
			stopApp()
			return runErr
		}
		runStages = append(runStages, selected.Order.Stage)
		lastStage = selected.Order.Stage
	}
}

func waitAtTaskRunGateAttached(ctx context.Context, c *client, controller *runTUIController, item workerservice.DispatchOrder, stage runTUIStage) (runGateDecision, string, error) {
	if item.Gate == nil && len(item.PendingProposals) == 0 {
		return runGatePoll, "", nil
	}
	controller.drainActions()
	presentation := taskProposalPresentation{actions: controller.actions, update: controller.UpdateProposals, notice: controller.Notice}
	controller.UpdateProposals(item.PendingProposals)
	if item.Gate != nil {
		controller.UpdateGate(runTUIGate{task: item.Task, gate: *item.Gate})
	} else {
		controller.ClearGate()
	}
	ticker := time.NewTicker(runGatePollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return runGateStop, "", nil
		case <-controller.interrupt:
			return runGateStop, "", nil
		case <-controller.finished:
			if err := controller.result; err != nil && err != context.Canceled {
				return runGateStop, "", err
			}
			return runGateStop, "", nil
		case action := <-presentation.actions:
			if action.decision != runConfirmProposal || action.proposal == nil {
				return action.decision, action.feedback, nil
			}
			confirmTaskRunProposal(ctx, c, c.token, item.Task.ID, *action.proposal, presentation)
		case <-ticker.C:
			fresh, err := c.getTaskRunOrderContext(ctx, c.token, item.Task.ID)
			if err != nil {
				return runGateStop, "", err
			}
			if fresh == nil || fresh.Order.ID != "" || fresh.Task.State == core.TaskMerged || fresh.Task.State == core.TaskClosed || fresh.Task.State == core.TaskParked {
				return runGatePoll, "", nil
			}
			controller.UpdateProposals(fresh.PendingProposals)
			if fresh.Gate != nil {
				controller.UpdateGate(runTUIGate{task: fresh.Task, gate: *fresh.Gate})
			} else {
				controller.ClearGate()
				if len(fresh.PendingProposals) == 0 {
					return runGatePoll, "", nil
				}
			}
		}
	}
}

func runStageWithTaskProposalPolling(ctx context.Context, c *client, credential, taskID string, initial []workerservice.TaskRunProposal, controller *runTUIController, run func() error) error {
	controller.drainActions()
	presentation := taskProposalPresentation{actions: controller.actions, update: controller.UpdateProposals, notice: controller.Notice}
	return runStageWithTaskProposalPresentation(ctx, c, credential, taskID, initial, presentation, run)
}

type taskProposalPresentation struct {
	actions <-chan runTUIAction
	update  func([]workerservice.TaskRunProposal)
	notice  func(string)
}

func runStageWithTaskProposalPresentation(ctx context.Context, c *client, credential, taskID string, initial []workerservice.TaskRunProposal, presentation taskProposalPresentation, run func() error) error {
	presentation.update(initial)
	result := make(chan error, 1)
	go func() { result <- run() }()
	ticker := time.NewTicker(runGatePollInterval)
	defer ticker.Stop()
	for {
		select {
		case err := <-result:
			return err
		case action := <-presentation.actions:
			if action.decision == runConfirmProposal && action.proposal != nil {
				confirmTaskRunProposal(ctx, c, credential, taskID, *action.proposal, presentation)
			}
		case <-ticker.C:
			fresh, err := c.getTaskRunOrderContext(ctx, credential, taskID)
			if err != nil {
				presentation.notice("Pending proposal refresh failed; the run is continuing: " + err.Error())
				continue
			}
			if fresh == nil {
				presentation.update(nil)
				continue
			}
			presentation.update(fresh.PendingProposals)
		}
	}
}

func confirmTaskRunProposal(ctx context.Context, c *client, credential, taskID string, proposal workerservice.TaskRunProposal, presentation taskProposalPresentation) {
	err := c.confirmTaskRunProposalContext(ctx, credential, taskID, proposal)
	if err != nil {
		var response *workerHTTPError
		if errors.As(err, &response) && (response.StatusCode == http.StatusConflict || response.StatusCode == http.StatusNotFound) {
			presentation.notice("Proposal state changed before confirmation; refreshing pending proposals.")
		} else {
			presentation.notice("Proposal confirmation was not recorded: " + err.Error())
		}
	} else {
		presentation.notice(fmt.Sprintf("Confirmed %s %s v%d; refreshing pending proposals.", proposal.Kind, proposal.DocumentID, proposal.Version))
	}
	fresh, refreshErr := c.getTaskRunOrderContext(ctx, credential, taskID)
	if refreshErr != nil {
		presentation.notice("Pending proposal refresh failed; the run is continuing: " + refreshErr.Error())
		return
	}
	if fresh == nil {
		presentation.update(nil)
		return
	}
	presentation.update(fresh.PendingProposals)
}

type runGateDecision int

const (
	runGatePoll runGateDecision = iota
	runGateApprove
	runGateRequestChanges
	runGateStop
	runConfirmStage
	runConfirmProposal
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
	reader  *bufio.Reader
	results chan runInputResult
	pending bool
}

func newRunInputSource(reader *bufio.Reader) *runInputSource {
	return &runInputSource{reader: reader, results: make(chan runInputResult, 1)}
}

func (s *runInputSource) startRead() {
	if s.pending {
		return
	}
	s.pending = true
	go func() {
		value, err := s.reader.ReadString('\n')
		s.results <- runInputResult{value: value, err: err}
	}()
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
	answers.startRead()
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
	case value := <-answers.results:
		answers.pending = false
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

// runOrderPreviewLines renders the pre-claim order presentation as plain
// lines for the attached app's confirm panel.
func runOrderPreviewLines(item workerservice.DispatchOrder, timeout string) []string {
	var buffer bytes.Buffer
	_ = presentRunOrderStyled(&buffer, item, timeout, false)
	return strings.Split(strings.TrimSuffix(buffer.String(), "\n"), "\n")
}

// renderRunStageSummary is the stage summary as a single plain line, for the
// attached app's notice log.
func renderRunStageSummary(stage core.Stage, duration time.Duration, runErr error) string {
	var buffer bytes.Buffer
	_ = presentRunStageSummary(&buffer, stage, duration, runErr)
	return strings.TrimSuffix(buffer.String(), "\n")
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
		answers.startRead()
		var answer string
		var err error
		select {
		case <-ctx.Done():
			_, _ = fmt.Fprintln(output)
			return false, nil
		case read := <-answers.results:
			answers.pending = false
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
	if item.Order.Stage == core.StageReview && item.Order.ReviewSeat > len(local.Review.Seats) {
		return item, &reviewSeatCapacityError{Required: item.Order.ReviewSeat, Configured: len(local.Review.Seats)}
	}
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

type reviewSeatCapacityError struct {
	Required   int
	Configured int
}

func probeLocalExecutionConfig(ctx context.Context, local *config.Config) error {
	wanted := make(map[string]bool)
	for _, route := range local.Routing.Stages {
		if route.Harness != "" {
			wanted[route.Harness] = true
		}
	}
	for _, seat := range local.Review.Seats {
		if seat.Harness != "" {
			wanted[seat.Harness] = true
		}
	}
	candidates := make([]config.Harness, 0, len(wanted))
	for _, harness := range local.Harnesses {
		if wanted[harness.Name] {
			candidates = append(candidates, harness)
		}
	}
	probes := probeHarnesses(ctx, candidates)
	if len(probes) != len(candidates) {
		return errors.New("one or more selected harnesses could not be probe-validated")
	}
	for _, probe := range probes {
		if !validLocalHarnessProbe(probe) {
			return fmt.Errorf("harness %q failed validation probe: %s", probe.Harness, probe.Message)
		}
	}
	return nil
}

func (e *reviewSeatCapacityError) Error() string {
	return fmt.Sprintf("review round requires seat %d but local setup configures %d seat(s)", e.Required, e.Configured)
}
