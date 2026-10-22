package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/huh"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"

	"github.com/kidus-tiliksew/conveyor/cmd/conveyor/localgit"
	"github.com/kidus-tiliksew/conveyor/internal/config"
)

// Worktree placement is client-local configuration (component-harness-execution)
// consumed by checkout (component-git-delivery) and the launchers
// (component-local-launchers). No silent default exists: an unconfigured
// location creates no worktree.

type worktreeLocationContextKey struct{}

func contextWithWorktreeLocation(ctx context.Context, location config.WorktreeLocation) context.Context {
	return context.WithValue(ctx, worktreeLocationContextKey{}, location)
}

func worktreeLocationFromContext(ctx context.Context) (config.WorktreeLocation, bool) {
	location, ok := ctx.Value(worktreeLocationContextKey{}).(config.WorktreeLocation)
	return location, ok && location.Mode != ""
}

// contextWithWorktreeRoot attaches a machine-mode location rooted at root.
func contextWithWorktreeRoot(ctx context.Context, root string) context.Context {
	return contextWithWorktreeLocation(ctx, config.WorktreeLocation{Mode: config.WorktreeLocationMachine, Root: root})
}

// resolveWorktreeContainer is the one resolution function for both modes
// (component-git-delivery): repository mode yields <primary>/<dir>, machine
// mode yields the configured root.
func resolveWorktreeContainer(location config.WorktreeLocation, primary string) (string, error) {
	switch location.Mode {
	case config.WorktreeLocationRepository:
		if strings.TrimSpace(primary) == "" {
			return "", errors.New("repository worktree location needs the primary checkout")
		}
		return filepath.Join(filepath.Clean(primary), location.Dir), nil
	case config.WorktreeLocationMachine:
		return location.Root, nil
	}
	return "", fmt.Errorf("worktree location mode %q must be repository or machine", location.Mode)
}

// forwardedWorktreeContainer is the CONVEYOR_WORKTREE_ROOT value for a child
// whose verified working directory is workingDirectory. Repository mode
// forwards <primary>/<dir> for that checkout; a spec child gets none because
// it never creates a worktree (component-local-launchers).
func forwardedWorktreeContainer(ctx context.Context, workingDirectory string, spec bool) (string, error) {
	location, ok := worktreeLocationFromContext(ctx)
	if !ok {
		return "", nil
	}
	if location.Mode == config.WorktreeLocationMachine {
		return location.Root, nil
	}
	if spec || strings.TrimSpace(workingDirectory) == "" {
		return "", nil
	}
	primary, err := primaryWorktreeRoot(ctx, workingDirectory)
	if err != nil {
		return "", fmt.Errorf("resolve primary checkout for worktree location: %w", err)
	}
	return resolveWorktreeContainer(location, primary)
}

func describeWorktreeLocation(location config.WorktreeLocation) string {
	if location.Mode == config.WorktreeLocationRepository {
		return fmt.Sprintf("repository (%s under each primary checkout)", location.Dir)
	}
	return fmt.Sprintf("machine (%s)", location.Root)
}

func missingWorktreeLocationError(configPath string) error {
	if strings.TrimSpace(configPath) == "" {
		configPath = "<local execution config>"
	}
	return fmt.Errorf("no worktree location is configured in %[1]s, so no task worktree is created; choose repository (<primary checkout>/_worktrees/<repo>-task-<id>, beside the code) or machine (~/.conveyor/worktrees/<repo>-task-<id>, one place for every repository) and record it with `conveyor config set worktree_location repository --config %[1]s` or `conveyor config set worktree_location machine --config %[1]s`, or with `conveyor config init-execution --worktree-location <repository|machine> --config %[1]s`", configPath)
}

// requireConfiguredWorktreeLocation is the launcher startup check: a document
// that relied on the retired default stops run and the worker before any
// enrollment, heartbeat, or claim (component-local-launchers).
func requireConfiguredWorktreeLocation(local *config.Config, configPath string) (config.WorktreeLocation, error) {
	location, ok := local.ConfiguredWorktreeLocation()
	if !ok {
		return config.WorktreeLocation{}, missingWorktreeLocationError(configPath)
	}
	return location, nil
}

