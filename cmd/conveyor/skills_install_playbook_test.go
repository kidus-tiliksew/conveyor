package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/playbookcheck"
)

const installPlaybookSource = "docs/playbooks/conveyor-install.md"

func readInstallPlaybook(t *testing.T) []byte {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	content, err := os.ReadFile(filepath.Join(filepath.Dir(currentFile), "..", "..", filepath.FromSlash(installPlaybookSource)))
	if err != nil {
		t.Fatal(err)
	}
	return content
}

// Every conveyor command and flag the setup playbook directs is registered
// on the command tree main builds (req-agent-guided-setup AC-5.3). The test
// is not parallel because newRootCmd binds the package's global flag
// variables.
func TestInstallPlaybookConveyorCommandsExist(t *testing.T) {
	invocations := playbookcheck.Extract(readInstallPlaybook(t))
	checked := 0
	for _, invocation := range invocations {
		if invocation.Program != "conveyor" {
			continue
		}
		checked++
		if err := playbookcheck.CheckCobra(newRootCmd(), invocation.Args); err != nil {
			t.Errorf("%s %s: %v", installPlaybookSource, invocation, err)
		}
	}
	// The playbook directs dozens of conveyor commands; a near-empty count
	// means the extractor stopped reading it.
	if checked < 30 {
		t.Fatalf("checked %d conveyor invocations in %s, want at least 30", checked, installPlaybookSource)
	}
}

// A misspelled subcommand or flag in a playbook fixture fails the same check
// the published playbook passes (req-agent-guided-setup AC-5.3).
func TestInstallPlaybookCommandCheckRefusesMisspellings(t *testing.T) {
	fixture := "Install skills with `conveyor skills install`.\n\n" +
		"```sh\n" +
		"conveyor skils install\n" +
		"conveyor --server <server-url> mcp install --nmae factory\n" +
		"conveyor --server <server-url> worker pair --tll 5m\n" +
		"```\n\n" +
		"Change a setup with `conveyor setup create|edti main`.\n"
	invocations := playbookcheck.Extract([]byte(fixture))
	if len(invocations) != 5 {
		t.Fatalf("fixture invocations = %d, want 5", len(invocations))
	}
	if err := playbookcheck.CheckCobra(newRootCmd(), invocations[0].Args); err != nil {
		t.Errorf("%s: %v", invocations[0], err)
	}
	for _, test := range []struct {
		invocation playbookcheck.Invocation
		want       string
	}{
		{invocation: invocations[1], want: `unknown command "skils" for "conveyor"`},
		{invocation: invocations[2], want: `unknown flag "--nmae" for "conveyor mcp install"`},
		{invocation: invocations[3], want: `unknown flag "--tll" for "conveyor worker pair"`},
		{invocation: invocations[4], want: `unknown command "create|edti" for "conveyor setup"`},
	} {
		err := playbookcheck.CheckCobra(newRootCmd(), test.invocation.Args)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error = %v, want %s", test.invocation, err, test.want)
		}
	}
}

