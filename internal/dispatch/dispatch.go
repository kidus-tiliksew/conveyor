// Package dispatch takes a durable queued task through isolated checkout →
// sandbox → redacted transcript → artifacts → PR. Postgres mutations
// enqueue River transactionally; conveyor-runner owns execution (spec §3.1,
// §3.2, §17.0).
package dispatch

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/adapter"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
	"github.com/kidus-tiliksew/conveyor/internal/jobartifact"
	"github.com/kidus-tiliksew/conveyor/internal/routing"
	"github.com/kidus-tiliksew/conveyor/internal/runner"
	"github.com/kidus-tiliksew/conveyor/internal/snapshot"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

type Dispatcher struct {
	Store  store.Store
	Git    *gitx.Manager
	Runner runner.Runner
	Router routing.Selector
	Cfg    *config.Config

	queue        chan string
	durableQueue bool
}

func New(st store.Store, git *gitx.Manager, r runner.Runner, cfg *config.Config) *Dispatcher {
	return &Dispatcher{Store: st, Git: git, Runner: r, Cfg: cfg, queue: make(chan string, 64)}
}

// Enqueue schedules a task for dispatch. Safe from HTTP handlers.
func (d *Dispatcher) Enqueue(taskID string) {
	if d.durableQueue {
		return // Postgres Store inserted the River job in the mutation transaction.
	}
	d.queue <- taskID
}

// UseDurableQueue disables the compatibility in-memory channel. It must be
// called during startup before API/poller goroutines begin.
func (d *Dispatcher) UseDurableQueue() { d.durableQueue = true }

// Run consumes the compatibility memory queue until ctx is cancelled.
func (d *Dispatcher) Run(ctx context.Context) {
	if d.durableQueue {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-d.queue:
			if err := d.runTask(ctx, id); err != nil {
				log.Printf("[task %s] failed: %v", id, err)
				_ = d.Store.UpdateTaskState(context.Background(), id, core.TaskParked)
			}
		}
	}
}