// authoredWorktreeLocation fills each mode's default in the written form so
// the persisted file states the chosen path.
func authoredWorktreeLocation(location config.WorktreeLocation) config.WorktreeLocation {
	if location.Mode == config.WorktreeLocationRepository && location.Dir == "" {
		location.Dir = config.DefaultRepositoryWorktreeDir
	}
	if location.Mode == config.WorktreeLocationMachine && location.Root == "" {
		location.Root = "~/.conveyor/worktrees"
	}
	return location
}

// writeWorktreeLocation records the location in the local execution document,
// replacing a legacy worktree_root. A missing document is first created like
// `conveyor config set` creates one (component-harness-execution).
func writeWorktreeLocation(path, workspace string, location config.WorktreeLocation) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	location = authoredWorktreeLocation(location)
	if _, err = location.Normalized(home); err != nil {
		return err
	}
	if _, err = os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if strings.TrimSpace(workspace) == "" {
			workspace = "local"
		}
		choices, harnesses := configSetDefaultDocument()
		if err = writeLocalExecutionConfig(path, workspace, choices, harnesses); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	original, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var document yaml.Node
	if err = yaml.Unmarshal(original, &document); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if len(document.Content) == 0 || document.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("parse %s: local execution document is not a mapping", path)
	}
	var value yaml.Node
	if err = value.Encode(location); err != nil {
		return err
	}
	root := document.Content[0]
	removeExecutionYAMLField(root, "worktree_root")
	setExecutionYAMLField(root, "worktree_location", &value)
	return writeValidatedLocalExecutionConfig(path, &document)
}

// streamIsTerminal reports whether one standard stream is a terminal.
var streamIsTerminal = func(file *os.File) bool {
	return file != nil && term.IsTerminal(int(file.Fd()))
}

// checkoutIsInteractive requires both standard input and standard output to
// be terminals, so a harness with a pseudo-terminal on one stream never
// blocks on a prompt (component-git-delivery).
func checkoutIsInteractive() bool {
	return streamIsTerminal(os.Stdin) && streamIsTerminal(os.Stdout)
}

// promptWorktreeLocation asks once, showing the exact resulting path of each
// mode for the current repository and task.
var promptWorktreeLocation = func(input io.Reader, output io.Writer, repositoryPath, machinePath string) (config.WorktreeLocation, error) {
	choice := config.WorktreeLocationRepository
	field := huh.NewSelect[string]().
		Title("Where should Conveyor create task worktrees?").
		Description("Conveyor saves this choice in the local execution config and uses it for every new task worktree.").
		Options(
			huh.NewOption("In this repository: "+repositoryPath+" (visible beside the code; Conveyor adds the folder to .git/info/exclude)", config.WorktreeLocationRepository),
			huh.NewOption("Machine-wide: "+machinePath+" (one place for every repository; outside the project folder)", config.WorktreeLocationMachine),
		).
		Value(&choice)
	if err := huh.NewForm(huh.NewGroup(field)).WithInput(input).WithOutput(output).Run(); err != nil {
		return config.WorktreeLocation{}, fmt.Errorf("worktree location prompt: %w", err)
	}
	return config.WorktreeLocation{Mode: choice}, nil
}

// worktreePlacement resolves the implicit task worktree container only when
// checkout must create a new worktree, so reusing a registered worktree needs
// no configured location (component-git-delivery).
type worktreePlacement struct {
	// container is a fixed container: a forwarded CONVEYOR_WORKTREE_ROOT or
	// a caller-supplied root. It wins over location.
	container   string
	location    *config.WorktreeLocation
	configPath  string
	workspace   string
	interactive bool
	input       io.Reader
	output      io.Writer
}

func fixedWorktreePlacement(root string) worktreePlacement {
	return worktreePlacement{container: root}
}

// checkoutWorktreePlacement builds the placement for `conveyor checkout`:
// CONVEYOR_WORKTREE_ROOT, then the configured location, then an interactive
// choice. --path is applied by the caller before any placement is read.
func checkoutWorktreePlacement(configPath, workspace string) (worktreePlacement, error) {
	placement := worktreePlacement{configPath: configPath, workspace: workspace, interactive: checkoutIsInteractive(), input: os.Stdin, output: os.Stderr}
	if forwarded := strings.TrimSpace(os.Getenv("CONVEYOR_WORKTREE_ROOT")); forwarded != "" {
		placement.container = forwarded
		return placement, nil
	}
	local, err := config.Load(configPath)
	if errors.Is(err, os.ErrNotExist) {
		return placement, nil
	}
	if err != nil {
		return worktreePlacement{}, fmt.Errorf("load local execution config for worktree location: %w", err)
	}
	if location, ok := local.ConfiguredWorktreeLocation(); ok {
		placement.location = &location
	}
	return placement, nil
}

