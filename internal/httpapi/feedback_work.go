package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/feedback"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// Bounds of the background feedback work (component-production-feedback
// invariants 8 and 20; feature-production-feedback-live FB-3).
const (
	feedbackWorkQueue  = 256
	feedbackWorkers    = 2
	feedbackJobTimeout = 2 * time.Minute
)

// feedbackJob is one unit of background work for one workspace: an accepted
// signal, or the answers a read found missing. It holds immutable copies and
// no request credential.
type feedbackJob struct {
	key       string
	workspace string
	signal    *store.FeedbackSignal
	missing   feedback.Missing
	// reports asks for the linking of an environment's pending reports, and
	// report runs one linking attempt of one report
	// (component-feedback-reports invariant 15).
	reports *feedbackReportsRequest
	report  string
	// linking marks a read job that the linking work queued. Its forge calls
	// honor the workspace's retry hold.
	linking bool
	env     feedbackJobEnv
}

// feedbackJobEnv is what a job uses, taken from the server when the job is
// handed off, so a job reads no server field while it runs.
type feedbackJobEnv struct {
	store     store.Store
	forge     feedback.Forge
	makeForge func(token, identity string) feedback.Forge
	apps      github.AppStore
	client    *github.AppClient
	clock     func() time.Time
	backoff   *feedbackTokenBackoff
	// worker is the queue a job hands further jobs to. A further job takes
	// this environment, so a running job reads no server field.
	worker *feedbackWorker
}

func (s *Server) feedbackJobEnv() feedbackJobEnv {
	worker := s.feedbackWorker()
	env := feedbackJobEnv{store: s.Store, forge: s.feedbackForge, makeForge: s.feedbackForgeFor, client: s.appClient(), clock: time.Now, backoff: worker.backoff, worker: worker}
	if env.makeForge == nil {
		env.makeForge = func(token, identity string) feedback.Forge {
			return githubFeedbackForge{token: token, identity: identity}
		}
	}
	if s.WorkspaceGitHubApps != nil {
		env.apps = s.WorkspaceGitHubApps
	}
	if s.feedbackClock != nil {
		env.clock = s.feedbackClock
	}
	return env
}

// feedbackWorker runs feedback jobs on a bounded queue. A job whose key is
// queued or running is not queued again. A job that finds the queue full is
// logged and dropped, and its work stays eligible: the version stays
// unclaimed for a later signal, and a later read asks for the answer again.
type feedbackWorker struct {
	jobs    chan feedbackJob
	mu      sync.Mutex
	pending map[string]bool
	running sync.WaitGroup
	backoff *feedbackTokenBackoff
}

func (s *Server) feedbackWorker() *feedbackWorker {
	s.feedbackWorkOnce.Do(func() {
		worker := &feedbackWorker{jobs: make(chan feedbackJob, feedbackWorkQueue), pending: map[string]bool{}, backoff: &feedbackTokenBackoff{until: map[string]feedbackTokenFailure{}}}
		for range feedbackWorkers {
			go func() {
				for job := range worker.jobs {
					runFeedbackJob(job)
					worker.mu.Lock()
					delete(worker.pending, job.key)
					worker.mu.Unlock()
					worker.running.Done()
				}
			}()
		}
		s.feedbackWork = worker
	})
	return s.feedbackWork
}

// enqueue adds a job unless the same job is queued or running.
func (w *feedbackWorker) enqueue(job feedbackJob) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pending[job.key] {
		return
	}
	w.running.Add(1)
	select {
	case w.jobs <- job:
		w.pending[job.key] = true
	default:
		w.running.Done()
		log.Printf("feedback_work workspace=%s outcome=deferred reason=queue_full", job.workspace)
	}
}

// wait returns when no job is queued or running. Tests use it.
func (w *feedbackWorker) wait() { w.running.Wait() }

