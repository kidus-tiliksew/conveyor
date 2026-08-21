package main

// Named local setup management is governed by req-260811-0ee057
// REQ-15/AC-15.1-15.4 and REQ-16/AC-16.1-16.3. Setup contents never leave
// the operator host.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/spf13/cobra"
)

func setupCmd() *cobra.Command {
	configPath := defaultLocalExecutionConfigPath()
	command := &cobra.Command{Use: "setup", Short: "Manage named local execution setups"}
	command.PersistentFlags().StringVar(&configPath, "config", configPath, "local execution configuration")

	create := &cobra.Command{
		Use: "create <name>", Short: "Interactively create a named local execution setup", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveLocalExecutionConfigPath(cmd, configPath)
			if err != nil {
				return err
			}
			resolved, err := resolveClientConfig()
			if err != nil {
				return err
			}
			return runNamedExecutionSetupWizard(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), path.Path, resolved.Workspace.Value, args[0], false)
		},
	}
	edit := &cobra.Command{
		Use: "edit <name>", Short: "Interactively edit a named local execution setup", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveLocalExecutionConfigPath(cmd, configPath)
			if err != nil {
				return err
			}
			resolved, err := resolveClientConfig()
			if err != nil {
				return err
			}
			return runNamedExecutionSetupWizard(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), path.Path, resolved.Workspace.Value, args[0], true)
		},
	}
	list := &cobra.Command{
		Use: "list", Short: "List every named local execution setup", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := resolveLocalExecutionConfigPath(cmd, configPath)
			if err != nil {
				return err
			}
			return printNamedExecutionSetups(cmd.OutOrStdout(), path.Path)
		},
	}
	remove := &cobra.Command{
		Use: "delete <name>", Short: "Delete a non-default local execution setup", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveLocalExecutionConfigPath(cmd, configPath)
			if err != nil {
				return err
			}
			if err := deleteNamedExecutionSetup(path.Path, args[0]); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Deleted local execution setup %s\n", strings.TrimSpace(args[0]))
			return err
		},
	}
	makeDefault := &cobra.Command{
		Use: "default <name>", Short: "Designate the default local execution setup", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveLocalExecutionConfigPath(cmd, configPath)
			if err != nil {
				return err
			}
			if err := setDefaultExecutionSetup(path.Path, args[0]); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Default local execution setup is now %s\n", strings.TrimSpace(args[0]))
			return err
		},
	}
	command.AddCommand(create, edit, list, remove, makeDefault, setupSeatCmd(&configPath))
	return command
}

func setupSeatCmd(configPath *string) *cobra.Command {
	command := &cobra.Command{Use: "seat", Short: "Manage priority-ordered review seats"}
	var harness, model, effort string
	add := &cobra.Command{
		Use: "add <setup>", Short: "Append a review seat at lowest priority", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveLocalExecutionConfigPath(cmd, *configPath)
			if err != nil {
				return err
			}
			seat := config.ReviewSeat{Harness: strings.TrimSpace(harness), Model: strings.TrimSpace(model), Effort: strings.TrimSpace(effort)}
			if seat.Harness == "" || seat.Model == "" || seat.Effort == "" {
				return errors.New("--harness, --model, and --effort are required")
			}
			if err := appendNamedReviewSeat(cmd.Context(), path.Path, args[0], seat); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Appended review seat to setup %s\n", strings.TrimSpace(args[0]))
			return err
		},
	}
	add.Flags().StringVar(&harness, "harness", "", "seat harness")
	add.Flags().StringVar(&model, "model", "", "seat model")
	add.Flags().StringVar(&effort, "effort", "", "seat effort (low, medium, or high)")
	remove := &cobra.Command{
		Use: "remove <setup> <position>", Short: "Remove a review seat by one-based position", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveLocalExecutionConfigPath(cmd, *configPath)
			if err != nil {
				return err
			}
			position, err := positivePosition(args[1])
			if err != nil {
				return err
			}
			return mutateNamedReviewSeats(path.Path, args[0], func(seats []config.ReviewSeat) ([]config.ReviewSeat, error) {
				if len(seats) == 1 {
					return nil, errors.New("cannot remove the last review seat; every setup requires at least one")
				}
				if position > len(seats) {
					return nil, fmt.Errorf("review seat position %d exceeds configured seat count %d", position, len(seats))
				}
				return append(seats[:position-1], seats[position:]...), nil
			})
		},
	}
	move := &cobra.Command{
		Use: "move <setup> <from> <to>", Short: "Move a review seat to a new one-based priority position", Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveLocalExecutionConfigPath(cmd, *configPath)
			if err != nil {
				return err
			}
			from, err := positivePosition(args[1])
			if err != nil {
				return err
			}
			to, err := positivePosition(args[2])
			if err != nil {
				return err
			}
			return mutateNamedReviewSeats(path.Path, args[0], func(seats []config.ReviewSeat) ([]config.ReviewSeat, error) {
				if from > len(seats) || to > len(seats) {
					return nil, fmt.Errorf("review seat positions must be between 1 and %d", len(seats))
				}
				seat := seats[from-1]
				seats = append(seats[:from-1], seats[from:]...)
				index := to - 1
				seats = append(seats, config.ReviewSeat{})
				copy(seats[index+1:], seats[index:])
				seats[index] = seat
				return seats, nil
			})
		},
	}
	command.AddCommand(add, remove, move)
	return command
}

