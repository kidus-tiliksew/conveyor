package main

// Local execution setup is a client-only creation path governed by
// req-260811-0ee057 REQ-14/AC-14.1-14.3. It deliberately has no HTTP client:
// detected harness definitions and stage choices stay on the operator host.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
	"gopkg.in/yaml.v3"
)

const localExecutionSetupCommand = "conveyor config init-execution"

type localExecutionSetup struct {
	Config               *config.Config
	WorkerDocument       workerservice.WorkerConfig
	FirstActivityTimeout time.Duration
}

func defaultLocalExecutionConfigPath() string {
	path := strings.TrimSpace(os.Getenv("CONVEYOR_CONFIG"))
	if path == "" {
		path = "conveyor.yaml"
	}
	return path
}

// loadLocalExecutionSetup is the one local launch-configuration boundary used
// by both explicit conveyor run and the auto-claiming worker loop. Keeping the
// actionable remedy here prevents the two entry points from drifting.
func loadLocalExecutionSetup(path string) (localExecutionSetup, error) {
	return loadNamedLocalExecutionSetup(path, "")
}

func loadNamedLocalExecutionSetup(path, name string) (localExecutionSetup, error) {
	local, err := config.Load(path)
	if err != nil {
		return localExecutionSetup{}, fmt.Errorf("load local execution config: %w; create it with `%s --config %s`", err, localExecutionSetupCommand, path)
	}
	if strings.TrimSpace(name) != "" {
		selected, ok := local.Setup(name)
		if !ok {
			return localExecutionSetup{}, fmt.Errorf("unknown setup %q; configured setups: %s", strings.TrimSpace(name), strings.Join(configuredSetupNames(local), ", "))
		}
		local = local.WithSetup(selected)
	}
	document := workerservice.WorkerConfig{WorkspaceDocument: local.WorkspaceDocument()}
	if err = validateWorkerConfig(document); err != nil {
		return localExecutionSetup{}, fmt.Errorf("invalid local execution config: %w; recreate it with `%s --config %s`", err, localExecutionSetupCommand, path)
	}
	firstActivityTimeout, err := configuredFirstActivityTimeout(local)
	if err != nil {
		return localExecutionSetup{}, localExecutionSetupRemedy(path, err)
	}
	return localExecutionSetup{Config: local, WorkerDocument: document, FirstActivityTimeout: firstActivityTimeout}, nil
}

var wizardTerminal = inputIsTerminal

var runExecutionWizardUI = func(model executionWizardModel, input io.Reader, output io.Writer) (executionWizardModel, error) {
	result, err := tea.NewProgram(model, tea.WithInput(input), tea.WithOutput(output)).Run()
	if err != nil {
		return executionWizardModel{}, err
	}
	completed, ok := result.(executionWizardModel)
	if !ok {
		return executionWizardModel{}, errors.New("execution setup returned an unexpected UI result")
	}
	return completed, nil
}

type localStageChoice struct {
	Harness string
	Model   string
	Effort  string
	Timeout string
}

type localExecutionChoices struct {
	Spec, Implement, Review localStageChoice
	ReviewSeats             []localStageChoice
}

type wizardField struct {
	stage   string
	name    string
	seat    int
	options []string
	value   string
}

type executionWizardModel struct {
	fields     []wizardField
	field      int
	selected   int
	input      textinput.Model
	choices    localExecutionChoices
	cancelled  bool
	validation string
}

var (
	wizardTitleStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("63"))
	wizardProgressStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	wizardFieldStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	wizardSelectedStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42"))
	wizardHelpStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	wizardValidationStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196"))
)

