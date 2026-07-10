package localdocker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/adapter"
	"github.com/kidus-tiliksew/conveyor/internal/runner"
	"github.com/kidus-tiliksew/conveyor/internal/secrets"
	"github.com/kidus-tiliksew/conveyor/internal/snapshot"
)

type staticResolver map[string]string

func (s staticResolver) Resolve(_ context.Context, ref secrets.Ref) (string, error) {
	value, ok := s[ref.String()]
	if !ok {
		return "", errors.New("missing test secret")
	}
	return value, nil
}

func TestStartJobStagesOnlyCodexAuthAndCleansUp(t *testing.T) {
	tmp := t.TempDir()
	source := filepath.Join(tmp, "codex")
	if err := os.MkdirAll(filepath.Join(source, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "auth.json"), []byte(`{"token":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "config.toml"), []byte("interactive = true"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "sessions", "history.jsonl"), []byte("history"), 0o600); err != nil {
		t.Fatal(err)
	}

	argsPath := filepath.Join(tmp, "run-args")
	t.Setenv("FAKE_DOCKER_ARGS", argsPath)
	fakeDocker := writeFakeDocker(t, tmp)
	r := New()
	r.Binary = fakeDocker
	control := filepath.Join(tmp, "control")
	if err := os.MkdirAll(control, 0o700); err != nil {
		t.Fatal(err)
	}

	h, err := r.StartJob(context.Background(), runner.StartJobSpec{
		JobID:              "task-1-implement-1",
		TaskID:             "task-1",
		Image:              "conveyor:test",
		Workdir:            "/work",
		ControlDir:         control,
		CredentialsDir:     source,
		CredentialStageDir: filepath.Join(tmp, "staging"),
		Harness:            "codex",
		SandboxTTL:         "job",
	})
	if err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(args), source) {
		t.Fatalf("docker args expose source credential directory: %s", args)
	}

	info := r.jobs[h]
	entries, err := os.ReadDir(info.credentialDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "auth.json" {
		t.Fatalf("staged entries = %v, want only auth.json", entries)
	}
	handoffPath, err := snapshot.Path(control, "task-1-implement-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := (&snapshot.Handoff{State: "done"}).Save(handoffPath); err != nil {
		t.Fatal(err)
	}
	artifacts, err := r.CollectArtifacts(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	if artifacts.HandoffSnapshot != handoffPath {
		t.Fatalf("handoff artifact = %q, want %q", artifacts.HandoffSnapshot, handoffPath)
	}
	if _, err := os.Stat(info.credentialDir); !os.IsNotExist(err) {
		t.Fatalf("credential staging directory still exists: %v", err)
	}
	if _, ok := r.jobs[h]; ok {
		t.Fatal("completed job remains registered")
	}
}

func TestStartJobResolvesSecretsIntoShortLivedEnvFile(t *testing.T) {
	tmp := t.TempDir()
	argsPath := filepath.Join(tmp, "run-args")
	envCapture := filepath.Join(tmp, "captured.env")
	t.Setenv("FAKE_DOCKER_ARGS", argsPath)
	t.Setenv("FAKE_DOCKER_ENV_CAPTURE", envCapture)
	r := New()
	r.Binary = writeFakeDocker(t, tmp)
	r.SecretResolver = staticResolver{
		"secretref://demo/integration/CANARY": "quiet-canary-value",
	}
	r.SecretPolicies = map[string]secrets.SetPolicy{
		"demo/integration": {LocalEligible: true},
	}
	control := filepath.Join(tmp, "control")
	if err := os.MkdirAll(control, 0o700); err != nil {
		t.Fatal(err)
	}
	h, err := r.StartJob(context.Background(), runner.StartJobSpec{
		JobID:          "job-1",
		TaskID:         "task-1",
		Image:          "conveyor:test",
		Workdir:        "/conveyor/jobs/task-task-1/api",
		ControlDir:     control,
		ControlPath:    "/conveyor/control",
		SecretStageDir: filepath.Join(tmp, "secret-stage"),
		SecretRefs:     []string{"secretref://demo/integration/CANARY"},
		Policy: adapter.ToolPolicy{
			DeniedCommands: [][]string{{"printenv"}},
		},
		Harness: "codex",
	})
	if err != nil {
		t.Fatal(err)
	}
	captured, err := os.ReadFile(envCapture)
	if err != nil {
		t.Fatal(err)
	}
	if string(captured) != "CANARY=quiet-canary-value\n" {
		t.Fatalf("captured env = %q", captured)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(args), "quiet-canary-value") {
		t.Fatal("secret value leaked into docker argv")
	}
	if !strings.Contains(string(args), "/conveyor/control") {
		t.Fatalf("deterministic control path missing: %s", args)
	}
	policy, err := os.ReadFile(filepath.Join(control, "policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(policy), "printenv") {
		t.Fatalf("policy was not staged: %s", policy)
	}
	if entries, err := os.ReadDir(filepath.Join(tmp, "secret-stage")); err != nil || len(entries) != 0 {
		t.Fatalf("secret stage was not cleaned: entries=%v err=%v", entries, err)
	}
	if _, err := r.CollectArtifacts(context.Background(), h); err != nil {
		t.Fatal(err)
	}
}

func TestStartJobRejectsNonLocalSecretSet(t *testing.T) {
	r := New()
	r.SecretResolver = staticResolver{"secretref://demo/prod/TOKEN": "secret"}
	r.SecretPolicies = map[string]secrets.SetPolicy{"demo/prod": {LocalEligible: false}}
	_, err := r.StartJob(context.Background(), runner.StartJobSpec{
		SecretStageDir: t.TempDir(),
		SecretRefs:     []string{"secretref://demo/prod/TOKEN"},
	})
	var bootErr *runner.BootError
	if !errors.As(err, &bootErr) || !strings.Contains(bootErr.Diagnostics.ValidationError, "not local_eligible") {
		t.Fatalf("error = %#v, want local_eligible boot diagnostic", err)
	}
}

func TestStartJobReportsMissingAuthAsBootDiagnostics(t *testing.T) {
	tmp := t.TempDir()
	r := New()
	_, err := r.StartJob(context.Background(), runner.StartJobSpec{
		CredentialsDir:     filepath.Join(tmp, "missing"),
		CredentialStageDir: filepath.Join(tmp, "staging"),
		Harness:            "codex",
	})
	var bootErr *runner.BootError
	if !errors.As(err, &bootErr) {
		t.Fatalf("error = %T %v, want *runner.BootError", err, err)
	}
	if !strings.Contains(bootErr.Diagnostics.ValidationError, "auth.json") {
		t.Fatalf("validation error = %q", bootErr.Diagnostics.ValidationError)
	}
}

func writeFakeDocker(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-docker")
	script := `#!/bin/sh
case "$1" in
  run)
    printf '%s\n' "$@" > "$FAKE_DOCKER_ARGS"
    previous=''
    for arg in "$@"; do
      if [ "$previous" = "--env-file" ] && [ -n "$FAKE_DOCKER_ENV_CAPTURE" ]; then
        cp "$arg" "$FAKE_DOCKER_ENV_CAPTURE"
      fi
      previous="$arg"
    done
    printf 'container-id\n'
    ;;
  wait)
    printf '0\n'
    ;;
  rm|kill|pause|unpause|logs)
    ;;
  *)
    exit 2
    ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