// prepare resolves the container and, when it lies inside the primary
// checkout, refuses an escaping or tracked dir and ensures the exclude entry
// before the caller's primary-cleanliness check.
func (p worktreePlacement) prepare(ctx context.Context, primary, repo, taskID, branch string) (string, error) {
	container := strings.TrimSpace(p.container)
	if container == "" {
		location, err := p.chosenLocation(primary, repo, taskID, branch)
		if err != nil {
			return "", err
		}
		if container, err = resolveWorktreeContainer(location, primary); err != nil {
			return "", err
		}
	}
	if !filepath.IsAbs(container) {
		return container, nil
	}
	dir, inside, err := inRepositoryWorktreeDir(primary, container)
	if err != nil || !inside {
		return container, err
	}
	if _, err = localgit.EnsureExcluded(ctx, primary, dir); err != nil {
		return "", err
	}
	return container, nil
}

func (p worktreePlacement) chosenLocation(primary, repo, taskID, branch string) (config.WorktreeLocation, error) {
	if p.location != nil {
		return *p.location, nil
	}
	if !p.interactive {
		return config.WorktreeLocation{}, missingWorktreeLocationError(p.configPath)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return config.WorktreeLocation{}, err
	}
	name := implicitWorktreeName(repo, taskID, branch)
	repositoryPath := filepath.Join(primary, config.DefaultRepositoryWorktreeDir, name)
	machinePath := filepath.Join(config.DefaultWorktreeRoot(home), name)
	chosen, err := promptWorktreeLocation(p.input, p.output, repositoryPath, machinePath)
	if err != nil {
		return config.WorktreeLocation{}, err
	}
	if err = writeWorktreeLocation(p.configPath, p.workspace, chosen); err != nil {
		return config.WorktreeLocation{}, fmt.Errorf("save worktree location: %w", err)
	}
	normalized, err := authoredWorktreeLocation(chosen).Normalized(home)
	if err != nil {
		return config.WorktreeLocation{}, err
	}
	if p.output != nil {
		fmt.Fprintf(p.output, "Saved worktree_location %s to %s\n", normalized.String(), p.configPath)
	}
	return normalized, nil
}

// inRepositoryWorktreeDir reports the container's path relative to the
// primary checkout when the container lies inside it. A container whose
// existing parts resolve outside the repository is refused, so a symlinked dir
// cannot escape (component-git-delivery; req-delegated-execution AC-3.3).
func inRepositoryWorktreeDir(primary, container string) (string, bool, error) {
	canonicalPrimary, err := canonicalWorktreePath(primary)
	if err != nil {
		return "", false, err
	}
	cleaned := filepath.Clean(container)
	relative, err := filepath.Rel(canonicalPrimary, cleaned)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", false, nil
	}
	resolved, err := canonicalPathThroughExistingParent(cleaned)
	if err != nil {
		return "", false, fmt.Errorf("resolve in-repository worktree directory %s: %w", cleaned, err)
	}
	if resolved != cleaned {
		resolvedRelative, relErr := filepath.Rel(canonicalPrimary, resolved)
		if relErr != nil || resolvedRelative == ".." || strings.HasPrefix(resolvedRelative, ".."+string(filepath.Separator)) {
			return "", false, fmt.Errorf("refusing in-repository worktree directory %s: it resolves to %s outside the repository", cleaned, resolved)
		}
		return "", false, fmt.Errorf("refusing in-repository worktree directory %s: it resolves through a symlink to %s", cleaned, resolved)
	}
	return filepath.ToSlash(relative), true, nil
}

// unsafePrimaryError names a missing exclude entry when every untracked entry
// in the primary checkout lies under an in-repository worktree container,
// instead of reporting a generic unsafe primary (component-git-delivery).
func unsafePrimaryError(ctx context.Context, primary, taskID string, candidates []string, cause error) error {
	seen := map[string]bool{}
	for _, dir := range candidates {
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		if only, err := localgit.UntrackedOnlyUnder(ctx, primary, dir); err == nil && only {
			return fmt.Errorf("primary checkout is unsafe: in-repository worktree directory %s/ is not excluded from Git; run `conveyor checkout %s` to add %s/ to .git/info/exclude", dir, taskID, dir)
		}
	}
	return fmt.Errorf("primary checkout is unsafe: %w", cause)
}

