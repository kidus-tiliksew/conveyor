package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
	"github.com/kidus-tiliksew/conveyor/internal/releaseinfo"
	"github.com/spf13/cobra"
)

const repoInitOwnerPrefix = "<!-- conveyor:repo-init owner=v1 version="
const repoInitClose = "<!-- /conveyor:repo-init -->"

func repoCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "repo", Short: "Prepare repositories for Conveyor work"}
	cmd.AddCommand(repoInitCmd())
	return cmd
}

func repoInitCmd() *cobra.Command {
	return &cobra.Command{
		Use: "init", Short: "Install repository guidance and project-scoped agent skills",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root, err := repositoryRoot(cmd.Context())
			if err != nil {
				return fmt.Errorf("req-repository-onboarding REQ-4/AC-4.6: conveyor repo init requires a repository checkout")
			}
			root, err = filepath.EvalSymlinks(root)
			if err != nil {
				return err
			}
			name, base := repoInitMetadata(cmd.Context(), root, func() (config.VersionedDocument, error) {
				return newClient().getWorkspaceConfig()
			})
			return prepareRepository(root, releaseinfo.Version, name, base, cmd.OutOrStdout())
		},
	}
}

// Metadata uses checkout's authenticated config read and origin normalization.
// Unavailable or ambiguous registration leaves placeholders, never raw URLs or errors.
func repoInitMetadata(ctx context.Context, root string, lookup func() (config.VersionedDocument, error)) (string, string) {
	const missingName, missingBase = "<registered-repository>", "<base-branch>"
	origin, err := gitx.RepositoryOriginIdentity(ctx, root)
	if err != nil {
		return missingName, missingBase
	}
	record, err := lookup()
	if err != nil {
		return missingName, missingBase
	}
	var matches []config.Repo
	for _, repo := range record.Document.Repos {
		identity, err := gitx.NormalizeRepositoryIdentity(repo.URL)
		if err == nil && identity == origin {
			matches = append(matches, repo)
		}
	}
	if len(matches) != 1 || !repoInitLabel(matches[0].Name) || !repoInitLabel(matches[0].Base) {
		return missingName, missingBase
	}
	return matches[0].Name, matches[0].Base
}

func repoInitLabel(value string) bool {
	return value != "" && !strings.ContainsAny(value, "\r\n\t`<>")
}

func renderRepoInit(version, name, base string) ([]byte, error) {
	if err := validMarkerValue(version); err != nil {
		return nil, err
	}
	asset, err := embeddedSkills.ReadFile("skills_assets/conveyor-repo-init/AGENTS.md")
	if err != nil {
		return nil, err
	}
	return []byte(strings.NewReplacer("{{version}}", version, "{{repository}}", name, "{{base}}", base).Replace(string(asset))), nil
}

// Replace only the owned span, including neither the prefix nor the suffix.
// Ambiguous, unsupported, or incomplete markers refuse before any file is staged.
func replaceRepoInit(prior, section []byte) ([]byte, error) {
	text := string(prior)
	opening, closing := strings.Count(text, "<!-- conveyor:repo-init"), strings.Count(text, "<!-- /conveyor:repo-init")
	if opening == 0 && closing == 0 {
		separator := ""
		if len(prior) > 0 && prior[len(prior)-1] != '\n' {
			separator = "\n"
		}
		return append(append(append([]byte{}, prior...), separator...), section...), nil
	}
	if opening != 1 || closing != 1 {
		return nil, fmt.Errorf("malformed or multiple Conveyor repo-init markers")
	}
	start, end := strings.Index(text, repoInitOwnerPrefix), strings.Index(text, repoInitClose)
	if start < 0 || end < start {
		return nil, fmt.Errorf("malformed Conveyor repo-init markers")
	}
	headerEnd := strings.Index(text[start:], " -->")
	if headerEnd < len(repoInitOwnerPrefix) || start+headerEnd >= end {
		return nil, fmt.Errorf("malformed Conveyor repo-init opening marker")
	}
	if err := validMarkerValue(text[start+len(repoInitOwnerPrefix) : start+headerEnd]); err != nil {
		return nil, fmt.Errorf("malformed Conveyor repo-init version: %w", err)
	}
	end += len(repoInitClose)
	return []byte(text[:start] + strings.TrimSuffix(string(section), "\n") + text[end:]), nil
}

