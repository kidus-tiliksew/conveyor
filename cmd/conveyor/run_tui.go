package main

// The attended-run terminal app: one persistent Bubble Tea program owns the
// terminal for the whole conveyor run invocation (req-260811-0ee057 AC-5.6,
// AC-5.7). Every interaction — stage previews and confirmations, the live
// agent stream, gate prompts, notices, and the child's stderr — flows through
// this model; nothing else may write to the terminal while it runs, because a
// single competing raw writer corrupts the repaint (the v0.4.1 lesson).

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
	// The output box is a fixed-height window onto the agent stream, never a
	// screen-filling pane; bounded frames are what keep repaint stable.
	runTUIBoxMaxHeight = 14
	runTUIBoxMaxWidth  = 110
	runTUINoticeLimit  = 5
)

var (
	runTUITaskStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	runTUITabStyle    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("6")).Padding(0, 1)
	runTUIMetaStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	runTUIValueStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("7"))
	runTUIWaitStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11"))
	runTUIBoxStyle    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("8"))
	runTUIPromptStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	runTUIStatusStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	runTUIHintStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	runTUINoticeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	runTUIDoneStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
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

type runTUIConfirm struct {
	stage   core.Stage
	preview []string
}

type runTUIOutputMsg string
type runTUIStderrMsg string
type runTUITickMsg time.Time
type runTUICollapseMsg struct{}
type runTUIGateMsg runTUIGate
type runTUIStageMsg runTUIStage
type runTUIStageEndMsg string
type runTUIConfirmMsg runTUIConfirm
type runTUINoticeMsg string
type runTUIIdleMsg string

type runTUIAction struct {
	decision runGateDecision
	confirm  bool
	feedback string
}

