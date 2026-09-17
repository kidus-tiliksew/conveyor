package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/kidus-tiliksew/conveyor/cmd/conveyor/localgit"
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
	var guidanceOnly bool
	cmd := &cobra.Command{
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
			c := newClient()
			connection := repoInitConnection(cmd.Context(), root, c, c.getWorkspaceConfig)
			return prepareRepositoryWithOptions(root, releaseinfo.Version, connection.Name, connection.Base, cmd.OutOrStdout(), repoInitOptions{guidanceOnly: guidanceOnly}, installEmbeddedSkillsForDestinationsWithForce, connection)
		},
	}
	cmd.Flags().BoolVar(&guidanceOnly, "guidance-only", false, "Refresh root guidance without inspecting or installing project skills")
	return cmd
}

// req-repository-onboarding AC-4.7: all fields come from one authenticated
// registration, never from a singleton fallback or inferred network target.
type repoInitContext struct{ Server, Workspace, Name, Base string }

func unresolvedRepoInitContext() repoInitContext {
	return repoInitContext{Name: "<registered-repository>", Base: "<base-branch>"}
}

func repoInitConnection(ctx context.Context, root string, c *client, lookup func() (config.VersionedDocument, error)) repoInitContext {
	missing := unresolvedRepoInitContext()
	server, ok := repoInitServer(c.base)
	if c.configErr != nil || !ok || server != c.base || c.resolved.Server.Source == "default" || c.resolved.Server.Source == "" || c.token == "" || !repoInitLabel(c.workspace) {
		return missing
	}
	origin, err := localgit.RepositoryOriginIdentity(ctx, root)
	if err != nil {
		return missing
	}
	record, err := lookup()
	if err != nil || record.Document.Workspace != c.workspace {
		return missing
	}
	var matches []config.Repo
	for _, repo := range record.Document.Repos {
		identity, err := gitx.NormalizeRepositoryIdentity(repo.URL)
		if err == nil && identity == origin {
			matches = append(matches, repo)
		}
	}
	if len(matches) != 1 || !repoInitLabel(matches[0].Name) || !repoInitLabel(matches[0].Base) {
		return missing
	}
	result := repoInitContext{Server: server, Workspace: c.workspace, Name: matches[0].Name, Base: matches[0].Base}
	for _, field := range []string{result.Server, result.Workspace, result.Name, result.Base} {
		if strings.Contains(field, c.token) {
			return missing
		}
	}
	return result
}

func repoInitLabel(value string) bool {
	if value == "" {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._/-", c)) {
			return false
		}
	}
	return true
}

func repoInitServer(value string) (string, bool) {
	// Restrict both raw and decoded URL text before it reaches Markdown or a
	// single-quoted shell argument. normalizeServerURL supplies CLI parity.
	safe := func(s string) bool {
		for _, c := range s {
			if c <= ' ' || c >= 127 || strings.ContainsRune("'\"`<>$\\", c) {
				return false
			}
		}
		return true
	}
	if !safe(value) {
		return "", false
	}
	canonical, err := normalizeServerURL(value)
	if err != nil {
		return "", false
	}
	u, err := url.Parse(canonical)
	if err != nil || !safe(u.Path) || u.ForceQuery || strings.ContainsAny(value, "?#") {
		return "", false
	}
	host := strings.TrimSuffix(u.Hostname(), ".")
	ip := net.ParseIP(host)
	if strings.Contains(host, "%") || host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") || ip != nil && (ip.IsLoopback() || ip.IsUnspecified()) {
		return "", false
	}
	return canonical, true
}