func positivePosition(value string) (int, error) {
	position, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || position < 1 {
		return 0, fmt.Errorf("position must be a positive one-based integer")
	}
	return position, nil
}

func configuredSetupNames(local *config.Config) []string {
	names := make([]string, 0, len(local.Setups))
	for _, setup := range local.Setups {
		names = append(names, setup.Name)
	}
	sort.Strings(names)
	return names
}

func namedSetup(local *config.Config, name string) (config.ExecutionSetup, int, error) {
	name = strings.TrimSpace(name)
	for index, setup := range local.Setups {
		if setup.Name == name {
			return setup, index, nil
		}
	}
	return config.ExecutionSetup{}, -1, fmt.Errorf("unknown setup %q; configured setups: %s", name, strings.Join(configuredSetupNames(local), ", "))
}

func setDefaultExecutionSetup(path, name string) error {
	local, err := config.Load(path)
	if err != nil {
		return fmt.Errorf("load local execution config: %w", err)
	}
	if _, _, err = namedSetup(local, name); err != nil {
		return err
	}
	local.DefaultSetup = strings.TrimSpace(name)
	return writeNamedLocalExecutionConfig(path, local)
}

func deleteNamedExecutionSetup(path, name string) error {
	local, err := config.Load(path)
	if err != nil {
		return fmt.Errorf("load local execution config: %w", err)
	}
	setup, index, err := namedSetup(local, name)
	if err != nil {
		return err
	}
	if len(local.Setups) == 1 {
		return errors.New("cannot delete the last remaining setup")
	}
	if setup.Name == local.DefaultSetup {
		return fmt.Errorf("cannot delete default setup %q; designate another setup with `conveyor setup default <name>` first", setup.Name)
	}
	local.Setups = append(local.Setups[:index], local.Setups[index+1:]...)
	return writeNamedLocalExecutionConfig(path, local)
}

func mutateNamedReviewSeats(path, name string, mutate func([]config.ReviewSeat) ([]config.ReviewSeat, error)) error {
	local, err := config.Load(path)
	if err != nil {
		return fmt.Errorf("load local execution config: %w", err)
	}
	setup, index, err := namedSetup(local, name)
	if err != nil {
		return err
	}
	seats := append([]config.ReviewSeat(nil), setup.Review.Seats...)
	seats, err = mutate(seats)
	if err != nil {
		return err
	}
	setup.Review.Seats = seats
	local.Setups[index] = setup
	return writeNamedLocalExecutionConfig(path, local)
}

func appendNamedReviewSeat(ctx context.Context, path, name string, seat config.ReviewSeat) error {
	if seat.Effort != "low" && seat.Effort != "medium" && seat.Effort != "high" {
		return errors.New("effort must be low, medium, or high")
	}
	local, err := config.Load(path)
	if err != nil {
		return fmt.Errorf("load local execution config: %w", err)
	}
	setup, index, err := namedSetup(local, name)
	if err != nil {
		return err
	}
	if err = ensureConfiguredHarness(local, seat.Harness); err != nil {
		return err
	}
	if err = probeConfiguredHarness(ctx, local, seat.Harness); err != nil {
		return err
	}
	setup.Review.Seats = append(setup.Review.Seats, seat)
	local.Setups[index] = setup
	return writeNamedLocalExecutionConfig(path, local)
}

