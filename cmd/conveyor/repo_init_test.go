package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
)

func TestRepoInitGuidanceStatesAndRepeatability(t *testing.T) {
	for _, state := range []string{"neither", "agents only", "claude only", "claude symlink", "two regular files"} {
		t.Run(state, func(t *testing.T) {
			root := t.TempDir()
			old, err := renderRepoInit("v1.0.0", "old", "old-base")
			if err != nil {
				t.Fatal(err)
			}
			prefix, suffix := "Operator rules.\r\n\n", "\nKeep this trailing text without a newline"
			prior := prefix + strings.TrimSuffix(string(old), "\n") + suffix
			if state == "agents only" || state == "claude symlink" || state == "two regular files" {
				writeRepoFixture(t, root, "AGENTS.md", prior)
			}
			if state == "claude only" || state == "two regular files" {
				writeRepoFixture(t, root, "CLAUDE.md", prior)
			}
			if state == "claude symlink" {
				if err := os.Symlink("AGENTS.md", filepath.Join(root, "CLAUDE.md")); err != nil {
					t.Fatal(err)
				}
			}
			var output bytes.Buffer
			if err := prepareRepository(root, "v2.0.0", "example", "trunk", &output); err != nil {
				t.Fatal(err)
			}
			section, _ := renderRepoInit("v2.0.0", "example", "trunk")
			for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
				content, err := os.ReadFile(filepath.Join(root, name))
				if err != nil || !bytes.Contains(content, bytes.TrimSuffix(section, []byte("\n"))) {
					t.Fatalf("%s does not carry section: %s, %v", name, content, err)
				}
				if state == "two regular files" || state == "claude symlink" || state == "agents only" || state == "claude only" && name == "CLAUDE.md" {
					want := prefix + strings.TrimSuffix(string(section), "\n") + suffix
					if string(content) != want {
						t.Fatalf("operator text changed in %s: %q", name, content)
					}
				}
			}
			if state != "two regular files" && state != "claude only" {
				if target, err := os.Readlink(filepath.Join(root, "CLAUDE.md")); err != nil || target != "AGENTS.md" {
					t.Fatalf("CLAUDE.md link = %q, %v", target, err)
				}
			}
			before := repoFixtureSnapshot(t, root)
			output.Reset()
			if err := prepareRepository(root, "v2.0.0", "example", "trunk", &output); err != nil {
				t.Fatal(err)
			}
			if after := repoFixtureSnapshot(t, root); !reflect.DeepEqual(before, after) {
				t.Fatal("repeat changed file bytes or symlinks")
			}
			lines := strings.Split(strings.TrimSpace(output.String()), "\n")
			if len(lines) != 2+len(supportedSkillTools)*len(embeddedSkillManifest) {
				t.Fatalf("missing reports: %s", output.String())
			}
			for _, line := range lines {
				fields := strings.Split(line, "\t")
				if len(fields) != 3 || fields[1] != "unchanged" || !filepath.IsAbs(fields[2]) {
					t.Errorf("repeat report = %q", line)
				}
			}
			for _, destination := range skillDestinations(root, supportedSkillTools) {
				for _, asset := range embeddedSkillManifest {
					want, _ := renderEmbeddedSkill(asset, "v2.0.0")
					got, err := os.ReadFile(filepath.Join(destination.root, asset.relative))
					if err != nil || !bytes.Equal(want, got) {
						t.Fatalf("%s/%s differs from embedded skill", destination.tool.name, asset.relative)
					}
				}
			}
		})
	}
}

func TestRepoInitSectionContract(t *testing.T) {
	section, err := renderRepoInit("v1.2.3", "example", "trunk")
	if err != nil {
		t.Fatal(err)
	}
	const want = "<!-- conveyor:repo-init owner=v1 version=v1.2.3 -->\n" +
		"## Conveyor factory work\n\n" +
		"Repository: `example`. Base branch: `trunk`.\n" +
		"The confirmed document corpus is the design authority: Requirements, System Design documents, and DEC-n decisions.\n" +
		"Changes are filed as tasks through Conveyor.\n" +
		"An agent edits only under a live claim in a task worktree resolved by `conveyor checkout <task-id>`, never on the base branch.\n" +
		"Follow the installed `conveyor-plan` skill for planning, `conveyor-file-tasks` for filing tasks, and `conveyor-work` for task work.\n" +
		"This section and the project-scoped skills are versioned with the CLI. Re-run `conveyor repo init` after an upgrade to refresh them.\n" +
		"<!-- /conveyor:repo-init -->\n"
	if string(section) != want || strings.Count(string(section), "\n") >= 20 {
		t.Fatalf("section contract changed: %s", section)
	}
}