func newExecutionWizardModel(harnesses []config.Harness) executionWizardModel {
	names := make([]string, 0, len(harnesses))
	for _, harness := range harnesses {
		names = append(names, harness.Name)
	}
	fields := make([]wizardField, 0, 12)
	defaultHarness := "codex"
	if len(names) > 0 {
		defaultHarness = names[0]
	}
	defaults := map[string]localStageChoice{
		"spec":      {Model: suggestedHarnessModel(defaultHarness, "spec"), Effort: "high", Timeout: "30m"},
		"implement": {Model: suggestedHarnessModel(defaultHarness, "implement"), Effort: "high", Timeout: "4h"},
		"review":    {Model: suggestedHarnessModel(defaultHarness, "review"), Effort: "high", Timeout: "1h"},
	}
	for _, stage := range []string{"spec", "implement", "review"} {
		choice := defaults[stage]
		fields = append(fields,
			wizardField{stage: stage, name: "harness", options: append([]string(nil), names...)},
			wizardField{stage: stage, name: "model", value: choice.Model},
			wizardField{stage: stage, name: "effort", options: []string{"high", "medium", "low"}},
			wizardField{stage: stage, name: "timeout", value: choice.Timeout},
		)
	}
	input := textinput.New()
	input.Prompt = "› "
	input.Width = 56
	input.CharLimit = 256
	input.PromptStyle = wizardSelectedStyle
	input.TextStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	input.PlaceholderStyle = lipgloss.NewStyle().Italic(true).Foreground(lipgloss.Color("244"))
	input.Cursor.SetMode(cursor.CursorStatic)
	model := executionWizardModel{fields: fields, input: input}
	model.prepareInput()
	return model
}

// newReviewSeatExecutionWizardModel keeps the established stage editor while
// replacing the singleton review choice with an ordered seat-list editor.
// The seat count adds/removes seats and the final permutation reorders them.
func newReviewSeatExecutionWizardModel(harnesses []config.Harness, seats []localStageChoice) executionWizardModel {
	model := newExecutionWizardModel(harnesses)
	model.fields = append([]wizardField(nil), model.fields[:8]...)
	review := model.choices.Review
	if len(seats) == 0 {
		defaultHarness := "codex"
		if len(harnesses) > 0 {
			defaultHarness = harnesses[0].Name
		}
		seats = []localStageChoice{{Harness: defaultHarness, Model: suggestedHarnessModel(defaultHarness, "review"), Effort: "high"}}
	}
	model.choices.Review = review
	model.choices.Review.Timeout = "1h"
	model.choices.ReviewSeats = append([]localStageChoice(nil), seats...)
	model.fields = append(model.fields,
		wizardField{stage: "review", name: "timeout", value: "1h"},
		wizardField{stage: "review", name: "seat_count", value: strconv.Itoa(len(seats))},
	)
	model.field = 0
	model.prepareInput()
	return model
}

func (m executionWizardModel) Init() tea.Cmd { return m.input.Focus() }

func (m executionWizardModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := message.(tea.KeyMsg)
	if !ok {
		if m.field < len(m.fields) && len(m.fields[m.field].options) == 0 {
			var command tea.Cmd
			m.input, command = m.input.Update(message)
			return m, command
		}
		return m, nil
	}
	if key.Type == tea.KeyCtrlC || key.Type == tea.KeyEsc {
		m.cancelled = true
		return m, tea.Quit
	}
	if m.field >= len(m.fields) {
		return m, tea.Quit
	}
	field := m.fields[m.field]
	if len(field.options) > 0 {
		switch key.Type {
		case tea.KeyUp:
			if m.selected > 0 {
				m.selected--
			}
		case tea.KeyDown:
			if m.selected+1 < len(field.options) {
				m.selected++
			}
		case tea.KeyEnter:
			m.store(field, field.options[m.selected])
			m.advance()
		}
		return m, nil
	}
	if key.Type == tea.KeyEnter {
		value := strings.TrimSpace(m.input.Value())
		if value == "" {
			value = field.value
		}
		if value == "" {
			m.validation = field.name + " is required"
			return m, nil
		}
		if field.name == "timeout" {
			parsed, err := time.ParseDuration(value)
			if err != nil || parsed <= 0 {
				m.validation = "timeout must be a positive duration"
				return m, nil
			}
		}
		if field.name == "seat_count" {
			count, err := strconv.Atoi(value)
			if err != nil || count < 1 {
				m.validation = "review seat count must be at least one"
				return m, nil
			}
			m.configureReviewSeatFields(count)
			m.advance()
			return m, nil
		}
		if field.name == "seat_order" {
			if err := m.reorderReviewSeats(value); err != nil {
				m.validation = err.Error()
				return m, nil
			}
			m.advance()
			return m, nil
		}
		m.store(field, value)
		m.advance()
		return m, nil
	}
	m.validation = ""
	var command tea.Cmd
	m.input, command = m.input.Update(message)
	return m, command
}