func printNamedExecutionSetups(output io.Writer, path string) error {
	local, err := config.Load(path)
	if err != nil {
		return fmt.Errorf("load local execution config: %w", err)
	}
	styled := outputIsTerminal(output)
	for _, setup := range local.Setups {
		marker := ""
		if setup.Name == local.DefaultSetup {
			marker = " (default)"
		}
		if err := renderCLIConfigRow(output, styled, "setup", setup.Name+marker, "stored file "+path); err != nil {
			return err
		}
		stages := []struct {
			name string
			item localStageChoice
		}{
			{"spec", localStageChoice{Harness: setup.ExecutionSettings.Spec.Harness, Model: setup.ExecutionSettings.Spec.Model, Effort: setup.ExecutionSettings.Spec.Effort, Timeout: setup.ExecutionSettings.Spec.TimeoutText}},
			{"implement", localStageChoice{Harness: setup.ExecutionSettings.Implementation.Harness, Model: setup.ExecutionSettings.Implementation.Model, Effort: setup.ExecutionSettings.Implementation.Effort, Timeout: setup.ExecutionSettings.Implementation.TimeoutText}},
		}
		for _, stage := range stages {
			value := fmt.Sprintf("harness=%s model=%s effort=%s timeout=%s", stage.item.Harness, stage.item.Model, stage.item.Effort, stage.item.Timeout)
			if err := renderCLIConfigRow(output, styled, "  "+stage.name, value, ""); err != nil {
				return err
			}
		}
		if err := renderCLIConfigRow(output, styled, "  review", "timeout="+setup.ExecutionSettings.Review.TimeoutText, "ordered seats follow"); err != nil {
			return err
		}
		for index, seat := range setup.Review.Seats {
			value := fmt.Sprintf("harness=%s model=%s effort=%s", seat.Harness, seat.Model, seat.Effort)
			if err := renderCLIConfigRow(output, styled, fmt.Sprintf("  review.seat.%d", index+1), value, "priority order"); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeNamedLocalExecutionConfig(path string, local *config.Config) error {
	for index := range local.Setups {
		if local.Setups[index].Name != local.DefaultSetup {
			continue
		}
		local.ExecutionSettings = &local.Setups[index].ExecutionSettings
		local.Review = local.Setups[index].Review
		break
	}
	return writeValidatedLocalExecutionConfig(path, local)
}

func runNamedExecutionSetupWizard(ctx context.Context, input io.Reader, output io.Writer, path, workspace, name string, edit bool) error {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, `/\\`) || name == "." || name == ".." {
		return fmt.Errorf("setup name must be one path-safe segment")
	}
	if !wizardTerminal(input) {
		return fmt.Errorf("execution setup requires a terminal; use `conveyor config set --setup %s execution.<stage>.<field> <value> --config %s`", name, path)
	}
	harnesses := detectLocalHarnesses(ctx)
	if len(harnesses) == 0 {
		return errors.New("no supported harness was found on PATH (looked for codex, claude, and grok)")
	}
	local, loadErr := config.Load(path)
	if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
		return fmt.Errorf("load existing local execution config: %w", loadErr)
	}
	if edit && loadErr != nil {
		return fmt.Errorf("load named setup %q: %w", name, loadErr)
	}
	if !edit && loadErr == nil {
		if _, _, lookupErr := namedSetup(local, name); lookupErr == nil {
			return fmt.Errorf("setup %q already exists; use `conveyor setup edit %s`", name, name)
		}
	}
	var initialSeats []localStageChoice
	if edit {
		setup, _, lookupErr := namedSetup(local, name)
		if lookupErr != nil {
			return lookupErr
		}
		for _, seat := range setup.Review.Seats {
			initialSeats = append(initialSeats, localStageChoice{Harness: seat.Harness, Model: seat.Model, Effort: seat.Effort})
		}
	}
	model := newReviewSeatExecutionWizardModel(harnesses, initialSeats)
	if edit {
		setup, _, _ := namedSetup(local, name)
		prefillExecutionWizard(&model, setup)
	}
	completed, err := runExecutionWizardUI(model, input, output)
	if err != nil {
		return err
	}
	if completed.cancelled || completed.field < len(completed.fields) {
		_, _ = fmt.Fprintln(output, "Execution setup cancelled; nothing was written.")
		return nil
	}
	selected := selectedHarnesses(completed.choices, harnesses)
	probes := probeHarnesses(ctx, selected)
	if len(probes) != len(selected) {
		return errors.New("one or more selected harnesses could not be probe-validated")
	}
	for _, probe := range probes {
		if !validLocalHarnessProbe(probe) {
			return errors.New(wizardValidationStyle.Render(fmt.Sprintf("! harness %q failed validation probe: %s", probe.Harness, probe.Message)))
		}
	}
	if workspace == "" {
		workspace = "local"
	}
	if err = writeNamedExecutionSetup(path, local, workspace, name, completed.choices, selected, edit); err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "Saved local execution setup %s to %s\n", name, path)
	return err
}

