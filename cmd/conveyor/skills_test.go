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

func TestInstallEmbeddedSkillsRejectsDestinationSymlink(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(base, ".claude")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	root := filepath.Join(base, ".claude", "skills")
	if _, err := installEmbeddedSkills(base, root, "v1"); err == nil || !strings.Contains(err.Error(), "refusing symlink") {
		t.Fatalf("symlink error = %v", err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("installer wrote through destination symlink: %v", entries)
	}
	command := skillsInstallCmd()
	var output bytes.Buffer
	command.SetOut(&output)
	if err := listEmbeddedSkills(command, base, root, "v1"); err != nil {
		t.Fatal(err)
	}
	if strings.Count(output.String(), "unsafe: refusing symlink") != len(embeddedSkillManifest) {
		t.Fatalf("--list did not report the unsafe destination:\n%s", output.String())
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
	if !strings.Contains(output.String(), "embedded release v1") || strings.Count(output.String(), "\tcreated\n") != len(embeddedSkillManifest) {
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