// enqueueFeedbackSignal hands an accepted signal to the background work. The
// route calls it only after its answer is written and flushed
// (component-production-feedback invariant 8).
func (s *Server) enqueueFeedbackSignal(workspace string, signal store.FeedbackSignal) {
	copied := signal
	copied.Services = make([]store.FeedbackSignalService, len(signal.Services))
	for i, service := range signal.Services {
		copied.Services[i] = service
		copied.Services[i].Versions = slices.Clone(service.Versions)
	}
	// The key names the services and versions too: two GitHub deployment
	// reports of one environment can share a report time, and each one must
	// reach the background work.
	services := sha256.Sum256(core.JSONPayload(copied.Services))
	key := "signal:" + workspace + ":" + signal.EnvironmentID + ":" + signal.ArrivedAt.Format(time.RFC3339Nano) + ":" + hex.EncodeToString(services[:])
	s.feedbackWorker().enqueue(feedbackJob{key: key, workspace: workspace, signal: &copied, env: s.feedbackJobEnv()})
	// An accepted signal also asks for the linking of the environment's
	// pending reports. The background job reads them, so the route makes no
	// scan (component-feedback-reports invariant 15). A GitHub deployment
	// report is not a watcher signal and asks for none.
	if signal.KeyID != "" {
		s.enqueueFeedbackPendingReports(workspace, signal.EnvironmentID, nil)
	}
}

// FeedbackDeploymentReports returns the handler that applies GitHub
// deployment_status deliveries, or nil when the store keeps no environments
// or deploys. The tracked-branch check reads through the workspace App's
// installation token, and a recorded deploy goes to the background resolving
// and comparing as an accepted signal does (feature-production-feedback-live
// FB-4; component-production-feedback invariants 9 and 10; DEC-59(3)).
func (s *Server) FeedbackDeploymentReports() func(ctx context.Context, repository string, status github.DeploymentStatus) error {
	backend, ok := s.Store.(feedback.DeploymentStore)
	if !ok {
		return nil
	}
	reports := feedback.DeploymentReports{
		Store: backend,
		Repositories: func(ctx context.Context) ([]feedback.Repository, error) {
			return feedbackRepositories(ctx, s.Store)
		},
		Forge: func(_ context.Context, workspace string) feedback.Forge {
			return s.feedbackJobEnv().lazyForge(workspace)
		},
		Recorded: s.enqueueFeedbackSignal,
		Logf:     log.Printf,
	}
	return reports.Apply
}

// requestFeedbackAnswers asks the background work for the answers a read
// found missing (component-production-feedback invariant 20). The read does
// not wait. Every candidate goes to the store's admission, which skips stored
// and in-flight pairs and admits at most the workspace allowance, so a pair
// that keeps failing never holds back the pairs after it.
func (s *Server) requestFeedbackAnswers(workspace string, missing feedback.Missing) {
	s.feedbackJobEnv().requestAnswers(workspace, missing)
}

// requestAnswers queues the read job for missing answers with this
// environment, so a running job can ask without reading a server field.
func (env feedbackJobEnv) requestAnswers(workspace string, missing feedback.Missing) {
	comparisons := store.NormalizeFeedbackComparisonKeys(missing.Comparisons)
	branches := []feedback.BranchRef{}
	seen := map[feedback.BranchRef]bool{}
	for _, branch := range missing.Branches {
		if !seen[branch] {
			seen[branch] = true
			branches = append(branches, branch)
		}
	}
	if len(comparisons) == 0 && len(branches) == 0 {
		return
	}
	parts := []string{}
	for _, key := range comparisons {
		parts = append(parts, key.Repository+"/"+key.Base+"/"+key.Head)
	}
	for _, branch := range branches {
		parts = append(parts, branch.Slug+"@"+branch.Branch)
	}
	sort.Strings(parts)
	digest := sha256.Sum256([]byte(strings.Join(parts, ",")))
	key := "read:" + workspace + ":" + hex.EncodeToString(digest[:])
	env.worker.enqueue(feedbackJob{key: key, workspace: workspace, missing: feedback.Missing{Comparisons: comparisons, Branches: branches}, env: env})
}

