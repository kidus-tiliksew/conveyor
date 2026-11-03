package dispatch

import (
	"context"
	"errors"
	"log"
	"slices"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// Start-up checks replace the minute reconcile tick: each repair pass runs
// once per workspace at daemon start, every repair is recorded as one
// drift.repaired event and one error-level log line, and nothing repeats the
// passes on a timer (component-runtime, Start-up checks and background loops;
// req-quiet-idle-runtime REQ-4, AC-4.1 to AC-4.3, AC-3.4; DEC-99(6)).

// The start-up GitHub reads (req-quiet-idle-runtime AC-3.4).
const (
	StartupMergeReadinessPass = "merge_readiness"
	StartupPullRequestsPass   = "pull_requests"
)

const (
	startupRetryInitial = 5 * time.Second
	startupRetryMaximum = time.Minute
)

// StartupCheckStore is the store surface the drift-repair passes use.
type StartupCheckStore interface {
	store.Store
	ReconcileQueuedTasks(context.Context) (int, error)
	ReconcileBlueprintClosures(context.Context) (int, error)
}

// StartupChecks runs the daemon's start-up checks.
type StartupChecks struct {
	Dispatcher *Dispatcher
	// Store runs the drift-repair passes and records their events.
	Store StartupCheckStore
	// Logf writes the error-level lines; nil uses log.Printf.
	Logf func(string, ...any)
	// RetryInitial and RetryMaximum bound the delay before a failed pass
	// runs again. Zero values use five seconds and one minute.
	RetryInitial, RetryMaximum time.Duration
	// Sleep waits before a retry and returns ctx's error when it ends
	// first; nil waits on a timer.
	Sleep func(context.Context, time.Duration) error

	retries sync.WaitGroup
}

type startupPass struct {
	name string
	run  func(context.Context) (int, error)
}

func (c *StartupChecks) driftPasses() []startupPass {
	st := c.Store
	return []startupPass{
		{store.DriftPassGitHubLifecycleIntents, c.Dispatcher.ReconcileGitHubLifecycles},
		{store.DriftPassGitHubIssuePublications, st.ReconcileGitHubLifecycles},
		{store.DriftPassQueuedTasks, st.ReconcileQueuedTasks},
		{store.DriftPassBlueprintClosures, st.ReconcileBlueprintClosures},
		{store.DriftPassReviewPublications, st.ReconcileReviewPublications},
		{store.DriftPassVerificationDeliveries, func(ctx context.Context) (int, error) {
			deliveries, ok := st.(store.VerificationDeliveryStore)
			if !ok {
				return 0, nil
			}
			return 0, deliveries.ReconcileVerificationDeliveries(store.WithActor(ctx, store.SystemActor("verification-publication")))
		}},
	}
}

// RunDrift runs every drift-repair pass once for each workspace, in order.
// The daemon calls it before the queue runtime starts, so no alarm fire or
// worker on this daemon repairs drift before a pass can report it. A failed
// pass never skips a later pass or workspace; it is retried in the
// background until it completes or ctx, the service context, ends.
func (c *StartupChecks) RunDrift(ctx context.Context, workspaces []string) {
	for _, workspace := range workspaces {
		for _, pass := range c.driftPasses() {
			c.run(ctx, workspace, pass)
		}
	}
}

// RunGitHub reads GitHub once for every task that waits on it in each
// workspace (req-quiet-idle-runtime AC-3.4). ReconcileMergeReadiness runs
// unchanged; the pull_requests pass then reads the pull request of every task
// that waits on GitHub (waitsOnGitHub). The daemon calls it once the server is wired and before it
// serves requests (component-runtime, Start-up checks and background loops).
func (c *StartupChecks) RunGitHub(ctx context.Context, workspaces []string) {
	for _, workspace := range workspaces {
		c.run(ctx, workspace, startupPass{StartupMergeReadinessPass, c.Dispatcher.ReconcileMergeReadiness})
		c.run(ctx, workspace, startupPass{StartupPullRequestsPass, c.readPullRequestsAtStart})
	}
}

// readPullRequests reads, under the workspace GitHub App identity
// (Dispatcher.ViewPullRequest), the branch pull request of every task that
// waits on GitHub and hands each observation to ReconcileObservedPullRequest,
// the path the repository monitor uses. That path records a merge only for a
// task its eligibility rules accept and never requests one, so reading a
// parked, queued, or running task never merges it. A failed read writes one
// error line and is not retried, so no GitHub call repeats on a timer; a
// failed candidate read fails the pass, which retries it. A rate-limited read
// ends the reads and is returned, because every later read would fail until
// GitHub's reset. It returns the number of pull requests read.
func (c *StartupChecks) readPullRequests(ctx context.Context) (int, error) {
	d := c.Dispatcher
	candidates, err := startupPullRequestCandidates(ctx, d.Store)
	if err != nil {
		return 0, err
	}
	if len(candidates) == 0 {
		return 0, nil
	}
	cfg, err := d.currentConfig(ctx)
	if err != nil {
		return 0, err
	}
	workspace, _ := store.WorkspaceFromContext(ctx)
	read := 0
	for _, task := range candidates {
		if err := ctx.Err(); err != nil {
			return read, err
		}
		repo, ok := cfg.Repo(task.Repo)
		if !ok || repo.GitHub == "" || task.Branch == "" || d.ViewPullRequest == nil {
			continue
		}
		pr, viewErr := d.ViewPullRequest(ctx, repo.GitHub, task.Branch)
		switch {
		case errors.Is(viewErr, github.ErrPullRequestNotFound):
			// GitHub answered: the branch has no pull request to apply.
			read++
		case viewErr != nil:
			if _, limited := github.RateLimitRetryAt(viewErr); limited {
				// Every later read would fail until GitHub's reset; the
				// caller writes the one line for it.
				return read, viewErr
			}
			c.logf(`level=error msg="start-up GitHub read failed" workspace=%s task=%s repository=%s err=%v`, workspace, task.ID, repo.GitHub, viewErr)
		default:
			read++
			if _, err := d.ReconcileObservedPullRequest(ctx, task.Repo, repo.GitHub, task.ID, pr); err != nil {
				c.logf(`level=error msg="start-up GitHub read failed" workspace=%s task=%s repository=%s err=%v`, workspace, task.ID, repo.GitHub, err)
			}
		}
	}
	return read, nil
}

// readPullRequestsAtStart runs the start-up pull_requests pass. A
// rate-limited read stops the pass with one error line naming the workspace
// and GitHub's reset time and is not retried, as no failed read is; the next
// start or the timed fallback reads the tasks again.
func (c *StartupChecks) readPullRequestsAtStart(ctx context.Context) (int, error) {
	read, err := c.readPullRequests(ctx)
	if retryAt, limited := github.RateLimitRetryAt(err); limited {
		workspace, _ := store.WorkspaceFromContext(ctx)
		reset := "unknown"
		if !retryAt.IsZero() {
			reset = retryAt.UTC().Format(time.RFC3339)
		}
		c.logf(`level=error msg="start-up GitHub reads stopped: GitHub rate limit exhausted" workspace=%s reset_at=%s err=%v`, workspace, reset, err)
		return read, nil
	}
	return read, err
}

// startupPullRequestCandidates lists, in store order and without duplicates,
// the tasks that wait on GitHub (waitsOnGitHub) among the tasks in a state
// mergeReadinessTaskStates selects and the open tasks, parked included. It
// makes two ListTasksFiltered calls and one batched
// ListMonitorPullRequestEventsForTasks call, and no GitHub call. The
// start-up pull_requests pass, the timed fallback's per-fire read, and the
// fallback's arming all use this set (component-runtime, Timed GitHub
// fallback; req-quiet-idle-runtime AC-3.4, AC-3.5).
func startupPullRequestCandidates(ctx context.Context, st store.Store) ([]core.Task, error) {
	live, err := st.ListTasksFiltered(ctx, store.TaskFilter{States: mergeReadinessTaskStates})
	if err != nil {
		return nil, err
	}
	open, err := st.ListTasksFiltered(ctx, store.TaskFilter{Open: true})
	if err != nil {
		return nil, err
	}
	tasks := make([]core.Task, 0, len(live)+len(open))
	seen := make(map[string]bool, len(live)+len(open))
	for _, task := range append(live, open...) {
		if !seen[task.ID] {
			seen[task.ID] = true
			tasks = append(tasks, task)
		}
	}
	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.ID)
	}
	recorded, err := st.ListMonitorPullRequestEventsForTasks(ctx, ids)
	if err != nil {
		return nil, err
	}
	candidates := make([]core.Task, 0, len(recorded))
	for _, task := range tasks {
		if waitsOnGitHub(task, len(recorded[task.ID]) > 0) {
			candidates = append(candidates, task)
		}
	}
	return candidates, nil
}

