package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
)

const (
	runTUIDefaultWidth  = 80
	runTUIDefaultHeight = 24
)

type runTUIStage struct {
	task    core.Task
	stage   core.Stage
	harness string
	model   string
	started time.Time
}

type runTUIGate struct {
	task core.Task
	gate workerservice.TaskRunGate
}

type runTUIOutputMsg string
type runTUITickMsg time.Time
type runTUICollapseMsg struct{}
type runTUIGateMsg runTUIGate

type runTUIAction struct {
	decision runGateDecision
	feedback string
}

type runTUIModel struct {
	viewport  viewport.Model
	width     int
	height    int
	stage     runTUIStage
	gate      *runTUIGate
	lines     []string
	elapsed   time.Duration
	input     string
	feedback  bool
	status    string
	actions   chan<- runTUIAction
	interrupt chan<- struct{}
	collapsed bool
}

func newRunTUIModel(stage runTUIStage, gate *runTUIGate, actions chan<- runTUIAction, interrupt chan<- struct{}) runTUIModel {
	model := runTUIModel{
		viewport:  viewport.New(runTUIDefaultWidth, 1),
		width:     runTUIDefaultWidth,
		height:    runTUIDefaultHeight,
		stage:     stage,
		gate:      gate,
		actions:   actions,
		interrupt: interrupt,
	}
	model.resize()
	return model
}

func (m runTUIModel) Init() tea.Cmd {
	return runTUITick()
}

func runTUITick() tea.Cmd {
	return tea.Tick(time.Second, func(now time.Time) tea.Msg { return runTUITickMsg(now) })
}

func (m runTUIModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = message.Width, message.Height
		m.resize()
	case runTUIOutputMsg:
		follow := m.viewport.AtBottom()
		for _, line := range strings.Split(strings.TrimSuffix(string(message), "\n"), "\n") {
			if line != "" {
				m.lines = append(m.lines, line)
			}
		}
		m.viewport.SetContent(strings.Join(m.lines, "\n"))
		if follow {
			m.viewport.GotoBottom()
		}
	case runTUIGateMsg:
		gate := runTUIGate(message)
		if m.gate == nil || !sameRunTUIGate(*m.gate, gate) {
			m.gate = &gate
			m.input, m.status, m.feedback = "", "", false
			m.resize()
		}
	case runTUITickMsg:
		if !m.stage.started.IsZero() {
			m.elapsed = time.Time(message).Sub(m.stage.started).Round(time.Second)
		}
		return m, runTUITick()
	case runTUICollapseMsg:
		m.collapsed = true
		return m, tea.Quit
	case tea.KeyMsg:
		if message.Type == tea.KeyCtrlC {
			m.sendInterrupt()
			m.collapsed = true
			return m, tea.Quit
		}
		if m.gate != nil {
			if m.handleGateKey(message) {
				m.resize()
				return m, nil
			}
		}
		updated, command := m.viewport.Update(message)
		m.viewport = updated
		return m, command
	}
	return m, nil
}

func (m *runTUIModel) handleGateKey(key tea.KeyMsg) bool {
	switch key.Type {
	case tea.KeyEnter:
		m.submitGateInput()
		return true
	case tea.KeyBackspace, tea.KeyDelete:
		value := []rune(m.input)
		if len(value) > 0 {
			m.input = string(value[:len(value)-1])
		}
		return true
	case tea.KeyRunes:
		m.input += string(key.Runes)
		m.status = ""
		return true
	}
	return false
}

func (m *runTUIModel) submitGateInput() {
	answer := strings.TrimSpace(m.input)
	m.input = ""
	if m.feedback {
		if answer == "" {
			m.status = "Feedback is required to request changes."
			return
		}
		m.sendAction(runTUIAction{decision: runGateRequestChanges, feedback: answer})
		return
	}
	gate := m.gate.gate
	switch strings.ToLower(answer) {
	case "":
		m.sendAction(runTUIAction{decision: runGateStop})
	case "wait":
		m.status = "Waiting; factory state continues to refresh."
	case "approve":
		if gate.CanOperate {
			m.sendAction(runTUIAction{decision: runGateApprove})
			return
		}
		m.status = "Approval is unavailable for this credential."
	case "changes", "request-changes":
		if gate.CanOperate || gate.CanRequestChanges {
			m.feedback = true
			m.status = ""
			return
		}
		m.status = "Request changes is unavailable for this credential."
	default:
		m.status = fmt.Sprintf("Type one of: %s. Approval requires the full word approve.", m.gateActions())
	}
}

func (m runTUIModel) sendAction(action runTUIAction) {
	select {
	case m.actions <- action:
	default:
	}
}

func (m runTUIModel) sendInterrupt() {
	select {
	case m.interrupt <- struct{}{}:
	default:
	}
}

func (m runTUIModel) View() string {
	if m.collapsed {
		return ""
	}
	sections := []string{m.chrome(), m.viewport.View()}
	if m.gate != nil {
		prompt := fmt.Sprintf("Gate action [%s]: %s", m.gateActions(), m.input)
		if m.feedback {
			prompt = "Feedback: " + m.input
		}
		if m.status != "" {
			prompt += "\n" + m.status
		}
		sections = append(sections, prompt)
	}
	return strings.Join(sections, "\n")
}