func (d *Dispatcher) runTask(ctx context.Context, taskID string) error {
	if d.Router == nil {
		return fmt.Errorf("dispatcher requires an explicit credential router")
	}
	t, err := d.Store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if t.Workspace != d.Cfg.Workspace {
		return fmt.Errorf("task %s belongs to workspace %q, worker serves %q", t.ID, t.Workspace, d.Cfg.Workspace)
	}
	repo, ok := d.Cfg.Repo(t.Repo)
	if !ok {
		return fmt.Errorf("unknown repo %q", t.Repo)
	}
	priorJobs, err := d.Store.ListJobs(ctx, t.ID)
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
	jobID := fmt.Sprintf("%s-implement-%d", t.ID, attempt)
	harness := ""
	modelTier := ""
	budgetUSD := 0.0
	credentialID := ""
	authMode := ""
	credentialDir := ""
	secretRefs := append([]string(nil), repo.SecretRefs...)
	var selection routing.Selection
	var routeOutcome = routing.Outcome{Error: "job did not complete"}
	selection, err = d.Router.Select(ctx, t.ID, jobID, core.StageImplement)
	if err != nil {
		return err
	}
	defer func() {
		completeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := d.Router.Complete(completeCtx, selection, routeOutcome); err != nil {
			log.Printf("[task %s] release credential %s: %v", t.ID, selection.ID, err)
		}
	}()
	harness = selection.Harness
	modelTier = selection.ModelTier
	budgetUSD = selection.BudgetUSD
	credentialID = selection.ID
	authMode = selection.Kind
	if strings.HasPrefix(selection.Ref, "secretref://") {
		secretRefs = append(secretRefs, selection.Ref)
	} else {
		credentialDir = selection.Ref
	}
	// Keep the task queued until capacity is secured. A River snooze retains
	// the current queue row; transitioning back to queued here would insert a
	// duplicate dispatch job in the durable store.
	if err := d.Store.UpdateTaskState(ctx, t.ID, core.TaskRunning); err != nil {
		return err
	}
	log.Printf("[task %s] dispatching %q (repo %s, base %s)", t.ID, t.Title, repo.Name, t.BaseBranch)

	wt, err := d.Git.AddWorktree(ctx, repo.URL, repo.Name, t.ID, t.BaseBranch)
	if err != nil {
		return fmt.Errorf("task checkout: %w", err)
	}

	control := filepath.Join(d.Cfg.JobsDir, "task-"+t.ID, ".conveyor")
	if err := os.MkdirAll(control, 0o755); err != nil {
		return err
	}

	prompt := buildPrompt(t)
	interventions, err := d.Store.ListInterventions(ctx, t.ID)
	if err != nil {
		return err
	}
	redirectFeedback := redirectComments(interventions)
	feedbackDelivered := false
	// A prior attempt's handoff snapshot briefs the successor
	// (spec §8.3): the persistent worktree carries the code state, the
	// snapshot carries the reasoning state. Redirect comments join it
	// alongside structured review redirects.
	if predecessor != nil {
		handoffPath, pathErr := snapshot.Path(control, predecessor.ID)
		if pathErr != nil {
			return pathErr
		}
		if h, loadErr := snapshot.Load(handoffPath); loadErr == nil {
			if h.TaskID == t.ID && h.JobID == predecessor.ID {
				prompt += "\n\n" + h.OpeningContext(redirectFeedback)
				feedbackDelivered = redirectFeedback != ""
				log.Printf("[task %s] briefing successor with handoff from job %s", t.ID, h.JobID)
			} else {
				log.Printf("[task %s] ignoring handoff with mismatched provenance task=%q job=%q", t.ID, h.TaskID, h.JobID)
			}
		} else if !os.IsNotExist(loadErr) {
			log.Printf("[task %s] ignoring unreadable handoff for job %s: %v", t.ID, predecessor.ID, loadErr)
		}
	}
	if redirectFeedback != "" && !feedbackDelivered {
		prompt += "\n\nHuman reviewer feedback to address:\n" + redirectFeedback
	}
	if err := os.WriteFile(filepath.Join(control, "prompt.txt"), []byte(prompt), 0o644); err != nil {
		return err
	}
	job := core.Job{
		ID:           jobID,
		TaskID:       t.ID,
		Stage:        core.StageImplement,
		Harness:      harness,
		ModelTier:    modelTier,
		CredentialID: credentialID,
		AuthMode:     authMode,
		BudgetUSD:    budgetUSD,
		Runner:       d.Runner.Name(),
		// Tier A: containerized local (spec §8.5), recorded per job.
		Confinement: "tierA",
		State:       core.JobBooting,
		StartedAt:   time.Now(),
	}
	if err := d.Store.CreateJob(ctx, job); err != nil {
		return err
	}
	jobTerminal := false
	defer func() {
		if jobTerminal {
			return
		}
		job.State = core.JobFailed
		job.EndedAt = time.Now()
		if err := d.Store.UpdateJob(context.Background(), job); err != nil {
			log.Printf("[task %s] record failed job %s: %v", t.ID, job.ID, err)
		}
	}()

	sandboxWorkdir := gitx.SandboxPath(t.ID, repo.Name)
	handle, err := d.Runner.StartJob(ctx, runner.StartJobSpec{
		JobID:  job.ID,
		TaskID: t.ID,
		Image:  d.Cfg.Image,
		Worktrees: []runner.WorktreeMount{
			{HostPath: wt, SandboxPath: sandboxWorkdir},
		},
		Workdir:            sandboxWorkdir,
		ControlDir:         control,
		ControlPath:        "/conveyor/control",
		CredentialsDir:     credentialDir,
		CredentialStageDir: filepath.Join(d.Cfg.CacheDir, "credentials"),
		SecretStageDir:     filepath.Join(d.Cfg.CacheDir, "secrets"),
		SecretRefs:         secretRefs,
		BudgetUSD:          budgetUSD,
		Policy:             repo.ToolPolicy,
		Harness:            harness,
		// The current shim is one-shot; persistent task checkouts provide
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
		if updateErr := d.Store.UpdateJob(ctx, job); updateErr != nil {
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
	if err := d.Store.UpdateJob(ctx, job); err != nil {
		return err
	}

	// Docker's live stream is an operator convenience. The shim-authored,
	// redacted attempt-scoped events JSONL artifact is the authoritative transcript in Phase 2
	// (spec §10.3); a transient stream failure must not discard a successful
	// job whose artifact is complete.
	jobLogPath, err := jobartifact.LogPath(control, job.ID)
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(jobLogPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	logs, err := d.Runner.StreamLogs(ctx, handle)
	if err != nil {
		logs = nil
	}
	var streamErr = err
	var writeErr error
	if logs != nil {
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
	}
	if err := logFile.Close(); err != nil && writeErr == nil {
		writeErr = fmt.Errorf("close job log: %w", err)
	}
	art, err := d.Runner.CollectArtifacts(ctx, handle)
	runnerFinalized = true
	if err != nil {
		return fmt.Errorf("collect artifacts: %w", err)
	}
	if art.EventLog == "" {
		return fmt.Errorf("job produced no authoritative attempt event artifact")
	}
	summary, err := inspectTranscript(art.EventLog)
	if err != nil {
		return fmt.Errorf("validate transcript: %w", err)
	}
	if summary.rateLimited && art.ExitCode != 0 {
		routeOutcome.RateLimited = true
		routeOutcome.Error = "harness reported rate limiting"
	}
	job.TokensIn = summary.tokensIn
	job.TokensOut = summary.tokensOut
	job.CostUSD = summary.costUSD
	transcriptURI := (&url.URL{Scheme: "file", Path: art.EventLog}).String()
	if err := d.Store.UpsertTranscript(ctx, core.Transcript{
		JobID: job.ID, URI: transcriptURI, RedactionStats: summary.redactions, CreatedAt: time.Now().UTC(),
	}); err != nil {
		return fmt.Errorf("persist transcript metadata: %w", err)
	}
	if summary.agentSummary != "" {
		_ = d.Store.AppendEvent(ctx, core.Event{
			TaskID: t.ID, JobID: job.ID, Kind: "job.summary",
			Payload: core.JSONPayload(map[string]string{"summary": summary.agentSummary}),
		})
	}
	if streamErr != nil || writeErr != nil {
		problem := streamErr
		if problem == nil {
			problem = writeErr
		}
		log.Printf("[task %s] live log stream degraded; authoritative artifact retained: %v", t.ID, problem)
		_ = d.Store.AppendEvent(ctx, core.Event{
			TaskID: t.ID, JobID: job.ID, Kind: "job.log_stream_degraded",
			Payload: json.RawMessage(`{"authoritative_artifact":true}`),
		})
	}
	job.EndedAt = time.Now()
	if art.ExitCode == 0 {
		job.State = core.JobDone
	} else {
		job.State = core.JobFailed
	}
	if err := d.Store.UpdateJob(ctx, job); err != nil {
		return err
	}
	jobTerminal = true
	if art.ExitCode != 0 {
		return fmt.Errorf("job exited %d (log: %s)", art.ExitCode, jobLogPath)
	}
	routeOutcome = routing.Outcome{}

	commits, err := gitx.CommitsAhead(ctx, wt, t.BaseBranch)
	if err != nil {
		return err
	}
	if len(commits) == 0 {
		log.Printf("[task %s] job succeeded but produced no commits — parking for a human", t.ID)
		return d.Store.UpdateTaskState(ctx, t.ID, core.TaskParked)
	}
	log.Printf("[task %s] %d commit(s) on %s", t.ID, len(commits), t.Branch)

	if repo.GitHub == "" {
		log.Printf("[task %s] no github slug configured for repo %s — leaving branch unpushed", t.ID, repo.Name)
		return d.Store.UpdateTaskState(ctx, t.ID, core.TaskAwaiting)
	}
	prURL, err := github.OpenPR(ctx, wt, repo.GitHub, t.Branch, t.BaseBranch, t.Title, prBody(t))
	if err != nil {
		return fmt.Errorf("open PR: %w", err)
	}
	log.Printf("[task %s] opened %s", t.ID, prURL)
	return d.Store.UpdateTaskState(ctx, t.ID, core.TaskAwaiting)
}

type transcriptSummary struct {
	redactions   core.RedactionStats
	tokensIn     int64
	tokensOut    int64
	costUSD      float64
	rateLimited  bool
	agentSummary string
}

func inspectTranscript(path string) (transcriptSummary, error) {
	file, err := os.Open(path)
	if err != nil {
		return transcriptSummary{}, err
	}
	defer file.Close()

	var summary transcriptSummary
	phaseCost := make(map[string]float64)
	terminal := false
	lines := 0
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		lines++
		var event adapter.Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return transcriptSummary{}, fmt.Errorf("line %d: %w", lines, err)
		}
		if event.Usage != nil {
			summary.tokensIn += event.Usage.In
			summary.tokensOut += event.Usage.Out
		}
		if event.CostUSD > phaseCost[event.Phase] {
			phaseCost[event.Phase] = event.CostUSD
		}
		if event.Kind == adapter.EventAssistantText && event.Phase == adapter.PhaseMain && strings.TrimSpace(event.Text) != "" {
			summary.agentSummary = truncateRunes(strings.TrimSpace(event.Text), 2000)
		}
		switch event.Kind {
		case adapter.EventDone:
			terminal = true
			summary.rateLimited = false
		case adapter.EventError:
			terminal = true
			summary.rateLimited = isRateLimitError(event.Err)
		case adapter.EventRedaction:
			var stats core.RedactionStats
			if err := json.Unmarshal(event.Payload, &stats); err != nil {
				return transcriptSummary{}, fmt.Errorf("line %d redaction stats: %w", lines, err)
			}
			if stats.Exact < 0 || stats.Encoded < 0 || stats.Pattern < 0 || stats.Entropy < 0 {
				return transcriptSummary{}, fmt.Errorf("line %d has negative redaction count", lines)
			}
			summary.redactions.Exact += stats.Exact
			summary.redactions.Encoded += stats.Encoded
			summary.redactions.Pattern += stats.Pattern
			summary.redactions.Entropy += stats.Entropy
		}
	}
	if err := scanner.Err(); err != nil {
		return transcriptSummary{}, err
	}
	if lines == 0 {
		return transcriptSummary{}, fmt.Errorf("empty event artifact")
	}
	if !terminal {
		return transcriptSummary{}, fmt.Errorf("event artifact has no terminal event")
	}
	mainCost := phaseCost[adapter.PhaseMain]
	resumeCost := phaseCost[adapter.PhaseHandoffResume] - mainCost
	if resumeCost < 0 {
		resumeCost = 0
	}
	summary.costUSD = mainCost + resumeCost + phaseCost[adapter.PhaseHandoffFallback]
	return summary, nil
}

func isRateLimitError(message string) bool {
	message = strings.ToLower(message)
	return strings.Contains(message, "rate limit") || strings.Contains(message, "rate_limit") ||
		strings.Contains(message, "usage limit") || strings.Contains(message, "status 429")
}

func redirectComments(interventions []core.Intervention) string {
	var comments []string
	for _, intervention := range interventions {
		comment := strings.TrimSpace(intervention.Comment)
		if intervention.Action != core.InterventionRedirect || comment == "" {
			continue
		}
		comments = append(comments, comment)
	}
	return strings.Join(comments, "\n\n")
}

func truncateRunes(value string, max int) string {
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max]) + "…"
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
	d.recoverGitHubClaims(ctx)
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
			existing, exists := d.taskBySource(ctx, source)
			if exists && existing.State != core.TaskQueued && existing.State != core.TaskClaiming {
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
					State:      core.TaskClaiming,
					CreatedAt:  time.Now(),
				}
				if err := d.Store.CreateTask(ctx, existing); err != nil {
					log.Printf("poll %s: create task: %v", repo.GitHub, err)
					continue
				}
			}
			// The durable claiming state bridges Postgres and GitHub. A crash
			// after the label transition is recovered idempotently on the next
			// poll before River receives the queued transition.
			if err := github.MarkIssueDispatched(ctx, repo.GitHub, is.Number, id); err != nil {
				// Leave the task claiming. The ready label remains, so the next
				// poll retries the GitHub transition without running the task.
				log.Printf("poll %s: claim issue #%d: %v", repo.GitHub, is.Number, err)
				continue
			}
			if existing.State != core.TaskQueued {
				if err := d.Store.UpdateTaskState(ctx, id, core.TaskQueued); err != nil {
					log.Printf("poll %s: finalize claim for issue #%d: %v", repo.GitHub, is.Number, err)
					continue
				}
			}
			log.Printf("[task %s] created from %s (%q)", id, source, is.Title)
			d.Enqueue(id)
		}
	}
}

