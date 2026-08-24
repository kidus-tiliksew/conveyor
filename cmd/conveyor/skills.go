package main

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/releaseinfo"
	"github.com/spf13/cobra"
)

const skillsOwnerPrefix = "<!-- conveyor:skills-install owner=v1 version="

// embeddedSkills is the release-carried snapshot of the repository's agent
// skill wrappers and the canonical playbooks those wrappers consume
// (req-260811-0ee057 REQ-12/AC-12.1).
//
//go:embed skills_assets/*/*
var embeddedSkills embed.FS

type embeddedSkillFile struct {
	assetPath   string
	sourcePath  string
	relative    string
	rewriteFrom string
	rewriteTo   string
	skill       bool
}

var embeddedSkillManifest = []embeddedSkillFile{
	{
		assetPath:   "skills_assets/conveyor-testing-doc/SKILL.md",
		sourcePath:  ".claude/skills/conveyor-testing-doc/SKILL.md",
		relative:    "conveyor-testing-doc/SKILL.md",
		rewriteFrom: "[docs/playbooks/conveyor-testing-doc.md](../../../docs/playbooks/conveyor-testing-doc.md)",
		rewriteTo:   "[conveyor-testing-doc.md](conveyor-testing-doc.md)",
		skill:       true,
	},
	{
		assetPath:  "skills_assets/conveyor-testing-doc/conveyor-testing-doc.md",
		sourcePath: "docs/playbooks/conveyor-testing-doc.md",
		relative:   "conveyor-testing-doc/conveyor-testing-doc.md",
	},
	{
		assetPath:   "skills_assets/conveyor-work/SKILL.md",
		sourcePath:  ".claude/skills/conveyor-work/SKILL.md",
		relative:    "conveyor-work/SKILL.md",
		rewriteFrom: "[docs/playbooks/conveyor-work.md](../../../docs/playbooks/conveyor-work.md)",
		rewriteTo:   "[conveyor-work.md](conveyor-work.md)",
		skill:       true,
	},
	{
		assetPath:  "skills_assets/conveyor-work/conveyor-work.md",
		sourcePath: "docs/playbooks/conveyor-work.md",
		relative:   "conveyor-work/conveyor-work.md",
	},
	{
		assetPath:   "skills_assets/conveyor-plan/SKILL.md",
		sourcePath:  ".claude/skills/conveyor-plan/SKILL.md",
		relative:    "conveyor-plan/SKILL.md",
		rewriteFrom: "[docs/playbooks/conveyor-planning.md](../../../docs/playbooks/conveyor-planning.md)",
		rewriteTo:   "[conveyor-planning.md](conveyor-planning.md)",
		skill:       true,
	},
	{
		assetPath:  "skills_assets/conveyor-plan/conveyor-planning.md",
		sourcePath: "docs/playbooks/conveyor-planning.md",
		relative:   "conveyor-plan/conveyor-planning.md",
	},
	{
		assetPath:   "skills_assets/conveyor-file-tasks/SKILL.md",
		sourcePath:  ".claude/skills/conveyor-file-tasks/SKILL.md",
		relative:    "conveyor-file-tasks/SKILL.md",
		rewriteFrom: "[docs/playbooks/conveyor-task-filing.md](../../../docs/playbooks/conveyor-task-filing.md)",
		rewriteTo:   "[conveyor-task-filing.md](conveyor-task-filing.md)",
		skill:       true,
	},
	{
		assetPath:  "skills_assets/conveyor-file-tasks/conveyor-task-filing.md",
		sourcePath: "docs/playbooks/conveyor-task-filing.md",
		relative:   "conveyor-file-tasks/conveyor-task-filing.md",
	},
}

type skillInstallFile struct {
	embeddedSkillFile
	tool       string
	target     string
	content    []byte
	prior      []byte
	mode       fs.FileMode
	exists     bool
	status     string
	safetyRoot string
}

type skillTool struct {
	name       string
	binary     string
	root       string
	legacyPath string
}