func renderRepoInit(version, name, base string, connection ...repoInitContext) ([]byte, error) {
	if err := validMarkerValue(version); err != nil {
		return nil, err
	}
	asset, err := embeddedSkills.ReadFile("skills_assets/conveyor-repo-init/AGENTS.md")
	if err != nil {
		return nil, err
	}
	contextText := "Connection context is unresolved. Obtain an explicit server URL and immutable workspace ID, then rerun `conveyor --server '<server>' --workspace '<workspace-id>' repo init`. Do not use a guessed endpoint."
	if len(connection) > 0 && connection[0].Server != "" {
		c := connection[0]
		contextText = fmt.Sprintf("Server: `%s`. Workspace: `%s`.\nSelect a native MCP connection whose endpoint matches this server and pass workspace `%s` on every call. MCP registration names vary by machine; registering MCP does not set CLI defaults.\nCLI example: `conveyor --server '%s' --workspace '%s' task list`.\nRefresh this owned section and the project-scoped skills through ordinary task delivery with `conveyor --server '%s' --workspace '%s' repo init`. To preserve maintained source skill wrappers, refresh only guidance with `conveyor --server '%s' --workspace '%s' repo init --guidance-only`.", c.Server, c.Workspace, c.Workspace, c.Server, c.Workspace, c.Server, c.Workspace, c.Server, c.Workspace)
	}
	return []byte(strings.NewReplacer("{{version}}", version, "{{repository}}", name, "{{base}}", base, "{{connection}}", contextText).Replace(string(asset))), nil
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
	// AC-4.1 / component-runtime: preflight the logical pair before reading
	// sections. Only a direct link to the other regular root file is supported.
	names := []string{"AGENTS.md", "CLAUDE.md"}
	plan = make([]repoGuidanceFile, 2)
	for i, name := range names {
		item := &plan[i]
		*item = repoGuidanceFile{target: filepath.Join(root, name), mode: 0o644, status: "written"}
		target = item.target
		info, statErr := os.Lstat(item.target)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil {
			return nil, statErr
		}
		item.exists, item.mode = true, info.Mode()
		if info.Mode()&os.ModeSymlink != 0 {
			item.link, err = os.Readlink(item.target)
			if err != nil {
				return nil, err
			}
			other := filepath.Join(root, names[1-i])
			// Do not clean away traversal or resolve intermediate symlinks.
			if item.link != names[1-i] && item.link != "./"+names[1-i] && item.link != other {
				return nil, fmt.Errorf("refusing %s: unsafe guidance symlink", item.target)
			}
			peer, peerErr := os.Lstat(other)
			if peerErr != nil || !peer.Mode().IsRegular() {
				return nil, fmt.Errorf("refusing %s: guidance symlink target must be regular", item.target)
			}
		} else if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("refusing %s: guidance must be a regular file", item.target)
		}
	}
	if !plan[1].exists {
		plan[1].link = "AGENTS.md"
	}
	for i := range plan {
		item := &plan[i]
		target = item.target
		if item.link != "" {
			continue
		}
		if item.exists {
			item.prior, err = os.ReadFile(item.target)
			if err != nil {
				return nil, err
			}
			item.status = "updated"
		}
		item.content, err = replaceRepoInit(item.prior, section)
		if err != nil {
			return nil, err
		}
		if item.exists && bytes.Equal(item.content, item.prior) {
			item.status = "unchanged"
		}
	}
	mirrorRepoGuidanceStatus(plan)
	return plan, nil
}

func mirrorRepoGuidanceStatus(plan []repoGuidanceFile) {
	for i := range plan {
		if plan[i].exists && plan[i].link != "" {
			plan[i].status = plan[1-i].status
		}
	}
}

var repoInitContextLine = regexp.MustCompile("(?m)^Server: `([^`]+)`\\. Workspace: `([^`]+)`\\.$")