// The setup skill installs for every supported tool in user-global and
// project scope, and its playbook matches the published playbook of the same
// release (req-agent-guided-setup AC-1.3; req-agent-guidance-install REQ-1).
func TestConveyorInstallSkillInstallsForEveryTool(t *testing.T) {
	published := readInstallPlaybook(t)
	lookPath := func(name string) (string, error) { return "/tools/" + name, nil }
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := t.TempDir()
	t.Chdir(project)
	for _, scope := range []struct {
		name string
		args []string
		base string
	}{
		{name: "global", base: home},
		{name: "project", args: []string{"--project"}, base: project},
	} {
		command := skillsInstallCmdWithLookPath(lookPath)
		command.SetArgs(scope.args)
		var output bytes.Buffer
		command.SetOut(&output)
		command.SetErr(&output)
		if err := command.Execute(); err != nil {
			t.Fatalf("%s install: %v\n%s", scope.name, err, output.String())
		}
		base, err := filepath.EvalSymlinks(scope.base)
		if err != nil {
			t.Fatal(err)
		}
		destinations := skillDestinations(base, supportedSkillTools, scope.name == "project")
		if len(destinations) != 4 {
			t.Fatalf("%s destinations = %d, want 4", scope.name, len(destinations))
		}
		for _, destination := range destinations {
			root := filepath.Join(destination.root, "conveyor-install")
			for _, name := range []string{"SKILL.md", "conveyor-install.md"} {
				if !strings.Contains(output.String(), destination.tool.name+"\tcreated\t"+filepath.Join(root, name)) {
					t.Errorf("%s %s install did not report conveyor-install/%s:\n%s", scope.name, destination.tool.name, name, output.String())
				}
			}
			wrapper, err := os.ReadFile(filepath.Join(root, "SKILL.md"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(wrapper, []byte("[conveyor-install.md](conveyor-install.md)")) || bytes.Contains(wrapper, []byte("../../../docs/")) {
				t.Errorf("%s %s setup wrapper does not link to its sibling playbook", scope.name, destination.tool.name)
			}
			playbook, err := os.ReadFile(filepath.Join(root, "conveyor-install.md"))
			if err != nil {
				t.Fatal(err)
			}
			if _, owned := managedSkillVersion(playbook, installPlaybookSource); !owned {
				t.Errorf("%s %s setup playbook has no ownership marker", scope.name, destination.tool.name)
			}
			_, body, _ := bytes.Cut(playbook, []byte("\n"))
			if !bytes.Equal(body, published) {
				t.Errorf("%s %s installed setup playbook differs from %s", scope.name, destination.tool.name, installPlaybookSource)
			}
		}
	}
}

// The playbook cites each criterion it implements, so a reviewer can map
// every req-agent-guided-setup REQ-2, REQ-3, and REQ-4 criterion to its
// section.
func TestInstallPlaybookCitesSetupCriteria(t *testing.T) {
	playbook := readInstallPlaybook(t)
	for _, criterion := range []string{"AC-1.1", "AC-1.3", "AC-2.1", "AC-2.2", "AC-2.3", "AC-2.4", "AC-3.1", "AC-3.2", "AC-3.3", "AC-3.4", "AC-3.5", "AC-3.6", "AC-3.7", "AC-4.1", "AC-4.2", "AC-5.1", "AC-5.2"} {
		if !bytes.Contains(playbook, []byte(criterion)) {
			t.Errorf("%s does not cite req-agent-guided-setup %s", installPlaybookSource, criterion)
		}
	}
}

// repo init installs the setup skill project-scoped for every supported
// tool, whether or not the tool is present (req-agent-guidance-install
// AC-2.3).
func TestRepoInitInstallsSetupSkillForEveryTool(t *testing.T) {
	root := t.TempDir()
	var output bytes.Buffer
	if err := prepareRepository(root, "v2.0.0", "example", "trunk", &output); err != nil {
		t.Fatal(err)
	}
	destinations := skillDestinations(root, supportedSkillTools, true)
	if len(destinations) != 4 {
		t.Fatalf("destinations = %d, want 4", len(destinations))
	}
	installed := 0
	for _, destination := range destinations {
		for _, asset := range embeddedSkillManifest {
			if !strings.HasPrefix(asset.relative, "conveyor-install/") {
				continue
			}
			installed++
			path := filepath.Join(destination.root, filepath.FromSlash(asset.relative))
			if !strings.Contains(output.String(), destination.tool.name+"\twritten\t"+path) {
				t.Errorf("repo init did not report %s for %s:\n%s", path, destination.tool.name, output.String())
			}
			want, err := renderEmbeddedSkill(asset, "v2.0.0")
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(want, got) {
				t.Fatalf("%s %s differs from the embedded skill: %v", destination.tool.name, asset.relative, err)
			}
		}
	}
	if installed != 2*len(destinations) {
		t.Fatalf("repo init installed %d setup skill files, want %d", installed, 2*len(destinations))
	}
}

// The playbook's REST requests never print the CLI credential or a secret
// the response carries (req-agent-guided-setup AC-3.3). Each shell block that
// pipes `conveyor auth token` runs verbatim against a fixture server, with a
// stub conveyor that prints a fixture token. In the hostile mode every
// response carries a token-bearing sign_in_url, so no block's allowlist can
// pass it through; HTTP failures still show their status and error.
func TestInstallPlaybookRESTRequestsKeepSecretsOutOfOutput(t *testing.T) {
	for _, tool := range []string{"sh", "curl", "grep", "sed"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed: %v", tool, err)
		}
	}
	const (
		fixtureToken  = "cv_pat_fixture_credential_secret"
		signInSecret  = "fixture-sign-in-secret"
		fixtureSignIn = "http://127.0.0.1/sign-in#token=" + signInSecret
	)
	bin := t.TempDir()
	stub := "#!/bin/sh\ncase \"$*\" in\n  *'auth token'*) echo " + fixtureToken + " ;;\n  *) exit 2 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "conveyor"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	var blocks []playbookcheck.ShellBlock
	for _, block := range playbookcheck.ShellBlocks(readInstallPlaybook(t)) {
		if strings.Contains(block.Text, "auth token |") {
			blocks = append(blocks, block)
		}
	}
	if len(blocks) != 3 {
		t.Fatalf("found %d REST request blocks in %s, want 3 (identity, workspace, invitation)", len(blocks), installPlaybookSource)
	}
	respond := func(hostile bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+fixtureToken {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			switch {
			case hostile && r.URL.Path == "/v1/workspaces":
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"workspace_conflict","message":"workspace could not be created","sign_in_url":"` + fixtureSignIn + `"}`))
			case hostile:
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"email":"teammate@example.com","role":"contributor","display_name":"Ada","sign_in_url":"` + fixtureSignIn + `","token":"` + signInSecret + `","delivery":"fallback"}`))
			case r.Method == http.MethodGet && r.URL.Path == "/v1/me":
				_, _ = w.Write([]byte(`{"id":"usr_1","email":"ada@example.com","display_name":"Ada","can_create_workspaces":true}`))
			case r.Method == http.MethodPost && r.URL.Path == "/v1/workspaces":
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"id":"team-a","name":"Team A","config_version":1}`))
			case r.Method == http.MethodPost && r.URL.Path == "/v1/workspaces/demo/members":
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"email":"teammate@example.com","role":"contributor","sign_in_url":"` + fixtureSignIn + `","delivery":"fallback"}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}
	}
	for _, mode := range []struct {
		name    string
		hostile bool
		want    []string
	}{
		{name: "ordinary", want: []string{"HTTP 200", `"can_create_workspaces":true`, "HTTP 201", `"id":"team-a"`, `"delivery":"fallback"`, `"email":"teammate@example.com"`}},
		{name: "hostile", hostile: true, want: []string{"HTTP 409", `"error":"workspace_conflict"`, "HTTP 201", `"delivery":"fallback"`}},
	} {
		server := httptest.NewServer(respond(mode.hostile))
		var combined strings.Builder
		for _, block := range blocks {
			script := strings.NewReplacer("<server-url>", server.URL, "<workspace-id>", "demo").Replace(block.Text)
			directory := t.TempDir()
			command := exec.Command("sh", "-c", script)
			command.Dir = directory
			command.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			output, err := command.CombinedOutput()
			if err != nil {
				t.Errorf("%s %s line %d: %v\n%s", mode.name, installPlaybookSource, block.Line, err, output)
			}
			for _, secret := range []string{fixtureToken, signInSecret, "sign_in_url"} {
				if bytes.Contains(output, []byte(secret)) {
					t.Errorf("%s %s line %d printed %q:\n%s", mode.name, installPlaybookSource, block.Line, secret, output)
				}
			}
			if _, err := os.Stat(filepath.Join(directory, "response.json")); !os.IsNotExist(err) {
				t.Errorf("%s %s line %d left its response file behind: %v", mode.name, installPlaybookSource, block.Line, err)
			}
			combined.Write(output)
		}
		server.Close()
		for _, want := range mode.want {
			if !strings.Contains(combined.String(), want) {
				t.Errorf("%s output lacks %q:\n%s", mode.name, want, combined.String())
			}
		}
	}
}
