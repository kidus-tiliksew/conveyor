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
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
	"gopkg.in/yaml.v3"
)

const localExecutionSetupCommand = "conveyor config init-execution"

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
}

type wizardField struct {
	stage   string
	name    string
	options []string
	value   string
}

type executionWizardModel struct {
	fields     []wizardField
	field      int
	selected   int
	buffer     string
	dirty      bool
	choices    localExecutionChoices
	cancelled  bool
	validation string
}

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
	return executionWizardModel{fields: fields}
}

func (m executionWizardModel) Init() tea.Cmd { return nil }

func (m executionWizardModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := message.(tea.KeyMsg)
	if !ok {
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
	switch key.Type {
	case tea.KeyBackspace, tea.KeyDelete:
		if !m.dirty {
			m.buffer = field.value
			m.dirty = true
		}
		if len(m.buffer) > 0 {
			runes := []rune(m.buffer)
			m.buffer = string(runes[:len(runes)-1])
		}
	case tea.KeyEnter:
		value := field.value
		if m.dirty {
			value = strings.TrimSpace(m.buffer)
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
		m.store(field, value)
		m.advance()
	default:
		if key.Type == tea.KeyRunes {
			if !m.dirty {
				m.buffer = ""
				m.dirty = true
			}
			m.buffer += string(key.Runes)
			m.validation = ""
		}
	}
	return m, nil
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
	fmt.Fprintf(&view, "Local execution setup — %s %s\n\n", field.stage, field.name)
	if len(field.options) > 0 {
		for index, option := range field.options {
			cursor := "  "
			if index == m.selected {
				cursor = "> "
			}
			fmt.Fprintf(&view, "%s%s\n", cursor, option)
		}
		view.WriteString("\nUse ↑/↓ and Enter. Esc cancels.\n")
	} else {
		value := field.value
		if m.dirty {
			value = m.buffer
		}
		fmt.Fprintf(&view, "> %s\n\nEnter accepts the suggestion. Esc cancels.\n", value)
	}
	if m.validation != "" {
		fmt.Fprintf(&view, "\n%s\n", m.validation)
	}
	return view.String()
}

func (m *executionWizardModel) advance() {
	m.field++
	m.selected = 0
	m.buffer = ""
	m.dirty = false
	m.validation = ""
}

func (m *executionWizardModel) store(field wizardField, value string) {
	choice := m.stageChoice(field.stage)
	switch field.name {
	case "harness":
		choice.Harness = value
		if m.field+1 < len(m.fields) && m.fields[m.field+1].stage == field.stage && m.fields[m.field+1].name == "model" {
			m.fields[m.field+1].value = suggestedHarnessModel(value, field.stage)
		}
	case "model":
		choice.Model = value
	case "effort":
		choice.Effort = value
	case "timeout":
		choice.Timeout = value
	}
}

func suggestedHarnessModel(harness, stage string) string {
	switch harness {
	case "claude":
		return "claude-opus-4.1"
	case "grok":
		return "grok-code-fast-1"
	default:
		if stage == "review" {
			return "gpt-5.6-terra"
		}
		return "gpt-5.6-sol"
	}
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
	completed, err := runExecutionWizardUI(newExecutionWizardModel(harnesses), input, output)
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
			return fmt.Errorf("harness %q failed validation probe: %s", probe.Harness, probe.Message)
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
		Review:    config.ReviewPanel{Seats: []config.ReviewSeat{{Harness: choices.Review.Harness, Model: choices.Review.Model, Effort: choices.Review.Effort}}},
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
	choices, _, _, err := readLocalExecutionConfig(path)
	if errors.Is(err, os.ErrNotExist) {
		_, err = fmt.Fprintf(output, "execution\t(not configured)\t%s\n", path)
		return err
	}
	if err != nil {
		return fmt.Errorf("load local execution config: %w", err)
	}
	for _, item := range []struct {
		stage  string
		choice localStageChoice
	}{{"spec", choices.Spec}, {"implement", choices.Implement}, {"review", choices.Review}} {
		for _, field := range []struct{ name, value string }{{"harness", item.choice.Harness}, {"model", item.choice.Model}, {"effort", item.choice.Effort}, {"timeout", item.choice.Timeout}} {
			fmt.Fprintf(output, "execution.%s.%s\t%s\tstored file %s\n", item.stage, field.name, field.value, path)
		}
	}
	return nil
}