// AC-4.9 / component-runtime: validate both prior contexts before staging any
// file. An unavailable refresh retains the owned bytes, not a reconstructed copy.
func preserveRepoInitContext(plan []repoGuidanceFile, verified bool) (bool, error) {
	var priorContext repoInitContext
	var retainedSection []byte
	sections := make(map[int][]byte)
	for i, item := range plan {
		if item.link != "" {
			continue
		}
		text := string(item.prior)
		start, end := strings.Index(text, repoInitOwnerPrefix), strings.Index(text, repoInitClose)
		if start < 0 || end < start {
			continue
		}
		section := text[start : end+len(repoInitClose)]
		fields := repoInitContextLine.FindAllStringSubmatch(section, -1)
		if len(fields) == 0 && !strings.Contains(section, "Server:") && !strings.Contains(section, "Workspace:") {
			continue
		}
		if len(fields) != 1 || strings.Count(section, "Server:") != 1 || strings.Count(section, "Workspace:") != 1 {
			return false, fmt.Errorf("malformed prior Conveyor connection context")
		}
		server, ok := repoInitServer(fields[0][1])
		if !ok || server != fields[0][1] || !repoInitLabel(fields[0][2]) {
			return false, fmt.Errorf("unsafe prior Conveyor connection context")
		}
		current := repoInitContext{Server: server, Workspace: fields[0][2]}
		if retainedSection != nil && current != priorContext {
			return false, fmt.Errorf("conflicting prior Conveyor connection contexts")
		}
		priorContext, retainedSection = current, []byte(section)
		sections[i] = retainedSection
	}
	if verified || retainedSection == nil {
		return false, nil
	}
	for i := range plan {
		item := &plan[i]
		if item.link != "" {
			continue
		}
		section := retainedSection
		if own, ok := sections[i]; ok {
			section = own
		}
		content, err := replaceRepoInit(item.prior, append(append([]byte{}, section...), '\n'))
		if err != nil {
			return false, err
		}
		item.content = content
		if item.exists {
			item.status = "updated"
			if bytes.Equal(item.prior, content) {
				item.status = "unchanged"
			}
		}
	}
	mirrorRepoGuidanceStatus(plan)
	return true, nil
}

// prepareRepository keeps req-repository-onboarding REQ-4/AC-4.1 through
// AC-4.5 in one preflight and rollback boundary (component-runtime, DEC-40).
// The shared skill installer retains ownership and refresh semantics unchanged.
type repoSkillInstaller func(string, []skillDestination, string, bool, bool) ([]skillInstallFile, []skillInstallReport, error)

func prepareRepository(root, version, name, base string, out io.Writer) error {
	return prepareRepositoryWithInstaller(root, version, name, base, out, installEmbeddedSkillsForDestinationsWithForce)
}

func prepareRepositoryWithInstaller(root, version, name, base string, out io.Writer, install repoSkillInstaller, connection ...repoInitContext) error {
	return prepareRepositoryWithOptions(root, version, name, base, out, repoInitOptions{}, install, connection...)
}

type repoInitOptions struct {
	guidanceOnly bool
	rename       func(string, string) error
}

func prepareRepositoryWithOptions(root, version, name, base string, out io.Writer, options repoInitOptions, install repoSkillInstaller, connection ...repoInitContext) error {
	if options.rename == nil {
		options.rename = os.Rename
	}
	refuse := func(tool, target string, err error) error {
		fmt.Fprintf(out, "%s\trefused\t%s\n", tool, target)
		return err
	}
	section, err := renderRepoInit(version, name, base, connection...)
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
	verified := len(connection) > 0 && connection[0].Server != ""
	retained, err := preserveRepoInitContext(guidance, verified)
	if err != nil {
		return refuse("repo", root, err)
	}
	if retained {
		fmt.Fprintln(out, "repo\tcontext retained without reverification\tprior verified guidance")
	}
	// AC-4.3: explicit guidance-only mode never inspects skill destinations.
	var destinations []skillDestination
	if !options.guidanceOnly {
		destinations = skillDestinations(root, supportedSkillTools, true)
	}
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
	var results []skillInstallFile
	var reports []skillInstallReport
	if !options.guidanceOnly {
		results, reports, err = install(root, destinations, version, false, false)
	}
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
	// Recheck preserved links as well as all regular preimages before publishing.
	for _, item := range guidance {
		if err := checkRepoInitPrior(item); err != nil {
			return refuse("repo", item.target, errors.Join(err, rollback()))
		}
	}
	for _, item := range guidance {
		if item.status == "unchanged" {
			continue
		}
		// An existing supported link shares its regular target write. A planned
		// link must reach os.Symlink below when CLAUDE.md is absent.
		if item.link != "" && item.exists {
			continue
		}
		if err = checkRepoInitPrior(item); err == nil {
			if item.link != "" {
				err = os.Symlink(item.link, item.target)
			} else {
				err = options.rename(item.staged, item.target)
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
	if err == nil && item.exists && item.link != "" && info.Mode()&os.ModeSymlink != 0 {
		link, linkErr := os.Readlink(item.target)
		if linkErr == nil && link == item.link {
			return nil
		}
	}
	if err != nil || !item.exists || !info.Mode().IsRegular() || item.link != "" || info.Mode() != item.mode {
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
