// Package dispatch is the Phase 1 orchestration glue: it takes a queued
// task through worktree → sandbox → log stream → artifacts → PR. It is
// the seed of the spec §3.1 orchestrator; the event-sourced state
// machine, retries, stage timeouts, and escalation policy grow here in
// Phase 2.
//
// Phase 1 runs the runner in-process inside conveyord. The spec's
// runner daemon ("conveyor runner start --local" polling the control
// plane) splits out once the runner protocol crosses a network boundary.
package dispatch

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
	"github.com/kidus-tiliksew/conveyor/internal/runner"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

type Dispatcher struct {
	Store  store.Store
	Git    *gitx.Manager
	Runner runner.Runner
	Cfg    *config.Config

	queue chan string
}

func New(st store.Store, git *gitx.Manager, r runner.Runner, cfg *config.Config) *Dispatcher {
	return &Dispatcher{Store: st, Git: git, Runner: r, Cfg: cfg, queue: make(chan string, 64)}
}

// Enqueue schedules a task for dispatch. Safe from HTTP handlers.
func (d *Dispatcher) Enqueue(taskID string) {
	d.queue <- taskID
}

// Run consumes the queue until ctx is cancelled. Tasks run serially in
// Phase 1; parallelism is a routing/budget concern for later phases.
func (d *Dispatcher) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-d.queue:
			if err := d.runTask(ctx, id); err != nil {
				log.Printf("[task %s] failed: %v", id, err)
				_ = d.Store.UpdateTaskState(id, core.TaskParked)
			}
		}
	}
}

func (d *Dispatcher) runTask(ctx context.Context, taskID string) error {
	t, err := d.Store.GetTask(taskID)
	if err != nil {
		return err
	}
	repo, ok := d.Cfg.Repo(t.Repo)
	if !ok {
		return fmt.Errorf("unknown repo %q", t.Repo)
	}
	if err := d.Store.UpdateTaskState(t.ID, core.TaskRunning); err != nil {
		return err
	}
	log.Printf("[task %s] dispatching %q (repo %s, base %s)", t.ID, t.Title, repo.Name, t.BaseBranch)

	wt, err := d.Git.AddWorktree(ctx, repo.URL, repo.Name, t.ID, t.BaseBranch)
	if err != nil {
		return fmt.Errorf("worktree: %w", err)
	}

	control := filepath.Join(d.Cfg.JobsDir, "task-"+t.ID, ".conveyor")
	if err := os.MkdirAll(control, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(control, "prompt.txt"), []byte(buildPrompt(t)), 0o644); err != nil {
		return err
	}

	job := core.Job{
		ID:      t.ID + "-implement-1",
		TaskID:  t.ID,
		Stage:   core.StageImplement,
		Harness: "codex",
		Runner:  d.Runner.Name(),
		// Tier A: containerized local (spec §8.5), recorded per job.
		Confinement: "tierA",
		State:       core.JobBooting,
		StartedAt:   time.Now(),
	}
	if err := d.Store.CreateJob(job); err != nil {
		return err
	}

	bare, err := d.Git.EnsureMirror(ctx, repo.URL)
	if err != nil {
		return err
	}
	handle, err := d.Runner.StartJob(ctx, runner.StartJobSpec{
		JobID:  job.ID,
		TaskID: t.ID,
		Image:  d.Cfg.Image,
		Worktrees: []runner.WorktreeMount{
			// Identical host/sandbox paths; see the TODO on
			// WorktreeMount for the deterministic-path follow-up.
			{HostPath: filepath.Join(d.Cfg.JobsDir, "task-"+t.ID), SandboxPath: filepath.Join(d.Cfg.JobsDir, "task-"+t.ID)},
			{HostPath: bare, SandboxPath: bare},
		},
		Workdir:        wt,
		ControlDir:     control,
		CredentialsDir: d.Cfg.CodexCredentials,
		Harness:        "codex",
		SandboxTTL:     "task",
	})
	if err != nil {
		job.State = core.JobSandboxBootFail
		job.EndedAt = time.Now()
		_ = d.Store.UpdateJob(job)
		return err
	}
	job.State = core.JobRunning
	job.SandboxRef = string(handle)
	_ = d.Store.UpdateJob(job)

	// Phase 1 is "logs only": stream to conveyord's stdout and a job
	// log file in the control dir.
	logs, err := d.Runner.StreamLogs(ctx, handle)
	if err != nil {
		return fmt.Errorf("stream logs: %w", err)
	}
	logFile, err := os.Create(filepath.Join(control, "job.log"))
	if err != nil {
		return err
	}
	for ev := range logs {
		fmt.Fprintln(logFile, ev.Line)
		log.Printf("[task %s] %s", t.ID, ev.Line)
	}
	logFile.Close()

	art, err := d.Runner.CollectArtifacts(ctx, handle)
	if err != nil {
		return fmt.Errorf("collect artifacts: %w", err)
	}
	job.EndedAt = time.Now()
	if art.ExitCode == 0 {
		job.State = core.JobDone
	} else {
		job.State = core.JobFailed
	}
	_ = d.Store.UpdateJob(job)
	if art.ExitCode != 0 {
		return fmt.Errorf("job exited %d (log: %s)", art.ExitCode, filepath.Join(control, "job.log"))
	}

	commits, err := gitx.CommitsAhead(ctx, wt, t.BaseBranch)
	if err != nil {
		return err
	}
	if len(commits) == 0 {
		log.Printf("[task %s] job succeeded but produced no commits — parking for a human", t.ID)
		return d.Store.UpdateTaskState(t.ID, core.TaskParked)
	}
	log.Printf("[task %s] %d commit(s) on %s", t.ID, len(commits), t.Branch)

	if repo.GitHub == "" {
		log.Printf("[task %s] no github slug configured for repo %s — leaving branch unpushed", t.ID, repo.Name)
		return d.Store.UpdateTaskState(t.ID, core.TaskAwaiting)
	}
	prURL, err := github.OpenPR(ctx, wt, repo.GitHub, t.Branch, t.BaseBranch, t.Title, prBody(t))
	if err != nil {
		return fmt.Errorf("open PR: %w", err)
	}
	log.Printf("[task %s] opened %s", t.ID, prURL)
	return d.Store.UpdateTaskState(t.ID, core.TaskAwaiting)
}