func (m executionWizardModel) View() string {
	if m.cancelled {
		return "Execution setup cancelled; nothing was written.\n"
	}
	if m.field >= len(m.fields) {
		return "Execution setup complete.\n"
	}
	field := m.fields[m.field]
	var view strings.Builder
	view.WriteString(wizardTitleStyle.Render("Local execution setup"))
	view.WriteString("\n")
	fmt.Fprintf(&view, "%s\n\n", wizardProgressStyle.Render(fmt.Sprintf("Step %d of %d", m.field+1, len(m.fields))))
	stageLabel := strings.ToUpper(field.stage)
	if field.seat > 0 {
		stageLabel = fmt.Sprintf("REVIEW SEAT %d", field.seat)
	}
	fmt.Fprintf(&view, "%s  %s\n\n", wizardFieldStyle.Render(stageLabel), wizardFieldStyle.Render(field.name))
	if len(field.options) > 0 {
		for index, option := range field.options {
			marker := "  "
			line := option
			if index == m.selected {
				marker = "› "
				line = wizardSelectedStyle.Render(option)
			}
			fmt.Fprintf(&view, "%s%s\n", marker, line)
		}
		view.WriteString("\n")
		view.WriteString(wizardHelpStyle.Render("↑/↓ select • enter confirm • esc cancel"))
		view.WriteString("\n")
	} else {
		view.WriteString(m.input.View())
		view.WriteString("\n\n")
		help := "type any value • enter accept • esc cancel"
		if field.name == "model" {
			help = "suggested default shown dimmed; type any model to replace it • enter accept • esc cancel"
		}
		view.WriteString(wizardHelpStyle.Render(help))
		view.WriteString("\n")
	}
	if m.validation != "" {
		fmt.Fprintf(&view, "\n%s\n", wizardValidationStyle.Render("! "+m.validation))
	}
	return view.String()
}

func (m *executionWizardModel) advance() {
	m.field++
	m.selected = 0
	m.validation = ""
	m.prepareInput()
}

func (m *executionWizardModel) prepareInput() {
	m.input.Reset()
	if m.field >= len(m.fields) || len(m.fields[m.field].options) > 0 {
		m.input.Blur()
		return
	}
	m.input.Placeholder = m.fields[m.field].value
	_ = m.input.Focus()
}

func (m *executionWizardModel) store(field wizardField, value string) {
	choice := m.stageChoice(field.stage)
	if field.seat > 0 {
		choice = &m.choices.ReviewSeats[field.seat-1]
	}
	switch field.name {
	case "harness":
		previousHarness := choice.Harness
		choice.Harness = value
		if m.field+1 < len(m.fields) && m.fields[m.field+1].stage == field.stage && m.fields[m.field+1].name == "model" {
			next := &m.fields[m.field+1]
			if field.seat == 0 || next.value == "" || next.value == suggestedHarnessModel(previousHarness, field.stage) {
				next.value = suggestedHarnessModel(value, field.stage)
			}
		}
	case "model":
		choice.Model = value
	case "effort":
		choice.Effort = value
	case "timeout":
		choice.Timeout = value
	}
}

func (m *executionWizardModel) configureReviewSeatFields(count int) {
	for len(m.choices.ReviewSeats) < count {
		seat := m.choices.Review
		if seat.Harness == "" && len(m.fields) > 0 {
			seat = localStageChoice{Harness: m.choices.Spec.Harness, Model: suggestedHarnessModel(m.choices.Spec.Harness, "review"), Effort: "high"}
		}
		m.choices.ReviewSeats = append(m.choices.ReviewSeats, seat)
	}
	m.choices.ReviewSeats = m.choices.ReviewSeats[:count]
	fields := append([]wizardField(nil), m.fields[:m.field+1]...)
	for index, seat := range m.choices.ReviewSeats {
		harnessOptions := append([]string(nil), m.availableHarnessNames()...)
		harnessOptions = preferredOptionFirst(harnessOptions, seat.Harness)
		effortOptions := preferredOptionFirst([]string{"high", "medium", "low"}, seat.Effort)
		fields = append(fields,
			wizardField{stage: "review", seat: index + 1, name: "harness", options: harnessOptions},
			wizardField{stage: "review", seat: index + 1, name: "model", value: seat.Model},
			wizardField{stage: "review", seat: index + 1, name: "effort", options: effortOptions},
		)
	}
	order := make([]string, count)
	for index := range order {
		order[index] = strconv.Itoa(index + 1)
	}
	fields = append(fields, wizardField{stage: "review", name: "seat_order", value: strings.Join(order, ",")})
	m.fields = fields
}