type skillDestination struct {
	tool       skillTool
	root       string
	legacyPath string
}

type skillInstallReport struct {
	tool   string
	status string
	target string
}

var supportedSkillTools = []skillTool{
	{name: "claude", binary: "claude", root: ".claude/skills"},
	{name: "codex", binary: "codex", root: ".codex/skills", legacyPath: ".codex/plugins/cache/personal/conveyor/0.1.0"},
}

func skillsCmd() *cobra.Command {
	command := &cobra.Command{Use: "skills", Short: "Manage Conveyor's embedded agent skills"}
	command.AddCommand(skillsInstallCmd())
	return command
}

func skillsInstallCmd() *cobra.Command {
	return skillsInstallCmdWithLookPath(exec.LookPath)
}

func skillsInstallCmdWithLookPath(lookPath func(string) (string, error)) *cobra.Command {
	var project, list, adopt, force bool
	var selectedTool string
	command := &cobra.Command{
		Use:   "install",
		Short: "Install Conveyor's embedded agent skills",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			base, err := skillsInstallBase(project)
			if err != nil {
				return err
			}
			tools, err := selectSkillTools(selectedTool, lookPath)
			if err != nil {
				return err
			}
			destinations := skillDestinations(base, tools)
			if list {
				return listEmbeddedSkillsForDestinations(cmd, base, destinations, releaseinfo.Version)
			}
			results, reports, err := installEmbeddedSkillsForDestinationsWithForce(base, destinations, releaseinfo.Version, adopt, force)
			if err != nil {
				return err
			}
			for _, result := range results {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", result.tool, result.status, result.target)
			}
			for _, report := range reports {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", report.tool, report.status, report.target)
			}
			return nil
		},
	}
	command.Flags().BoolVar(&project, "project", false, "install under each tool's project skills directory instead of the user-global directory")
	command.Flags().BoolVar(&list, "list", false, "list embedded files and their installed state without writing")
	command.Flags().StringVar(&selectedTool, "tool", "", "install only for one detected tool (claude or codex)")
	command.Flags().BoolVar(&adopt, "adopt", false, "adopt unmarked skill files in a selected native destination")
	command.Flags().BoolVar(&force, "force", false, "allow replacing managed skills installed by a newer Conveyor release")
	return command
}

func skillsInstallBase(project bool) (string, error) {
	var base string
	var err error
	if project {
		base, err = os.Getwd()
	} else {
		base, err = os.UserHomeDir()
	}
	if err != nil {
		return "", fmt.Errorf("resolve skills destination: %w", err)
	}
	base, err = filepath.Abs(base)
	if err != nil {
		return "", fmt.Errorf("resolve skills destination: %w", err)
	}
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		return "", fmt.Errorf("resolve skills destination %s: %w", base, err)
	}
	return base, nil
}

func skillsDestination(project bool) (string, string, error) {
	base, err := skillsInstallBase(project)
	if err != nil {
		return "", "", err
	}
	return base, filepath.Join(base, ".claude", "skills"), nil
}

func selectSkillTools(selected string, lookPath func(string) (string, error)) ([]skillTool, error) {
	selected = strings.TrimSpace(strings.ToLower(selected))
	var tools []skillTool
	known := false
	for _, tool := range supportedSkillTools {
		if selected != "" && selected != tool.name {
			continue
		}
		known = true
		if _, err := lookPath(tool.binary); err == nil {
			tools = append(tools, tool)
		} else if selected != "" {
			return nil, fmt.Errorf("selected tool %q is not available on PATH", selected)
		}
	}
	if selected != "" && !known {
		return nil, fmt.Errorf("unsupported tool %q; supported tools: claude, codex", selected)
	}
	if len(tools) == 0 {
		return nil, fmt.Errorf("no supported agent tooling detected on PATH (looked for claude and codex)")
	}
	return tools, nil
}