// runFeedbackJob runs one job under a fresh, bounded context that carries the
// job's workspace and the system actor, never a request's credential.
func runFeedbackJob(job feedbackJob) {
	ctx, cancel := context.WithTimeout(store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), job.workspace), feedbackJobTimeout)
	defer cancel()
	backend, ok := job.env.store.(store.FeedbackStore)
	if !ok {
		return
	}
	switch {
	case job.reports != nil:
		runFeedbackPendingReports(ctx, job)
		return
	case job.report != "":
		runFeedbackReportLinking(ctx, job)
		return
	}
	// The forge gets the installation token at its first request. For a
	// signal that request comes after a version's claim, so a token failure
	// is recorded as that version's attempt, with its reason and any later
	// retry time GitHub named (component-production-feedback invariants 7
	// to 9; req-production-feedback AC-1.4).
	forge := job.env.lazyForge(job.workspace)
	if job.linking {
		forge = &feedbackAttemptForge{inner: forge, env: job.env, workspace: job.workspace}
	}
	work := feedback.Work{Store: backend, Forge: forge, Now: job.env.clock, Logf: log.Printf}
	if job.signal == nil {
		// A rate limit on a branch read stops the job before any comparison
		// starts. The branches not read and every comparison stay eligible
		// for a later read.
		if err := work.RefreshBranches(ctx, job.missing.Branches); err != nil {
			recordFeedbackJobStop(job.env, job.workspace, "branches", err)
			return
		}
		if err := work.CompareDirect(ctx, job.missing.Comparisons); err != nil {
			recordFeedbackJobStop(job.env, job.workspace, "compare", err)
		}
		return
	}
	request, err := feedbackSignalRequest(ctx, job.env.store, *job.signal)
	if err != nil {
		log.Printf("feedback_work workspace=%s outcome=failed step=load error=%v", job.workspace, err)
		return
	}
	if err := work.ProcessSignal(ctx, request); err != nil {
		recordFeedbackJobStop(job.env, job.workspace, "signal", err)
	}
}

// logFeedbackJobStop logs why a job stopped early. A GitHub rate limit
// defers the rest of the job, and it is not a failure.
func logFeedbackJobStop(workspace, step string, err error) {
	logFeedbackJobStopAt(workspace, step, err)
}

// recordFeedbackJobStop keeps the retry time of a job that GitHub
// rate-limited, so the linking work of the workspace sends no request and
// asks for no comparison before it, and then logs the stop.
func recordFeedbackJobStop(env feedbackJobEnv, workspace, step string, err error) {
	if errors.Is(err, feedback.ErrRateLimited) {
		env.backoff.recordRateLimit(workspace, err, env.clock())
	}
	logFeedbackJobStopAt(workspace, step, err)
}

func logFeedbackJobStopAt(workspace, step string, err error) {
	if errors.Is(err, feedback.ErrRateLimited) {
		log.Printf("feedback_work workspace=%s outcome=deferred step=%s category=%s", workspace, step, github.ForgeRateLimited)
		return
	}
	log.Printf("feedback_work workspace=%s outcome=failed step=%s error=%v", workspace, step, err)
}

// feedbackSignalRequest loads the signal's environment and the workspace's
// repositories as they are now.
func feedbackSignalRequest(ctx context.Context, backend store.Store, signal store.FeedbackSignal) (feedback.SignalRequest, error) {
	request := feedback.SignalRequest{Signal: signal}
	if environments, ok := backend.(store.EnvironmentStore); ok {
		listed, err := environments.ListEnvironments(ctx, store.EnvironmentListQuery{})
		if err != nil {
			return request, err
		}
		for _, environment := range listed {
			if environment.ID == signal.EnvironmentID {
				request.Environment = environment
			}
		}
	}
	repositories, err := feedbackRepositories(ctx, backend)
	if err != nil {
		return request, err
	}
	request.Repositories = repositories
	return request, nil
}