func (m executionWizardModel) availableHarnessNames() []string {
	for _, field := range m.fields {
		if field.name == "harness" && len(field.options) > 0 {
			return append([]string(nil), field.options...)
		}
	}
	return nil
}

func preferredOptionFirst(options []string, preferred string) []string {
	for index, option := range options {
		if option == preferred {
			return append([]string{option}, append(options[:index], options[index+1:]...)...)
		}
	}
	return options
}

func (m *executionWizardModel) reorderReviewSeats(value string) error {
	parts := strings.Split(value, ",")
	if len(parts) != len(m.choices.ReviewSeats) {
		return fmt.Errorf("seat order must list each position once, for example 1,2,3")
	}
	seen := make(map[int]bool, len(parts))
	reordered := make([]localStageChoice, len(parts))
	for index, part := range parts {
		position, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || position < 1 || position > len(parts) || seen[position] {
			return fmt.Errorf("seat order must list each position once, for example 1,2,3")
		}
		seen[position] = true
		reordered[index] = m.choices.ReviewSeats[position-1]
	}
	m.choices.ReviewSeats = reordered
	return nil
}

// harnessModelSuggestions supplies editable placeholders only. It is never a
// validation list: the selected harness probe remains the source of truth.
var harnessModelSuggestions = map[string]map[string]string{
	"codex":  {"spec": "gpt-5.6-sol", "implement": "gpt-5.6-sol", "review": "gpt-5.6-terra"},
	"claude": {"spec": "opus", "implement": "opus", "review": "opus"},
	"grok":   {"spec": "grok-code-fast-1", "implement": "grok-code-fast-1", "review": "grok-code-fast-1"},
}

func suggestedHarnessModel(harness, stage string) string {
	if stages, ok := harnessModelSuggestions[harness]; ok {
		return stages[stage]
	}
	return harness
}

func (m *executionWizardModel) stageChoice(stage string) *localStageChoice {
	switch stage {
	case "spec":
		return &m.choices.Spec
	case "implement":
		return &m.choices.Implement
	default:
		return &m.choices.Review
	}
}

func detectLocalHarnesses(ctx context.Context) []config.Harness {
	candidates := make([]config.Harness, 0, len(config.HarnessTemplates()))
	for _, template := range config.HarnessTemplates() {
		if _, err := exec.LookPath(template.Harness.ProbeCommand[0]); err == nil {
			candidates = append(candidates, template.Harness)
		}
	}
	probes := probeHarnesses(ctx, candidates)
	healthy := make(map[string]bool, len(probes))
	for _, probe := range probes {
		healthy[probe.Harness] = validLocalHarnessProbe(probe)
	}
	available := candidates[:0]
	for _, candidate := range candidates {
		if healthy[candidate.Name] {
			available = append(available, candidate)
		}
	}
	return available
}

func runExecutionSetupWizard(ctx context.Context, input io.Reader, output io.Writer, configPath, workspace string) error {
	if !wizardTerminal(input) {
		return fmt.Errorf("execution setup requires a terminal; use `conveyor config set execution.<stage>.<field> <value> --config %s`", configPath)
	}
	harnesses := detectLocalHarnesses(ctx)
	if len(harnesses) == 0 {
		return errors.New("no supported harness was found on PATH (looked for codex, claude, and grok)")
	}
	completed, err := runExecutionWizardUI(newReviewSeatExecutionWizardModel(harnesses, nil), input, output)
	if err != nil {
		return err
	}
	if completed.cancelled || completed.field < len(completed.fields) {
		_, _ = fmt.Fprintln(output, "Execution setup cancelled; nothing was written.")
		return nil
	}
	selected := selectedHarnesses(completed.choices, harnesses)
	probes := probeHarnesses(ctx, selected)
	for _, probe := range probes {
		if !validLocalHarnessProbe(probe) {
			return errors.New(wizardValidationStyle.Render(fmt.Sprintf("! harness %q failed validation probe: %s", probe.Harness, probe.Message)))
		}
	}
	if len(probes) != len(selected) {
		return errors.New("one or more selected harnesses could not be probe-validated")
	}
	if workspace == "" {
		workspace = "local"
	}
	existing, loadErr := config.Load(configPath)
	if loadErr == nil {
		if err := writeUpdatedLocalExecutionConfig(configPath, existing, completed.choices, mergeLocalHarnesses(existing.Harnesses, selected)); err != nil {
			return err
		}
	} else if errors.Is(loadErr, os.ErrNotExist) {
		if err := writeLocalExecutionConfig(configPath, workspace, completed.choices, selected); err != nil {
			return err
		}
	} else {
		return fmt.Errorf("load existing local execution config: %w", loadErr)
	}
	_, err = fmt.Fprintf(output, "Saved local execution setup to %s\n", configPath)
	return err
}