func skillDestinations(base string, tools []skillTool) []skillDestination {
	destinations := make([]skillDestination, 0, len(tools))
	for _, tool := range tools {
		destination := skillDestination{tool: tool, root: filepath.Join(base, filepath.FromSlash(tool.root))}
		if tool.legacyPath != "" {
			destination.legacyPath = filepath.Join(base, filepath.FromSlash(tool.legacyPath))
		}
		destinations = append(destinations, destination)
	}
	return destinations
}

func listEmbeddedSkills(cmd *cobra.Command, base, root, version string) error {
	destination := skillDestination{tool: supportedSkillTools[0], root: root}
	return listEmbeddedSkillsForDestinations(cmd, base, []skillDestination{destination}, version)
}

func listEmbeddedSkillsForDestinations(cmd *cobra.Command, base string, destinations []skillDestination, version string) error {
	fmt.Fprintf(cmd.OutOrStdout(), "embedded release %s\n", version)
	for _, destination := range destinations {
		plan, err := buildSkillInstallPlanForTool(base, destination, version, false, false)
		if err != nil {
			return err
		}
		for _, item := range plan {
			status := item.status
			if status == "created" {
				status = "not installed (would create)"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\t%s\n", item.tool, item.relative, item.sourcePath, item.target, status)
		}
		if exists, err := legacySkillArtifactExists(base, destination); err != nil {
			return err
		} else if exists {
			fmt.Fprintf(cmd.OutOrStdout(), "%s\tlegacy plugin\t%s\tunmanaged (left untouched)\n", destination.tool.name, destination.legacyPath)
		}
	}
	return nil
}

// installEmbeddedSkills preflights the complete set before writing and only
// refreshes files carrying Conveyor's marker (req-260811-0ee057 AC-12.2).
func installEmbeddedSkills(base, root, version string) ([]skillInstallFile, error) {
	destination := skillDestination{tool: supportedSkillTools[0], root: root}
	plan, _, err := installEmbeddedSkillsForDestinations(base, []skillDestination{destination}, version, false)
	return plan, err
}

func installEmbeddedSkillsForDestinations(base string, destinations []skillDestination, version string, adopt bool) ([]skillInstallFile, []skillInstallReport, error) {
	return installEmbeddedSkillsForDestinationsWithForce(base, destinations, version, adopt, false)
}

func installEmbeddedSkillsForDestinationsWithForce(base string, destinations []skillDestination, version string, adopt, force bool) ([]skillInstallFile, []skillInstallReport, error) {
	active, reports, err := activeSkillDestinations(base, destinations)
	if err != nil {
		return nil, nil, err
	}
	plan := make([]skillInstallFile, 0, len(active)*len(embeddedSkillManifest))
	for _, destination := range active {
		items, buildErr := buildSkillInstallPlanForToolWithForce(base, destination, version, true, adopt, force)
		if buildErr != nil {
			return nil, nil, buildErr
		}
		plan = append(plan, items...)
	}
	for _, item := range plan {
		if item.status == "collision" {
			return nil, nil, fmt.Errorf("refusing to overwrite %s: file is not owned by Conveyor; move it aside or rerun with --adopt", item.target)
		}
	}

	type stagedFile struct {
		index int
		path  string
	}
	staged := make([]stagedFile, 0, len(plan))
	cleanup := func() {
		for _, item := range staged {
			_ = os.Remove(item.path)
		}
	}
	defer cleanup()

	for index := range plan {
		if plan[index].status == "unchanged" {
			continue
		}
		if err = ensureSafeInstallPath(plan[index].safetyRoot, plan[index].target); err != nil {
			return nil, nil, err
		}
		directory := filepath.Dir(plan[index].target)
		if err = os.MkdirAll(directory, 0o755); err != nil {
			return nil, nil, fmt.Errorf("create skills directory %s: %w", directory, err)
		}
		if err = ensureSafeInstallPath(plan[index].safetyRoot, plan[index].target); err != nil {
			return nil, nil, err
		}
		temporary, createErr := os.CreateTemp(directory, ".conveyor-skills-*")
		if createErr != nil {
			return nil, nil, fmt.Errorf("stage %s: %w", plan[index].target, createErr)
		}
		temporaryPath := temporary.Name()
		if chmodErr := temporary.Chmod(0o644); chmodErr != nil {
			_ = temporary.Close()
			_ = os.Remove(temporaryPath)
			return nil, nil, fmt.Errorf("set permissions on staged %s: %w", plan[index].target, chmodErr)
		}
		if _, writeErr := temporary.Write(plan[index].content); writeErr != nil {
			_ = temporary.Close()
			_ = os.Remove(temporaryPath)
			return nil, nil, fmt.Errorf("stage %s: %w", plan[index].target, writeErr)
		}
		if syncErr := temporary.Sync(); syncErr != nil {
			_ = temporary.Close()
			_ = os.Remove(temporaryPath)
			return nil, nil, fmt.Errorf("sync staged %s: %w", plan[index].target, syncErr)
		}
		if closeErr := temporary.Close(); closeErr != nil {
			_ = os.Remove(temporaryPath)
			return nil, nil, fmt.Errorf("close staged %s: %w", plan[index].target, closeErr)
		}
		staged = append(staged, stagedFile{index: index, path: temporaryPath})
	}

	// Re-run the full preflight after staging so a collision or symlink that
	// appeared while files were prepared cannot be overwritten.
	for _, destination := range active {
		rechecked, recheckErr := buildSkillInstallPlanForToolWithForce(base, destination, version, true, adopt, force)
		if recheckErr != nil {
			return nil, nil, recheckErr
		}
		for _, item := range rechecked {
			if item.status == "collision" {
				return nil, nil, fmt.Errorf("refusing to overwrite %s: file is not owned by Conveyor", item.target)
			}
		}
	}

	written := make([]int, 0, len(staged))
	rollback := func() error {
		var rollbackErr error
		for index := len(written) - 1; index >= 0; index-- {
			item := plan[written[index]]
			if item.exists {
				if restoreErr := os.WriteFile(item.target, item.prior, item.mode.Perm()); restoreErr != nil {
					rollbackErr = errors.Join(rollbackErr, fmt.Errorf("restore %s: %w", item.target, restoreErr))
				}
			} else if removeErr := os.Remove(item.target); removeErr != nil && !os.IsNotExist(removeErr) {
				rollbackErr = errors.Join(rollbackErr, fmt.Errorf("remove partial %s: %w", item.target, removeErr))
			}
		}
		return rollbackErr
	}
	for _, item := range staged {
		if err = ensureSafeInstallPath(plan[item.index].safetyRoot, plan[item.index].target); err == nil {
			err = os.Rename(item.path, plan[item.index].target)
		}
		if err != nil {
			return nil, nil, errors.Join(fmt.Errorf("install %s: %w", plan[item.index].target, err), rollback())
		}
		written = append(written, item.index)
	}
	return plan, reports, nil
}

func buildSkillInstallPlan(base, root, version string, rejectUnsafe bool) ([]skillInstallFile, error) {
	destination := skillDestination{tool: supportedSkillTools[0], root: root}
	return buildSkillInstallPlanForTool(base, destination, version, rejectUnsafe, false)
}

func buildSkillInstallPlanForTool(base string, destination skillDestination, version string, rejectUnsafe, adopt bool) ([]skillInstallFile, error) {
	return buildSkillInstallPlanForToolWithForce(base, destination, version, rejectUnsafe, adopt, false)
}

func buildSkillInstallPlanForToolWithForce(base string, destination skillDestination, version string, rejectUnsafe, adopt, force bool) ([]skillInstallFile, error) {
	if err := validMarkerValue(version); err != nil {
		return nil, err
	}
	absoluteBase, err := filepath.Abs(base)
	if err != nil {
		return nil, fmt.Errorf("resolve install base: %w", err)
	}
	absoluteRoot, err := filepath.Abs(destination.root)
	if err != nil {
		return nil, fmt.Errorf("resolve install root: %w", err)
	}
	if !pathWithin(absoluteBase, absoluteRoot) {
		return nil, fmt.Errorf("skills root %s is outside selected destination %s", absoluteRoot, absoluteBase)
	}
	resolvedRoot, err := resolveInstallDestination(absoluteRoot)
	if err != nil {
		return nil, err
	}
	safetyRoot, err := existingInstallAncestor(resolvedRoot)
	if err != nil {
		return nil, err
	}
	plan := make([]skillInstallFile, 0, len(embeddedSkillManifest))
	for _, asset := range embeddedSkillManifest {
		target := filepath.Join(resolvedRoot, filepath.FromSlash(asset.relative))
		if !pathWithin(resolvedRoot, target) {
			return nil, fmt.Errorf("embedded skills path %q escapes %s", asset.relative, resolvedRoot)
		}
		if safetyErr := ensureSafeInstallPath(safetyRoot, target); safetyErr != nil {
			if rejectUnsafe {
				return nil, safetyErr
			}
			plan = append(plan, skillInstallFile{embeddedSkillFile: asset, tool: destination.tool.name, target: target, mode: 0o644, status: "unsafe: " + safetyErr.Error(), safetyRoot: safetyRoot})
			continue
		}
		content, renderErr := renderEmbeddedSkill(asset, version)
		if renderErr != nil {
			return nil, renderErr
		}
		item := skillInstallFile{embeddedSkillFile: asset, tool: destination.tool.name, target: target, content: content, mode: 0o644, status: "missing", safetyRoot: safetyRoot}
		info, statErr := os.Lstat(target)
		switch {
		case statErr == nil:
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				if rejectUnsafe {
					return nil, fmt.Errorf("refusing unsafe skills destination %s", target)
				}
				item.status = "unsafe"
				break
			}
			item.prior, err = os.ReadFile(target)
			if err != nil {
				return nil, fmt.Errorf("read installed skill %s: %w", target, err)
			}
			item.exists, item.mode = true, info.Mode()
			installedVersion, owned := managedSkillVersion(item.prior, asset.sourcePath)
			switch {
			case !owned && adopt:
				item.status = "adopted"
			case !owned:
				item.status = "collision"
			case bytes.Equal(item.prior, item.content):
				item.status = "unchanged"
			case compareReleaseVersions(installedVersion, version) > 0 && !force:
				if rejectUnsafe {
					return nil, fmt.Errorf("refusing to downgrade managed skill %s from %s to %s; rerun with --force to override", target, installedVersion, version)
				}
				item.status = "downgrade refused " + installedVersion + " -> " + version + " (use --force)"
			case compareReleaseVersions(installedVersion, version) > 0:
				item.status = "downgrade " + installedVersion + " -> " + version + " (forced)"
			default:
				item.status = "refresh " + installedVersion + " -> " + version
			}
		case os.IsNotExist(statErr):
			item.status = "created"
		default:
			return nil, fmt.Errorf("inspect skills destination %s: %w", target, statErr)
		}
		plan = append(plan, item)
	}
	sort.Slice(plan, func(i, j int) bool { return plan[i].relative < plan[j].relative })
	return plan, nil
}

