package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
)

func TestRepoInitGuidanceStatesAndRepeatability(t *testing.T) {
	for _, state := range []string{"neither", "agents only", "claude only", "claude symlink", "agents symlink", "two regular files"} {
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
			if state == "claude only" || state == "agents symlink" || state == "two regular files" {
				writeRepoFixture(t, root, "CLAUDE.md", prior)
			}
			if state == "claude symlink" {
				if err := os.Symlink("AGENTS.md", filepath.Join(root, "CLAUDE.md")); err != nil {
					t.Fatal(err)
				}
			}
			if state == "agents symlink" {
				if err := os.Symlink("CLAUDE.md", filepath.Join(root, "AGENTS.md")); err != nil {
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
				if state == "two regular files" || state == "agents symlink" || state == "claude symlink" || state == "agents only" || state == "claude only" && name == "CLAUDE.md" {
					want := prefix + strings.TrimSuffix(string(section), "\n") + suffix
					if string(content) != want {
						t.Fatalf("operator text changed in %s: %q", name, content)
					}
				}
			}
			if state != "two regular files" && state != "claude only" && state != "agents symlink" {
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
			for _, destination := range skillDestinations(root, supportedSkillTools, true) {
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
		"Connection context is unresolved. Obtain an explicit server URL and immutable workspace ID, then rerun `conveyor --server '<server>' --workspace '<workspace-id>' repo init`. Do not use a guessed endpoint.\n" +
		"On connection failure, report the failed endpoint and missing context. Do not infer a replacement host from localhost defaults, SSH configuration, or release instructions.\n\n" +
		"The confirmed document corpus is the design authority: Requirements, System Design documents, and DEC-n decisions.\n" +
		"Changes are filed as tasks through Conveyor.\n" +
		"An agent edits only under a live claim in a task worktree resolved by `conveyor checkout <task-id>`, never on the base branch.\n" +
		"Follow the `conveyor-plan` skill for planning, `conveyor-file-tasks` for filing tasks, and `conveyor-work` for task work.\n" +
		"This section and the project-scoped skills are versioned with the CLI. Re-run `conveyor repo init` after an upgrade to refresh both, or use `conveyor repo init --guidance-only` to preserve maintained source skill wrappers.\n" +
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
	for _, state := range []string{"unowned skill", "broken symlink", "external symlink", "tool symlink", "skill symlink", "guidance directory", "missing close", "missing open", "multiple spans", "wrong order", "bad version", "unsupported owner"} {
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
			connection := repoInitConnection(context.Background(), root, &client{base: "https://conveyor.example.com", token: "test-secret", workspace: "demo", resolved: resolvedClientConfig{Server: resolvedValue{Source: "flag"}}}, func() (config.VersionedDocument, error) {
				return config.VersionedDocument{Document: config.WorkspaceDocument{Workspace: "demo", Repos: test.repos}}, test.err
			})
			name, base := connection.Name, connection.Base
			if name != test.wantName || base != test.wantBase {
				t.Fatalf("metadata = %q, %q", name, base)
			}
		})
	}
	mustGit(t, root, "remote", "remove", "origin")
	connection := repoInitConnection(context.Background(), root, &client{base: "https://conveyor.example.com", token: "test-secret", workspace: "demo", resolved: resolvedClientConfig{Server: resolvedValue{Source: "flag"}}}, func() (config.VersionedDocument, error) {
		t.Fatal("lookup without origin")
		return config.VersionedDocument{}, nil
	})
	name, base := connection.Name, connection.Base
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
	// Git 2.55 may detach commit's auto-maintenance after creating
	// objects/maintenance.lock. Disable that fixture-owned background writer;
	// retain the complete .git comparison rather than ignoring its lock.
	mustGit(t, root, "config", "maintenance.auto", "false")
	writeRepoFixture(t, root, "tracked.txt", "tracked fixture\n")
	mustGit(t, root, "add", "tracked.txt")
	mustGit(t, root, "commit", "-m", "fixture")
	remote := t.TempDir()
	mustGit(t, remote, "init", "--bare")
	mustGit(t, root, "remote", "add", "origin", remote)
	remoteBefore := repoFixtureSnapshot(t, remote)
	semanticBefore := repoInitGitState(t, root)
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
	assertRepoInitSnapshot(t, before, repoFixtureSnapshot(t, filepath.Join(root, ".git")))
	if !reflect.DeepEqual(semanticBefore, repoInitGitState(t, root)) {
		t.Fatal("repo init changed HEAD, branch, worktree, refs, remotes, config, index, or staged state")
	}
	assertRepoInitSnapshot(t, remoteBefore, repoFixtureSnapshot(t, remote))
	for _, destination := range skillDestinations(root, supportedSkillTools, true) {
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
	if !reflect.DeepEqual(semanticBefore, repoInitGitState(t, root)) {
		t.Fatal("repeat changed semantic Git state")
	}
	assertRepoInitSnapshot(t, remoteBefore, repoFixtureSnapshot(t, remote))
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

// AC-4.7/4.10: unresolved input never leaks into owned guidance or reports.
func TestRepoInitConnectionVerification(t *testing.T) {
	root := t.TempDir()
	mustGit(t, root, "init", "-b", "main")
	mustGit(t, root, "remote", "add", "origin", "git@github.com:Example/Repo.git")
	for _, name := range []string{"verified", "default", "no source", "no workspace", "no token", "config error", "unavailable", "mismatch", "ambiguous", "unregistered", "unsafe name", "unsafe base", "unsafe workspace", "token in label", "noncanonical endpoint"} {
		t.Run(name, func(t *testing.T) {
			c := &client{base: "https://conveyor.example.com", workspace: "demo", token: "secret-value", resolved: resolvedClientConfig{Server: resolvedValue{Source: "environment"}}}
			record := config.VersionedDocument{Document: config.WorkspaceDocument{Workspace: "demo", Repos: []config.Repo{{Name: "example", Base: "main", URL: "https://credential@github.com/example/repo"}}}}
			var lookupErr error
			switch name {
			case "default":
				c.resolved.Server.Source = "default"
			case "no source":
				c.resolved.Server.Source = ""
			case "no workspace":
				c.workspace = ""
			case "no token":
				c.token = ""
			case "config error":
				c.configErr = errors.New("secret-value raw config error")
			case "unavailable":
				lookupErr = errors.New("secret-value raw lookup error")
			case "mismatch":
				record.Document.Workspace = "other"
			case "ambiguous":
				record.Document.Repos = append(record.Document.Repos, record.Document.Repos[0])
			case "unregistered":
				record.Document.Repos = nil
			case "unsafe name":
				record.Document.Repos[0].Name = "bad`label"
			case "unsafe base":
				record.Document.Repos[0].Base = "bad\nbranch"
			case "unsafe workspace":
				c.workspace = "demo'; echo injected"
			case "noncanonical endpoint":
				c.base = "https://conveyor.example.com/mcp"
			case "token in label":
				record.Document.Repos[0].Name = c.token
			}
			got := repoInitConnection(t.Context(), root, c, func() (config.VersionedDocument, error) { return record, lookupErr })
			if name == "verified" {
				if got != (repoInitContext{Server: "https://conveyor.example.com", Workspace: "demo", Name: "example", Base: "main"}) {
					t.Fatalf("context = %+v", got)
				}
			} else if got != unresolvedRepoInitContext() {
				t.Fatalf("unverified context = %+v", got)
			}
			rendered, err := renderRepoInit("v1", got.Name, got.Base, got)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"secret-value", "credential@", "raw lookup error", "raw config error", "injected"} {
				if strings.Contains(string(rendered), secret) {
					t.Fatalf("guidance leaked %q", secret)
				}
			}
		})
	}
}

func TestRepoInitPortableServer(t *testing.T) {
	for _, value := range []string{"http://localhost:8080", "http://LOCALHOST./", "http://foo.localhost", "http://127.0.0.2", "http://[::1]", "http://[::1%25lo]", "http://0.0.0.0", "https://user:secret@example.com", "https://example.com?token=secret", "https://example.com?", "https://example.com#", "https://example.com/#secret", "https://example.com/%0a", "https://example.com/%27", "https://example.com/`injected`", "https://example.com/$(cmd)", "https://example.com/\n", "file:///tmp/local", "not-a-url", ""} {
		if got, ok := repoInitServer(value); ok {
			t.Errorf("accepted %q as %q", value, got)
		}
	}
	for _, value := range []string{"https://conveyor.example.com", "https://other.example.com:8443/factory"} {
		if got, ok := repoInitServer(value + "/mcp/"); !ok || got != value {
			t.Errorf("canonical %q = %q, %v", value, got, ok)
		}
	}
}

func TestRepoInitVerifiedRefresh(t *testing.T) {
	first := repoInitContext{Server: "https://conveyor.example.com", Workspace: "demo", Name: "example", Base: "main"}
	second := repoInitContext{Server: "https://factory.example.org:8443/team", Workspace: "other", Name: "another", Base: "trunk"}
	for _, form := range []string{"symlink", "regular", "reverse", "symlink guidance-only", "reverse guidance-only"} {
		t.Run(form, func(t *testing.T) {
			root := t.TempDir()
			old := "<!-- conveyor:repo-init owner=v1 version=v0 -->\nOld guidance without context.\n<!-- /conveyor:repo-init -->"
			writeRepoFixture(t, root, "AGENTS.md", "Before\n"+old+"\nAfter")
			if form == "regular" {
				writeRepoFixture(t, root, "CLAUDE.md", "Claude before\n"+old+"\nClaude after")
			}
			if strings.HasPrefix(form, "reverse") {
				if err := os.Rename(filepath.Join(root, "AGENTS.md"), filepath.Join(root, "CLAUDE.md")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("./CLAUDE.md", filepath.Join(root, "AGENTS.md")); err != nil {
					t.Fatal(err)
				}
			}
			run := func(version string, c repoInitContext) string {
				t.Helper()
				var out bytes.Buffer
				if err := prepareRepositoryWithOptions(root, version, c.Name, c.Base, &out, repoInitOptions{guidanceOnly: strings.Contains(form, "guidance-only")}, installEmbeddedSkillsForDestinationsWithForce, c); err != nil {
					t.Fatal(err)
				}
				return out.String()
			}
			run("v1", first)
			snapshot := repoFixtureSnapshot(t, root)
			run("v1", first)
			if !reflect.DeepEqual(snapshot, repoFixtureSnapshot(t, root)) {
				t.Fatal("verified rerun changed files")
			}
			report := run("v2", unresolvedRepoInitContext())
			for _, file := range []string{"AGENTS.md", "CLAUDE.md"} {
				if repoFixtureSnapshot(t, root)[file] != snapshot[file] {
					t.Fatalf("unavailable refresh changed %s", file)
				}
			}
			if !strings.Contains(report, "retained without reverification") {
				t.Fatal("missing retained-context report")
			}
			run("v2", second)
			for _, file := range []string{"AGENTS.md", "CLAUDE.md"} {
				data, err := os.ReadFile(filepath.Join(root, file))
				if err != nil {
					t.Fatal(err)
				}
				text := string(data)
				for _, want := range []string{second.Server, "Workspace: `other`", "--server '" + second.Server + "' --workspace 'other'", "native MCP", "failed endpoint", "SSH configuration"} {
					if !strings.Contains(text, want) {
						t.Errorf("%s missing %q", file, want)
					}
				}
				if strings.Contains(text, first.Server) {
					t.Error("stale endpoint")
				}
			}
			content, _ := os.ReadFile(filepath.Join(root, "AGENTS.md"))
			if !strings.HasPrefix(string(content), "Before\n") || !strings.HasSuffix(string(content), "\nAfter") {
				t.Fatal("outside text changed")
			}
		})
	}
}

func TestRepoInitPriorContextRefusalBeforeWrites(t *testing.T) {
	c := repoInitContext{Server: "https://conveyor.example.com", Workspace: "demo", Name: "example", Base: "main"}
	section, err := renderRepoInit("v1", c.Name, c.Base, c)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"conflict", "malformed", "unsafe", "duplicate"} {
		for _, fresh := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fresh=%v", name, fresh), func(t *testing.T) {
				root := t.TempDir()
				writeRepoFixture(t, root, "AGENTS.md", string(section))
				other := string(section)
				switch name {
				case "conflict":
					other = strings.ReplaceAll(other, "Workspace: `demo`", "Workspace: `other`")
				case "malformed":
					other = strings.ReplaceAll(other, "Workspace: `demo`", "Workspace: demo")
				case "unsafe":
					other = strings.ReplaceAll(other, c.Server, "http://localhost:8080")
				case "duplicate":
					other = strings.ReplaceAll(other, "## Conveyor factory work", "Server: `https://elsewhere.example.com`. Workspace: `demo`.\n## Conveyor factory work")
				}
				writeRepoFixture(t, root, "CLAUDE.md", other)
				before := repoFixtureSnapshot(t, root)
				input := unresolvedRepoInitContext()
				if fresh {
					input = c
				}
				err := prepareRepositoryWithInstaller(root, "v2", input.Name, input.Base, io.Discard, installEmbeddedSkillsForDestinationsWithForce, input)
				if err == nil {
					t.Fatal("accepted invalid prior context")
				}
				if !reflect.DeepEqual(before, repoFixtureSnapshot(t, root)) {
					t.Fatal("refusal wrote files")
				}
			})
		}
	}
}

