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
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
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
	Pack   *pack.Bundle

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
				recovery := core.Stage("")
				if task, getErr := d.Store.GetTask(context.Background(), id); getErr == nil {
					recovery = task.NextStage
				}
				_ = d.Store.SetTaskTransition(context.Background(), id, core.TaskParked, "", recovery)
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
	stage, proceed := nextStage(t)
	if !proceed {
		return nil
	}
	image := repo.Image
	if image == "" {
		image = d.Cfg.Image
	}
	attempt := 1
	var predecessor *core.Job
	for i := range priorJobs {
		prior := priorJobs[i]
		if prior.Stage != stage {
			continue
		}
		attempt++
		if predecessor == nil || !prior.StartedAt.Before(predecessor.StartedAt) {
			candidate := prior
			predecessor = &candidate
		}
	}
	jobID := fmt.Sprintf("%s-%s-%d", t.ID, stage, attempt)
	harness := ""
	modelTier := ""
	budgetUSD := 0.0
	credentialID := ""
	authMode := ""
	credentialDir := ""
	secretRefs := append([]string(nil), repo.SecretRefs...)
	var selection routing.Selection
	var routeOutcome = routing.Outcome{Error: "job did not complete"}
	excludeHarness := ""
	if stage == core.StageReview {
		excludeHarness = latestHarness(priorJobs, core.StageImplement)
	}
	selection, err = d.Router.Select(ctx, t.ID, jobID, stage, excludeHarness)
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

	prompt, err := d.buildStagePrompt(ctx, stage, t, wt)
	if err != nil {
		return err
	}
	toolPolicy := repo.ToolPolicy
	if t.Level != "" {
		if d.Pack == nil {
			return fmt.Errorf("Phase 3 pack is not loaded")
		}
		toolPolicy = d.Pack.Policy(stage, repo.ToolPolicy)
	}
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
	if predecessor != nil && stage == core.StageImplement {
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
		Stage:        stage,
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
		Image:  image,
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
		Policy:             toolPolicy,
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
	timeout := stageTimeout(d.Cfg.Routing, stage)
	jobCtx, cancelJob := context.WithTimeout(ctx, timeout)
	defer cancelJob()
	logs, err := d.Runner.StreamLogs(jobCtx, handle)
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
	if errors.Is(jobCtx.Err(), context.DeadlineExceeded) {
		killCtx, killCancel := context.WithTimeout(context.Background(), 15*time.Second)
		_ = d.Runner.Signal(killCtx, handle, runner.SignalKill)
		killCancel()
		_ = d.Store.AppendEvent(ctx, core.Event{TaskID: t.ID, JobID: job.ID, Kind: "job.timeout", Payload: core.JSONPayload(map[string]string{"timeout": timeout.String()})})
	}
	if err := logFile.Close(); err != nil && writeErr == nil {
		writeErr = fmt.Errorf("close job log: %w", err)
	}
	art, err := d.Runner.CollectArtifacts(ctx, handle)
	runnerFinalized = true
	if err != nil {
		return fmt.Errorf("collect artifacts: %w", err)
	}
	timedOut := errors.Is(jobCtx.Err(), context.DeadlineExceeded)
	var summary transcriptSummary
	if art.EventLog == "" {
		if !timedOut {
			return fmt.Errorf("job produced no authoritative attempt event artifact")
		}
	} else {
		summary, err = inspectTranscript(art.EventLog)
		if err != nil && !timedOut {
			return fmt.Errorf("validate transcript: %w", err)
		}
	}
	if summary.rateLimited && art.ExitCode != 0 {
		routeOutcome.RateLimited = true
		routeOutcome.Error = "harness reported rate limiting"
	}
	job.TokensIn = summary.tokensIn
	job.TokensOut = summary.tokensOut
	job.CostUSD = summary.costUSD
	if art.EventLog != "" {
		transcriptURI := (&url.URL{Scheme: "file", Path: art.EventLog}).String()
		if err := d.Store.UpsertTranscript(ctx, core.Transcript{
			JobID: job.ID, URI: transcriptURI, RedactionStats: summary.redactions, CreatedAt: time.Now().UTC(),
		}); err != nil {
			return fmt.Errorf("persist transcript metadata: %w", err)
		}
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
	if timedOut {
		job.State = core.JobPaused
	} else if budgetUSD > 0 && summary.costUSD >= budgetUSD {
		job.State = core.JobPaused
		_ = d.Store.AppendEvent(ctx, core.Event{TaskID: t.ID, JobID: job.ID, Kind: "job.budget_exhausted", Payload: core.JSONPayload(map[string]float64{"budget_usd": budgetUSD, "cost_usd": summary.costUSD})})
	} else if art.ExitCode == 0 {
		job.State = core.JobDone
	} else {
		job.State = core.JobFailed
	}
	if err := d.Store.UpdateJob(ctx, job); err != nil {
		return err
	}
	jobTerminal = true
	if job.State == core.JobPaused {
		if timedOut {
			routeOutcome.Error = "job timed out"
		} else {
			routeOutcome.Error = "job budget exhausted"
		}
		return d.transition(ctx, t.ID, core.TaskAwaiting, "", stage)
	}
	if art.ExitCode != 0 {
		return fmt.Errorf("job exited %d (log: %s)", art.ExitCode, jobLogPath)
	}
	routeOutcome = routing.Outcome{}

	return d.completeStage(ctx, t, repo, wt, job, summary.agentOutput)
}

type transcriptSummary struct {
	redactions   core.RedactionStats
	tokensIn     int64
	tokensOut    int64
	costUSD      float64
	rateLimited  bool
	agentSummary string
	agentOutput  string
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
			// Harnesses may emit the same final answer once as an assistant
			// message and again in their terminal result. The last normalized
			// assistant text is authoritative; concatenating duplicates specs
			// and machine-owned fenced blocks.
			summary.agentOutput = event.Text
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

func stageTimeout(routingConfig config.Routing, stage core.Stage) time.Duration {
	if route, ok := routingConfig.Stages[string(stage)]; ok && route.Timeout > 0 {
		return route.Timeout
	}
	return config.DefaultStageTimeout
}

func latestHarness(jobs []core.Job, stage core.Stage) string {
	for i := len(jobs) - 1; i >= 0; i-- {
		if jobs[i].Stage == stage {
			return jobs[i].Harness
		}
	}
	return ""
}

func nextStage(task core.Task) (core.Stage, bool) {
	return task.NextStage, task.NextStage != ""
}

func (d *Dispatcher) buildStagePrompt(ctx context.Context, stage core.Stage, task core.Task, worktree string) (string, error) {
	if task.Level == "" && stage == core.StageImplement {
		return buildPrompt(task), nil
	}
	if d.Pack == nil {
		return "", fmt.Errorf("Phase 3 pack is not loaded")
	}
	role, err := d.Pack.Role(stage)
	if err != nil {
		return "", err
	}
	var prompt strings.Builder
	prompt.WriteString(role)
	fmt.Fprintf(&prompt, "\n\n# Task %s: %s\n\n%s\n\nBranch: %s (base %s).\n", task.ID, task.Title, task.Body, task.Branch, task.BaseBranch)
	if stage == core.StageImplement || stage == core.StageReview {
		spec, exists, err := d.Store.GetLatestSpecVersion(ctx, task.ID)
		if err != nil {
			return "", err
		}
		if exists && spec.Approved {
			fmt.Fprintf(&prompt, "\n# Approved specification v%d\n\n%s\n", spec.Version, spec.Content)
		}
	}
	if stage == core.StageReview {
		diff, err := gitx.DiffAgainstBase(ctx, worktree, task.BaseBranch)
		if err != nil {
			return "", err
		}
		prompt.WriteString("\n# Branch diff\n\n```diff\n" + diff + "\n```\n")
	}
	return prompt.String(), nil
}

func (d *Dispatcher) completeStage(ctx context.Context, task core.Task, repo config.Repo, worktree string, job core.Job, output string) error {
	invalid := func(err error) error {
		kind := string(job.Stage) + ".output_invalid"
		if appendErr := d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, JobID: job.ID, Kind: kind, Payload: core.JSONPayload(map[string]string{"error": err.Error()})}); appendErr != nil {
			return appendErr
		}
		count, countErr := d.Store.CountEvents(ctx, task.ID, kind)
		if countErr != nil {
			return countErr
		}
		if count >= d.Cfg.MaxBounces {
			_ = d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, JobID: job.ID, Kind: "pipeline.bounce_limit", Payload: core.JSONPayload(map[string]any{"stage": job.Stage, "count": count})})
			return d.transition(ctx, task.ID, core.TaskAwaiting, "", job.Stage)
		}
		return d.transition(ctx, task.ID, core.TaskQueued, job.Stage, "")
	}

	switch job.Stage {
	case core.StageTriage:
		result, err := pipeline.ParseTriage(output)
		if err != nil {
			return invalid(err)
		}
		if err := d.Store.UpdateTaskClassification(ctx, task.ID, result.Class); err != nil {
			return err
		}
		if err := d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, JobID: job.ID, Kind: "triage.completed", Payload: core.JSONPayload(result)}); err != nil {
			return err
		}
		if task.Level == core.L3 || result.Route == "human" {
			return d.transition(ctx, task.ID, core.TaskAwaiting, "", core.StageTriage)
		}
		if result.Route == "parked" {
			return d.transition(ctx, task.ID, core.TaskParked, "", core.StageTriage)
		}
		next := core.StageImplement
		if task.Level == core.L2 || result.Route == "spec" {
			next = core.StageSpec
		}
		return d.transition(ctx, task.ID, core.TaskQueued, next, "")
	case core.StageSpec:
		parsed, err := pipeline.ParseSpec(output)
		if err != nil {
			return invalid(err)
		}
		version, err := d.Store.CreateSpecVersion(ctx, core.SpecVersion{
			TaskID: task.ID, Content: parsed.Markdown, AcceptanceCount: len(parsed.Acceptance),
			Acceptance: core.JSONPayload(parsed.Acceptance), Decomposition: core.JSONPayload(parsed.Decomposition),
		})
		if err != nil {
			return err
		}
		if task.Level == core.L2 {
			return d.transition(ctx, task.ID, core.TaskAwaiting, "", core.StageImplement)
		}
		if err := d.Store.ApproveSpecVersion(ctx, task.ID, version.Version); err != nil {
			return err
		}
		return d.transition(ctx, task.ID, core.TaskQueued, core.StageImplement, "")
	case core.StageImplement:
		commits, err := gitx.CommitsAhead(ctx, worktree, task.BaseBranch)
		if err != nil {
			return err
		}
		if len(commits) == 0 {
			return d.transition(ctx, task.ID, core.TaskParked, "", core.StageImplement)
		}
		if task.Level == "" {
			if repo.GitHub != "" {
				if _, err := github.OpenPR(ctx, worktree, repo.GitHub, task.Branch, task.BaseBranch, task.Title, prBody(task)); err != nil {
					return fmt.Errorf("open PR: %w", err)
				}
			}
			return d.transition(ctx, task.ID, core.TaskAwaiting, core.StageImplement, core.StageImplement)
		}
		return d.transition(ctx, task.ID, core.TaskQueued, core.StageReview, "")
	case core.StageReview:
		result, err := pipeline.ParseReview(output)
		if err != nil {
			return invalid(err)
		}
		if err := d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, JobID: job.ID, Kind: "review.completed", Payload: core.JSONPayload(result)}); err != nil {
			return err
		}
		if result.Verdict == "changes_requested" {
			priorBounces, err := d.Store.CountEvents(ctx, task.ID, "pipeline.bounced")
			if err != nil {
				return err
			}
			bounces := priorBounces + 1
			_ = d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, JobID: job.ID, Kind: "pipeline.bounced", Payload: core.JSONPayload(map[string]any{"from": "review", "to": "implement", "reason_code": result.ReasonCode, "feedback": result.Feedback, "count": bounces})})
			if err := d.Store.CreateIntervention(store.WithActor(ctx, store.Actor{ID: "review-agent", Role: core.ActorAgent}), core.Intervention{TaskID: task.ID, JobID: job.ID, Action: core.InterventionRedirect, ReasonCode: result.ReasonCode, Comment: result.Feedback}); err != nil {
				return err
			}
			if bounces >= d.Cfg.MaxBounces {
				_ = d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, JobID: job.ID, Kind: "pipeline.bounce_limit", Payload: core.JSONPayload(map[string]any{"stage": job.Stage, "count": bounces, "recovery_stage": core.StageImplement})})
				return d.transition(ctx, task.ID, core.TaskAwaiting, "", core.StageImplement)
			}
			return d.transition(ctx, task.ID, core.TaskQueued, core.StageImplement, "")
		}
		if repo.GitHub != "" {
			prURL, err := github.OpenPR(ctx, worktree, repo.GitHub, task.Branch, task.BaseBranch, task.Title, prBody(task))
			if err != nil {
				return fmt.Errorf("open PR: %w", err)
			}
			_ = d.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, JobID: job.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]string{"url": prURL})})
		}
		if task.Level == core.L0 {
			return d.transition(ctx, task.ID, core.TaskApproved, "", "")
		}
		return d.transition(ctx, task.ID, core.TaskAwaiting, "", core.StageImplement)
	}
	return fmt.Errorf("unsupported stage %s", job.Stage)
}