// resolveInstallDestination evaluates every existing path component and then
// appends any missing suffix. This permits dotfile-manager symlinks such as
// ~/.claude while ensuring all subsequent safety checks operate on the real
// destination rather than the logical symlink path.
func resolveInstallDestination(path string) (string, error) {
	path = filepath.Clean(path)
	cursor := path
	var suffix []string
	for {
		_, err := os.Lstat(cursor)
		if err == nil {
			resolved, resolveErr := filepath.EvalSymlinks(cursor)
			if resolveErr != nil {
				return "", fmt.Errorf("resolve skills destination %s: %w", path, resolveErr)
			}
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("inspect skills destination %s: %w", cursor, err)
		}
		parent := filepath.Dir(cursor)
		if parent == cursor {
			return "", fmt.Errorf("resolve skills destination %s: no existing ancestor", path)
		}
		suffix = append(suffix, filepath.Base(cursor))
		cursor = parent
	}
}

func existingInstallAncestor(path string) (string, error) {
	cursor := filepath.Clean(path)
	for {
		info, err := os.Lstat(cursor)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return "", fmt.Errorf("refusing unsafe skills destination %s", cursor)
			}
			return cursor, nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("inspect skills destination %s: %w", cursor, err)
		}
		parent := filepath.Dir(cursor)
		if parent == cursor {
			return "", fmt.Errorf("resolve skills destination %s: no existing ancestor", path)
		}
		cursor = parent
	}
}

