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
	"github.com/charmbracelet/huh"
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

var runExecutionWizardUI = func(model *executionWizardModel, input io.Reader, output io.Writer) (*executionWizardModel, error) {
	result, err := tea.NewProgram(model, tea.WithInput(input), tea.WithOutput(output)).Run()
	if err != nil {
		return nil, err
	}
	completed, ok := result.(*executionWizardModel)
	if !ok {
		return nil, errors.New("execution setup returned an unexpected UI result")
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

type executionWizardModel struct {
	form      *huh.Form
	state     *executionWizardState
	cancelled bool
	completed bool
}

type executionWizardState struct {
	choices        localExecutionChoices
	acceptDefaults bool
	confirmSummary bool
	seatIndex      int
	seatAction     string
	focusSeats     bool
	skipDefaults   bool
}

type detectedHarness struct {
	Harness config.Harness
	Probe   core.HarnessProbe
}

func newExecutionWizardState(harnesses []detectedHarness, seats []localStageChoice) *executionWizardState {
	name := harnesses[0].Harness.Name
	for _, detected := range harnesses {
		if validLocalHarnessProbe(detected.Probe) {
			name = detected.Harness.Name
			break
		}
	}
	state := &executionWizardState{acceptDefaults: true, confirmSummary: true, seatAction: "continue"}
	state.choices = localExecutionChoices{
		Spec:      localStageChoice{Harness: name, Model: suggestedHarnessModel(name, "spec"), Effort: "high", Timeout: "30m"},
		Implement: localStageChoice{Harness: name, Model: suggestedHarnessModel(name, "implement"), Effort: "high", Timeout: "4h"},
		Review:    localStageChoice{Harness: name, Model: suggestedHarnessModel(name, "review"), Effort: "high", Timeout: "1h"},
	}
	if len(seats) == 0 {
		seats = []localStageChoice{{Harness: name, Model: suggestedHarnessModel(name, "review"), Effort: "high"}}
	}
	state.choices.ReviewSeats = append([]localStageChoice(nil), seats...)
	return state
}

func newExecutionWizardModel(state *executionWizardState, harnesses []detectedHarness) *executionWizardModel {
	if state == nil {
		state = newExecutionWizardState(harnesses, nil)
	}
	options := harnessOptions(harnesses)
	groups := []*huh.Group{
		huh.NewGroup(huh.NewConfirm().Title("Use detected defaults?").Description(defaultsSummary(state.choices)).Affirmative("Use defaults").Negative("Customize").Value(&state.acceptDefaults)).Title("Local execution setup").WithHideFunc(func() bool { return state.skipDefaults }),
		stageGroup("Spec", &state.choices.Spec, options, harnesses).WithHideFunc(func() bool { return state.acceptDefaults || state.focusSeats }),
		stageGroup("Implement", &state.choices.Implement, options, harnesses).WithHideFunc(func() bool { return state.acceptDefaults || state.focusSeats }),
		reviewStageGroup(state, options, harnesses).WithHideFunc(func() bool { return state.acceptDefaults }),
		seatManagementGroup(state).WithHideFunc(func() bool { return state.acceptDefaults }),
		huh.NewGroup(huh.NewConfirm().Title("Write this execution setup?").DescriptionFunc(func() string { return choicesSummary(state.choices) }, nil).Affirmative("Write setup").Negative("Back to edit").Value(&state.confirmSummary)).Title("Summary").WithHideFunc(func() bool { return state.acceptDefaults || state.seatAction != "continue" }),
	}
	form := huh.NewForm(groups...).WithShowHelp(true).WithShowErrors(true)
	return &executionWizardModel{form: form, state: state}
}

func (m *executionWizardModel) Init() tea.Cmd { return m.form.Init() }

func (m *executionWizardModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	updated, cmd := m.form.Update(message)
	m.form = updated.(*huh.Form)
	m.cancelled = m.form.State == huh.StateAborted
	m.completed = m.form.State == huh.StateCompleted
	if m.cancelled || m.completed {
		return m, tea.Quit
	}
	return m, cmd
}

func (m *executionWizardModel) View() string {
	if m.cancelled {
		return "Execution setup cancelled; nothing was written.\n"
	}
	return m.form.View()
}

func stageGroup(title string, choice *localStageChoice, options []huh.Option[string], harnesses []detectedHarness) *huh.Group {
	return huh.NewGroup(
		huh.NewSelect[string]().Title("Harness").Options(options...).Value(&choice.Harness).Validate(harnessProbeValidator(harnesses)),
		huh.NewInput().Title("Model").Value(&choice.Model).Validate(requiredValue("model")),
		huh.NewSelect[string]().Title("Effort").Options(huh.NewOptions("high", "medium", "low")...).Value(&choice.Effort),
		huh.NewInput().Title("Timeout").Value(&choice.Timeout).Validate(positiveDuration),
	).Title(title + " stage").Description("Tab and shift+tab move within this stage; use the form's previous key to revisit earlier stages.")
}

func reviewStageGroup(state *executionWizardState, options []huh.Option[string], harnesses []detectedHarness) *huh.Group {
	fields := []huh.Field{huh.NewInput().Title("Review timeout").Value(&state.choices.Review.Timeout).Validate(positiveDuration)}
	for index := range state.choices.ReviewSeats {
		seat := &state.choices.ReviewSeats[index]
		prefix := fmt.Sprintf("Seat %d ", index+1)
		fields = append(fields,
			huh.NewSelect[string]().Title(prefix+"harness").Options(options...).Value(&seat.Harness).Validate(harnessProbeValidator(harnesses)),
			huh.NewInput().Title(prefix+"model").Value(&seat.Model).Validate(requiredValue("model")),
			huh.NewSelect[string]().Title(prefix+"effort").Options(huh.NewOptions("high", "medium", "low")...).Value(&seat.Effort),
		)
	}
	return huh.NewGroup(fields...).Title("Review stage").Description("Seats are shown in priority order; the next step adds, removes, or moves them without typed counts or permutations.")
}

func seatManagementGroup(state *executionWizardState) *huh.Group {
	positions := make([]huh.Option[int], len(state.choices.ReviewSeats))
	for index, seat := range state.choices.ReviewSeats {
		positions[index] = huh.NewOption(fmt.Sprintf("%d: %s / %s", index+1, seat.Harness, seat.Model), index)
	}
	if state.seatIndex >= len(positions) {
		state.seatIndex = len(positions) - 1
	}
	return huh.NewGroup(
		huh.NewSelect[int]().Title("Selected seat").Options(positions...).Value(&state.seatIndex),
		huh.NewSelect[string]().Title("Seat action").Options(
			huh.NewOption("Continue to summary", "continue"),
			huh.NewOption("Add a seat after this one", "add"),
			huh.NewOption("Remove this seat", "remove"),
			huh.NewOption("Move this seat earlier", "up"),
			huh.NewOption("Move this seat later", "down"),
		).Value(&state.seatAction).Validate(func(action string) error {
			if action == "remove" && len(state.choices.ReviewSeats) == 1 {
				return errors.New("at least one review seat is required")
			}
			return nil
		}),
	).Title("Manage review seats")
}

func harnessOptions(harnesses []detectedHarness) []huh.Option[string] {
	options := make([]huh.Option[string], 0, len(harnesses))
	for _, detected := range harnesses {
		label := detected.Harness.Name + " — healthy"
		if !validLocalHarnessProbe(detected.Probe) {
			label = detected.Harness.Name + " — unavailable: " + detected.Probe.Message
		}
		options = append(options, huh.NewOption(label, detected.Harness.Name))
	}
	return options
}

func harnessProbeValidator(harnesses []detectedHarness) func(string) error {
	return func(name string) error {
		for _, detected := range harnesses {
			if detected.Harness.Name == name {
				if validLocalHarnessProbe(detected.Probe) {
					return nil
				}
				return fmt.Errorf("harness %q failed validation probe: %s", name, detected.Probe.Message)
			}
		}
		return fmt.Errorf("harness %q was not detected", name)
	}
}

func requiredValue(name string) func(string) error {
	return func(value string) error {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
		return nil
	}
}

func positiveDuration(value string) error {
	parsed, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return errors.New("timeout must be a positive duration")
	}
	return nil
}

func defaultsSummary(choices localExecutionChoices) string {
	return "Accept the first healthy detected harness and its suggested models, effort, timeouts, and one review seat.\n\n" + choicesSummary(choices)
}

func choicesSummary(choices localExecutionChoices) string {
	var summary strings.Builder
	for _, stage := range []struct {
		name   string
		choice localStageChoice
	}{{"spec", choices.Spec}, {"implement", choices.Implement}} {
		fmt.Fprintf(&summary, "%s: harness=%s model=%s effort=%s timeout=%s\n", stage.name, stage.choice.Harness, stage.choice.Model, stage.choice.Effort, stage.choice.Timeout)
	}
	fmt.Fprintf(&summary, "review: timeout=%s\n", choices.Review.Timeout)
	for index, seat := range choices.ReviewSeats {
		fmt.Fprintf(&summary, "  seat %d: harness=%s model=%s effort=%s\n", index+1, seat.Harness, seat.Model, seat.Effort)
	}
	return strings.TrimSpace(summary.String())
}

func applySeatAction(state *executionWizardState) {
	index := state.seatIndex
	if len(state.choices.ReviewSeats) == 0 || index < 0 || index >= len(state.choices.ReviewSeats) {
		state.seatAction = "continue"
		return
	}
	switch state.seatAction {
	case "add":
		seat := state.choices.ReviewSeats[index]
		state.choices.ReviewSeats = append(state.choices.ReviewSeats, localStageChoice{})
		copy(state.choices.ReviewSeats[index+2:], state.choices.ReviewSeats[index+1:])
		state.choices.ReviewSeats[index+1] = seat
		state.seatIndex = index + 1
	case "remove":
		if len(state.choices.ReviewSeats) == 1 {
			state.seatAction = "continue"
			return
		}
		state.choices.ReviewSeats = append(state.choices.ReviewSeats[:index], state.choices.ReviewSeats[index+1:]...)
		if state.seatIndex >= len(state.choices.ReviewSeats) {
			state.seatIndex = len(state.choices.ReviewSeats) - 1
		}
	case "up":
		if index > 0 {
			state.choices.ReviewSeats[index-1], state.choices.ReviewSeats[index] = state.choices.ReviewSeats[index], state.choices.ReviewSeats[index-1]
			state.seatIndex--
		}
	case "down":
		if index+1 < len(state.choices.ReviewSeats) {
			state.choices.ReviewSeats[index+1], state.choices.ReviewSeats[index] = state.choices.ReviewSeats[index], state.choices.ReviewSeats[index+1]
			state.seatIndex++
		}
	}
	state.seatAction = "continue"
	state.focusSeats = true
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

func detectLocalHarnessHealth(ctx context.Context) []detectedHarness {
	candidates := make([]config.Harness, 0, len(config.HarnessTemplates()))
	for _, template := range config.HarnessTemplates() {
		if _, err := exec.LookPath(template.Harness.ProbeCommand[0]); err == nil {
			candidates = append(candidates, template.Harness)
		}
	}
	probes := probeHarnesses(ctx, candidates)
	byName := make(map[string]core.HarnessProbe, len(probes))
	for _, probe := range probes {
		byName[probe.Harness] = probe
	}
	detected := make([]detectedHarness, 0, len(candidates))
	for _, candidate := range candidates {
		probe, ok := byName[candidate.Name]
		if !ok {
			probe = core.HarnessProbe{Harness: candidate.Name, Message: "probe returned no result"}
		}
		detected = append(detected, detectedHarness{Harness: candidate, Probe: probe})
	}
	return detected
}

func detectLocalHarnesses(ctx context.Context) []config.Harness {
	return healthyDetectedHarnesses(detectLocalHarnessHealth(ctx))
}

func healthyDetectedHarnesses(detected []detectedHarness) []config.Harness {
	healthy := make([]config.Harness, 0, len(detected))
	for _, item := range detected {
		if validLocalHarnessProbe(item.Probe) {
			healthy = append(healthy, item.Harness)
		}
	}
	return healthy
}

func runExecutionSetupWizard(ctx context.Context, input io.Reader, output io.Writer, configPath, workspace string) error {
	if !wizardTerminal(input) {
		return fmt.Errorf("execution setup requires a terminal; use `conveyor config set execution.<stage>.<field> <value> --config %s`", configPath)
	}
	detected := detectLocalHarnessHealth(ctx)
	healthy := healthyDetectedHarnesses(detected)
	if len(healthy) == 0 {
		return noHealthyHarnessError(detected)
	}
	state := newExecutionWizardState(detected, nil)
	for {
		completed, err := runExecutionWizardUI(newExecutionWizardModel(state, detected), input, output)
		if err != nil {
			return err
		}
		if completed.cancelled || !completed.completed {
			_, _ = fmt.Fprintln(output, "Execution setup cancelled; nothing was written.")
			return nil
		}
		state = completed.state
		if state.acceptDefaults {
			break
		}
		if state.seatAction != "continue" {
			applySeatAction(state)
			state.skipDefaults = true
			continue
		}
		if !state.confirmSummary {
			state.confirmSummary = true
			state.focusSeats = false
			state.skipDefaults = true
			continue
		}
		break
	}
	return persistExecutionSetup(output, configPath, workspace, state.choices, healthy)
}

func runExecutionSetupDefaults(ctx context.Context, output io.Writer, configPath, workspace string) error {
	detected := detectLocalHarnessHealth(ctx)
	healthy := healthyDetectedHarnesses(detected)
	if len(healthy) == 0 {
		return noHealthyHarnessError(detected)
	}
	state := newExecutionWizardState(detected, nil)
	return persistExecutionSetup(output, configPath, workspace, state.choices, healthy)
}

func noHealthyHarnessError(detected []detectedHarness) error {
	if len(detected) == 0 {
		return errors.New("no supported harness was found on PATH (looked for codex, claude, and grok)")
	}
	messages := make([]string, 0, len(detected))
	for _, item := range detected {
		messages = append(messages, fmt.Sprintf("%s: %s", item.Harness.Name, item.Probe.Message))
	}
	return fmt.Errorf("no detected harness passed its validation probe (%s)", strings.Join(messages, "; "))
}

func persistExecutionSetup(output io.Writer, configPath, workspace string, choices localExecutionChoices, healthy []config.Harness) error {
	selected := selectedHarnesses(choices, healthy)
	if len(selected) == 0 {
		return errors.New("selected harnesses did not include a healthy detected harness")
	}
	if workspace == "" {
		workspace = "local"
	}
	existing, loadErr := config.Load(configPath)
	if loadErr == nil {
		if err := writeUpdatedLocalExecutionConfig(configPath, existing, choices, mergeLocalHarnesses(existing.Harnesses, selected)); err != nil {
			return err
		}
	} else if errors.Is(loadErr, os.ErrNotExist) {
		if err := writeLocalExecutionConfig(configPath, workspace, choices, selected); err != nil {
			return err
		}
	} else {
		return fmt.Errorf("load existing local execution config: %w", loadErr)
	}
	_, err := fmt.Fprintf(output, "Saved local execution setup to %s\n", configPath)
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