// waitsOnGitHub reports whether a task waits on GitHub: it is neither
// merged nor closed, and it has a recorded pull_request.opened event or a
// merge-readiness pass can act on it. A task without a recorded pull
// request has nothing GitHub can report that ReconcileObservedPullRequest
// would apply, because monitor.RecordedLineage requires that event, so the
// 285 queued backlog tasks of a workspace cost no GitHub call. A queued task
// bounced back after it opened a pull request still waits. The check reads
// only stored task fields and events.
func waitsOnGitHub(task core.Task, recordedPullRequest bool) bool {
	if task.State == core.TaskMerged || task.State == core.TaskClosed {
		return false
	}
	return recordedPullRequest || mergeReadinessCanAct(task)
}

// mergeReadinessCanAct reports, from stored task fields alone, whether a
// ReconcileMergeReadiness pass can act on a task: the gate-on pass's
// approved or merge-gated task, or a gate-off task that is approved or has
// a review head the accepted-review recovery reads.
func mergeReadinessCanAct(task core.Task) bool {
	if task.MergeApproval {
		return task.State == core.TaskApproved || task.State == core.TaskAwaiting && task.GateKind == core.GateKindMergeApproval
	}
	if !slices.Contains(mergeReadinessTaskStates, task.State) || !gateOffMergeReadinessCandidate(task) {
		return false
	}
	return task.State == core.TaskApproved || task.ApprovedHeadSHA != "" || task.ReviewedHeadSHA != ""
}