func compareReleaseVersions(left, right string) int {
	type releaseVersion struct {
		numbers    []int
		prerelease []string
	}
	parse := func(value string) (releaseVersion, bool) {
		value = strings.TrimPrefix(strings.TrimSpace(value), "v")
		value = strings.SplitN(value, "+", 2)[0]
		mainAndPrerelease := strings.SplitN(value, "-", 2)
		parts := strings.Split(mainAndPrerelease[0], ".")
		if len(parts) == 0 || len(parts) > 3 {
			return releaseVersion{}, false
		}
		result := make([]int, 3)
		for index, part := range parts {
			parsed, err := strconv.Atoi(part)
			if err != nil || parsed < 0 {
				return releaseVersion{}, false
			}
			result[index] = parsed
		}
		parsed := releaseVersion{numbers: result}
		if len(mainAndPrerelease) == 2 {
			if mainAndPrerelease[1] == "" {
				return releaseVersion{}, false
			}
			parsed.prerelease = strings.Split(mainAndPrerelease[1], ".")
		}
		return parsed, true
	}
	l, lok := parse(left)
	r, rok := parse(right)
	if !lok || !rok {
		return 0
	}
	for index := range l.numbers {
		if l.numbers[index] < r.numbers[index] {
			return -1
		}
		if l.numbers[index] > r.numbers[index] {
			return 1
		}
	}
	if len(l.prerelease) == 0 && len(r.prerelease) > 0 {
		return 1
	}
	if len(l.prerelease) > 0 && len(r.prerelease) == 0 {
		return -1
	}
	for index := 0; index < min(len(l.prerelease), len(r.prerelease)); index++ {
		leftPart, leftErr := strconv.Atoi(l.prerelease[index])
		rightPart, rightErr := strconv.Atoi(r.prerelease[index])
		switch {
		case leftErr == nil && rightErr == nil && leftPart != rightPart:
			if leftPart < rightPart {
				return -1
			}
			return 1
		case leftErr == nil && rightErr != nil:
			return -1
		case leftErr != nil && rightErr == nil:
			return 1
		case l.prerelease[index] < r.prerelease[index]:
			return -1
		case l.prerelease[index] > r.prerelease[index]:
			return 1
		}
	}
	if len(l.prerelease) < len(r.prerelease) {
		return -1
	}
	if len(l.prerelease) > len(r.prerelease) {
		return 1
	}
	return 0
}