type repoInitTransport func(*http.Request) (*http.Response, error)

func (f repoInitTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRepoInitCommandManualAndDispatchedParity(t *testing.T) {
	oldHTTP := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: repoInitTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.URL.Path != "/v1/workspace/config" || r.Header.Get("Authorization") != "Bearer fixture-secret" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		workspace := r.Header.Get("X-Workspace-ID")
		if (r.URL.Host == "one.example.com" && workspace != "one") || (r.URL.Host == "two.example.com" && workspace != "two") {
			t.Fatal("server/workspace mismatch")
		}
		data, _ := json.Marshal(config.VersionedDocument{Document: config.WorkspaceDocument{Workspace: workspace, Repos: []config.Repo{{Name: "example", Base: "main", URL: "https://github.com/example/repo"}}}})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(data)), Header: make(http.Header)}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = oldHTTP })
	oldServer, oldWorkspace, oldSE, oldWE := serverFlag, workspaceFlag, serverFlagExplicit, workspaceFlagExplicit
	t.Cleanup(func() {
		serverFlag, workspaceFlag, serverFlagExplicit, workspaceFlagExplicit = oldServer, oldWorkspace, oldSE, oldWE
	})
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("CONVEYOR_API_TOKEN", "fixture-secret")
	for _, workspace := range []string{"one", "two"} {
		var outputs []string
		for _, manual := range []bool{true, false} {
			root := t.TempDir()
			mustGit(t, root, "init", "-b", "main")
			mustGit(t, root, "remote", "add", "origin", "git@github.com:example/repo.git")
			t.Chdir(root)
			endpoint := "https://" + workspace + ".example.com"
			// Worker environment transport carries /mcp; CLI canonicalizes to the same base.
			t.Setenv("CONVEYOR_ADDR", endpoint+"/mcp")
			t.Setenv("CONVEYOR_WORKSPACE", workspace)
			serverFlag, workspaceFlag, serverFlagExplicit, workspaceFlagExplicit = "", "", false, false
			if manual {
				serverFlag, workspaceFlag, serverFlagExplicit, workspaceFlagExplicit = endpoint, workspace, true, true
			}
			command := repoCmd()
			command.SetArgs([]string{"init"})
			var out bytes.Buffer
			command.SetOut(&out)
			if err := command.Execute(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(root, "AGENTS.md"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), "Server: `"+endpoint+"`. Workspace: `"+workspace+"`.") {
				t.Fatalf("unverified guidance: %s", data)
			}
			if strings.Contains(string(data)+out.String(), "fixture-secret") {
				t.Fatal("credential leaked")
			}
			outputs = append(outputs, string(data))
		}
		if outputs[0] != outputs[1] {
			t.Fatal("manual and dispatched guidance differ")
		}
	}
}

