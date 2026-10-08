package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
	"gopkg.in/yaml.v3"
)

// toolchainHost is a synthetic host: a Homebrew-shaped prefix whose bin entry
// is a symlink into Cellar, a custom Go bin, an operator HOME with config, and
// an absent cache. No installed package manager or real user HOME is used.
type toolchainHost struct {
	dir, brew, cellarTool, gobin, gopath, home, xdg, goenv, cache, observed string
}

const toolchainGoenv = "GOFLAGS=-mod=mod\n"

const toolchainMainScript = `#!/bin/sh
env > "$1/main.env"
if [ "$2" = ui ] && [ -e "$1/expect-ui" ]; then
	i=0
	while [ ! -s "$1/ui.env" ] && [ $i -lt 100 ]; do sleep 0.05; i=$((i+1)); done
fi
if [ "$2" = mutate ]; then
	printf 'exit 0\n' >> "$3"
fi
if [ "$2" = replace ]; then
	cp "$3" "$3.new" && mv "$3.new" "$3"
fi
fixture-lint > "$1/lint.out" || exit 3
`

func newToolchainHost(t *testing.T) toolchainHost {
	t.Helper()
	dir := t.TempDir()
	h := toolchainHost{dir: dir, brew: filepath.Join(dir, "opt/homebrew/bin"), gopath: filepath.Join(dir, "operator/go"), gobin: filepath.Join(dir, "operator/go/bin"), home: filepath.Join(dir, "operator"), xdg: filepath.Join(dir, "operator/.config"), goenv: filepath.Join(dir, "operator/.config/go/env"), cache: filepath.Join(dir, "cache/go-build"), observed: filepath.Join(dir, "observed")}
	cellar := filepath.Join(dir, "opt/homebrew/Cellar/fixture-go/1.0/bin")
	for _, d := range []string{h.brew, cellar, h.gobin, filepath.Dir(h.goenv), h.observed} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	h.cellarTool = filepath.Join(cellar, "fixture-go")
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write(h.cellarTool, toolchainMainScript)
	if err := os.Symlink("../Cellar/fixture-go/1.0/bin/fixture-go", filepath.Join(h.brew, "fixture-go")); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(h.gobin, "fixture-lint"), "#!/bin/sh\necho lint-ok\n")
	write(h.goenv, toolchainGoenv)
	write(filepath.Join(h.brew, "fixture-ui"), "#!/bin/sh\nenv > \"$1/ui.env\"\nsleep 30 & wait\n")
	resolved, err := filepath.EvalSymlinks(h.cellarTool)
	if err != nil {
		t.Fatal(err)
	}
	h.cellarTool = resolved
	return h
}

func (h toolchainHost) record(server string) config.VerificationToolchain {
	return config.VerificationToolchain{Server: server, Workspace: "demo", Repository: "repo", SearchPaths: []string{h.brew, h.gobin, "/usr/bin", "/bin"}, Home: h.home, Settings: map[string]string{"GOPATH": h.gopath, "GOENV": "off", "GOCACHE": h.cache, "XDG_CONFIG_HOME": h.xdg}}
}

func (h toolchainHost) exercise(mode ...string) verification.Exercise {
	return verification.Exercise{ID: "build-all", Kind: "script", Argv: append([]string{"fixture-go", h.observed}, mode...), Cwd: ".", TimeoutSeconds: 10, RequiredAssertions: []verification.Assertion{}, Operations: []verification.Operation{}, Prerequisites: []verification.Prerequisite{{ID: "lint", Kind: "executable", EnvironmentBinding: "fixture-lint"}}}
}

func (h toolchainHost) observedEnv(t *testing.T, name string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h.observed, name))
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			env[key] = value
		}
	}
	return env
}

// grantToolchainSubject gives the subject an unrevoked grant and local
// kit_permissions, so only the toolchain decides admission.
func grantToolchainSubject(f *kitExecutionFixture, subject store.VerificationSubjectContract, toolchains []config.VerificationToolchain) {
	f.snapshot.Attempts, f.v.snapshot.Attempts = nil, nil
	grant := store.VerificationPermissionGrant{ID: "grant", Subject: subject.Subject, Actions: []core.VerificationPermission{{Kind: "operator_interaction", Binding: "repo"}}}
	f.snapshot.PermissionGrants = []store.VerificationPermissionGrant{grant}
	f.v.snapshot.PermissionGrants = f.snapshot.PermissionGrants
	f.v.config = &config.Config{KitPermissions: []config.KitPermissionGrant{{Server: f.v.rpc.client.base, Workspace: "demo", Repository: "repo", Binding: "repo", Actions: []verification.VerificationPermission{{Kind: "operator_interaction", Binding: "repo"}}}}, VerificationToolchains: toolchains}
	f.v.configPath = "/operator/conveyor.yaml"
}

