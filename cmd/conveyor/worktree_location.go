package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"

	"github.com/kidus-tiliksew/conveyor/cmd/conveyor/localgit"
	"github.com/kidus-tiliksew/conveyor/internal/config"
)

// The local configuration file and the worktree placement it records are
// client-local settings that conveyor checkout reads (component-git-delivery).
// No silent default exists: an unconfigured location creates no worktree.

const localConfigName = "conveyor.yaml"

type localConfigPath struct {
	Path   string
	Source string
}

func defaultLocalConfigPath() string {
	resolved, err := resolveLocalConfigPath(nil, "")
	if err == nil {
		return resolved.Path
	}
	return localConfigName
}

// resolveLocalConfigPath selects one local configuration file: --config, then
// CONVEYOR_CONFIG, then conveyor.yaml in the working directory, then the user
// default. Nothing in the file is sent to the control plane.
func resolveLocalConfigPath(cmd *cobra.Command, flagValue string) (localConfigPath, error) {
	if cmd != nil && (cmd.Flags().Changed("config") || cmd.InheritedFlags().Changed("config")) {
		return resolvedLocalConfigPath(flagValue, "flag")
	}
	if value := strings.TrimSpace(os.Getenv("CONVEYOR_CONFIG")); value != "" {
		return resolvedLocalConfigPath(value, "environment CONVEYOR_CONFIG")
	}
	cwdPath, err := filepath.Abs(localConfigName)
	if err != nil {
		return localConfigPath{}, fmt.Errorf("resolve working-directory config: %w", err)
	}
	if _, statErr := os.Stat(cwdPath); statErr == nil || !errors.Is(statErr, os.ErrNotExist) {
		return localConfigPath{Path: cwdPath, Source: "working-directory file"}, nil
	}
	root, err := userConfigDir()
	if err != nil {
		return localConfigPath{}, fmt.Errorf("resolve user config directory: %w", err)
	}
	return resolvedLocalConfigPath(filepath.Join(root, "conveyor", localConfigName), "user default")
}

func resolvedLocalConfigPath(path, source string) (localConfigPath, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return localConfigPath{}, fmt.Errorf("local config path selected by %s is empty", source)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return localConfigPath{}, fmt.Errorf("resolve local config path selected by %s: %w", source, err)
	}
	return localConfigPath{Path: filepath.Clean(absolute), Source: source}, nil
}

// loadLocalConfig reads the local configuration file through the client
// loader, which ignores execution keys with one warning line
// (component-runtime).
func loadLocalConfig(path string) (*config.Config, error) {
	return config.LoadClient(path, log.Printf)
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

func missingWorktreeLocationError(configPath string) error {
	if strings.TrimSpace(configPath) == "" {
		configPath = "<local config>"
	}
	return fmt.Errorf("no worktree location is configured in %[1]s, so no task worktree is created; choose repository (<primary checkout>/_worktrees/<repo>-task-<id>, beside the code) or machine (~/.conveyor/worktrees/<repo>-task-<id>, one place for every repository) and record it with `conveyor config set worktree_location repository --config %[1]s` or `conveyor config set worktree_location machine --config %[1]s`", configPath)
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

// writeWorktreeLocation records the location in the local configuration
// file, replacing a legacy worktree_root. It edits only those two keys of the
// authored YAML, so comments, key order, and every other key survive; a
// missing file is created holding only the location. The result is reloaded
// and validated before it replaces the file (component-git-delivery).
func writeWorktreeLocation(path string, location config.WorktreeLocation) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	location = authoredWorktreeLocation(location)
	if _, err = location.Normalized(home); err != nil {
		return err
	}
	var document yaml.Node
	original, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		document = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	case err != nil:
		return err
	default:
		if err = yaml.Unmarshal(original, &document); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		if len(document.Content) == 0 {
			document = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
		}
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("parse %s: local config is not a mapping", path)
	}
	var value yaml.Node
	if err = value.Encode(location); err != nil {
		return err
	}
	removeYAMLMappingKey(root, "worktree_root")
	setYAMLMappingKey(root, "worktree_location", &value)
	data, err := yaml.Marshal(&document)
	if err != nil {
		return err
	}
	return writeValidatedLocalConfig(path, data)
}

func removeYAMLMappingKey(node *yaml.Node, key string) {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			node.Content = append(node.Content[:i], node.Content[i+2:]...)
			return
		}
	}
}

// setYAMLMappingKey replaces a key's value in place, keeping its comments, or
// appends the key.
func setYAMLMappingKey(node *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			current := node.Content[i+1]
			head, line, foot := current.HeadComment, current.LineComment, current.FootComment
			*current = *value
			current.HeadComment, current.LineComment, current.FootComment = head, line, foot
			return
		}
	}
	node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

// writeValidatedLocalConfig writes data to a mode-0600 temporary file beside
// path, reloads it through the client loader, and renames it into place; a
// failed validation leaves the original unchanged.
func writeValidatedLocalConfig(path string, data []byte) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create local config directory: %w", err)
	}
	temp, err := os.CreateTemp(directory, ".conveyor-config-*.tmp")
	if err != nil {
		return fmt.Errorf("create local config: %w", err)
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
		return fmt.Errorf("write local config: %w", err)
	}
	if _, err = config.LoadClient(tempPath, nil); err != nil {
		return fmt.Errorf("validate local config: %w", err)
	}
	if err = os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace local config: %w", err)
	}
	return nil
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
		Description("Conveyor saves this choice in the local config and uses it for every new task worktree.").
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
	// container is a caller-supplied fixed container. It wins over location.
	container   string
	location    *config.WorktreeLocation
	configPath  string
	interactive bool
	input       io.Reader
	output      io.Writer
}

func fixedWorktreePlacement(root string) worktreePlacement {
	return worktreePlacement{container: root}
}

// checkoutWorktreePlacement builds the placement for `conveyor checkout`:
// the configured location, then an interactive choice. --path is applied by
// the caller before any placement is read. No environment variable supplies a
// container.
func checkoutWorktreePlacement(configPath string) (worktreePlacement, error) {
	placement := worktreePlacement{configPath: configPath, interactive: checkoutIsInteractive(), input: os.Stdin, output: os.Stderr}
	local, err := loadLocalConfig(configPath)
	if errors.Is(err, os.ErrNotExist) {
		return placement, nil
	}
	if err != nil {
		return worktreePlacement{}, fmt.Errorf("load local config for worktree location: %w", err)
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
	if err = writeWorktreeLocation(p.configPath, chosen); err != nil {
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
