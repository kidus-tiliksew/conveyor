package main

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
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
	target  string
	content []byte
	prior   []byte
	mode    fs.FileMode
	exists  bool
	status  string
}

func skillsCmd() *cobra.Command {
	command := &cobra.Command{Use: "skills", Short: "Manage Conveyor's embedded agent skills"}
	command.AddCommand(skillsInstallCmd())
	return command
}

func skillsInstallCmd() *cobra.Command {
	var project, list bool
	command := &cobra.Command{
		Use:   "install",
		Short: "Install Conveyor's embedded agent skills",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			base, root, err := skillsDestination(project)
			if err != nil {
				return err
			}
			if list {
				return listEmbeddedSkills(cmd, base, root, releaseinfo.Version)
			}
			results, err := installEmbeddedSkills(base, root, releaseinfo.Version)
			if err != nil {
				return err
			}
			for _, result := range results {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", result.status, result.target)
			}
			return nil
		},
	}
	command.Flags().BoolVar(&project, "project", false, "install under ./.claude/skills instead of the user-global directory")
	command.Flags().BoolVar(&list, "list", false, "list embedded files and their installed state without writing")
	return command
}

func skillsDestination(project bool) (string, string, error) {
	var base string
	var err error
	if project {
		base, err = os.Getwd()
	} else {
		base, err = os.UserHomeDir()
	}
	if err != nil {
		return "", "", fmt.Errorf("resolve skills destination: %w", err)
	}
	base, err = filepath.Abs(base)
	if err != nil {
		return "", "", fmt.Errorf("resolve skills destination: %w", err)
	}
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		return "", "", fmt.Errorf("resolve skills destination %s: %w", base, err)
	}
	return base, filepath.Join(base, ".claude", "skills"), nil
}

func listEmbeddedSkills(cmd *cobra.Command, base, root, version string) error {
	plan, err := buildSkillInstallPlan(base, root, version, false)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "embedded release %s\n", version)
	for _, item := range plan {
		fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\n", item.relative, item.sourcePath, item.target, item.status)
	}
	return nil
}

// installEmbeddedSkills preflights the complete set before writing and only
// refreshes files carrying Conveyor's marker (req-260811-0ee057 AC-12.2).
func installEmbeddedSkills(base, root, version string) ([]skillInstallFile, error) {
	plan, err := buildSkillInstallPlan(base, root, version, true)
	if err != nil {
		return nil, err
	}
	for _, item := range plan {
		if item.status == "collision" {
			return nil, fmt.Errorf("refusing to overwrite %s: file is not owned by Conveyor; move it aside or choose the other install scope", item.target)
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
		if err = ensureSafeInstallPath(base, plan[index].target); err != nil {
			return nil, err
		}
		directory := filepath.Dir(plan[index].target)
		if err = os.MkdirAll(directory, 0o755); err != nil {
			return nil, fmt.Errorf("create skills directory %s: %w", directory, err)
		}
		if err = ensureSafeInstallPath(base, plan[index].target); err != nil {
			return nil, err
		}
		temporary, createErr := os.CreateTemp(directory, ".conveyor-skills-*")
		if createErr != nil {
			return nil, fmt.Errorf("stage %s: %w", plan[index].target, createErr)
		}
		temporaryPath := temporary.Name()
		if chmodErr := temporary.Chmod(0o644); chmodErr != nil {
			_ = temporary.Close()
			_ = os.Remove(temporaryPath)
			return nil, fmt.Errorf("set permissions on staged %s: %w", plan[index].target, chmodErr)
		}
		if _, writeErr := temporary.Write(plan[index].content); writeErr != nil {
			_ = temporary.Close()
			_ = os.Remove(temporaryPath)
			return nil, fmt.Errorf("stage %s: %w", plan[index].target, writeErr)
		}
		if syncErr := temporary.Sync(); syncErr != nil {
			_ = temporary.Close()
			_ = os.Remove(temporaryPath)
			return nil, fmt.Errorf("sync staged %s: %w", plan[index].target, syncErr)
		}
		if closeErr := temporary.Close(); closeErr != nil {
			_ = os.Remove(temporaryPath)
			return nil, fmt.Errorf("close staged %s: %w", plan[index].target, closeErr)
		}
		staged = append(staged, stagedFile{index: index, path: temporaryPath})
	}

	// Re-run the full preflight after staging so a collision or symlink that
	// appeared while files were prepared cannot be overwritten.
	rechecked, err := buildSkillInstallPlan(base, root, version, true)
	if err != nil {
		return nil, err
	}
	for _, item := range rechecked {
		if item.status == "collision" {
			return nil, fmt.Errorf("refusing to overwrite %s: file is not owned by Conveyor", item.target)
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
		if err = ensureSafeInstallPath(base, plan[item.index].target); err == nil {
			err = os.Rename(item.path, plan[item.index].target)
		}
		if err != nil {
			return nil, errors.Join(fmt.Errorf("install %s: %w", plan[item.index].target, err), rollback())
		}
		written = append(written, item.index)
	}
	return plan, nil
}

func buildSkillInstallPlan(base, root, version string, rejectUnsafe bool) ([]skillInstallFile, error) {
	if err := validMarkerValue(version); err != nil {
		return nil, err
	}
	absoluteBase, err := filepath.Abs(base)
	if err != nil {
		return nil, fmt.Errorf("resolve install base: %w", err)
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve install root: %w", err)
	}
	if !pathWithin(absoluteBase, absoluteRoot) {
		return nil, fmt.Errorf("skills root %s is outside selected destination %s", absoluteRoot, absoluteBase)
	}
	plan := make([]skillInstallFile, 0, len(embeddedSkillManifest))
	for _, asset := range embeddedSkillManifest {
		target := filepath.Join(absoluteRoot, filepath.FromSlash(asset.relative))
		if !pathWithin(absoluteRoot, target) {
			return nil, fmt.Errorf("embedded skills path %q escapes %s", asset.relative, absoluteRoot)
		}
		if safetyErr := ensureSafeInstallPath(absoluteBase, target); safetyErr != nil {
			if rejectUnsafe {
				return nil, safetyErr
			}
			plan = append(plan, skillInstallFile{embeddedSkillFile: asset, target: target, mode: 0o644, status: "unsafe: " + safetyErr.Error()})
			continue
		}
		content, renderErr := renderEmbeddedSkill(asset, version)
		if renderErr != nil {
			return nil, renderErr
		}
		item := skillInstallFile{embeddedSkillFile: asset, target: target, content: content, mode: 0o644, status: "missing"}
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
			case !owned:
				item.status = "collision"
			case bytes.Equal(item.prior, item.content):
				item.status = "unchanged"
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