func (d *Dispatcher) recoverGitHubClaims(ctx context.Context) {
	tasks, err := d.Store.ListTasks(ctx)
	if err != nil {
		log.Printf("recover GitHub claims: %v", err)
		return
	}
	for _, task := range tasks {
		if task.State != core.TaskClaiming {
			continue
		}
		slug, number, ok := githubSource(task.Source)
		if !ok {
			log.Printf("[task %s] claiming task has invalid GitHub source %q", task.ID, task.Source)
			continue
		}
		if err := github.MarkIssueDispatched(ctx, slug, number, task.ID); err != nil {
			log.Printf("[task %s] recover GitHub claim: %v", task.ID, err)
			continue
		}
		if err := d.Store.UpdateTaskState(ctx, task.ID, core.TaskQueued); err != nil {
			log.Printf("[task %s] finalize recovered claim: %v", task.ID, err)
			continue
		}
		d.Enqueue(task.ID)
	}
}

func githubSource(source string) (string, int, bool) {
	rest, ok := strings.CutPrefix(source, "github:")
	if !ok {
		return "", 0, false
	}
	separator := strings.LastIndexByte(rest, '#')
	if separator <= 0 {
		return "", 0, false
	}
	number, err := strconv.Atoi(rest[separator+1:])
	if err != nil || number <= 0 {
		return "", 0, false
	}
	return rest[:separator], number, true
}

func (d *Dispatcher) taskBySource(ctx context.Context, source string) (core.Task, bool) {
	tasks, err := d.Store.ListTasks(ctx)
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
