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
	"errors"
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
	"github.com/kidus-tiliksew/conveyor/internal/snapshot"
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
	priorJobs, err := d.Store.ListJobs(t.ID)
	if err != nil {
		return err
	}
	attempt := 1
	var predecessor *core.Job
	for i := range priorJobs {
		prior := priorJobs[i]
		if prior.Stage != core.StageImplement {
			continue
		}
		attempt++
		if predecessor == nil || !prior.StartedAt.Before(predecessor.StartedAt) {
			candidate := prior
			predecessor = &candidate
		}
	}

	prompt := buildPrompt(t)
	// A prior attempt's handoff snapshot briefs the successor
	// (spec §8.3): the persistent worktree carries the code state, the
	// snapshot carries the reasoning state. Redirect comments join it
	// here when the review queue lands in Phase 2.
	if predecessor != nil {
		handoffPath, pathErr := snapshot.Path(control, predecessor.ID)
		if pathErr != nil {
			return pathErr
		}
		if h, loadErr := snapshot.Load(handoffPath); loadErr == nil {
			if h.TaskID == t.ID && h.JobID == predecessor.ID {
				prompt += "\n\n" + h.OpeningContext("")
				log.Printf("[task %s] briefing successor with handoff from job %s", t.ID, h.JobID)
			} else {
				log.Printf("[task %s] ignoring handoff with mismatched provenance task=%q job=%q", t.ID, h.TaskID, h.JobID)
			}
		} else if !os.IsNotExist(loadErr) {
			log.Printf("[task %s] ignoring unreadable handoff for job %s: %v", t.ID, predecessor.ID, loadErr)
		}
	}
	if err := os.WriteFile(filepath.Join(control, "prompt.txt"), []byte(prompt), 0o644); err != nil {
		return err
	}
	job := core.Job{
		ID:      fmt.Sprintf("%s-implement-%d", t.ID, attempt),
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
	jobTerminal := false
	defer func() {
		if jobTerminal {
			return
		}
		job.State = core.JobFailed
		job.EndedAt = time.Now()
		if err := d.Store.UpdateJob(job); err != nil {
			log.Printf("[task %s] record failed job %s: %v", t.ID, job.ID, err)
		}
	}()

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
		Workdir:            wt,
		ControlDir:         control,
		CredentialsDir:     d.Cfg.CodexCredentials,
		CredentialStageDir: filepath.Join(d.Cfg.CacheDir, "credentials"),
		Harness:            "codex",
		// The current shim is one-shot; persistent worktrees provide
		// continuity while task-TTL container reuse is implemented.
		SandboxTTL: "job",
	})
	if err != nil {
		job.State = core.JobSandboxBootFail
		job.EndedAt = time.Now()
		var bootErr *runner.BootError
		if errors.As(err, &bootErr) {
			diagnostics := bootErr.Diagnostics
			job.BootDiagnostics = &diagnostics
		} else {
			job.BootDiagnostics = &core.BootDiagnostics{RuntimeError: err.Error()}
		}
		if updateErr := d.Store.UpdateJob(job); updateErr != nil {
			return fmt.Errorf("%w; record boot failure: %v", err, updateErr)
		}
		jobTerminal = true
		return err
	}
	runnerFinalized := false
	defer func() {
		if runnerFinalized {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = d.Runner.Signal(cleanupCtx, handle, runner.SignalKill)
		_, _ = d.Runner.CollectArtifacts(cleanupCtx, handle)
	}()
	job.State = core.JobRunning
	job.SandboxRef = string(handle)
	if err := d.Store.UpdateJob(job); err != nil {
		return err
	}

	// Phase 1 is "logs only": stream to conveyord's stdout and a job
	// log file in the control dir.
	logFile, err := os.Create(filepath.Join(control, "job.log"))
	if err != nil {
		return err
	}
	logs, err := d.Runner.StreamLogs(ctx, handle)
	if err != nil {
		_ = logFile.Close()
		return fmt.Errorf("stream logs: %w", err)
	}
	// A stream failure fails the job even if the container succeeded:
	// in a logs-only phase an incomplete transcript is a corrupt audit
	// record, so we fail closed. Commits survive in the worktree for
	// re-dispatch. See docs/known-limitations.md #2.
	var streamErr error
	var writeErr error
	for ev := range logs {
		if ev.Err != "" {
			streamErr = fmt.Errorf("runner log stream: %s", ev.Err)
			continue
		}
		if _, err := fmt.Fprintln(logFile, ev.Line); err != nil && writeErr == nil {
			writeErr = fmt.Errorf("write job log: %w", err)
		}
		log.Printf("[task %s] %s", t.ID, ev.Line)
	}
	if err := logFile.Close(); err != nil && writeErr == nil {
		writeErr = fmt.Errorf("close job log: %w", err)
	}
	if streamErr != nil {
		return streamErr
	}
	if writeErr != nil {
		return writeErr
	}

	art, err := d.Runner.CollectArtifacts(ctx, handle)
	runnerFinalized = true
	if err != nil {
		return fmt.Errorf("collect artifacts: %w", err)
	}
	job.EndedAt = time.Now()
	if art.ExitCode == 0 {
		job.State = core.JobDone
	} else {
		job.State = core.JobFailed
	}
	if err := d.Store.UpdateJob(job); err != nil {
		return err
	}
	jobTerminal = true
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
			existing, exists := d.taskBySource(source)
			if exists && existing.State != core.TaskQueued {
				continue
			}

			id := existing.ID
			if !exists {
				id = core.NewTaskID()
				existing = core.Task{
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
				if err := d.Store.CreateTask(existing); err != nil {
					log.Printf("poll %s: create task: %v", repo.GitHub, err)
					continue
				}
			}
			// Claim before enqueue: replaying an unclaimed issue after a
			// restart would dispatch a duplicate agent run, which is the
			// costlier failure. The price is that a crash between this
			// claim and the PR orphans the issue (task state is
			// in-memory until Phase 2); recovery is re-adding the
			// conveyor:ready label. See docs/known-limitations.md #1.
			if err := github.MarkIssueDispatched(ctx, repo.GitHub, is.Number, id); err != nil {
				// Leave the task queued. The ready label remains, so the next
				// poll retries the durable GitHub claim before dispatching.
				log.Printf("poll %s: claim issue #%d: %v", repo.GitHub, is.Number, err)
				continue
			}
			log.Printf("[task %s] created from %s (%q)", id, source, is.Title)
			d.Enqueue(id)
		}
	}
}

func (d *Dispatcher) taskBySource(source string) (core.Task, bool) {
	tasks, err := d.Store.ListTasks()
	if err != nil {
		return core.Task{}, false
	}
	for _, t := range tasks {
		if t.Source == source {
			return t, true
		}
	}
	return core.Task{}, false
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