func prefillExecutionWizard(model *executionWizardModel, setup config.ExecutionSetup) {
	choices := localExecutionChoices{
		Spec:      localStageChoice{Harness: setup.ExecutionSettings.Spec.Harness, Model: setup.ExecutionSettings.Spec.Model, Effort: setup.ExecutionSettings.Spec.Effort, Timeout: setup.ExecutionSettings.Spec.TimeoutText},
		Implement: localStageChoice{Harness: setup.ExecutionSettings.Implementation.Harness, Model: setup.ExecutionSettings.Implementation.Model, Effort: setup.ExecutionSettings.Implementation.Effort, Timeout: setup.ExecutionSettings.Implementation.TimeoutText},
		Review:    localStageChoice{Harness: setup.ExecutionSettings.Review.FallbackHarness, Model: setup.ExecutionSettings.Review.FallbackModel, Timeout: setup.ExecutionSettings.Review.TimeoutText},
	}
	if len(setup.Review.Seats) > 0 {
		choices.Review.Harness = setup.Review.Seats[0].Harness
		choices.Review.Model = setup.Review.Seats[0].Model
		choices.Review.Effort = setup.Review.Seats[0].Effort
	}
	for _, seat := range setup.Review.Seats {
		choices.ReviewSeats = append(choices.ReviewSeats, localStageChoice{Harness: seat.Harness, Model: seat.Model, Effort: seat.Effort})
	}
	model.choices = choices
	for index := range model.fields {
		field := &model.fields[index]
		if field.name == "seat_count" {
			field.value = strconv.Itoa(len(choices.ReviewSeats))
			continue
		}
		if field.name == "seat_order" {
			continue
		}
		choice := choices.Spec
		if field.stage == "implement" {
			choice = choices.Implement
		} else if field.stage == "review" {
			choice = choices.Review
		}
		if field.seat > 0 && field.seat <= len(choices.ReviewSeats) {
			choice = choices.ReviewSeats[field.seat-1]
		}
		value := map[string]string{"harness": choice.Harness, "model": choice.Model, "effort": choice.Effort, "timeout": choice.Timeout}[field.name]
		if len(field.options) == 0 {
			field.value = value
			continue
		}
		field.options = preferredOptionFirst(field.options, value)
	}
	model.prepareInput()
}

func writeNamedExecutionSetup(path string, local *config.Config, workspace, name string, choices localExecutionChoices, selected []config.Harness, edit bool) error {
	document := localExecutionDocument(workspace, choices, selected)
	setup := config.ExecutionSetup{Name: name, ExecutionSettings: *document.ExecutionSettings, Review: document.Review, RefreshReview: config.RefreshReviewDelta}
	if local == nil {
		document.Setups = []config.ExecutionSetup{setup}
		document.DefaultSetup = name
		return writeValidatedLocalExecutionConfig(path, document)
	}
	local.Harnesses = mergeLocalHarnesses(local.Harnesses, selected)
	if edit {
		_, index, err := namedSetup(local, name)
		if err != nil {
			return err
		}
		local.Setups[index] = setup
	} else {
		local.Setups = append(local.Setups, setup)
	}
	return writeNamedLocalExecutionConfig(path, local)
}

func setNamedLocalExecutionField(path, name, key, value string) error {
	return setNamedLocalExecutionFieldContext(context.Background(), path, name, key, value, false)
}