func (d *Dispatcher) transition(ctx context.Context, taskID string, state core.TaskState, nextStage, recoveryStage core.Stage) error {
	if err := d.Store.SetTaskTransition(ctx, taskID, state, nextStage, recoveryStage); err != nil {
		return err
	}
	if state == core.TaskQueued {
		d.Enqueue(taskID)
	}
	return nil
}

// HandleIntervention advances a spec gate or redispatches a redirect after the
// decision itself has been durably recorded by the store.
func (d *Dispatcher) HandleIntervention(ctx context.Context, task core.Task, latest core.Job, intervention core.Intervention) error {
	switch intervention.Action {
	case core.InterventionReject:
		return d.transition(ctx, task.ID, core.TaskClosed, "", "")
	case core.InterventionApprove:
		if latest.State == core.JobPaused {
			return d.transition(ctx, task.ID, core.TaskQueued, latest.Stage, "")
		}
		if latest.Stage == core.StageSpec {
			spec, ok, err := d.Store.GetLatestSpecVersion(ctx, task.ID)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("task %s has no spec to approve", task.ID)
			}
			if err := d.Store.ApproveSpecVersion(ctx, task.ID, spec.Version); err != nil {
				return err
			}
			return d.transition(ctx, task.ID, core.TaskQueued, core.StageImplement, "")
		}
		return d.transition(ctx, task.ID, core.TaskApproved, "", "")
	case core.InterventionRedirect:
		target := task.RecoveryStage
		if target == "" {
			if latest.Stage == core.StageReview {
				target = core.StageImplement
			} else {
				target = latest.Stage
			}
		}
		if target == "" {
			return fmt.Errorf("task %s has no recovery stage", task.ID)
		}
		return d.transition(ctx, task.ID, core.TaskQueued, target, "")
	case core.InterventionPull:
		target := task.RecoveryStage
		if target == "" && latest.Stage == core.StageReview {
			target = core.StageImplement
		} else if target == "" {
			target = latest.Stage
		}
		if target == "" {
			return fmt.Errorf("task %s has no recovery stage for local handoff", task.ID)
		}
		return d.transition(ctx, task.ID, task.State, target, target)
	}
	return nil
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
	d.pollReviewFeedback(ctx)
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
					NextStage:  core.InitialStage(core.L2),
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