// inRepositoryWorktreeCandidates lists dirs that may hold nested task
// worktrees: the placement's own container and the top-level directory of
// every registered worktree inside the primary checkout.
func inRepositoryWorktreeCandidates(primary string, placement worktreePlacement, worktrees []registeredWorktree) []string {
	var candidates []string
	if placement.location != nil && placement.location.Mode == config.WorktreeLocationRepository {
		candidates = append(candidates, placement.location.Dir)
	}
	if placement.container != "" {
		if dir, inside, err := inRepositoryWorktreeDir(primary, placement.container); err == nil && inside {
			candidates = append(candidates, dir)
		}
	}
	canonicalPrimary, err := canonicalWorktreePath(primary)
	if err != nil {
		return candidates
	}
	for _, entry := range worktrees {
		relative, err := filepath.Rel(canonicalPrimary, filepath.Clean(entry.Path))
		if err != nil || relative == "." || strings.HasPrefix(relative, "..") {
			continue
		}
		candidates = append(candidates, strings.SplitN(filepath.ToSlash(relative), "/", 2)[0])
	}
	return candidates
}

// configSetDefaultDocument is the Codex-based document that `conveyor config
// set` creates when the local execution file is missing.
func configSetDefaultDocument() (localExecutionChoices, []config.Harness) {
	defaultHarness := config.HarnessTemplates()[0].Harness
	return localExecutionChoices{
		Spec:      localStageChoice{Harness: defaultHarness.Name, Model: "gpt-5.6", Effort: "high", Timeout: "30m"},
		Implement: localStageChoice{Harness: defaultHarness.Name, Model: "gpt-5.6", Effort: "high", Timeout: "4h"},
		Review:    localStageChoice{Harness: defaultHarness.Name, Model: "gpt-5.6", Effort: "high", Timeout: "1h"},
	}, []config.Harness{defaultHarness}
}

// initialWorktreeChoice prefills the setup flow's worktree-location step from
// an explicit flag value or the document's recorded location.
func initialWorktreeChoice(path, flagValue string) string {
	if strings.TrimSpace(flagValue) != "" {
		return strings.TrimSpace(flagValue)
	}
	if local, err := config.Load(path); err == nil {
		if location, ok := local.ConfiguredWorktreeLocation(); ok {
			return location.Mode
		}
	}
	return ""
}

// persistWorktreeChoice records the setup flow's worktree-location choice. A
// choice that keeps the recorded mode leaves a custom dir or root unchanged;
// an empty choice is accepted only when the document already records one
// (component-harness-execution).
func persistWorktreeChoice(path, workspace, choice string) error {
	local, err := config.Load(path)
	if err != nil {
		return err
	}
	existing, configured := local.ConfiguredWorktreeLocation()
	if strings.TrimSpace(choice) == "" {
		if configured {
			return nil
		}
		return missingWorktreeLocationError(path)
	}
	location, err := config.ParseWorktreeLocation(choice)
	if err != nil {
		return err
	}
	if configured && location.Mode == existing.Mode && location.Dir == "" && location.Root == "" {
		return nil
	}
	return writeWorktreeLocation(path, workspace, location)
}

// worktreeLocationGroup is the setup flow's worktree-location step, shown on
// every flow including one that accepts defaults.
func worktreeLocationGroup(choice *string) *huh.Group {
	return huh.NewGroup(huh.NewSelect[string]().
		Title("Task worktree location").
		Description("Where conveyor checkout, conveyor run, and the worker create new task worktrees.").
		Options(
			huh.NewOption("In-repository: <primary checkout>/_worktrees/<repo>-task-<id> - visible beside the code and to IDE worktree pickers; needs a .git/info/exclude entry; tools that do not read Git excludes (Docker build contexts, some file watchers and indexers) see the nested worktrees", config.WorktreeLocationRepository),
			huh.NewOption("Machine-wide: ~/.conveyor/worktrees/<repo>-task-<id> - one place for every repository; outside the project folder", config.WorktreeLocationMachine),
		).
		Value(choice)).Title("Worktree location")
}