// feedbackRepositories returns the configured repositories of the context's
// workspace with their GitHub slugs.
func feedbackRepositories(ctx context.Context, backend store.Store) ([]feedback.Repository, error) {
	reader, ok := backend.(interface {
		WorkspaceConfig(context.Context) (config.VersionedDocument, error)
	})
	if !ok {
		return nil, fmt.Errorf("workspace configuration is not available on this store")
	}
	document, err := reader.WorkspaceConfig(ctx)
	if err != nil {
		return nil, err
	}
	repositories := make([]feedback.Repository, 0, len(document.Document.Repos))
	for _, repository := range document.Document.Repos {
		repositories = append(repositories, feedback.Repository{Name: repository.Name, Slug: repository.GitHub})
	}
	return repositories, nil
}

// feedbackTokenFailure is a failed installation token request and the time
// before which no job of the workspace asks again.
type feedbackTokenFailure struct {
	err   error
	until time.Time
}

// feedbackTokenBackoff remembers failed token requests per workspace, so a
// rate-limited or failing token exchange is not asked again by every job
// before GitHub's retry time.
type feedbackTokenBackoff struct {
	mu    sync.Mutex
	until map[string]feedbackTokenFailure
	// hold is, by workspace, the time before which the linking work sends
	// GitHub no request: the latest retry time a rate-limited forge call of
	// the workspace returned (component-production-feedback invariant 28).
	hold map[string]time.Time
	// lineReasons is, by workspace and report, the reason the last linking
	// attempt of the report left its line rule unanswered. It names the
	// not_checked note of an attempt at or after 24 hours, which sends GitHub
	// nothing (component-feedback-reports invariant 15).
	lineReasons map[string]string
}

// forgeHold reports whether a retry time GitHub returned has not passed at
// now, and that time. The comparison is exact.
func (b *feedbackTokenBackoff) forgeHold(workspace string, now time.Time) (time.Time, bool) {
	if b == nil {
		return time.Time{}, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	until, held := b.hold[workspace]
	if !held || !now.Before(until) {
		return time.Time{}, false
	}
	return until, true
}

// recordRateLimit keeps the retry time of a rate-limited forge failure for
// the workspace: GitHub's time when it named one, or a minute after now. A
// later time already kept stays. Any other failure keeps nothing.
func (b *feedbackTokenBackoff) recordRateLimit(workspace string, err error, now time.Time) {
	if b == nil || github.ErrorCategory(github.CategorizeError(err)) != github.ForgeRateLimited {
		return
	}
	until := now.Add(feedbackTokenRetry)
	if retryAt, ok := github.RateLimitRetryAt(err); ok && !retryAt.IsZero() {
		until = retryAt
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.hold == nil {
		b.hold = map[string]time.Time{}
	}
	if existing, ok := b.hold[workspace]; ok && !existing.Before(until) {
		return
	}
	b.hold[workspace] = until
}

func (b *feedbackTokenBackoff) rememberLineReason(key, reason string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lineReasons == nil {
		b.lineReasons = map[string]string{}
	}
	if reason == "" {
		delete(b.lineReasons, key)
		return
	}
	b.lineReasons[key] = reason
}

func (b *feedbackTokenBackoff) lineReason(key string) string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lineReasons[key]
}

// feedbackTokenRetry is the wait after a failed token request when GitHub
// named no retry time.
const feedbackTokenRetry = time.Minute

func (b *feedbackTokenBackoff) failure(workspace string, now time.Time) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if failure, ok := b.until[workspace]; ok && now.Before(failure.until) {
		return failure.err
	}
	delete(b.until, workspace)
	return nil
}