func validLocalHarnessProbe(probe core.HarnessProbe) bool {
	return probe.Healthy && probe.Fingerprint != "" && strings.TrimSpace(probe.Message) != ""
}

func mergeLocalHarnesses(existing, selected []config.Harness) []config.Harness {
	merged := append([]config.Harness(nil), existing...)
	for _, selectedHarness := range selected {
		replaced := false
		for index := range merged {
			if merged[index].Name == selectedHarness.Name {
				merged[index] = selectedHarness
				replaced = true
				break
			}
		}
		if !replaced {
			merged = append(merged, selectedHarness)
		}
	}
	return merged
}

func selectedHarnesses(choices localExecutionChoices, available []config.Harness) []config.Harness {
	wanted := map[string]bool{choices.Spec.Harness: true, choices.Implement.Harness: true, choices.Review.Harness: true}
	for _, seat := range choices.ReviewSeats {
		wanted[seat.Harness] = true
	}
	selected := make([]config.Harness, 0, len(wanted))
	for _, harness := range available {
		if wanted[harness.Name] {
			selected = append(selected, harness)
		}
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].Name < selected[j].Name })
	return selected
}

func localExecutionDocument(workspace string, choices localExecutionChoices, harnesses []config.Harness) config.WorkspaceDocument {
	reviewSeats := []config.ReviewSeat{{Harness: choices.Review.Harness, Model: choices.Review.Model, Effort: choices.Review.Effort}}
	if len(choices.ReviewSeats) > 0 {
		reviewSeats = make([]config.ReviewSeat, len(choices.ReviewSeats))
		for index, seat := range choices.ReviewSeats {
			reviewSeats[index] = config.ReviewSeat{Harness: seat.Harness, Model: seat.Model, Effort: seat.Effort}
		}
		choices.Review.Harness = choices.ReviewSeats[0].Harness
		choices.Review.Model = choices.ReviewSeats[0].Model
		choices.Review.Effort = choices.ReviewSeats[0].Effort
	}
	settings := config.ContextualExecutionSettings{
		ControlPlane: config.ControlPlaneSettings{
			Triage:   config.ModelTimeoutSettings{Model: "gpt-5.6-luna", TimeoutText: "20m"},
			Planning: config.PlanningSettings{Model: "gpt-5.6-luna", TimeoutText: "20m"},
		},
		Spec:           config.ImplementationSettings{Harness: choices.Spec.Harness, Model: choices.Spec.Model, ModelPolicy: config.ModelPolicyExplicit, Effort: choices.Spec.Effort, TimeoutText: choices.Spec.Timeout},
		Implementation: config.ImplementationSettings{Harness: choices.Implement.Harness, Model: choices.Implement.Model, ModelPolicy: config.ModelPolicyExplicit, Effort: choices.Implement.Effort, TimeoutText: choices.Implement.Timeout},
		Review:         config.ReviewExecutionSettings{Execution: config.ExecutionMCP, FallbackHarness: choices.Review.Harness, FallbackModel: choices.Review.Model, TimeoutText: choices.Review.Timeout},
	}
	return config.WorkspaceDocument{
		Workspace: workspace, ExecutionSettings: &settings, Harnesses: harnesses,
		Review:    config.ReviewPanel{Seats: reviewSeats},
		Execution: config.ExecutionPolicy{SpecApproval: true, MergeApproval: true, ImplementConcurrency: 1, ReviewConcurrency: 1, FirstActivityTimeoutText: config.DefaultFirstActivityTimeoutText},
	}
}