type runTUIModel struct {
	viewport  viewport.Model
	width     int
	height    int
	stage     runTUIStage
	gate      *runTUIGate
	confirm   *runTUIConfirm
	idle      string
	lines     []string
	notices   []string
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
	model.viewport.MouseWheelEnabled = true
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
		m.appendLines(string(message))
	case runTUIStderrMsg:
		m.appendLines(string(message))
	case runTUIStageMsg:
		stage := runTUIStage(message)
		m.stage = stage
		m.gate, m.confirm, m.idle = nil, nil, ""
		m.lines = nil
		m.elapsed = 0
		m.input, m.status, m.feedback = "", "", false
		m.resize()
	case runTUIStageEndMsg:
		m.pushNotice(string(message))
		m.stage.stage = ""
		m.lines = nil
		m.resize()
	case runTUIConfirmMsg:
		confirm := runTUIConfirm(message)
		m.confirm = &confirm
		m.gate, m.idle = nil, ""
		m.input, m.status = "", ""
		m.resize()
	case runTUINoticeMsg:
		m.pushNotice(string(message))
		m.resize()
	case runTUIIdleMsg:
		m.idle = string(message)
		m.confirm, m.gate = nil, nil
		m.resize()
	case runTUIGateMsg:
		gate := runTUIGate(message)
		if m.gate == nil || !sameRunTUIGate(*m.gate, gate) {
			if m.gate == nil {
				m.stage.started = time.Now()
				m.elapsed = 0
			}
			m.gate = &gate
			m.confirm, m.idle = nil, ""
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
	case tea.MouseMsg:
		updated, command := m.viewport.Update(message)
		m.viewport = updated
		return m, command
	case tea.KeyMsg:
		if message.Type == tea.KeyCtrlC {
			m.sendInterrupt()
			m.collapsed = true
			return m, tea.Quit
		}
		if m.confirm != nil {
			m.handleConfirmKey(message)
			return m, nil
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

func (m *runTUIModel) appendLines(chunk string) {
	follow := m.viewport.AtBottom()
	for _, line := range strings.Split(strings.TrimSuffix(chunk, "\n"), "\n") {
		if line != "" {
			m.lines = append(m.lines, line)
		}
	}
	m.refreshViewportContent()
	if follow {
		m.viewport.GotoBottom()
	}
}

func (m *runTUIModel) pushNotice(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	m.notices = append(m.notices, line)
	if len(m.notices) > runTUINoticeLimit {
		m.notices = m.notices[len(m.notices)-runTUINoticeLimit:]
	}
}

func (m *runTUIModel) handleConfirmKey(key tea.KeyMsg) {
	switch strings.ToLower(key.String()) {
	case "y":
		m.sendAction(runTUIAction{decision: runConfirmStage, confirm: true})
	case "n", "esc", "enter":
		m.sendAction(runTUIAction{decision: runConfirmStage, confirm: false})
	}
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
		if len(key.Runes) == 1 && (key.Runes[0] == 'k' || key.Runes[0] == 'j') && m.input == "" && len(m.lines) > 0 {
			return false // let bare j/k scroll stream scrollback under a gate
		}
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
	sections := []string{m.header()}
	if body := m.noticeBlock(); body != "" {
		sections = append(sections, body)
	}
	sections = append(sections, m.statusBlock())
	if m.showOutputBox() {
		sections = append(sections, m.outputBox())
	}
	if prompt := m.promptBlock(); prompt != "" {
		sections = append(sections, prompt)
	}
	sections = append(sections, m.footer())
	return lipgloss.NewStyle().MaxWidth(max(1, m.width)).Render(strings.Join(sections, "\n"))
}

// header renders the pager-style task tab.
func (m runTUIModel) header() string {
	task := m.stage.task
	if m.gate != nil {
		task = m.gate.task
	}
	tab := runTUITabStyle.Render(runTUITaskStyle.Render("Task " + task.ID))
	title := runTUIValueStyle.Render(runTUITruncate(task.Title, max(1, m.width-lipgloss.Width(tab)-2)))
	return lipgloss.JoinHorizontal(lipgloss.Center, tab, " "+title)
}

func (m runTUIModel) noticeBlock() string {
	if len(m.notices) == 0 {
		return ""
	}
	rendered := make([]string, len(m.notices))
	for i, notice := range m.notices {
		rendered[i] = runTUINoticeStyle.Render(runTUITruncate(notice, max(1, m.width-2)))
	}
	return strings.Join(rendered, "\n")
}

func (m runTUIModel) statusBlock() string {
	switch {
	case m.confirm != nil:
		lines := make([]string, 0, len(m.confirm.preview))
		for _, line := range m.confirm.preview {
			lines = append(lines, runTUIValueStyle.Render(runTUITruncate(line, max(1, m.width-2))))
		}
		return strings.Join(lines, "\n")
	case m.gate != nil:
		lines := []string{
			runTUIWaitStyle.Render(fmt.Sprintf("Waiting %s · %s", m.gate.gate.Label, m.elapsed)),
			runTUIMeta("Artifact", m.gate.gate.Summary),
		}
		if m.gate.gate.Rationale != "" {
			lines = append(lines, runTUIMeta("Rationale", m.gate.gate.Rationale))
		}
		if m.stage.stage != "" {
			lines = append(lines, runTUIMetaStyle.Render(fmt.Sprintf("Last stage %s · %s · %s", m.stage.stage, m.stage.harness, m.stage.model)))
		}
		lines = append(lines, runTUIHintStyle.Render("No claim held — factory state refreshes automatically; Ctrl+C exits safely."))
		return strings.Join(lines, "\n")
	case m.idle != "":
		return runTUIWaitStyle.Render(m.idle+" ") + runTUIHintStyle.Render(runTUISpinnerFrame(m.elapsed))
	default:
		stage := string(m.stage.stage)
		if stage == "" {
			stage = "starting"
		}
		return runTUIMeta(
			"Stage", stage, "Harness", m.stage.harness, "Model", m.stage.model, "Elapsed", m.elapsed.String(),
		)
	}
}

func (m runTUIModel) outputBox() string {
	inner := m.viewport.View()
	if len(m.lines) == 0 {
		inner = runTUIHintStyle.Italic(true).Render("waiting for agent output…")
	}
	box := runTUIBoxStyle.Width(m.viewport.Width).Render(inner)
	if m.width > lipgloss.Width(box) {
		box = lipgloss.PlaceHorizontal(m.width, lipgloss.Center, box)
	}
	return box
}

func (m runTUIModel) promptBlock() string {
	switch {
	case m.confirm != nil:
		return runTUIPromptStyle.Render(fmt.Sprintf("Proceed with %s? [y/N]", m.confirm.stage))
	case m.gate != nil:
		prompt := runTUIPromptStyle.Render(fmt.Sprintf("Gate action [%s]:", m.gateActions())) + " " + m.input + "▌"
		if m.feedback {
			prompt = runTUIPromptStyle.Render("Feedback:") + " " + m.input + "▌"
		}
		if m.status != "" {
			prompt += "\n" + runTUIStatusStyle.Render(m.status)
		}
		return prompt
	}
	return ""
}

func (m runTUIModel) footer() string {
	parts := []string{"Ctrl+C exit"}
	if m.showOutputBox() && len(m.lines) > 0 {
		parts = append([]string{fmt.Sprintf("%3.0f%%", m.viewport.ScrollPercent()*100), "↑/↓ · PgUp/PgDn · wheel scroll"}, parts...)
	}
	if m.gate != nil {
		parts = append(parts, "type an action + Enter")
	}
	return runTUIHintStyle.Render(strings.Join(parts, " · "))
}

func runTUISpinnerFrame(elapsed time.Duration) string {
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧"}
	return frames[int(elapsed.Seconds())%len(frames)]
}

func runTUIMeta(pairs ...string) string {
	parts := make([]string, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, runTUIMetaStyle.Render(pairs[i]+" ")+runTUIValueStyle.Render(pairs[i+1]))
	}
	return strings.Join(parts, runTUIMetaStyle.Render("  "))
}

// showOutputBox reports whether the frame renders the output window: while a
// stage executes always, and elsewhere only when a stream actually produced
// lines — an empty box under a gate would just push the prompt to the floor.
func (m runTUIModel) showOutputBox() bool {
	if m.confirm != nil || m.idle != "" {
		return false
	}
	return m.gate == nil || len(m.lines) > 0
}

func (m *runTUIModel) resize() {
	if m.width < 1 {
		m.width = 1
	}
	if m.height < 1 {
		m.height = 1
	}
	m.viewport.Width = max(1, min(m.width, runTUIBoxMaxWidth)-2)
	reserved := lipgloss.Height(m.header()) + lipgloss.Height(m.statusBlock()) + 1 // footer
	if body := m.noticeBlock(); body != "" {
		reserved += lipgloss.Height(body)
	}
	if m.showOutputBox() {
		reserved += 2 // box border rows
	}
	if prompt := m.promptBlock(); prompt != "" {
		reserved += lipgloss.Height(prompt)
	}
	m.viewport.Height = max(1, min(runTUIBoxMaxHeight, m.height-reserved-1))
	m.refreshViewportContent()
}

// refreshViewportContent re-renders the stored raw lines at the current box
// width. Lines are hard-truncated so a long line can never wrap inside the
// border and silently grow the frame past the terminal height.
func (m *runTUIModel) refreshViewportContent() {
	if len(m.lines) == 0 {
		m.viewport.SetContent("")
		return
	}
	rendered := make([]string, len(m.lines))
	for i, line := range m.lines {
		rendered[i] = runTUITruncate(line, m.viewport.Width)
	}
	m.viewport.SetContent(strings.Join(rendered, "\n"))
}

func runTUITruncate(line string, width int) string {
	if width < 1 {
		return ""
	}
	runes := []rune(line)
	if len(runes) <= width {
		return line
	}
	if width == 1 {
		return "…"
	}
	return string(runes[:width-1]) + "…"
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
	options := []tea.ProgramOption{tea.WithContext(ctx), tea.WithOutput(output), tea.WithoutSignalHandler(), tea.WithMouseCellMotion()}
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

// StderrWriter adapts the child's stderr into model messages so stray child
// diagnostics can never write raw to the terminal under the program.
type runTUIStderrWriter struct{ controller *runTUIController }

func (w runTUIStderrWriter) Write(p []byte) (int, error) {
	w.controller.program.Send(runTUIStderrMsg(append([]byte(nil), p...)))
	return len(p), nil
}

func (c *runTUIController) Stderr() io.Writer { return runTUIStderrWriter{controller: c} }

func (c *runTUIController) StartStage(stage runTUIStage) { c.program.Send(runTUIStageMsg(stage)) }

func (c *runTUIController) EndStage(summary string) { c.program.Send(runTUIStageEndMsg(summary)) }

func (c *runTUIController) Notice(line string) { c.program.Send(runTUINoticeMsg(line)) }

func (c *runTUIController) Idle(state string) { c.program.Send(runTUIIdleMsg(state)) }

func (c *runTUIController) UpdateGate(gate runTUIGate) {
	c.program.Send(runTUIGateMsg(gate))
}

// Confirm presents a stage preview and blocks until the user answers y/N,
// the program ends, or the context cancels.
func (c *runTUIController) Confirm(ctx context.Context, stage core.Stage, preview []string) (bool, error) {
	c.drainActions()
	c.program.Send(runTUIConfirmMsg(runTUIConfirm{stage: stage, preview: preview}))
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-c.interrupt:
		return false, nil
	case <-c.finished:
		return false, c.result
	case action := <-c.actions:
		return action.confirm, nil
	}
}

func (c *runTUIController) drainActions() {
	for {
		select {
		case <-c.actions:
		default:
			return
		}
	}
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