func setNamedLocalExecutionFieldContext(ctx context.Context, path, name, key, value string, requireProbe bool) error {
	local, err := config.Load(path)
	if err != nil {
		return fmt.Errorf("load local execution config: %w", err)
	}
	setup, index, err := namedSetup(local, name)
	if err != nil {
		return err
	}
	parts := strings.Split(strings.TrimSpace(key), ".")
	if len(parts) == 4 && parts[0] == "review" && parts[1] == "seat" {
		parts = []string{"execution", "review", "seat", parts[2], parts[3]}
	}
	if len(parts) == 5 && parts[0] == "execution" && parts[1] == "review" && parts[2] == "seat" {
		position, positionErr := positivePosition(parts[3])
		if positionErr != nil {
			return positionErr
		}
		if position > len(setup.Review.Seats) {
			return fmt.Errorf("review seat position %d exceeds configured seat count %d; add a seat first", position, len(setup.Review.Seats))
		}
		seat := &setup.Review.Seats[position-1]
		if err = setReviewSeatField(seat, parts[4], value, &local.Harnesses); err != nil {
			return err
		}
		if requireProbe && parts[4] == "harness" {
			if err = probeConfiguredHarness(ctx, local, value); err != nil {
				return err
			}
		}
		local.Setups[index] = setup
		return writeNamedLocalExecutionConfig(path, local)
	}
	if len(parts) != 3 || parts[0] != "execution" {
		return errors.New("field must be execution.<stage>.<field> or review.seat.<position>.<field>")
	}
	stage, field := parts[1], parts[2]
	value = strings.TrimSpace(value)
	var choice *config.ImplementationSettings
	switch stage {
	case "spec":
		choice = &setup.ExecutionSettings.Spec
	case "implement":
		choice = &setup.ExecutionSettings.Implementation
	case "review":
		if field == "timeout" {
			setup.ExecutionSettings.Review.TimeoutText = value
		} else {
			if len(setup.Review.Seats) == 0 {
				return errors.New("setup must contain at least one review seat")
			}
			if err = setReviewSeatField(&setup.Review.Seats[0], field, value, &local.Harnesses); err != nil {
				return err
			}
			setup.ExecutionSettings.Review.FallbackHarness = setup.Review.Seats[0].Harness
			setup.ExecutionSettings.Review.FallbackModel = setup.Review.Seats[0].Model
		}
	default:
		return errors.New("execution stage must be spec, implement, or review")
	}
	if choice != nil {
		if err = setImplementationField(choice, field, value, &local.Harnesses); err != nil {
			return err
		}
	}
	if requireProbe && field == "harness" {
		if err = probeConfiguredHarness(ctx, local, value); err != nil {
			return err
		}
	}
	local.Setups[index] = setup
	return writeNamedLocalExecutionConfig(path, local)
}

func ensureConfiguredHarness(local *config.Config, name string) error {
	for _, harness := range local.Harnesses {
		if harness.Name == name {
			return nil
		}
	}
	var ignored string
	return selectTemplateHarness(name, &ignored, &local.Harnesses)
}

func probeConfiguredHarness(ctx context.Context, local *config.Config, name string) error {
	for _, harness := range local.Harnesses {
		if harness.Name != strings.TrimSpace(name) {
			continue
		}
		probes := probeHarnesses(ctx, []config.Harness{harness})
		if len(probes) != 1 || !validLocalHarnessProbe(probes[0]) {
			message := "probe did not return a valid fingerprint"
			if len(probes) == 1 {
				message = probes[0].Message
			}
			return fmt.Errorf("harness %q failed validation probe: %s", name, message)
		}
		return nil
	}
	return fmt.Errorf("local execution config has no harness %q", name)
}

func setImplementationField(choice *config.ImplementationSettings, field, value string, harnesses *[]config.Harness) error {
	switch field {
	case "harness":
		return selectTemplateHarness(value, &choice.Harness, harnesses)
	case "model":
		if value == "" {
			return errors.New("model is required")
		}
		choice.Model, choice.ModelPolicy = value, config.ModelPolicyExplicit
	case "effort":
		if value != "low" && value != "medium" && value != "high" {
			return errors.New("effort must be low, medium, or high")
		}
		choice.Effort = value
	case "timeout":
		choice.TimeoutText = value
	default:
		return errors.New("execution field must be harness, model, effort, or timeout")
	}
	return nil
}

func setReviewSeatField(seat *config.ReviewSeat, field, value string, harnesses *[]config.Harness) error {
	value = strings.TrimSpace(value)
	switch field {
	case "harness":
		return selectTemplateHarness(value, &seat.Harness, harnesses)
	case "model":
		if value == "" {
			return errors.New("model is required")
		}
		seat.Model = value
	case "effort":
		if value != "low" && value != "medium" && value != "high" {
			return errors.New("effort must be low, medium, or high")
		}
		seat.Effort = value
	default:
		return errors.New("review seat field must be harness, model, or effort")
	}
	return nil
}

func selectTemplateHarness(name string, destination *string, harnesses *[]config.Harness) error {
	name = strings.TrimSpace(name)
	for _, template := range config.HarnessTemplates() {
		if template.Harness.Name != name {
			continue
		}
		*destination = name
		for index := range *harnesses {
			if (*harnesses)[index].Name == name {
				(*harnesses)[index] = template.Harness
				return nil
			}
		}
		*harnesses = append(*harnesses, template.Harness)
		return nil
	}
	return fmt.Errorf("unsupported harness %q; choose codex, claude, or grok", name)
}