func writeLocalExecutionConfig(path, workspace string, choices localExecutionChoices, harnesses []config.Harness) error {
	document := localExecutionDocument(workspace, choices, harnesses)
	return writeValidatedLocalExecutionConfig(path, document)
}

func writeUpdatedLocalExecutionConfig(path string, existing *config.Config, choices localExecutionChoices, harnesses []config.Harness) error {
	document := localExecutionDocument(existing.Workspace, choices, harnesses)
	if choices.Spec.Model == "" && existing.ExecutionSettings != nil {
		document.ExecutionSettings.Spec.ModelPolicy = existing.ExecutionSettings.Spec.ModelPolicy
	}
	if choices.Implement.Model == "" && existing.ExecutionSettings != nil {
		document.ExecutionSettings.Implementation.ModelPolicy = existing.ExecutionSettings.Implementation.ModelPolicy
	}
	existing.ExecutionSettings = document.ExecutionSettings
	existing.Harnesses = harnesses
	existing.Review = document.Review
	for index := range existing.Setups {
		if existing.Setups[index].Name == existing.DefaultSetup {
			existing.Setups[index].ExecutionSettings = *document.ExecutionSettings
			existing.Setups[index].Review = document.Review
		}
	}
	return writeValidatedLocalExecutionConfig(path, existing)
}

func writeValidatedLocalExecutionConfig(path string, value any) error {
	data, err := yaml.Marshal(value)
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err = os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create local execution config directory: %w", err)
	}
	temp, err := os.CreateTemp(directory, ".conveyor-execution-*.tmp")
	if err != nil {
		return fmt.Errorf("create local execution config: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err = temp.Chmod(0o600); err == nil {
		_, err = temp.Write(data)
	}
	if err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	loaded, err := config.Load(tempPath)
	if err != nil {
		return fmt.Errorf("validate local execution config: %w", err)
	}
	if err = validateWorkerConfig(workerservice.WorkerConfig{WorkspaceDocument: loaded.WorkspaceDocument()}); err != nil {
		return fmt.Errorf("validate local execution config: %w", err)
	}
	if err = os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("publish local execution config: %w", err)
	}
	return os.Chmod(path, 0o600)
}

func setLocalExecutionField(path, workspace, key, value string) error {
	return setLocalExecutionFieldContext(context.Background(), path, workspace, key, value, false)
}

