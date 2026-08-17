package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func TestEmbeddedSkillsMatchRepositorySources(t *testing.T) {
	t.Parallel()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))

	manifestSources := make([]string, 0, len(embeddedSkillManifest))
	for _, asset := range embeddedSkillManifest {
		manifestSources = append(manifestSources, filepath.ToSlash(asset.sourcePath))
		embedded, err := embeddedSkills.ReadFile(asset.assetPath)
		if err != nil {
			t.Fatalf("read embedded %s: %v", asset.assetPath, err)
		}
		source, err := os.ReadFile(filepath.Join(repositoryRoot, filepath.FromSlash(asset.sourcePath)))
		if err != nil {
			t.Fatalf("read source %s: %v", asset.sourcePath, err)
		}
		if !bytes.Equal(embedded, source) {
			t.Fatalf("embedded asset %s drifted from %s", asset.assetPath, asset.sourcePath)
		}
	}

	var repositorySkills []string
	skillsRoot := filepath.Join(repositoryRoot, ".claude", "skills")
	if err := filepath.WalkDir(skillsRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type().IsRegular() {
			relative, err := filepath.Rel(repositoryRoot, path)
			if err != nil {
				return err
			}
			repositorySkills = append(repositorySkills, filepath.ToSlash(relative))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(manifestSources)
	sort.Strings(repositorySkills)
	wantedSkills := make([]string, 0, len(manifestSources))
	for _, source := range manifestSources {
		if strings.HasPrefix(source, ".claude/skills/") {
			wantedSkills = append(wantedSkills, source)
		}
	}
	if strings.Join(repositorySkills, "\n") != strings.Join(wantedSkills, "\n") {
		t.Fatalf("embedded skill set does not match repository skill set\nrepository:\n%s\nembedded:\n%s", strings.Join(repositorySkills, "\n"), strings.Join(wantedSkills, "\n"))
	}
}

func TestConveyorWorkSkillShipsScratchDiscipline(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	destinations := skillDestinations(base, supportedSkillTools)
	if _, _, err := installEmbeddedSkillsForDestinations(base, destinations, "v1", false); err != nil {
		t.Fatal(err)
	}

	required := []string{
		"$XDG_CACHE_HOME/conveyor/<task-id>",
		"$HOME/.cache/conveyor/<task-id>",
		"GOCACHE",
		"GOTMPDIR",
		"TMPDIR",
		"PLAYWRIGHT_BROWSERS_PATH",
		"npm_config_cache",
		"findmnt -T",
		"write permission scoped only to that exact task cache directory",
		"git check-ignore -q <fallback-path>",
		"git status --porcelain --untracked-files=normal",
		"normal exit, command failure, and catchable interruption",
	}
	for _, destination := range destinations {
		content, err := os.ReadFile(filepath.Join(destination.root, "conveyor-work", "conveyor-work.md"))
		if err != nil {
			t.Fatal(err)
		}
		for _, fragment := range required {
			if !bytes.Contains(content, []byte(fragment)) {
				t.Errorf("%s installed conveyor-work playbook missing %q", destination.tool.name, fragment)
			}
		}
	}
}

func TestInstallEmbeddedSkillsCreateNoopAndRefresh(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	root := filepath.Join(base, ".claude", "skills")

	created, err := installEmbeddedSkills(base, root, "v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	assertStatuses(t, created, "created")

	wrapperPath := filepath.Join(root, "conveyor-plan", "SKILL.md")
	wrapper, err := os.ReadFile(wrapperPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(wrapper, []byte("---\n")) {
		t.Fatal("installed skill frontmatter is no longer first")
	}
	if !bytes.Contains(wrapper, []byte(skillsOwnerPrefix+"v1.2.3 source=.claude/skills/conveyor-plan/SKILL.md -->")) {
		t.Fatal("installed wrapper has no release ownership marker")
	}
	if bytes.Contains(wrapper, []byte("../../../docs/playbooks")) || !bytes.Contains(wrapper, []byte("[conveyor-planning.md](conveyor-planning.md)")) {
		t.Fatal("installed wrapper is not self-contained")
	}
	playbook, err := os.ReadFile(filepath.Join(root, "conveyor-plan", "conveyor-planning.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(playbook, []byte(skillsOwnerPrefix+"v1.2.3 source=docs/playbooks/conveyor-planning.md -->\n")) {
		t.Fatal("installed playbook has no release ownership marker")
	}
	workWrapperPath := filepath.Join(root, "conveyor-work", "SKILL.md")
	workWrapper, err := os.ReadFile(workWrapperPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(workWrapper, []byte(skillsOwnerPrefix+"v1.2.3 source=.claude/skills/conveyor-work/SKILL.md -->")) {
		t.Fatal("installed conveyor-work wrapper has no release ownership marker")
	}
	if bytes.Contains(workWrapper, []byte("../../../docs/playbooks")) || !bytes.Contains(workWrapper, []byte("[conveyor-work.md](conveyor-work.md)")) {
		t.Fatal("installed conveyor-work wrapper is not self-contained")
	}
	workPlaybook, err := os.ReadFile(filepath.Join(root, "conveyor-work", "conveyor-work.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(workPlaybook, []byte(skillsOwnerPrefix+"v1.2.3 source=docs/playbooks/conveyor-work.md -->\n")) {
		t.Fatal("installed conveyor-work playbook has no release ownership marker")
	}

	unchanged, err := installEmbeddedSkills(base, root, "v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	assertStatuses(t, unchanged, "unchanged")

	refreshed, err := installEmbeddedSkills(base, root, "v1.2.4")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range refreshed {
		if item.status != "refresh v1.2.3 -> v1.2.4" {
			t.Fatalf("status for %s = %q", item.relative, item.status)
		}
		content, readErr := os.ReadFile(item.target)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if _, owned := managedSkillVersion(content, item.sourcePath); !owned || !bytes.Contains(content, []byte("version=v1.2.4")) {
			t.Fatalf("%s was not refreshed with the new version", item.target)
		}
	}
}

func TestInstallEmbeddedSkillsRefusesCollisionBeforeWriting(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	root := filepath.Join(base, ".claude", "skills")
	collision := filepath.Join(root, "conveyor-file-tasks", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(collision), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(collision, []byte("operator content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := installEmbeddedSkills(base, root, "v1")
	if err == nil || !strings.Contains(err.Error(), "not owned by Conveyor") || !strings.Contains(err.Error(), collision) {
		t.Fatalf("collision error = %v", err)
	}
	content, readErr := os.ReadFile(collision)
	if readErr != nil || string(content) != "operator content\n" {
		t.Fatalf("collision changed: content=%q err=%v", content, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(root, "conveyor-plan", "SKILL.md")); !os.IsNotExist(statErr) {
		t.Fatalf("preflight collision allowed another write: %v", statErr)
	}
}

func TestInstallEmbeddedSkillsResolvesEditorSymlinkAndRejectsNestedSymlink(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	managedRoot := t.TempDir()
	if err := os.Symlink(managedRoot, filepath.Join(base, ".claude")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	root := filepath.Join(base, ".claude", "skills")
	items, err := installEmbeddedSkills(base, root, "v1")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if !pathWithin(filepath.Join(managedRoot, "skills"), item.target) {
			t.Fatalf("resolved target %s is outside symlink destination", item.target)
		}
	}

	unsafeBase := t.TempDir()
	unsafeManagedRoot := t.TempDir()
	unsafeTarget := t.TempDir()
	if err = os.Symlink(unsafeManagedRoot, filepath.Join(unsafeBase, ".claude")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err = os.MkdirAll(filepath.Join(unsafeManagedRoot, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(unsafeTarget, filepath.Join(unsafeManagedRoot, "skills", "conveyor-plan")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err = installEmbeddedSkills(unsafeBase, filepath.Join(unsafeBase, ".claude", "skills"), "v1"); err == nil || !strings.Contains(err.Error(), "refusing symlink") {
		t.Fatalf("nested symlink error = %v", err)
	}
}

func TestInstallEmbeddedSkillsRefusesDowngradeUnlessForced(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	destination := skillDestination{tool: supportedSkillTools[0], root: filepath.Join(base, ".claude", "skills")}
	if _, _, err := installEmbeddedSkillsForDestinations(base, []skillDestination{destination}, "v2.0.0", false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := installEmbeddedSkillsForDestinations(base, []skillDestination{destination}, "v1.9.0", false); err == nil || !strings.Contains(err.Error(), "refusing to downgrade") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("downgrade error = %v", err)
	}
	items, _, err := installEmbeddedSkillsForDestinationsWithForce(base, []skillDestination{destination}, "v1.9.0", false, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.status != "downgrade v2.0.0 -> v1.9.0 (forced)" {
			t.Fatalf("forced downgrade status for %s = %q", item.relative, item.status)
		}
	}
}

func TestSkillsDestinationSelectsGlobalAndProjectScopes(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("HOME", home)

	base, root, err := skillsDestination(false)
	if err != nil {
		t.Fatal(err)
	}
	resolvedHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	if base != resolvedHome || root != filepath.Join(resolvedHome, ".claude", "skills") {
		t.Fatalf("global destination = %s, %s", base, root)
	}

	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chdir(project); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	base, root, err = skillsDestination(true)
	if err != nil {
		t.Fatal(err)
	}
	resolvedProject, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	if base != resolvedProject || root != filepath.Join(resolvedProject, ".claude", "skills") {
		t.Fatalf("project destination = %s, %s", base, root)
	}
}

func TestListEmbeddedSkillsReportsInstalledStateWithoutWriting(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	root := filepath.Join(base, ".claude", "skills")
	command := skillsInstallCmd()
	var output bytes.Buffer
	command.SetOut(&output)

	if err := listEmbeddedSkills(command, base, root, "v1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "embedded release v1") || strings.Count(output.String(), "\tnot installed (would create)\n") != len(embeddedSkillManifest) {
		t.Fatalf("missing list output:\n%s", output.String())
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("--list wrote its destination: %v", err)
	}

	if _, err := installEmbeddedSkills(base, root, "v1"); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := listEmbeddedSkills(command, base, root, "v1"); err != nil {
		t.Fatal(err)
	}
	if strings.Count(output.String(), "\tunchanged\n") != len(embeddedSkillManifest) {
		t.Fatalf("installed state missing:\n%s", output.String())
	}
}

func TestSkillsInstallDetectsEveryToolAndSupportsNarrowing(t *testing.T) {
	lookPath := func(name string) (string, error) { return "/tools/" + name, nil }
	home := t.TempDir()
	t.Setenv("HOME", home)
	command := skillsInstallCmdWithLookPath(lookPath)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Count(output.String(), "claude\tcreated\t") != len(embeddedSkillManifest) || strings.Count(output.String(), "codex\tcreated\t") != len(embeddedSkillManifest) {
		t.Fatalf("per-tool output missing:\n%s", output.String())
	}
	for _, asset := range embeddedSkillManifest {
		claude, err := os.ReadFile(filepath.Join(home, ".claude", "skills", filepath.FromSlash(asset.relative)))
		if err != nil {
			t.Fatal(err)
		}
		codex, err := os.ReadFile(filepath.Join(home, ".codex", "skills", filepath.FromSlash(asset.relative)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(claude, codex) {
			t.Fatalf("%s differs across editor roots", asset.relative)
		}
	}

	narrowedHome := t.TempDir()
	t.Setenv("HOME", narrowedHome)
	command = skillsInstallCmdWithLookPath(lookPath)
	command.SetArgs([]string{"--tool", "codex"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(narrowedHome, ".codex", "skills")); err != nil {
		t.Fatalf("codex destination missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(narrowedHome, ".claude")); !os.IsNotExist(err) {
		t.Fatalf("narrowed install touched claude: %v", err)
	}
}

func TestSkillsInstallDetectionErrorsAreReadOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	missing := func(name string) (string, error) { return "", fs.ErrNotExist }

	command := skillsInstallCmdWithLookPath(missing)
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "no supported agent tooling") {
		t.Fatalf("no-tool error = %v", err)
	}
	command = skillsInstallCmdWithLookPath(func(name string) (string, error) { return "/tools/" + name, nil })
	command.SetArgs([]string{"--tool", "unknown"})
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "unsupported tool") {
		t.Fatalf("unknown-tool error = %v", err)
	}
	command = skillsInstallCmdWithLookPath(missing)
	command.SetArgs([]string{"--tool", "codex"})
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "not available on PATH") {
		t.Fatalf("missing selected-tool error = %v", err)
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("detection errors wrote home: %v", entries)
	}
}

func TestCodexLegacyArtifactIsReportOnlyForDefaultMultiToolInstall(t *testing.T) {
	base := t.TempDir()
	destinations := skillDestinations(base, supportedSkillTools)
	codexDestination := destinations[1]
	legacyFile := filepath.Join(codexDestination.legacyPath, "plugin.json")
	if err := os.MkdirAll(filepath.Dir(legacyFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyFile, []byte("operator plugin\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	items, reports, err := installEmbeddedSkillsForDestinations(base, destinations, "v1", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != len(supportedSkillTools)*len(embeddedSkillManifest) || len(reports) != 1 || !strings.Contains(reports[0].status, "skipped unmanaged") {
		t.Fatalf("default legacy result: items=%d reports=%+v", len(items), reports)
	}
	for _, item := range items {
		if item.status != "created" {
			t.Fatalf("default %s status for %s = %q", item.tool, item.relative, item.status)
		}
	}
	for _, destination := range destinations {
		if _, err := os.Stat(destination.root); err != nil {
			t.Fatalf("%s native root missing: %v", destination.tool.name, err)
		}
	}
	legacy, err := os.ReadFile(legacyFile)
	if err != nil || string(legacy) != "operator plugin\n" {
		t.Fatalf("default install modified legacy content: %q, %v", legacy, err)
	}
}

func TestAdoptReplacesOnlyUnmarkedNativeSkillFiles(t *testing.T) {
	base := t.TempDir()
	destination := skillDestinations(base, []skillTool{supportedSkillTools[1]})[0]
	collision := filepath.Join(destination.root, "conveyor-plan", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(collision), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(collision, []byte("operator content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := installEmbeddedSkillsForDestinations(base, []skillDestination{destination}, "v1", false); err == nil || !strings.Contains(err.Error(), "--adopt") {
		t.Fatalf("collision error = %v", err)
	}
	content, err := os.ReadFile(collision)
	if err != nil || string(content) != "operator content\n" {
		t.Fatalf("collision changed before adoption: %q, %v", content, err)
	}
	items, _, err := installEmbeddedSkillsForDestinations(base, []skillDestination{destination}, "v1", true)
	if err != nil {
		t.Fatal(err)
	}
	var adopted bool
	for _, item := range items {
		if item.target == collision && item.status == "adopted" {
			adopted = true
		}
	}
	if !adopted {
		t.Fatal("unmarked native skill was not reported adopted")
	}
	content, err = os.ReadFile(collision)
	if err != nil {
		t.Fatal(err)
	}
	if _, owned := managedSkillVersion(content, ".claude/skills/conveyor-plan/SKILL.md"); !owned {
		t.Fatal("adopted native skill has no ownership marker")
	}
}

func TestManagedCodexInstallRefreshesAlongsideLegacyArtifact(t *testing.T) {
	base := t.TempDir()
	destination := skillDestinations(base, []skillTool{supportedSkillTools[1]})[0]
	if _, _, err := installEmbeddedSkillsForDestinations(base, []skillDestination{destination}, "v1", false); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(destination.legacyPath, 0o755); err != nil {
		t.Fatal(err)
	}
	items, reports, err := installEmbeddedSkillsForDestinations(base, []skillDestination{destination}, "v2", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.status != "refresh v1 -> v2" {
			t.Fatalf("managed codex status for %s = %q", item.relative, item.status)
		}
	}
	if len(reports) != 1 || !strings.Contains(reports[0].status, "skipped") {
		t.Fatalf("legacy report = %+v", reports)
	}
}

func assertStatuses(t *testing.T, items []skillInstallFile, wanted string) {
	t.Helper()
	if len(items) != len(embeddedSkillManifest) {
		t.Fatalf("installed %d files, want %d", len(items), len(embeddedSkillManifest))
	}
	for _, item := range items {
		if item.status != wanted {
			t.Fatalf("status for %s = %q, want %q", item.relative, item.status, wanted)
		}
	}
}