func (d *Dispatcher) pollReviewFeedback(ctx context.Context) {
	tasks, err := d.Store.ListTasks(ctx)
	if err != nil {
		log.Printf("poll PR feedback: %v", err)
		return
	}
	for _, task := range tasks {
		if task.State != core.TaskAwaiting && task.State != core.TaskApproved {
			continue
		}
		repo, ok := d.Cfg.Repo(task.Repo)
		if !ok || repo.GitHub == "" {
			continue
		}
		feedback, err := github.ListReviewFeedback(ctx, repo.GitHub, task.Branch)
		if err != nil {
			continue // the branch may not have a PR yet
		}
		events, err := d.Store.ListEvents(ctx, task.ID)
		if err != nil {
			continue
		}
		seen := map[string]bool{}
		for _, event := range events {
			if event.Kind != "github.review_redirected" {
				continue
			}
			var payload struct {
				ExternalID string `json:"external_id"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil {
				seen[payload.ExternalID] = true
			}
		}
		for _, item := range feedback {
			if seen[item.ID] {
				continue
			}
			latest, _, _ := d.Store.GetLatestJob(ctx, task.ID)
			actorCtx := store.WithActor(ctx, store.Actor{ID: "github:" + item.Author, Role: core.ActorHuman})
			intervention := core.Intervention{TaskID: task.ID, JobID: latest.ID, Action: core.InterventionRedirect, ReasonCode: "github-review", Comment: item.Body}
			if err := d.Store.CreateIntervention(actorCtx, intervention); err != nil {
				log.Printf("[task %s] ingest PR feedback: %v", task.ID, err)
				continue
			}
			_ = d.Store.AppendEvent(actorCtx, core.Event{TaskID: task.ID, JobID: latest.ID, Kind: "github.review_redirected", Payload: core.JSONPayload(map[string]any{"external_id": item.ID, "pr_number": item.PR, "author": item.Author})})
			if err := d.HandleIntervention(actorCtx, task, latest, intervention); err != nil {
				log.Printf("[task %s] route PR feedback: %v", task.ID, err)
			}
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

// buildPrompt preserves the accepted Phase 1 compatibility fixture. Phase 3
// tasks load pack/roles/implement.md instead (spec §2.2).
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