// Wait returns once every background retry has ended.
func (c *StartupChecks) Wait() { c.retries.Wait() }

func (c *StartupChecks) run(ctx context.Context, workspace string, pass startupPass) {
	delay := c.retryInitial()
	if c.attempt(ctx, workspace, pass, 1, delay) == nil || ctx.Err() != nil {
		return
	}
	c.retries.Add(1)
	go func() {
		defer c.retries.Done()
		for attempt := 2; ; attempt++ {
			if err := c.sleep(ctx, delay); err != nil {
				return
			}
			delay = min(2*delay, c.retryMaximum())
			if c.attempt(ctx, workspace, pass, attempt, delay) == nil || ctx.Err() != nil {
				return
			}
		}
	}()
}

// attempt runs one pass for one workspace and records each repair it
// reported, including repairs reported before a failure.
func (c *StartupChecks) attempt(ctx context.Context, workspace string, pass startupPass, attempt int, retryIn time.Duration) error {
	var mu sync.Mutex
	var repairs []store.DriftRepair
	passCtx := store.WithWorkspace(store.WithActor(ctx, store.SystemActor("startup-check")), workspace)
	recordCtx := passCtx
	passCtx = store.WithDriftRecorder(passCtx, func(repair store.DriftRepair) {
		mu.Lock()
		repairs = append(repairs, repair)
		mu.Unlock()
	})
	_, err := pass.run(passCtx)
	mu.Lock()
	reported := repairs
	mu.Unlock()
	for _, repair := range reported {
		c.record(recordCtx, workspace, repair)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return err
		}
		c.logf(`level=error msg="start-up check failed" workspace=%s pass=%s attempt=%d retry_in=%s err=%v`, workspace, pass.name, attempt, retryIn, err)
	}
	return err
}

// record appends one drift.repaired event and writes one error-level line.
// A failed append writes a second error line, so the repair is never left
// unreported (req-quiet-idle-runtime AC-4.1, AC-4.2).
func (c *StartupChecks) record(ctx context.Context, workspace string, repair store.DriftRepair) {
	c.logf(`level=error msg="start-up check repaired drift" workspace=%s task=%s pass=%s mismatch=%q repair=%q`, workspace, repair.TaskID, repair.Pass, repair.Mismatch, repair.Repair)
	event := core.Event{TaskID: repair.TaskID, Kind: store.DriftRepairedEvent, Payload: store.DriftRepairedPayload(repair)}
	events := store.Store(c.Store)
	if c.Store == nil {
		events = c.Dispatcher.Store
	}
	if err := events.AppendEvent(ctx, event); err != nil {
		c.logf(`level=error msg="start-up check could not record drift.repaired" workspace=%s task=%s pass=%s err=%v`, workspace, repair.TaskID, repair.Pass, err)
	}
}

func (c *StartupChecks) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

func (c *StartupChecks) retryInitial() time.Duration {
	if c.RetryInitial > 0 {
		return c.RetryInitial
	}
	return startupRetryInitial
}

func (c *StartupChecks) retryMaximum() time.Duration {
	if c.RetryMaximum > 0 {
		return c.RetryMaximum
	}
	return startupRetryMaximum
}

func (c *StartupChecks) sleep(ctx context.Context, delay time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