func TestRepoInitAppendsAndRefusesSkillDowngrade(t *testing.T) {
	root := t.TempDir()
	writeRepoFixture(t, root, "AGENTS.md", "Operator rules without final newline")
	if err := prepareRepository(root, "v2.0.0", "example", "main", io.Discard); err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(filepath.Join(root, "AGENTS.md"))
	if !strings.HasPrefix(string(content), "Operator rules without final newline\n"+repoInitOwnerPrefix) {
		t.Fatalf("operator text was not preserved: %s", content)
	}
	before := repoFixtureSnapshot(t, root)
	if err := prepareRepository(root, "v1.0.0", "example", "main", io.Discard); err == nil || !strings.Contains(err.Error(), "refusing to downgrade") {
		t.Fatalf("downgrade error = %v", err)
	}
	if !reflect.DeepEqual(before, repoFixtureSnapshot(t, root)) {
		t.Fatal("downgrade refusal changed guidance or skills")
	}
}

func TestRepoInitRefusalsLeaveNoPartialFiles(t *testing.T) {
	section, _ := renderRepoInit("v1.0.0", "example", "main")
	for _, state := range []string{"unowned skill", "broken symlink", "reversed symlink", "external symlink", "tool symlink", "skill symlink", "guidance directory", "missing close", "missing open", "multiple spans", "wrong order", "bad version", "unsupported owner"} {
		t.Run(state, func(t *testing.T) {
			root, external := t.TempDir(), t.TempDir()
			writeRepoFixture(t, root, "AGENTS.md", "Keep my rules")
			badPath := filepath.Join(root, "CLAUDE.md")
			symlink := func(target, path string) {
				t.Helper()
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			switch state {
			case "unowned skill":
				badPath = filepath.Join(root, ".cursor/skills/conveyor-work/SKILL.md")
				writeRepoFixture(t, root, ".cursor/skills/conveyor-work/SKILL.md", "Operator skill")
			case "broken symlink":
				symlink("missing", badPath)
			case "reversed symlink":
				badPath = filepath.Join(root, "AGENTS.md")
				if err := os.Remove(badPath); err != nil {
					t.Fatal(err)
				}
				writeRepoFixture(t, root, "CLAUDE.md", "Operator rules")
				symlink("CLAUDE.md", badPath)
			case "external symlink":
				writeRepoFixture(t, external, "rules", "External rules")
				symlink(filepath.Join(external, "rules"), badPath)
			case "tool symlink":
				badPath = filepath.Join(root, ".claude")
				symlink(external, badPath)
			case "skill symlink":
				badPath = filepath.Join(root, ".claude/skills/conveyor-work/SKILL.md")
				if err := os.MkdirAll(filepath.Dir(badPath), 0o755); err != nil {
					t.Fatal(err)
				}
				writeRepoFixture(t, external, "skill", "External skill")
				symlink(filepath.Join(external, "skill"), badPath)
			case "guidance directory":
				if err := os.Mkdir(badPath, 0o755); err != nil {
					t.Fatal(err)
				}
			case "missing close":
				writeRepoFixture(t, root, "CLAUDE.md", repoInitOwnerPrefix+"v1 -->\n")
			case "missing open":
				writeRepoFixture(t, root, "CLAUDE.md", repoInitClose)
			case "multiple spans":
				writeRepoFixture(t, root, "CLAUDE.md", string(section)+string(section))
			case "wrong order":
				writeRepoFixture(t, root, "CLAUDE.md", repoInitClose+repoInitOwnerPrefix+"v1 -->")
			case "bad version":
				writeRepoFixture(t, root, "CLAUDE.md", repoInitOwnerPrefix+"v1\n -->\n"+repoInitClose)
			case "unsupported owner":
				writeRepoFixture(t, root, "CLAUDE.md", strings.ReplaceAll(string(section), "owner=v1", "owner=v2"))
			}
			before, outside := repoFixtureSnapshot(t, root), repoFixtureSnapshot(t, external)
			var output bytes.Buffer
			err := prepareRepository(root, "v2.0.0", "example", "main", &output)
			if err == nil || !strings.Contains(err.Error(), badPath) || !strings.Contains(output.String(), "\trefused\t") {
				t.Fatalf("missing named refusal: %v, %s", err, output.String())
			}
			if !reflect.DeepEqual(before, repoFixtureSnapshot(t, root)) || !reflect.DeepEqual(outside, repoFixtureSnapshot(t, external)) {
				t.Fatal("refusal changed files")
			}
		})
	}
}

func TestRepoInitMetadata(t *testing.T) {
	root := t.TempDir()
	mustGit(t, root, "init", "-b", "main")
	mustGit(t, root, "remote", "add", "origin", "git@github.com:Example/Repo.git")
	for _, test := range []struct {
		name               string
		repos              []config.Repo
		err                error
		wantName, wantBase string
	}{
		{"registered", []config.Repo{{Name: "registered", Base: "trunk", URL: "https://credential@github.com/example/repo"}}, nil, "registered", "trunk"},
		{"unregistered", []config.Repo{{Name: "other", Base: "main", URL: "https://github.com/example/other"}}, nil, "<registered-repository>", "<base-branch>"},
		{"unauthorized", nil, errors.New("secret server error"), "<registered-repository>", "<base-branch>"},
		{"ambiguous", []config.Repo{{Name: "one", Base: "main", URL: "https://github.com/example/repo"}, {Name: "two", Base: "main", URL: "https://github.com/example/repo"}}, nil, "<registered-repository>", "<base-branch>"},
		{"unsafe label", []config.Repo{{Name: "injected\nline", Base: "main", URL: "https://github.com/example/repo"}}, nil, "<registered-repository>", "<base-branch>"},
	} {
		t.Run(test.name, func(t *testing.T) {
			name, base := repoInitMetadata(context.Background(), root, func() (config.VersionedDocument, error) {
				return config.VersionedDocument{Document: config.WorkspaceDocument{Repos: test.repos}}, test.err
			})
			if name != test.wantName || base != test.wantBase {
				t.Fatalf("metadata = %q, %q", name, base)
			}
		})
	}
	mustGit(t, root, "remote", "remove", "origin")
	name, base := repoInitMetadata(context.Background(), root, func() (config.VersionedDocument, error) {
		t.Fatal("lookup without origin")
		return config.VersionedDocument{}, nil
	})
	if name != "<registered-repository>" || base != "<base-branch>" {
		t.Fatalf("missing-origin metadata = %q %q", name, base)
	}
}

func TestRepoInitCommandCheckoutBoundary(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	command := repoCmd()
	command.SetArgs([]string{"init"})
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "REQ-4/AC-4.6") {
		t.Fatalf("outside-checkout error = %v", err)
	}
	if len(repoFixtureSnapshot(t, root)) != 0 {
		t.Fatal("outside-checkout invocation wrote files")
	}
	mustGit(t, root, "init", "-b", "main")
	configureGitUser(t, root)
	mustGit(t, root, "commit", "--allow-empty", "-m", "fixture")
	before := repoFixtureSnapshot(t, filepath.Join(root, ".git"))
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(root, "nested"))
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	toolDir := t.TempDir()
	if err := os.Symlink(git, filepath.Join(toolDir, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", toolDir)
	command = repoCmd()
	command.SetArgs([]string{"init"})
	var output bytes.Buffer
	command.SetOut(&output)
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, repoFixtureSnapshot(t, filepath.Join(root, ".git"))) {
		t.Fatal("repo init changed Git state")
	}
	for _, destination := range skillDestinations(root, supportedSkillTools) {
		assertFile(t, filepath.Join(destination.root, "conveyor-work/SKILL.md"))
	}
	content, _ := os.ReadFile(filepath.Join(root, "AGENTS.md"))
	if !strings.Contains(string(content), "<registered-repository>") || !strings.Contains(string(content), "<base-branch>") {
		t.Fatalf("missing fallback guidance: %s", content)
	}
	claudePath := filepath.Join(root, "CLAUDE.md")
	if target, err := os.Readlink(claudePath); err != nil || target != "AGENTS.md" {
		t.Fatalf("fresh-checkout CLAUDE.md link = %q, %v", target, err)
	}
	if !strings.Contains(output.String(), "repo\twritten\t"+claudePath+"\n") {
		t.Fatalf("missing symlink creation report: %s", output.String())
	}
	prepared := repoFixtureSnapshot(t, root)
	output.Reset()
	command = repoCmd()
	command.SetArgs([]string{"init"})
	command.SetOut(&output)
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(prepared, repoFixtureSnapshot(t, root)) {
		t.Fatal("repeat command changed guidance, skills, symlinks, or Git state")
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2+len(supportedSkillTools)*len(embeddedSkillManifest) {
		t.Fatalf("missing repeat command reports: %s", output.String())
	}
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 || fields[1] != "unchanged" {
			t.Errorf("repeat command report = %q", line)
		}
	}
}