func setLocalExecutionFieldContext(ctx context.Context, path, workspace, key, value string, requireProbe bool) error {
	parts := strings.Split(key, ".")
	if len(parts) != 3 || parts[0] != "execution" {
		return errors.New("field must be execution.<stage>.<field>")
	}
	stage, field := parts[1], parts[2]
	if stage != "spec" && stage != "implement" && stage != "review" {
		return errors.New("execution stage must be spec, implement, or review")
	}
	if field != "harness" && field != "model" && field != "effort" && field != "timeout" {
		return errors.New("execution field must be harness, model, effort, or timeout")
	}
	choices, harnesses, currentWorkspace, err := readLocalExecutionConfig(path)
	var existing *config.Config
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if errors.Is(err, os.ErrNotExist) {
		defaultHarness := config.HarnessTemplates()[0].Harness
		harnesses = []config.Harness{defaultHarness}
		choices = localExecutionChoices{
			Spec:      localStageChoice{Harness: defaultHarness.Name, Model: "gpt-5.6", Effort: "high", Timeout: "30m"},
			Implement: localStageChoice{Harness: defaultHarness.Name, Model: "gpt-5.6", Effort: "high", Timeout: "4h"},
			Review:    localStageChoice{Harness: defaultHarness.Name, Model: "gpt-5.6", Effort: "high", Timeout: "1h"},
		}
	} else {
		existing, err = config.Load(path)
		if err != nil {
			return err
		}
	}
	if strings.TrimSpace(workspace) == "" {
		workspace = currentWorkspace
	}
	if workspace == "" {
		workspace = "local"
	}
	choice := &choices.Spec
	if stage == "implement" {
		choice = &choices.Implement
	} else if stage == "review" {
		choice = &choices.Review
	}
	value = strings.TrimSpace(value)
	switch field {
	case "harness":
		var selected *config.Harness
		for _, template := range config.HarnessTemplates() {
			if template.Harness.Name == value {
				candidate := template.Harness
				selected = &candidate
				break
			}
		}
		if selected == nil {
			return fmt.Errorf("unsupported harness %q; choose codex, claude, or grok", value)
		}
		choice.Harness = value
		found := false
		for index := range harnesses {
			if harnesses[index].Name == value {
				harnesses[index] = *selected
				found = true
			}
		}
		if !found {
			harnesses = append(harnesses, *selected)
		}
		if requireProbe {
			probes := probeHarnesses(ctx, []config.Harness{*selected})
			if len(probes) != 1 || !validLocalHarnessProbe(probes[0]) {
				message := "probe did not return a valid fingerprint"
				if len(probes) == 1 {
					message = probes[0].Message
				}
				return fmt.Errorf("harness %q failed validation probe: %s", value, message)
			}
		}
	case "model":
		if value == "" {
			return errors.New("model is required")
		}
		choice.Model = value
	case "effort":
		if value != "low" && value != "medium" && value != "high" {
			return errors.New("effort must be low, medium, or high")
		}
		choice.Effort = value
	case "timeout":
		parsed, parseErr := time.ParseDuration(value)
		if parseErr != nil || parsed <= config.DefaultFirstActivityTimeout {
			return fmt.Errorf("timeout must be greater than %s", config.DefaultFirstActivityTimeoutText)
		}
		choice.Timeout = value
	}
	if existing != nil {
		return writeUpdatedLocalExecutionConfig(path, existing, choices, harnesses)
	}
	return writeLocalExecutionConfig(path, workspace, choices, selectedHarnesses(choices, harnesses))
}

func readLocalExecutionConfig(path string) (localExecutionChoices, []config.Harness, string, error) {
	loaded, err := config.Load(path)
	if err != nil {
		return localExecutionChoices{}, nil, "", err
	}
	settings := loaded.ExecutionSettings
	if settings == nil {
		return localExecutionChoices{}, nil, "", errors.New("local execution config has no execution settings")
	}
	review := localStageChoice{Harness: settings.Review.FallbackHarness, Model: settings.Review.FallbackModel, Timeout: settings.Review.TimeoutText}
	if len(loaded.Review.Seats) > 0 {
		seat := loaded.Review.Seats[0]
		if seat.Harness != "" {
			review.Harness = seat.Harness
		}
		if seat.Model != "" {
			review.Model = seat.Model
		}
		if seat.Effort != "" {
			review.Effort = seat.Effort
		}
	}
	return localExecutionChoices{
		Spec:      localStageChoice{Harness: settings.Spec.Harness, Model: settings.Spec.Model, Effort: settings.Spec.Effort, Timeout: settings.Spec.TimeoutText},
		Implement: localStageChoice{Harness: settings.Implementation.Harness, Model: settings.Implementation.Model, Effort: settings.Implementation.Effort, Timeout: settings.Implementation.TimeoutText},
		Review:    review,
	}, append([]config.Harness(nil), loaded.Harnesses...), loaded.Workspace, nil
}

func printLocalExecutionConfig(output io.Writer, path string) error {
	styled := outputIsTerminal(output)
	choices, _, _, err := readLocalExecutionConfig(path)
	if errors.Is(err, os.ErrNotExist) {
		return renderCLIConfigRow(output, styled, "execution", "(not configured)", path)
	}
	if err != nil {
		return fmt.Errorf("load local execution config: %w", err)
	}
	for _, item := range []struct {
		stage  string
		choice localStageChoice
	}{{"spec", choices.Spec}, {"implement", choices.Implement}, {"review", choices.Review}} {
		for _, field := range []struct{ name, value string }{{"harness", item.choice.Harness}, {"model", item.choice.Model}, {"effort", item.choice.Effort}, {"timeout", item.choice.Timeout}} {
			if err := renderCLIConfigRow(output, styled, "execution."+item.stage+"."+field.name, field.value, "stored file "+path); err != nil {
				return err
			}
		}
	}
	return nil
}
