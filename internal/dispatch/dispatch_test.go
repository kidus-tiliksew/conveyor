package dispatch

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
	"github.com/kidus-tiliksew/conveyor/internal/runner"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type fakeRunner struct {
	startErr  error
	streamErr error
	specs     []runner.StartJobSpec
}

func (f *fakeRunner) Name() string { return "fake" }

func (f *fakeRunner) StartJob(_ context.Context, spec runner.StartJobSpec) (runner.JobHandle, error) {
	f.specs = append(f.specs, spec)
	if f.startErr != nil {
		return "", f.startErr
	}
	return runner.JobHandle(spec.JobID), nil
}

func (f *fakeRunner) StreamLogs(_ context.Context, _ runner.JobHandle) (<-chan runner.LogEvent, error) {
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	ch := make(chan runner.LogEvent)
	close(ch)
	return ch, nil
}

func (f *fakeRunner) Signal(context.Context, runner.JobHandle, runner.Signal) error { return nil }

func (f *fakeRunner) CollectArtifacts(context.Context, runner.JobHandle) (runner.Artifacts, error) {
	return runner.Artifacts{ExitCode: 0}, nil
}

func TestRunTaskPersistsBootDiagnostics(t *testing.T) {
	f := &fakeRunner{startErr: &runner.BootError{
		Diagnostics: runner.BootDiagnostics{RuntimeError: "docker unavailable"},
		Err:         errors.New("exit status 1"),
	}}
	d, st, task := testDispatcher(t, f)
	if err := d.runTask(context.Background(), task.ID); err == nil {
		t.Fatal("expected boot failure")
	}
	jobs, err := st.ListJobs(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].State != core.JobSandboxBootFail {
		t.Fatalf("jobs = %+v", jobs)
	}
	if jobs[0].BootDiagnostics == nil || jobs[0].BootDiagnostics.RuntimeError != "docker unavailable" {
		t.Fatalf("boot diagnostics = %+v", jobs[0].BootDiagnostics)
	}
	if jobs[0].EndedAt.IsZero() {
		t.Fatal("boot-failed job has no end time")
	}
}

func TestRunTaskMarksPostStartFailureTerminal(t *testing.T) {
	f := &fakeRunner{streamErr: errors.New("logs unavailable")}
	d, st, task := testDispatcher(t, f)
	if err := d.runTask(context.Background(), task.ID); err == nil {
		t.Fatal("expected stream failure")
	}
	jobs, err := st.ListJobs(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].State != core.JobFailed {
		t.Fatalf("jobs = %+v", jobs)
	}
	if jobs[0].EndedAt.IsZero() {
		t.Fatal("failed job has no end time")
	}
}

func TestRunTaskUsesUniqueAttemptIDs(t *testing.T) {
	f := &fakeRunner{}
	d, st, task := testDispatcher(t, f)
	if err := d.runTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.runTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	if len(f.specs) != 2 {
		t.Fatalf("start specs = %d, want 2", len(f.specs))
	}
	if f.specs[0].JobID != task.ID+"-implement-1" || f.specs[1].JobID != task.ID+"-implement-2" {
		t.Fatalf("job IDs = %q, %q", f.specs[0].JobID, f.specs[1].JobID)
	}
	jobs, err := st.ListJobs(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("jobs = %d, want 2", len(jobs))
	}
}

func testDispatcher(t *testing.T, r runner.Runner) (*Dispatcher, store.Store, core.Task) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin")
	mustRun(t, "", "git", "init", "-b", "main", origin)
	mustRun(t, origin, "git", "config", "user.email", "test@example.com")
	mustRun(t, origin, "git", "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(origin, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, origin, "git", "add", ".")
	mustRun(t, origin, "git", "commit", "-m", "init")

	cfg := &config.Config{
		Workspace: "test",
		Image:     "conveyor:test",
		CacheDir:  filepath.Join(tmp, "cache"),
		JobsDir:   filepath.Join(tmp, "jobs"),
		Repos: []config.Repo{{
			Name: "api",
			URL:  "file://" + origin,
			Base: "main",
		}},
	}
	st := store.NewMemory()
	task := core.Task{
		ID:         "test-task",
		Workspace:  "test",
		Source:     "test",
		Title:      "test task",
		Repo:       "api",
		BaseBranch: "main",
		Branch:     gitx.BranchName("test-task"),
		State:      core.TaskQueued,
		CreatedAt:  time.Now(),
	}
	if err := st.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	d := New(st, gitx.NewManager(cfg.CacheDir, cfg.JobsDir), r, cfg)
	return d, st, task
}

func mustRun(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
}