func TestRepoInitRollsBackAfterSkillInstallation(t *testing.T) {
	for _, existing := range []bool{false, true} {
		root := t.TempDir()
		if existing {
			if err := prepareRepository(root, "v1.0.0", "old", "main", io.Discard); err != nil {
				t.Fatal(err)
			}
		}
		before := repoFixtureSnapshot(t, root)
		installer := func(base string, destinations []skillDestination, version string, adopt, force bool) ([]skillInstallFile, []skillInstallReport, error) {
			files, reports, err := installEmbeddedSkillsForDestinationsWithForce(base, destinations, version, adopt, force)
			if err != nil {
				return files, reports, err
			}
			// Simulate a late filesystem failure after the shared installer commits.
			staged, err := filepath.Glob(filepath.Join(base, ".conveyor-repo-init-*"))
			if err != nil || len(staged) == 0 {
				t.Fatal("no staged guidance")
			}
			for _, path := range staged {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			return files, reports, nil
		}
		if err := prepareRepositoryWithInstaller(root, "v2.0.0", "new", "trunk", io.Discard, installer); err == nil {
			t.Fatal("expected late write failure")
		}
		if after := repoFixtureSnapshot(t, root); !reflect.DeepEqual(before, after) {
			t.Fatalf("failed write did not restore original checkout: before=%v after=%v", before, after)
		}
	}
}

func TestRepoInitRollsBackFirstGuidanceWriteWhenSecondChanges(t *testing.T) {
	root := t.TempDir()
	writeRepoFixture(t, root, "AGENTS.md", "Original rules")
	if err := os.Chmod(filepath.Join(root, "AGENTS.md"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := repoFixtureSnapshot(t, root)
	before["CLAUDE.md"] = "Concurrent operator rules"
	installer := func(base string, destinations []skillDestination, version string, adopt, force bool) ([]skillInstallFile, []skillInstallReport, error) {
		files, reports, err := installEmbeddedSkillsForDestinationsWithForce(base, destinations, version, adopt, force)
		if err == nil {
			writeRepoFixture(t, root, "CLAUDE.md", "Concurrent operator rules")
		}
		return files, reports, err
	}
	err := prepareRepositoryWithInstaller(root, "v1.0.0", "example", "main", io.Discard, installer)
	if err == nil || !strings.Contains(err.Error(), "CLAUDE.md") {
		t.Fatalf("concurrent file error = %v", err)
	}
	if !reflect.DeepEqual(before, repoFixtureSnapshot(t, root)) {
		t.Fatal("failed guidance commit changed operator files or left installed skills")
	}
	info, err := os.Stat(filepath.Join(root, "AGENTS.md"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("original guidance mode was not restored: %v, %v", info, err)
	}
}

func writeRepoFixture(t *testing.T, root, relative, content string) {
	t.Helper()
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func repoFixtureSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || path == root {
			return err
		}
		relative, _ := filepath.Rel(root, path)
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			result[relative] = "link:" + target
			return err
		}
		if entry.IsDir() {
			result[relative] = "directory"
			return nil
		}
		content, err := os.ReadFile(path)
		result[relative] = string(content)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