func setParentCredentials(t *testing.T) []string {
	t.Setenv("CONVEYOR_API_TOKEN", "factory-secret-fixture")
	t.Setenv("CONVEYOR_CLIENT_TOKEN", "claim-secret-fixture")
	t.Setenv("GH_TOKEN", "forge-secret-fixture")
	t.Setenv("OPENAI_API_KEY", "provider-secret-fixture")
	return []string{"factory-secret-fixture", "claim-secret-fixture", "forge-secret-fixture", "provider-secret-fixture"}
}

func assertPreflightRefusal(t *testing.T, f *kitExecutionFixture, err error, want ...string) {
	t.Helper()
	var refused *kitPreflightError
	if !errors.As(err, &refused) {
		t.Fatalf("expected toolchain preflight refusal, got %v", err)
	}
	for _, text := range append(want, "no attempt was started", "verification_toolchains", "/operator/conveyor.yaml") {
		if !strings.Contains(err.Error(), text) {
			t.Fatalf("diagnostic %q lacks %q", err, text)
		}
	}
	if f.starts != 0 || len(f.operations) != 0 || f.mutations != 0 || f.uploads != 0 || f.outcome != "" {
		t.Fatalf("preflight refusal consumed work: starts=%d operations=%v mutations=%d uploads=%d outcome=%q", f.starts, f.operations, f.mutations, f.uploads, f.outcome)
	}
}

func assertNoCredentials(t *testing.T, secrets []string, values ...any) {
	t.Helper()
	for _, value := range values {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range secrets {
			if strings.Contains(string(data), secret) {
				t.Fatalf("credential %q reached retained data", secret)
			}
		}
	}
}