func (b *feedbackTokenBackoff) record(workspace string, err error, now time.Time) {
	if b == nil {
		return
	}
	until := now.Add(feedbackTokenRetry)
	if retryAt, ok := github.RateLimitRetryAt(err); ok && retryAt.After(until) {
		until = retryAt
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	// A concurrent failure that set a later deadline, such as GitHub's
	// retry time, keeps it.
	if existing, ok := b.until[workspace]; ok && existing.until.After(until) {
		return
	}
	b.until[workspace] = feedbackTokenFailure{err: err, until: until}
}

// lazyForge returns the job's forge. A test forge is used as is. Otherwise
// the forge binds the workspace App's installation token at its first
// request (DEC-59(3)).
func (env feedbackJobEnv) lazyForge(workspace string) feedback.Forge {
	if env.forge != nil {
		return env.forge
	}
	return &lazyFeedbackForge{env: env, workspace: workspace}
}

// lazyFeedbackForge gets the installation token once, at the first request
// or readiness check of its job. A permission failure means the workspace
// has no usable App and answers feedback.ErrNoGitHubApp. Any other failure
// keeps its category and GitHub's retry time.
type lazyFeedbackForge struct {
	env       feedbackJobEnv
	workspace string
	once      sync.Once
	forge     feedback.Forge
	err       error
}

func (f *lazyFeedbackForge) acquire(ctx context.Context) (feedback.Forge, error) {
	f.once.Do(func() {
		if f.err = f.env.backoff.failure(f.workspace, f.env.clock()); f.err != nil {
			return
		}
		token, err := f.env.client.WorkspaceToken(ctx, f.env.apps, f.workspace)
		switch {
		case err == nil:
			f.forge = f.env.makeForge(token, github.AppIdentity(f.workspace))
			return
		case github.ErrorCategory(err) == github.ForgePermission:
			f.err = fmt.Errorf("%w: %w", feedback.ErrNoGitHubApp, err)
		default:
			f.err = err
			f.env.backoff.record(f.workspace, err, f.env.clock())
		}
	})
	return f.forge, f.err
}

// Ready implements feedback.ReadyForge.
func (f *lazyFeedbackForge) Ready(ctx context.Context) error {
	_, err := f.acquire(ctx)
	return err
}

func (f *lazyFeedbackForge) ReadCommit(ctx context.Context, slug, ref string) (string, error) {
	forge, err := f.acquire(ctx)
	if err != nil {
		return "", err
	}
	return forge.ReadCommit(ctx, slug, ref)
}

func (f *lazyFeedbackForge) Compare(ctx context.Context, slug, base, head string, arrived int) (feedback.CompareResult, error) {
	forge, err := f.acquire(ctx)
	if err != nil {
		return feedback.CompareResult{}, err
	}
	return forge.Compare(ctx, slug, base, head, arrived)
}

func (f *lazyFeedbackForge) ReadLineChange(ctx context.Context, slug, commit, path string, line int) (github.LineChange, error) {
	forge, err := f.acquire(ctx)
	if err != nil {
		return github.LineChange{}, err
	}
	return forge.ReadLineChange(ctx, slug, commit, path, line)
}

// githubFeedbackForge reads GitHub through the REST boundary with one
// workspace's installation token.
type githubFeedbackForge struct {
	token    string
	identity string
}

func (f githubFeedbackForge) ReadCommit(ctx context.Context, slug, ref string) (string, error) {
	return github.ReadCommit(github.WithCredential(ctx, f.token, f.identity), slug, ref)
}

func (f githubFeedbackForge) Compare(ctx context.Context, slug, base, head string, arrived int) (feedback.CompareResult, error) {
	ctx = github.WithCredential(ctx, f.token, f.identity)
	if arrived <= 0 {
		result, err := github.CompareAncestry(ctx, slug, base, head)
		return feedback.CompareResult{Status: result.Status}, err
	}
	result, err := github.CompareArrived(ctx, slug, base, head, arrived)
	return feedback.CompareResult{Status: result.Status, Commits: result.Commits, Complete: result.Complete}, err
}

func (f githubFeedbackForge) ReadLineChange(ctx context.Context, slug, commit, path string, line int) (github.LineChange, error) {
	return github.ReadLineChange(github.WithCredential(ctx, f.token, f.identity), slug, commit, path, line)
}