func activeSkillDestinations(base string, destinations []skillDestination) ([]skillDestination, []skillInstallReport, error) {
	active := make([]skillDestination, 0, len(destinations))
	var reports []skillInstallReport
	for _, destination := range destinations {
		legacy, err := legacySkillArtifactExists(base, destination)
		if err != nil {
			return nil, nil, err
		}
		if !legacy {
			active = append(active, destination)
			continue
		}
		active = append(active, destination)
		reports = append(reports, skillInstallReport{tool: destination.tool.name, status: "skipped unmanaged legacy plugin", target: destination.legacyPath})
	}
	return active, reports, nil
}

func legacySkillArtifactExists(base string, destination skillDestination) (bool, error) {
	if destination.legacyPath == "" {
		return false, nil
	}
	if !pathWithin(base, destination.legacyPath) {
		return false, fmt.Errorf("legacy skill artifact %s is outside selected destination %s", destination.legacyPath, base)
	}
	if info, err := os.Lstat(destination.legacyPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("refusing symlink legacy skill artifact: %s", destination.legacyPath)
	} else if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("inspect legacy skill artifact %s: %w", destination.legacyPath, err)
	}
	resolvedPath, err := resolveInstallDestination(destination.legacyPath)
	if err != nil {
		return false, err
	}
	info, err := os.Lstat(resolvedPath)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 {
			return false, fmt.Errorf("refusing symlink legacy skill artifact: %s", resolvedPath)
		}
		return true, nil
	case os.IsNotExist(err):
		return false, nil
	default:
		return false, fmt.Errorf("inspect legacy skill artifact %s: %w", resolvedPath, err)
	}
}