func (m runTUIModel) chrome() string {
	task := m.stage.task
	stage, harness, model := string(m.stage.stage), m.stage.harness, m.stage.model
	if m.gate != nil {
		task = m.gate.task
		if stage == "" {
			stage = "waiting"
		}
		if harness == "" {
			harness = "none"
		}
		if model == "" {
			model = "none"
		}
	}
	if stage == "" {
		stage = "starting"
	}
	lines := []string{
		fmt.Sprintf("Task %s · %s", task.ID, task.Title),
		fmt.Sprintf("Stage %s  Harness %s  Model %s  Elapsed %s", stage, harness, model, m.elapsed),
	}
	if m.gate != nil {
		lines = append(lines,
			fmt.Sprintf("Waiting %s", m.gate.gate.Label),
			fmt.Sprintf("Artifact %s", m.gate.gate.Summary),
			"Claim none (polling recorded factory state)",
		)
		if m.gate.gate.Rationale != "" {
			lines = append(lines, "Rationale "+m.gate.gate.Rationale)
		}
	}
	return lipgloss.NewStyle().MaxWidth(max(1, m.width)).Render(strings.Join(lines, "\n"))
}

func (m *runTUIModel) resize() {
	if m.width < 1 {
		m.width = 1
	}
	if m.height < 1 {
		m.height = 1
	}
	m.viewport.Width = m.width
	reserved := lipgloss.Height(m.chrome()) + 1
	if m.gate != nil {
		reserved += 1
		if m.status != "" {
			reserved++
		}
	}
	m.viewport.Height = max(1, m.height-reserved)
}

func (m runTUIModel) gateActions() string {
	if m.gate.gate.CanOperate {
		return "approve/changes/wait"
	}
	if m.gate.gate.CanRequestChanges {
		return "changes/wait"
	}
	return "wait"
}

func sameRunTUIGate(left, right runTUIGate) bool {
	return left.task.ID == right.task.ID && left.task.Title == right.task.Title &&
		left.gate.Kind == right.gate.Kind && left.gate.Label == right.gate.Label &&
		left.gate.Summary == right.gate.Summary && left.gate.Rationale == right.gate.Rationale &&
		left.gate.SpecVersion == right.gate.SpecVersion && left.gate.PlanVersion == right.gate.PlanVersion &&
		left.gate.CanOperate == right.gate.CanOperate && left.gate.CanRequestChanges == right.gate.CanRequestChanges
}

type runTUIController struct {
	program   *tea.Program
	actions   chan runTUIAction
	interrupt chan struct{}
	finished  chan struct{}
	result    error
	stopOnce  sync.Once
}

func startRunTUI(ctx context.Context, input io.Reader, output io.Writer, stage runTUIStage, gate *runTUIGate) *runTUIController {
	actions := make(chan runTUIAction, 1)
	interrupt := make(chan struct{}, 1)
	model := newRunTUIModel(stage, gate, actions, interrupt)
	options := []tea.ProgramOption{tea.WithContext(ctx), tea.WithOutput(output), tea.WithoutSignalHandler()}
	_, terminalInput := input.(*os.File)
	if !terminalInput {
		options = append(options, tea.WithInput(nil))
	} else {
		options = append(options, tea.WithInput(input))
	}
	controller := &runTUIController{
		program:   tea.NewProgram(model, options...),
		actions:   actions,
		interrupt: interrupt,
		finished:  make(chan struct{}),
	}
	go func() {
		_, controller.result = controller.program.Run()
		close(controller.finished)
	}()
	if input != nil && !terminalInput {
		go forwardRunTUITestInput(input, controller.program, controller.finished)
	}
	return controller
}

// Non-terminal readers are used only by lifecycle tests that explicitly mark
// their synthetic input/output as attached. Bubble Tea owns a real terminal
// directly; this small bridge keeps those tests deterministic without making
// the production program compete with the prompt reader.
func forwardRunTUITestInput(input io.Reader, program *tea.Program, finished <-chan struct{}) {
	// Let the renderer publish its initial frame before synthetic input can
	// immediately resolve the gate and collapse it.
	time.Sleep(25 * time.Millisecond)
	reader := bufio.NewReader(input)
	for {
		runeValue, _, err := reader.ReadRune()
		if err != nil {
			return
		}
		var message tea.KeyMsg
		switch runeValue {
		case '\n', '\r':
			message = tea.KeyMsg{Type: tea.KeyEnter}
		case 3:
			message = tea.KeyMsg{Type: tea.KeyCtrlC}
		case 8, 127:
			message = tea.KeyMsg{Type: tea.KeyBackspace}
		default:
			message = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{runeValue}}
		}
		select {
		case <-finished:
			return
		default:
			program.Send(message)
		}
	}
}

func (c *runTUIController) Write(p []byte) (int, error) {
	copyOfP := append([]byte(nil), p...)
	c.program.Send(runTUIOutputMsg(copyOfP))
	return len(p), nil
}

func (c *runTUIController) UpdateGate(gate runTUIGate) {
	c.program.Send(runTUIGateMsg(gate))
}

func (c *runTUIController) Stop() error {
	c.stopOnce.Do(func() { c.program.Send(runTUICollapseMsg{}) })
	<-c.finished
	err := c.result
	if err == tea.ErrProgramKilled || err == context.Canceled {
		return nil
	}
	return err
}