type repoGuidanceFile struct {
	target, link, status, staged string
	prior, content               []byte
	mode                         os.FileMode
	exists                       bool
}

func planRepoGuidance(root string, section []byte) (plan []repoGuidanceFile, err error) {
	var target string
	defer func() {
		if err != nil {
			err = &os.PathError{Op: "prepare guidance", Path: target, Err: err}
		}
	}()
	plan = make([]repoGuidanceFile, 0, 2)
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		item := repoGuidanceFile{target: filepath.Join(root, name), mode: 0o644, status: "written"}
		target = item.target
		info, err := os.Lstat(item.target)
		if err == nil {
			item.exists, item.mode = true, info.Mode()
			if info.Mode()&os.ModeSymlink != 0 {
				resolved, resolveErr := filepath.EvalSymlinks(item.target)
				if name != "CLAUDE.md" || resolveErr != nil || resolved != filepath.Join(root, "AGENTS.md") || !plan[0].exists {
					return nil, fmt.Errorf("refusing %s: broken, reversed, or unsafe guidance symlink", item.target)
				}
				item.link, item.status = "AGENTS.md", plan[0].status
				plan = append(plan, item)
				continue
			}
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("refusing %s: guidance must be a regular file", item.target)
			}
			item.prior, err = os.ReadFile(item.target)
			if err != nil {
				return nil, err
			}
			item.status = "updated"
		} else if !os.IsNotExist(err) {
			return nil, err
		} else if name == "CLAUDE.md" {
			item.link = "AGENTS.md"
		}
		if item.link == "" {
			item.content, err = replaceRepoInit(item.prior, section)
			if err != nil {
				return nil, fmt.Errorf("refusing %s: %w", item.target, err)
			}
			if item.exists && bytes.Equal(item.content, item.prior) {
				item.status = "unchanged"
			}
		}
		plan = append(plan, item)
	}
	return plan, nil
}

// prepareRepository keeps req-repository-onboarding REQ-4/AC-4.1 through
// AC-4.5 in one preflight and rollback boundary (component-runtime, DEC-40).
// The shared skill installer retains ownership and refresh semantics unchanged.
type repoSkillInstaller func(string, []skillDestination, string, bool, bool) ([]skillInstallFile, []skillInstallReport, error)

func prepareRepository(root, version, name, base string, out io.Writer) error {
	return prepareRepositoryWithInstaller(root, version, name, base, out, installEmbeddedSkillsForDestinationsWithForce)
}