// Explicit semantic checks supplement, never replace, the complete byte snapshot.
func repoInitGitState(t *testing.T, root string) map[string]string {
	t.Helper()
	commands := map[string][]string{
		"HEAD":                         {"rev-parse", "HEAD"},
		"branch":                       {"symbolic-ref", "HEAD"},
		"worktrees":                    {"worktree", "list", "--porcelain"},
		"refs including push tracking": {"show-ref"},
		"remotes":                      {"remote", "-v"},
		"config":                       {"config", "--local", "--null", "--list"},
		"index":                        {"ls-files", "--stage", "--debug"},
		"staged":                       {"diff", "--cached", "--raw"},
	}
	state := map[string]string{}
	for name, args := range commands {
		state[name] = mustGitOutput(t, root, args...)
	}
	return state
}

func assertRepoInitSnapshot(t *testing.T, before, after map[string]string) {
	t.Helper()
	for path, old := range before {
		if value, exists := after[path]; !exists || value != old {
			t.Errorf("Git snapshot changed or removed %s", path)
		}
	}
	for path := range after {
		if _, exists := before[path]; !exists {
			t.Errorf("Git snapshot added %s", path)
		}
	}
}

func TestRepoInitRetainedContextMissingPeer(t *testing.T) {
	c := repoInitContext{Server: "https://conveyor.example.com", Workspace: "demo", Name: "example", Base: "main"}
	section, err := renderRepoInit("v1", c.Name, c.Base, c)
	if err != nil {
		t.Fatal(err)
	}
	for _, existing := range []string{"AGENTS.md", "CLAUDE.md"} {
		t.Run(existing, func(t *testing.T) {
			root := t.TempDir()
			writeRepoFixture(t, root, existing, string(section))
			var out bytes.Buffer
			missing := unresolvedRepoInitContext()
			if err := prepareRepositoryWithInstaller(root, "v1", missing.Name, missing.Base, &out, installEmbeddedSkillsForDestinationsWithForce, missing); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
				content, err := os.ReadFile(filepath.Join(root, name))
				if err != nil || !bytes.Equal(content, section) {
					t.Fatalf("retained %s: %v", name, err)
				}
				status := "written"
				if name == existing {
					status = "unchanged"
				}
				if !strings.Contains(out.String(), "repo\t"+status+"\t"+filepath.Join(root, name)) {
					t.Fatalf("incorrect report: %s", out.String())
				}
			}
		})
	}
}