// PollGitHub converts conveyor:ready issues into tasks (spec §9),
// deduplicating on task provenance.
func (d *Dispatcher) PollGitHub(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		d.pollOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (d *Dispatcher) pollOnce(ctx context.Context) {
	for _, repo := range d.Cfg.Repos {
		if repo.GitHub == "" {
			continue
		}
		issues, err := github.ListReadyIssues(ctx, repo.GitHub)
		if err != nil {
			log.Printf("poll %s: %v", repo.GitHub, err)
			continue
		}
		for _, is := range issues {
			source := fmt.Sprintf("github:%s#%d", repo.GitHub, is.Number)
			if d.taskExists(source) {
				continue
			}
			id := core.NewTaskID()
			t := core.Task{
				ID:         id,
				Workspace:  d.Cfg.Workspace,
				Source:     source,
				Title:      is.Title,
				Body:       is.Body,
				Level:      core.L2,
				Repo:       repo.Name,
				BaseBranch: repo.Base,
				Branch:     gitx.BranchName(id),
				State:      core.TaskQueued,
				CreatedAt:  time.Now(),
			}
			if err := d.Store.CreateTask(t); err != nil {
				log.Printf("poll %s: create task: %v", repo.GitHub, err)
				continue
			}
			log.Printf("[task %s] created from %s (%q)", id, source, is.Title)
			d.Enqueue(id)
		}
	}
}

func (d *Dispatcher) taskExists(source string) bool {
	tasks, err := d.Store.ListTasks()
	if err != nil {
		return false
	}
	for _, t := range tasks {
		if t.Source == source {
			return true
		}
	}
	return false
}

// buildPrompt is the Phase 1 stand-in for the implement role prompt;
// it moves into the prompt/policy pack (spec §2.2) when packs exist.
func buildPrompt(t core.Task) string {
	var b strings.Builder
	b.WriteString("You are the implementation agent of an automated software factory.\n\n")
	fmt.Fprintf(&b, "# Task %s: %s\n\n", t.ID, t.Title)
	if t.Body != "" {
		b.WriteString(t.Body + "\n\n")
	}
	fmt.Fprintf(&b, `Your working directory is a git worktree on branch %s (based on %s).

Instructions:
- Implement the change described above.
- Run the project's tests or checks where they are quick enough to be practical.
- Commit all your work to the current branch with clear, conventional commit messages.
- Do NOT push, open PRs, or switch branches; the factory handles everything after the commit.
- Do not touch anything outside this worktree.
`, t.Branch, t.BaseBranch)
	return b.String()
}

func prBody(t core.Task) string {
	var b strings.Builder
	if t.Body != "" {
		b.WriteString(t.Body + "\n\n")
	}
	fmt.Fprintf(&b, "---\nConveyor task `%s`", t.ID)
	if t.Source != "" {
		fmt.Fprintf(&b, " · source: %s", t.Source)
	}
	b.WriteString("\n\n🤖 Generated with [Conveyor](https://github.com/kidus-tiliksew/conveyor)")
	return b.String()
}