func prepareRepositoryWithInstaller(root, version, name, base string, out io.Writer, install repoSkillInstaller) error {
	refuse := func(tool, target string, err error) error {
		fmt.Fprintf(out, "%s\trefused\t%s\n", tool, target)
		return err
	}
	section, err := renderRepoInit(version, name, base)
	if err != nil {
		return err
	}
	guidance, err := planRepoGuidance(root, section)
	if err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			return refuse("repo", pathErr.Path, err)
		}
		return refuse("repo", root, err)
	}
	destinations := skillDestinations(root, supportedSkillTools)
	var skillPlan []skillInstallFile
	for _, destination := range destinations {
		// Unlike user-global skills install, repo init never follows tool roots
		// through dotfile-manager symlinks, which could write outside the checkout.
		if err := ensureSafeInstallPath(root, destination.root); err != nil {
			return refuse(destination.tool.name, destination.root, err)
		}
		items, err := buildSkillInstallPlanForToolWithForce(root, destination, version, true, false, false)
		if err != nil {
			return refuse(destination.tool.name, destination.root, err)
		}
		for _, item := range items {
			if item.status == "collision" {
				return refuse(item.tool, item.target, fmt.Errorf("refusing %s: file is not owned by Conveyor", item.target))
			}
		}
		skillPlan = append(skillPlan, items...)
	}
	// Remember missing directories so failed preparation removes only empty
	// directories created by this operation, deepest first.
	var directories []string
	seen := map[string]bool{}
	for _, item := range skillPlan {
		for dir := filepath.Dir(item.target); dir != root; dir = filepath.Dir(dir) {
			if _, err := os.Lstat(dir); !os.IsNotExist(err) {
				break
			}
			if !seen[dir] {
				seen[dir] = true
				directories = append(directories, dir)
			}
		}
	}
	sort.Slice(directories, func(i, j int) bool { return len(directories[i]) > len(directories[j]) })
	success := false
	defer func() {
		for _, item := range guidance {
			if item.staged != "" {
				_ = os.Remove(item.staged)
			}
		}
		if !success {
			for _, dir := range directories {
				_ = os.Remove(dir)
			}
		}
	}()
	for index := range guidance {
		item := &guidance[index]
		if item.link != "" || item.status == "unchanged" {
			continue
		}
		file, err := os.CreateTemp(root, ".conveyor-repo-init-*")
		if err != nil {
			return err
		}
		item.staged = file.Name()
		_, writeErr := file.Write(item.content)
		err = errors.Join(writeErr, file.Chmod(item.mode.Perm()), file.Sync(), file.Close())
		if err != nil {
			return err
		}
	}
	results, reports, err := install(root, destinations, version, false, false)
	if err != nil {
		return refuse("repo", root, err)
	}
	var written []repoGuidanceFile
	rollback := func() error {
		var restoreErr error
		for i := len(written) - 1; i >= 0; i-- {
			item := written[i]
			restoreErr = errors.Join(restoreErr, restoreRepoInitFile(item.target, item.prior, item.mode, item.exists))
		}
		for _, item := range results {
			if item.status != "unchanged" {
				restoreErr = errors.Join(restoreErr, restoreRepoInitFile(item.target, item.prior, item.mode, item.exists))
			}
		}
		return restoreErr
	}
	for _, item := range guidance {
		if item.status == "unchanged" || item.link != "" && item.exists {
			continue
		}
		if err = checkRepoInitPrior(item); err == nil {
			if item.link != "" {
				err = os.Symlink(item.link, item.target)
			} else {
				err = os.Rename(item.staged, item.target)
			}
		}
		if err != nil {
			return refuse("repo", item.target, errors.Join(err, rollback()))
		}
		written = append(written, item)
	}
	success = true
	for _, item := range guidance {
		fmt.Fprintf(out, "repo\t%s\t%s\n", item.status, item.target)
	}
	for _, item := range results {
		status := "updated"
		if item.status == "created" {
			status = "written"
		} else if item.status == "unchanged" {
			status = "unchanged"
		}
		fmt.Fprintf(out, "%s\t%s\t%s\n", item.tool, status, item.target)
	}
	for _, report := range reports {
		fmt.Fprintf(out, "%s\t%s\t%s\n", report.tool, report.status, report.target)
	}
	return nil
}

func checkRepoInitPrior(item repoGuidanceFile) error {
	info, err := os.Lstat(item.target)
	if !item.exists && os.IsNotExist(err) {
		return nil
	}
	if err != nil || !item.exists || !info.Mode().IsRegular() {
		return fmt.Errorf("refusing %s: guidance changed during preparation", item.target)
	}
	prior, err := os.ReadFile(item.target)
	if err != nil || !bytes.Equal(prior, item.prior) {
		return fmt.Errorf("refusing %s: guidance changed during preparation", item.target)
	}
	return nil
}

func restoreRepoInitFile(target string, prior []byte, mode os.FileMode, existed bool) error {
	var err error
	if existed {
		err = os.WriteFile(target, prior, mode.Perm())
		if err == nil {
			err = os.Chmod(target, mode.Perm())
		}
	} else {
		err = os.Remove(target)
		if os.IsNotExist(err) {
			err = nil
		}
	}
	if err != nil {
		return fmt.Errorf("restore %s: %w", target, err)
	}
	return nil
}