func TestRepoInitGuidanceOnlySourceWrappers(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	mustGit(t, root, "init", "-b", "main")
	writeRepoFixture(t, root, "CLAUDE.md", "Maintained instructions\n")
	if err := os.Chmod(filepath.Join(root, "CLAUDE.md"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("CLAUDE.md", filepath.Join(root, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	writeRepoFixture(t, root, ".claude/skills/conveyor-work/SKILL.md", "Maintained source wrapper\n")
	if err := os.Chmod(filepath.Join(root, ".claude/skills/conveyor-work/SKILL.md"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := repoFixtureSnapshot(t, root)
	if err := prepareRepository(root, "v1", "example", "main", io.Discard); err == nil {
		t.Fatal("full installation accepted unowned source")
	}
	assertRepoInitSnapshot(t, before, repoFixtureSnapshot(t, root))
	for run := 0; run < 2; run++ {
		command := repoCmd()
		command.SetArgs([]string{"init", "--guidance-only"})
		var out bytes.Buffer
		command.SetOut(&out)
		if err := command.Execute(); err != nil {
			t.Fatal(err)
		}
		if len(strings.Split(strings.TrimSpace(out.String()), "\n")) != 2 || strings.Contains(out.String(), "skills") {
			t.Fatalf("unexpected reports: %s", out.String())
		}
		after := repoFixtureSnapshot(t, root)
		if run == 1 {
			assertRepoInitSnapshot(t, before, after)
		}
		delete(after, "CLAUDE.md")
		for path, value := range before {
			if path != "CLAUDE.md" && after[path] != value {
				t.Fatalf("changed %s", path)
			}
		}
		if len(after) != len(before)-1 {
			t.Fatal("created unexpected files")
		}
		info, err := os.Stat(filepath.Join(root, "CLAUDE.md"))
		if err != nil || info.Mode().Perm() != 0o640 {
			t.Fatal("target permissions changed")
		}
		before = repoFixtureSnapshot(t, root)
	}
}

func TestRepoInitUnsafeGuidancePairs(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		for _, only := range []bool{false, true} {
			for _, kind := range []string{"dangling", "cycle", "indirect", "escaping", "unrelated", "directory", "traversal"} {
				t.Run(fmt.Sprintf("reverse=%v/only=%v/%s", reverse, only, kind), func(t *testing.T) {
					root := t.TempDir()
					link, target := "CLAUDE.md", "AGENTS.md"
					if reverse {
						link, target = target, link
					}
					writeRepoFixture(t, root, target, "Keep rules")
					destination := target
					switch kind {
					case "dangling":
						if err := os.Remove(filepath.Join(root, target)); err != nil {
							t.Fatal(err)
						}
					case "cycle":
						if err := os.Remove(filepath.Join(root, target)); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink(link, filepath.Join(root, target)); err != nil {
							t.Fatal(err)
						}
					case "indirect":
						if err := os.Symlink(target, filepath.Join(root, "indirect")); err != nil {
							t.Fatal(err)
						}
						destination = "indirect"
					case "escaping":
						external := t.TempDir()
						writeRepoFixture(t, external, "rules", "External")
						destination = filepath.Join(external, "rules")
					case "unrelated":
						writeRepoFixture(t, root, "other", "Other")
						destination = "other"
					case "directory":
						if err := os.Remove(filepath.Join(root, target)); err != nil {
							t.Fatal(err)
						}
						if err := os.Mkdir(filepath.Join(root, target), 0o755); err != nil {
							t.Fatal(err)
						}
					case "traversal":
						destination = "nested/../" + target
					}
					if err := os.Symlink(destination, filepath.Join(root, link)); err != nil {
						t.Fatal(err)
					}
					before := repoFixtureSnapshot(t, root)
					if err := prepareRepositoryWithOptions(root, "v1", "example", "main", io.Discard, repoInitOptions{guidanceOnly: only}, installEmbeddedSkillsForDestinationsWithForce); err == nil {
						t.Fatal("accepted unsafe link")
					}
					assertRepoInitSnapshot(t, before, repoFixtureSnapshot(t, root))
				})
			}
		}
	}
}

func TestRepoInitGuidancePublicationFailure(t *testing.T) {
	for _, form := range []string{"regular", "forward", "reverse"} {
		for _, only := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/only=%v", form, only), func(t *testing.T) {
				root := t.TempDir()
				writeRepoFixture(t, root, "AGENTS.md", "Operator text")
				if form == "regular" {
					writeRepoFixture(t, root, "CLAUDE.md", "Other text")
				} else if form == "forward" {
					if err := os.Symlink("AGENTS.md", filepath.Join(root, "CLAUDE.md")); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Rename(filepath.Join(root, "AGENTS.md"), filepath.Join(root, "CLAUDE.md")); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink("CLAUDE.md", filepath.Join(root, "AGENTS.md")); err != nil {
						t.Fatal(err)
					}
				}
				before := repoFixtureSnapshot(t, root)
				calls := 0
				options := repoInitOptions{guidanceOnly: only, rename: func(from, to string) error {
					calls++
					if form != "regular" || calls == 2 {
						return errors.New("injected publication failure")
					}
					return os.Rename(from, to)
				}}
				err := prepareRepositoryWithOptions(root, "v1", "example", "main", io.Discard, options, installEmbeddedSkillsForDestinationsWithForce)
				if err == nil || !strings.Contains(err.Error(), "injected") {
					t.Fatalf("error = %v", err)
				}
				assertRepoInitSnapshot(t, before, repoFixtureSnapshot(t, root))
			})
		}
	}
}

func TestRepoInitGuidanceOnlySkipsInstallerAndWritesTargetOnce(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		for _, spelling := range []string{"relative", "dot", "absolute"} {
			t.Run(fmt.Sprintf("reverse=%v/%s", reverse, spelling), func(t *testing.T) {
				root := t.TempDir()
				link, target := "CLAUDE.md", "AGENTS.md"
				if reverse {
					link, target = target, link
				}
				writeRepoFixture(t, root, target, "Operator rules")
				destination := target
				if spelling == "dot" {
					destination = "./" + target
				}
				if spelling == "absolute" {
					destination = filepath.Join(root, target)
				}
				if err := os.Symlink(destination, filepath.Join(root, link)); err != nil {
					t.Fatal(err)
				}
				// Even unsafe skill roots must be uninspected in explicit guidance-only mode.
				if err := os.Symlink("missing", filepath.Join(root, ".claude")); err != nil {
					t.Fatal(err)
				}
				writes := 0
				options := repoInitOptions{guidanceOnly: true, rename: func(from, to string) error {
					writes++
					if to != filepath.Join(root, target) {
						t.Fatalf("wrote %s", to)
					}
					return os.Rename(from, to)
				}}
				installer := func(string, []skillDestination, string, bool, bool) ([]skillInstallFile, []skillInstallReport, error) {
					t.Fatal("called skill installer")
					return nil, nil, nil
				}
				var out bytes.Buffer
				if err := prepareRepositoryWithOptions(root, "v1", "example", "main", &out, options, installer); err != nil {
					t.Fatal(err)
				}
				if writes != 1 {
					t.Fatalf("writes = %d", writes)
				}
				if got, err := os.Readlink(filepath.Join(root, link)); err != nil || got != destination {
					t.Fatalf("link changed: %q %v", got, err)
				}
				for _, name := range []string{link, target} {
					if !strings.Contains(out.String(), "repo\tupdated\t"+filepath.Join(root, name)) {
						t.Fatalf("wrong logical report: %s", out.String())
					}
				}
			})
		}
	}
}

func TestRepoInitPreservedLinkPreimage(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		root := t.TempDir()
		link, target := "CLAUDE.md", "AGENTS.md"
		if reverse {
			link, target = target, link
		}
		writeRepoFixture(t, root, target, "Operator rules")
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
		installer := func(string, []skillDestination, string, bool, bool) ([]skillInstallFile, []skillInstallReport, error) {
			if err := os.Remove(filepath.Join(root, link)); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("./"+target, filepath.Join(root, link)); err != nil {
				t.Fatal(err)
			}
			return nil, nil, nil
		}
		err := prepareRepositoryWithInstaller(root, "v1", "example", "main", io.Discard, installer)
		if err == nil || !strings.Contains(err.Error(), "changed during preparation") {
			t.Fatalf("error = %v", err)
		}
		content, err := os.ReadFile(filepath.Join(root, target))
		if err != nil || string(content) != "Operator rules" {
			t.Fatal("published after link changed")
		}
	}
}