// component-verification-runner: tools only in a Homebrew-shaped prefix or a custom Go bin
// are refused by the default profile before any attempt, and succeed for an
// ordinary subject under an explicit scoped profile.
func TestKitToolchainOrdinarySubject(t *testing.T) {
	secrets := setParentCredentials(t)
	h := newToolchainHost(t)
	e := h.exercise()
	e.Operations = []verification.Operation{{ID: "create", TargetBinding: "fixture"}}

	f := newKitExecutionFixture(t, e)
	subject := store.VerificationSubjectContract{Subject: f.snapshot.Attempts[0].Subject, Contract: e}
	grantToolchainSubject(f, subject, nil)
	err := f.v.run(t.Context(), subject, t.TempDir())
	assertPreflightRefusal(t, f, err, "entrypoint fixture-go is unavailable", "default search path", "add a verification_toolchains record")

	partial := h.record(f.v.rpc.client.base)
	partial.SearchPaths = []string{h.brew, "/usr/bin", "/bin"}
	grantToolchainSubject(f, subject, []config.VerificationToolchain{partial})
	err = f.v.run(t.Context(), subject, t.TempDir())
	assertPreflightRefusal(t, f, err, "executable prerequisite lint fixture-lint is unavailable", "configured search path")

	e.Operations = []verification.Operation{}
	f = newKitExecutionFixture(t, e)
	subject = store.VerificationSubjectContract{Subject: f.snapshot.Attempts[0].Subject, Contract: e}
	grantToolchainSubject(f, subject, []config.VerificationToolchain{h.record(f.v.rpc.client.base)})
	if err := f.v.run(t.Context(), subject, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if f.starts != 1 || f.outcome != "succeeded" {
		t.Fatalf("configured toolchain: starts=%d outcome=%s", f.starts, f.outcome)
	}
	env := h.observedEnv(t, "main.env")
	want := map[string]string{"PATH": strings.Join([]string{h.brew, h.gobin, "/usr/bin", "/bin"}, string(os.PathListSeparator)), "HOME": h.home, "GOPATH": h.gopath, "GOENV": "off", "GOCACHE": h.cache, "XDG_CONFIG_HOME": h.xdg, "LANG": "C.UTF-8"}
	for key, value := range want {
		if env[key] != value {
			t.Fatalf("child %s = %q, want %q", key, env[key], value)
		}
	}
	for _, key := range []string{"CONVEYOR_API_TOKEN", "CONVEYOR_CLIENT_TOKEN", "GH_TOKEN", "OPENAI_API_KEY"} {
		if _, ok := env[key]; ok {
			t.Fatalf("parent credential %s reached the child", key)
		}
	}
	if lint, _ := os.ReadFile(filepath.Join(h.observed, "lint.out")); strings.TrimSpace(string(lint)) != "lint-ok" {
		t.Fatalf("child did not resolve the prerequisite through the same search path: %q", lint)
	}
	if _, err := os.Stat(h.cache); !os.IsNotExist(err) {
		t.Fatal("runner created a shared cache directory")
	}
	attributes := f.started[0].Environment.Attributes
	lint, _ := filepath.EvalSymlinks(filepath.Join(h.gobin, "fixture-lint"))
	if attributes["toolchain_scope"] != "configured" || attributes["entrypoint_path"] != h.cellarTool || attributes["prerequisite_lint_path"] != lint || len(attributes["prerequisite_lint_sha256"]) != 64 || attributes["toolchain_search_path"] != want["PATH"] || attributes["transitive_dependencies"] != "unknown" {
		t.Fatalf("toolchain provenance: %+v", attributes)
	}
	if !strings.HasPrefix(attributes["toolchain_home"], "configured sha256:") || strings.Contains(attributes["toolchain_home"], h.home) || !strings.HasPrefix(attributes["toolchain_setting_GOPATH"], "sha256:") || attributes["toolchain_setting_GOENV"] != "off" {
		t.Fatalf("HOME and settings are not fingerprinted: %+v", attributes)
	}
	if len(f.snapshot.Evidence) != 1 {
		t.Fatalf("execution reports: %d", len(f.snapshot.Evidence))
	}
	report := f.snapshot.Evidence[0].Envelope
	if keys := report.Environment.Attributes["child_environment_keys"]; !strings.Contains(keys, "GOPATH") || !strings.Contains(keys, "HOME") || strings.Contains(keys, "GH_TOKEN") {
		t.Fatalf("materialized environment description: %q", keys)
	}
	var payload core.ExecutionReportPayload
	if err := json.Unmarshal(report.Payload, &payload); err != nil || payload.Tool != h.cellarTool {
		t.Fatalf("launch used a different executable than preflight: %+v %v", payload, err)
	}
	assertNoCredentials(t, secrets, f.started, f.snapshot.Evidence, f.output.String())
}

const toolchainKitManifest = `schema_version: 1
kits:
  - id: sample
    name: Sample
    version: "1"
    path: kits/sample
    governing_pins:
      requirements: [{document_id: req-sample, version: 1}]
    ui:
      argv: ["fixture-ui", %q]
      port: 8767
      assets: [index.html]
    exercises:
      - id: build
        stages: [verify]
        kind: script
        argv: ["fixture-go", %q, "ui"]
        cwd: .
        timeout_seconds: 10
        prerequisites: [{id: lint, kind: executable, environment_binding: fixture-lint}]
        required_assertions: []
        retry_policy: safe_to_replay
        safety_basis: read-only
        operations: []
`

func newToolchainKitFixture(t *testing.T, h toolchainHost) (*kitExecutionFixture, store.VerificationSubjectContract) {
	t.Helper()
	f := newKitExecutionFixture(t, verification.Exercise{ID: "unused"})
	root := f.v.root
	manifestText := fmt.Sprintf(toolchainKitManifest, h.observed, h.observed)
	if err := os.WriteFile(filepath.Join(root, ".conveyor/kits/manifest.yaml"), []byte(manifestText), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "kits/sample/index.html"), []byte("<p>fixture</p>\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-qm", "toolchain kit"}} {
		if _, err := localKitGit(t.Context(), root, args...); err != nil {
			t.Fatal(err)
		}
	}
	head, err := localKitGit(t.Context(), root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	f.order.HeadSHA = strings.TrimSpace(string(head))
	f.v.order.HeadSHA = f.order.HeadSHA
	pins := []core.VerificationPin{{Kind: "requirement", DocumentID: "req-sample", Version: 1}}
	for _, snapshot := range []*store.VerificationSnapshot{&f.snapshot, &f.v.snapshot} {
		snapshot.Contexts[0].Revisions[0].SHA = f.order.HeadSHA
		snapshot.Contexts[0].GoverningPins = pins
	}
	receipt, err := validateLocalKits(t.Context(), root, "verify", []verification.Pin{{Kind: "requirement", DocumentID: "req-sample", Version: 1}})
	if err != nil || len(receipt.Kits) != 1 || receipt.Kits[0].Eligibility != "eligible" {
		t.Fatalf("kit receipt: %+v %v", receipt, err)
	}
	check, err := verification.FilesystemPathCheck(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := verification.Parse(strings.NewReader(manifestText), check)
	if err != nil {
		t.Fatal(err)
	}
	subject := store.VerificationSubjectContract{Subject: core.VerificationSubject{Kind: "kit", KitID: "sample", KitVersion: "1", ExerciseID: "build", ContentDigest: receipt.Kits[0].Digest}, Contract: manifest.Kits[0].Exercises[0]}
	f.snapshot.Selections = []store.VerificationSelection{{Receipt: receipt, Subjects: []store.VerificationSubjectContract{subject}}}
	f.v.snapshot.Selections = f.snapshot.Selections
	return f, subject
}

// A kit subject resolves through the same snapshot as an ordinary subject.
func TestKitToolchainKitSubject(t *testing.T) {
	secrets := setParentCredentials(t)
	h := newToolchainHost(t)
	f, subject := newToolchainKitFixture(t, h)
	grantToolchainSubject(f, subject, nil)
	err := f.v.run(t.Context(), subject, t.TempDir())
	assertPreflightRefusal(t, f, err, "entrypoint fixture-go is unavailable")

	grantToolchainSubject(f, subject, []config.VerificationToolchain{h.record(f.v.rpc.client.base)})
	if err := f.v.run(t.Context(), subject, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if f.starts != 1 || f.outcome != "succeeded" || len(f.snapshot.Evidence) != 1 {
		t.Fatalf("kit toolchain run: starts=%d outcome=%s evidence=%d", f.starts, f.outcome, len(f.snapshot.Evidence))
	}
	if env := h.observedEnv(t, "main.env"); env["GOPATH"] != h.gopath || env["HOME"] != h.home {
		t.Fatalf("kit child environment: %+v", env)
	}
	assertNoCredentials(t, secrets, f.started, f.snapshot.Evidence, f.output.String())
}

// An enabled loopback UI uses the same snapshot: the UI and exercise children
// observe one environment, and the UI identity comes from preflight.
func TestKitToolchainKitUI(t *testing.T) {
	secrets := setParentCredentials(t)
	h := newToolchainHost(t)
	f, subject := newToolchainKitFixture(t, h)
	f.v.withUI = true
	if err := os.WriteFile(filepath.Join(h.observed, "expect-ui"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	grantToolchainSubject(f, subject, []config.VerificationToolchain{h.record(f.v.rpc.client.base)})
	if err := f.v.run(t.Context(), subject, t.TempDir()); err != nil || f.starts != 1 || f.outcome != "succeeded" || len(f.snapshot.Evidence) != 2 {
		t.Fatalf("kit UI run: starts=%d outcome=%s evidence=%d err=%v", f.starts, f.outcome, len(f.snapshot.Evidence), err)
	}
	main, ui := h.observedEnv(t, "main.env"), h.observedEnv(t, "ui.env")
	for _, key := range []string{"PATH", "HOME", "GOPATH", "GOENV", "GOCACHE", "XDG_CONFIG_HOME", "LANG", "TMPDIR", "CONVEYOR_KIT_OPERATIONS"} {
		if main[key] == "" || main[key] != ui[key] {
			t.Fatalf("UI %s = %q, exercise %s = %q", key, ui[key], key, main[key])
		}
	}
	uiTool, _ := filepath.EvalSymlinks(filepath.Join(h.brew, "fixture-ui"))
	if f.started[0].Environment.Attributes["ui_path"] != uiTool {
		t.Fatalf("UI identity not recorded: %+v", f.started[0].Environment.Attributes)
	}
	assertNoCredentials(t, secrets, f.started, f.snapshot.Evidence, f.output.String())
}

// A resolved tool changed during execution invalidates the result.
func TestKitToolchainChangeDuringExecution(t *testing.T) {
	h := newToolchainHost(t)
	lint := filepath.Join(h.gobin, "fixture-lint")
	e := h.exercise("mutate", lint)
	f := newKitExecutionFixture(t, e)
	subject := store.VerificationSubjectContract{Subject: f.snapshot.Attempts[0].Subject, Contract: e}
	grantToolchainSubject(f, subject, []config.VerificationToolchain{h.record(f.v.rpc.client.base)})
	if err := f.v.run(t.Context(), subject, t.TempDir()); err == nil || f.outcome != "blocked" || !strings.Contains(err.Error(), "changed during execution") {
		t.Fatalf("changed prerequisite accepted: outcome=%s err=%v", f.outcome, err)
	}
}

// component-verification-runner: an explicitly configured GOENV file is fingerprinted at preflight.
// Editing it in place, or replacing it with identical content, during
// execution invalidates the result although every executable is unchanged.
func TestKitToolchainConfigurationChangeDuringExecution(t *testing.T) {
	for _, mode := range []string{"mutate", "replace"} {
		t.Run(mode, func(t *testing.T) {
			h := newToolchainHost(t)
			e := h.exercise(mode, h.goenv)
			f := newKitExecutionFixture(t, e)
			subject := store.VerificationSubjectContract{Subject: f.snapshot.Attempts[0].Subject, Contract: e}
			record := h.record(f.v.rpc.client.base)
			record.Settings["GOENV"] = h.goenv
			grantToolchainSubject(f, subject, []config.VerificationToolchain{record})
			if err := f.v.run(t.Context(), subject, t.TempDir()); err == nil || f.outcome != "blocked" || !strings.Contains(err.Error(), "configured toolchain location changed during execution") {
				t.Fatalf("changed GOENV accepted: outcome=%s err=%v", f.outcome, err)
			}
			if env := h.observedEnv(t, "main.env"); env["GOENV"] != h.goenv {
				t.Fatalf("child GOENV = %q", env["GOENV"])
			}
			want := fmt.Sprintf("%x", sha256.Sum256([]byte(toolchainGoenv)))
			if got := f.started[0].Environment.Attributes["toolchain_config_GOENV_sha256"]; got != want {
				t.Fatalf("GOENV content fingerprint %q, want %q", got, want)
			}
			if len(f.snapshot.Evidence) != 1 || f.snapshot.Evidence[0].Envelope.Environment.Attributes["toolchain_after"] != "changed" {
				t.Fatalf("execution evidence does not record the changed configuration: %+v", f.snapshot.Evidence)
			}
		})
	}
}

// Configured locations are rechecked before launch: a GOENV file or a
// configured HOME replaced after preflight blocks the attempt without a
// fabricated execution report, and the unchanged control launches.
func TestKitToolchainConfigurationChangedAfterPreflight(t *testing.T) {
	for _, change := range []string{"none", "goenv", "home"} {
		t.Run(change, func(t *testing.T) {
			h := newToolchainHost(t)
			e := h.exercise()
			f := newKitExecutionFixture(t, e)
			record := h.record("https://factory.test")
			record.Settings["GOENV"] = h.goenv
			toolchain, err := resolveKitToolchain(&config.Config{VerificationToolchains: []config.VerificationToolchain{record}}, "/operator/conveyor.yaml", "", "https://factory.test", "demo", "repo", nil)
			if err != nil {
				t.Fatal(err)
			}
			tools, err := toolchain.preflight("ordinary:build-all", e, f.v.root, nil, "", f.v.redactor)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "goenv":
				if err := os.WriteFile(h.goenv, []byte(toolchainGoenv+"GOTOOLCHAIN=local\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "home":
				if err := os.Rename(h.home, h.home+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(h.home, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			err = tools.recheck(toolchain, f.v.root, "")
			if (err == nil) != (change == "none") {
				t.Fatalf("recheck after %s change: %v", change, err)
			}
			if change == "none" {
				return
			}
			f.v.toolchain, f.v.tools = &toolchain, &tools
			env := core.VerificationEnvironment{Attributes: map[string]string{}}
			if err := f.v.launch(t.Context(), e, f.v.root, t.TempDir(), "run", "grant", f.snapshot.Attempts[0].Subject, env, nil); err == nil || !strings.Contains(err.Error(), "changed after preflight") {
				t.Fatalf("launch accepted changed configuration: %v", err)
			}
			if f.outcome != "blocked" || f.uploads != 0 {
				t.Fatalf("changed configuration: outcome=%s uploads=%d", f.outcome, f.uploads)
			}
		})
	}
}

// A configured GOENV file carrying a credential would bypass the approved
// CONVEYOR_KIT_SECRET_* transport; preflight refuses it without echo.
func TestKitToolchainConfigurationCredentialRefused(t *testing.T) {
	secrets := setParentCredentials(t)
	h := newToolchainHost(t)
	if err := os.WriteFile(h.goenv, []byte("GOPROXY=https://user:forge-secret-fixture@proxy.example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := h.exercise()
	f := newKitExecutionFixture(t, e)
	subject := store.VerificationSubjectContract{Subject: f.snapshot.Attempts[0].Subject, Contract: e}
	record := h.record(f.v.rpc.client.base)
	record.Settings["GOENV"] = h.goenv
	grantToolchainSubject(f, subject, []config.VerificationToolchain{record})
	err := f.v.run(t.Context(), subject, t.TempDir())
	assertPreflightRefusal(t, f, err, "settings.GOENV contains a credential value")
	assertNoCredentials(t, secrets, err.Error(), f.output.String())
}

// req-verification-kits REQ-7/AC-7.3; component-verification-runner: repository content never selects a toolchain profile.
// A matching record in a tracked working-directory conveyor.yaml, or in a file
// inside the checkout named explicitly, cannot widen PATH/HOME/settings or start
// an attempt; the same record in operator-selected configuration succeeds.
func TestKitToolchainRepositoryConfigurationRefused(t *testing.T) {
	h := newToolchainHost(t)
	e := h.exercise()
	f := newKitExecutionFixture(t, e)
	subject := f.snapshot.Attempts[0].Subject
	f.snapshot.Attempts = nil
	f.snapshot.Selections = []store.VerificationSelection{{Receipt: verification.SelectionReceipt{Kits: []verification.KitReceipt{{KitID: "sample", Eligibility: "ineligible", Reasons: []verification.SelectionReason{{Code: "pin_mismatch"}}}}}}}
	f.snapshot.PermissionGrants = []store.VerificationPermissionGrant{{ID: "grant", Subject: subject, Actions: []core.VerificationPermission{}}}
	cfg := config.Config{KitPermissions: []config.KitPermissionGrant{{Server: f.v.rpc.client.base, Workspace: "demo", Repository: "repo", Binding: "repo", Actions: []verification.VerificationPermission{}}}, VerificationToolchains: []config.VerificationToolchain{h.record(f.v.rpc.client.base)}}
	harness := config.HarnessTemplates()[0].Harness
	document := localExecutionDocument("demo", newExecutionWizardState(healthyDetections(harness), nil).choices, []config.Harness{harness})
	cfg.ExecutionSettings, cfg.Harnesses, cfg.Review = document.ExecutionSettings, document.Harnesses, document.Review
	cfgBytes, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	repoConfig := filepath.Join(f.v.root, localExecutionConfigName)
	if err := os.WriteFile(repoConfig, cfgBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", localExecutionConfigName}, {"commit", "-qm", "repository toolchain"}} {
		if _, err := localKitGit(t.Context(), f.v.root, args...); err != nil {
			t.Fatal(err)
		}
	}
	head, err := localKitGit(t.Context(), f.v.root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	f.order.HeadSHA = strings.TrimSpace(string(head))
	f.snapshot.Contexts[0].Revisions[0].SHA = f.order.HeadSHA

	// kit verify run in the checkout without --config or CONVEYOR_CONFIG
	// selects the working-directory file, which is not operator-selected.
	t.Setenv("CONVEYOR_CONFIG", "")
	t.Chdir(f.v.root)
	cmd := kitVerifyCmd()
	selected, err := resolveLocalExecutionConfigPath(cmd, cmd.Flags().Lookup("config").Value.String())
	if err != nil || selected.Source != "working-directory file" {
		t.Fatalf("default config selection: %+v %v", selected, err)
	}

	dir := t.TempDir()
	coverage := store.VerificationCoverage{ObligationIDs: []string{e.ID}, Justification: "ordinary checks remain required", Sources: []store.VerificationCoverageSource{{Source: store.VerificationCoverageReference{DocumentID: "fixture", Version: 1, SectionID: "REQ-1"}, Disposition: "covered", Explanation: "ordinary command", Subjects: []core.VerificationSubject{subject}}}}
	coveragePath := filepath.Join(dir, "coverage.json")
	if err := os.WriteFile(coveragePath, core.JSONPayload(coverage), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, refused := range []struct{ path, source, want string }{
		{selected.Path, selected.Source, "not operator-selected configuration"},
		{repoConfig, "flag", "repository content inside the verified checkout"},
		{repoConfig, "environment CONVEYOR_CONFIG", "repository content inside the verified checkout"},
	} {
		options := kitVerifyOptions{configPath: refused.path, configSource: refused.source, coveragePath: coveragePath, attemptRoot: filepath.Join(dir, "attempts")}
		err := verifyKits(t.Context(), f.v.rpc, f.v.root, "task", options, &f.output)
		// The same file also holds kit_permissions, which the runner refuses
		// first with the shared source rule; the toolchain refusal stays the
		// second line of defense below.
		var preflight *kitPreflightError
		if !errors.As(err, &preflight) || !strings.Contains(err.Error(), refused.want) || !strings.Contains(err.Error(), "no attempt was started") || !strings.Contains(err.Error(), "kit_permissions_untrusted_source") {
			t.Fatalf("%s %s: repository toolchain accepted: %v", refused.source, refused.path, err)
		}
		if _, err := resolveKitToolchain(&cfg, refused.path, kitConfigSourceRefusal(refused.path, refused.source, []string{f.v.root}), f.v.rpc.client.base, "demo", "repo", nil); !errors.As(err, &preflight) || !strings.Contains(err.Error(), "toolchain preflight refused") || !strings.Contains(err.Error(), refused.want) {
			t.Fatalf("%s %s: repository toolchain record accepted: %v", refused.source, refused.path, err)
		}
		if f.starts != 0 || len(f.operations) != 0 || f.uploads != 0 || f.outcome != "" {
			t.Fatalf("repository toolchain consumed work: starts=%d uploads=%d outcome=%q", f.starts, f.uploads, f.outcome)
		}
		if _, err := os.Stat(filepath.Join(h.observed, "main.env")); !os.IsNotExist(err) {
			t.Fatal("repository toolchain launched a child")
		}
	}

	operatorConfig := filepath.Join(dir, "operator", localExecutionConfigName)
	if err := os.MkdirAll(filepath.Dir(operatorConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(operatorConfig, cfgBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	options := kitVerifyOptions{configPath: operatorConfig, configSource: "flag", coveragePath: coveragePath, attemptRoot: filepath.Join(dir, "attempts")}
	if err := verifyKits(t.Context(), f.v.rpc, f.v.root, "task", options, &f.output); err != nil {
		t.Fatal(err)
	}
	if f.starts != 1 || f.outcome != "succeeded" {
		t.Fatalf("operator toolchain: starts=%d outcome=%s", f.starts, f.outcome)
	}
	if env := h.observedEnv(t, "main.env"); env["HOME"] != h.home || !strings.HasPrefix(env["PATH"], h.brew) {
		t.Fatalf("operator toolchain environment: %+v", env)
	}
}

// A tool replaced between preflight and launch follows the existing post-start
// blocked path without a fabricated execution report.
func TestKitToolchainChangedAfterPreflight(t *testing.T) {
	h := newToolchainHost(t)
	e := h.exercise()
	f := newKitExecutionFixture(t, e)
	toolchain, err := resolveKitToolchain(&config.Config{VerificationToolchains: []config.VerificationToolchain{h.record("https://factory.test")}}, "/operator/conveyor.yaml", "", "https://factory.test", "demo", "repo", nil)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := toolchain.preflight("ordinary:build-all", e, f.v.root, nil, "", f.v.redactor)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.cellarTool, []byte(toolchainMainScript+"# replaced\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if tools.recheck(toolchain, f.v.root, "") == nil {
		t.Fatal("changed entrypoint passed recheck")
	}
	f.v.toolchain, f.v.tools = &toolchain, &tools
	env := core.VerificationEnvironment{Attributes: map[string]string{}}
	if err := f.v.launch(t.Context(), e, f.v.root, t.TempDir(), "run", "grant", f.snapshot.Attempts[0].Subject, env, nil); err == nil || !strings.Contains(err.Error(), "changed after preflight") {
		t.Fatalf("launch accepted a changed entrypoint: %v", err)
	}
	if f.outcome != "blocked" || f.uploads != 0 {
		t.Fatalf("changed entrypoint: outcome=%s uploads=%d", f.outcome, f.uploads)
	}
}

func TestKitToolchainLookupAndConfigurationRefusals(t *testing.T) {
	h := newToolchainHost(t)
	record := h.record("https://factory.test")
	cfg := &config.Config{VerificationToolchains: []config.VerificationToolchain{record}}
	resolve := func(cfg *config.Config, secrets ...string) (kitToolchain, error) {
		return resolveKitToolchain(cfg, "/operator/conveyor.yaml", "", "https://factory.test", "demo", "repo", secrets)
	}
	toolchain, err := resolve(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Search order, symlink resolution, execute bits and explicit paths.
	if err := os.WriteFile(filepath.Join(h.gobin, "fixture-go"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if path, err := toolchain.lookPath("fixture-go", h.dir); err != nil || path != h.cellarTool {
		t.Fatalf("ordered lookup: %s %v", path, err)
	}
	if err := os.WriteFile(filepath.Join(h.brew, "not-executable"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(h.brew, "a-directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"not-executable", "a-directory", "absent"} {
		if _, err := toolchain.lookPath(name, h.dir); err == nil {
			t.Fatalf("%s resolved", name)
		}
	}
	if path, err := toolchain.lookPath("./opt/homebrew/bin/fixture-go", h.dir); err != nil || path != h.cellarTool {
		t.Fatalf("relative explicit path: %s %v", path, err)
	}
	if _, err := kitDefaultToolchain().lookPath("fixture-go", h.dir); err == nil {
		t.Fatal("default profile discovered a package-manager prefix")
	}

	// Credential aliases refuse without echoing the value.
	t.Setenv("GH_TOKEN", h.gopath)
	if _, err := resolve(cfg, kitParentSecrets()...); err == nil || strings.Contains(err.Error(), h.gopath) || !strings.Contains(err.Error(), "aliases a credential") {
		t.Fatalf("parent credential alias: %v", err)
	}
	t.Setenv("GH_TOKEN", "")
	t.Setenv("CONVEYOR_KIT_SECRET_API", h.home)
	if _, err := resolve(cfg, kitApprovedSecrets()...); err == nil || strings.Contains(err.Error(), h.home) {
		t.Fatalf("approved credential alias: %v", err)
	}

	// Reserved keys and conflicting duplicates.
	for _, env := range [][]string{{"PATH=/bin"}, {"HOME=/tmp"}, {"TMPDIR=/tmp"}, {"GOPATH=/other"}, {"CONVEYOR_KIT_OPERATIONS={}"}, {"CONVEYOR_KIT_UI_PORT=1"}, {"CONVEYOR_KIT_INPUTS={}", "CONVEYOR_KIT_INPUTS=[]"}} {
		if err := toolchain.checkEnvironmentKeys(env); err == nil {
			t.Fatalf("collision accepted: %v", env)
		}
	}
	if err := toolchain.checkEnvironmentKeys([]string{"CONVEYOR_KIT_BINDING_API=https://api.test", "CONVEYOR_KIT_INPUTS={}"}); err != nil {
		t.Fatal(err)
	}
	if _, err := kitMergeEnvironment([]string{"A=1"}, []string{"A=2"}); err == nil {
		t.Fatal("conflicting duplicate merged")
	}

	// Configured locations must have the right type; absent caches are allowed.
	file := filepath.Join(h.dir, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := h.exercise()
	for name, mutate := range map[string]func(*config.VerificationToolchain){
		"search path is a file": func(r *config.VerificationToolchain) { r.SearchPaths = []string{file, h.brew, h.gobin} },
		"absent search path": func(r *config.VerificationToolchain) {
			r.SearchPaths = append(r.SearchPaths, filepath.Join(h.dir, "absent"))
		},
		"absent home":            func(r *config.VerificationToolchain) { r.Home = filepath.Join(h.dir, "absent") },
		"GOENV is a directory":   func(r *config.VerificationToolchain) { r.Settings["GOENV"] = h.dir },
		"absent XDG_CONFIG_HOME": func(r *config.VerificationToolchain) { r.Settings["XDG_CONFIG_HOME"] = filepath.Join(h.dir, "absent") },
		"cache is a file":        func(r *config.VerificationToolchain) { r.Settings["GOCACHE"] = file },
		"GOPATH entry is a file": func(r *config.VerificationToolchain) {
			r.Settings["GOPATH"] = h.gopath + string(os.PathListSeparator) + file
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := h.record("https://factory.test")
			mutate(&r)
			toolchain, err := resolve(&config.Config{VerificationToolchains: []config.VerificationToolchain{r}})
			if err != nil {
				t.Fatal(err)
			}
			var refused *kitPreflightError
			if _, err := toolchain.preflight("ordinary:build-all", e, h.dir, nil, "", nil); !errors.As(err, &refused) || !strings.Contains(err.Error(), "verification_toolchains") {
				t.Fatalf("invalid configured location accepted: %v", err)
			}
		})
	}
	if _, err := toolchain.preflight("ordinary:build-all", e, h.dir, nil, "", nil); err != nil {
		t.Fatalf("absent cache refused: %v", err)
	}
}
