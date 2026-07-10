package localdocker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/runner"
	"github.com/kidus-tiliksew/conveyor/internal/snapshot"
)

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
