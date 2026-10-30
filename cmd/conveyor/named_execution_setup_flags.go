package main

// Field flags give `conveyor setup create|edit` a non-interactive path
// (req-agent-guided-setup AC-5.2; DEC-92(4)). The path starts from the
// wizard's state, applies the wizard's harness probe and field validation,
// and writes through the wizard's single staged write
// (component-harness-execution).

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/spf13/cobra"
)

// namedSetupFieldStages and namedSetupStageFields name the stage field flags.
// Flag --<stage>-<field> matches the `conveyor config set
// execution.<stage>.<field>` key; the review stage's fields set review seat 1,
// as that key does.
var (
	namedSetupFieldStages = []string{"spec", "implement", "verify", "review"}
	namedSetupStageFields = []string{"harness", "model", "effort"}
)

const namedSetupWorktreeLocationFlag = "worktree-location"

type namedSetupFieldFlags struct {
	names  []string
	values map[string]*string
}

// namedSetupFieldValue is one field flag the caller gave.
type namedSetupFieldValue struct {
	flag, stage, field, value string
}

func (f *namedSetupFieldFlags) register(cmd *cobra.Command) {
	f.values = map[string]*string{}
	add := func(name, usage string) {
		value := new(string)
		f.names = append(f.names, name)
		f.values[name] = value
		cmd.Flags().StringVar(value, name, "", usage)
	}
	for _, stage := range namedSetupFieldStages {
		subject := stage + " stage"
		if stage == "review" {
			subject = "review seat 1"
		}
		add(stage+"-harness", subject+" harness: codex, claude, grok, cursor, or opencode")
		add(stage+"-model", subject+" model; defaults to the harness's suggested model")
		add(stage+"-effort", subject+" effort: low, medium, high, or blank")
	}
	add(namedSetupWorktreeLocationFlag, "where new task worktrees go: repository, repository:<dir>, machine, or machine:<root>")
}

// given returns the field flags set on cmd in registration order.
func (f namedSetupFieldFlags) given(cmd *cobra.Command) []namedSetupFieldValue {
	var given []namedSetupFieldValue
	for _, name := range f.names {
		if !cmd.Flags().Changed(name) {
			continue
		}
		item := namedSetupFieldValue{flag: name, value: strings.TrimSpace(*f.values[name])}
		if stage, field, ok := strings.Cut(name, "-"); ok && name != namedSetupWorktreeLocationFlag {
			item.stage, item.field = stage, field
		}
		given = append(given, item)
	}
	return given
}

// runNamedExecutionSetupFlags creates or edits a named setup from field flags
// without prompting. It names every invalid field and writes nothing when any
// field is invalid.
func runNamedExecutionSetupFlags(ctx context.Context, output io.Writer, path, workspace, name string, edit bool, fields []namedSetupFieldValue) error {
	name = strings.TrimSpace(name)
	if !pathSafeSetupName(name) {
		return fmt.Errorf("setup name must be one path-safe segment")
	}
	start, err := startNamedExecutionSetup(ctx, path, name, edit)
	if err != nil {
		return err
	}
	choices := start.state.choices
	choices.ReviewSeats = append([]localStageChoice(nil), choices.ReviewSeats...)
	stageChoice := map[string]*localStageChoice{
		"spec": &choices.Spec, "implement": &choices.Implement, "verify": &choices.Verify, "review": &choices.ReviewSeats[0],
	}
	byStage := map[string]map[string]namedSetupFieldValue{}
	location := choices.WorktreeLocation
	for _, item := range fields {
		if item.flag == namedSetupWorktreeLocationFlag {
			location = item.value
			continue
		}
		if byStage[item.stage] == nil {
			byStage[item.stage] = map[string]namedSetupFieldValue{}
		}
		byStage[item.stage][item.field] = item
	}
	probe := harnessProbeValidator(start.detected)
	var invalid []string
	for _, stage := range namedSetupFieldStages {
		given := byStage[stage]
		choice := stageChoice[stage]
		model, modelGiven := given["model"]
		effort, effortGiven := given["effort"]
		if harness, ok := given["harness"]; ok {
			if err := applyNamedSetupHarness(choice, harness.value, modelGiven, effortGiven, probe); err != nil {
				invalid = append(invalid, fmt.Sprintf("--%s: %v", harness.flag, err))
			}
		}
		if modelGiven {
			if model.value == "" {
				invalid = append(invalid, fmt.Sprintf("--%s: model is required", model.flag))
			} else {
				choice.Model = model.value
			}
		}
		if effortGiven {
			switch {
			case effort.value != "" && effort.value != "low" && effort.value != "medium" && effort.value != "high":
				invalid = append(invalid, fmt.Sprintf("--%s: effort must be low, medium, high, or blank", effort.flag))
			case effort.value != "" && suggestedHarnessEffort(choice.Harness) == "":
				invalid = append(invalid, fmt.Sprintf("--%s: harness %q takes no effort", effort.flag, choice.Harness))
			default:
				choice.Effort = effort.value
			}
		}
		if strings.TrimSpace(choice.Model) == "" && suggestedHarnessModel(choice.Harness, stage) == "" {
			invalid = append(invalid, fmt.Sprintf("--%s-model: harness %q has no suggested model; a model is required", stage, choice.Harness))
		}
	}
	choices.Review.Harness, choices.Review.Model, choices.Review.Effort = choices.ReviewSeats[0].Harness, choices.ReviewSeats[0].Model, choices.ReviewSeats[0].Effort
	choices.WorktreeLocation = location
	if err := validateWorktreeChoice(path, location); err != nil {
		invalid = append(invalid, fmt.Sprintf("--%s: %v", namedSetupWorktreeLocationFlag, err))
	}
	if len(invalid) > 0 {
		return fmt.Errorf("invalid setup fields; nothing was written: %s", strings.Join(invalid, "; "))
	}
	return saveNamedExecutionSetup(output, path, start.local, workspace, name, choices, start.healthy, edit)
}

// applyNamedSetupHarness sets a stage harness after the wizard's probe check.
// A changed harness takes its suggested model unless a model flag is given,
// and keeps the stage effort only when the harness accepts one.
func applyNamedSetupHarness(choice *localStageChoice, harness string, modelGiven, effortGiven bool, probe func(string) error) error {
	supported := false
	for _, template := range config.HarnessTemplates() {
		if template.Harness.Name == harness {
			supported = true
			break
		}
	}
	if !supported {
		return fmt.Errorf("unsupported harness %q; choose codex, claude, grok, cursor, or opencode", harness)
	}
	if err := probe(harness); err != nil {
		return err
	}
	if harness == choice.Harness {
		return nil
	}
	choice.Harness = harness
	if !modelGiven {
		choice.Model = ""
	}
	if !effortGiven {
		switch suggested := suggestedHarnessEffort(harness); {
		case suggested == "":
			choice.Effort = ""
		case choice.Effort == "":
			choice.Effort = suggested
		}
	}
	return nil
}