func renderEmbeddedSkill(asset embeddedSkillFile, version string) ([]byte, error) {
	raw, err := embeddedSkills.ReadFile(asset.assetPath)
	if err != nil {
		return nil, fmt.Errorf("read embedded skill %s: %w", asset.sourcePath, err)
	}
	content := string(raw)
	if asset.rewriteFrom != "" {
		if strings.Count(content, asset.rewriteFrom) != 1 {
			return nil, fmt.Errorf("embedded skill %s does not contain its canonical playbook reference exactly once", asset.sourcePath)
		}
		content = strings.Replace(content, asset.rewriteFrom, asset.rewriteTo, 1)
	}
	marker := skillsOwnerPrefix + version + " source=" + asset.sourcePath + " -->\n"
	if asset.skill {
		if !strings.HasPrefix(content, "---\n") {
			return nil, fmt.Errorf("embedded skill %s has no YAML frontmatter", asset.sourcePath)
		}
		frontmatterEnd := strings.Index(content[4:], "\n---\n")
		if frontmatterEnd < 0 {
			return nil, fmt.Errorf("embedded skill %s has unterminated YAML frontmatter", asset.sourcePath)
		}
		insertAt := 4 + frontmatterEnd + len("\n---\n")
		return []byte(content[:insertAt] + marker + content[insertAt:]), nil
	}
	return []byte(marker + content), nil
}

func managedSkillVersion(content []byte, source string) (string, bool) {
	limit := len(content)
	if limit > 4096 {
		limit = 4096
	}
	lines := strings.Split(string(content[:limit]), "\n")
	suffix := " source=" + source + " -->"
	for _, line := range lines {
		if strings.HasPrefix(line, skillsOwnerPrefix) && strings.HasSuffix(line, suffix) {
			version := strings.TrimSuffix(strings.TrimPrefix(line, skillsOwnerPrefix), suffix)
			if validMarkerValue(version) == nil {
				return version, true
			}
		}
	}
	return "", false
}

func validMarkerValue(version string) error {
	if strings.TrimSpace(version) == "" || strings.ContainsAny(version, "\r\n") || strings.Contains(version, "-->") {
		return fmt.Errorf("invalid release version for skills ownership marker")
	}
	return nil
}

func pathWithin(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func ensureSafeInstallPath(base, target string) error {
	if !pathWithin(base, target) {
		return fmt.Errorf("skills destination %s is outside selected destination %s", target, base)
	}
	baseInfo, err := os.Lstat(base)
	if err != nil {
		return fmt.Errorf("inspect skills destination %s: %w", base, err)
	}
	if baseInfo.Mode()&os.ModeSymlink != 0 || !baseInfo.IsDir() {
		return fmt.Errorf("refusing unsafe skills destination %s", base)
	}
	relative, err := filepath.Rel(base, target)
	if err != nil {
		return fmt.Errorf("resolve skills destination %s: %w", target, err)
	}
	cursor := base
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		cursor = filepath.Join(cursor, component)
		info, statErr := os.Lstat(cursor)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil {
			return fmt.Errorf("inspect skills destination %s: %w", cursor, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink in skills destination: %s", cursor)
		}
		if cursor != target && !info.IsDir() {
			return fmt.Errorf("refusing non-directory in skills destination: %s", cursor)
		}
	}
	return nil
}
