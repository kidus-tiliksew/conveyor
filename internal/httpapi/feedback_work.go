package httpapi

import (
	"context"
	"fmt"
	"log"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/feedback"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// Bounds of the background feedback work (component-production-feedback
// invariants 8 and 20; feature-production-feedback-live FB-3).
const (
	feedbackWorkQueue     = 256
	feedbackWorkers       = 2
	feedbackJobTimeout    = 2 * time.Minute
	feedbackJobComparison = store.FeedbackComparisonsPerMinute
)

// feedbackJob is one unit of background work for one workspace: an accepted
// signal, or the answers a read found missing. It holds immutable copies and
// no request credential.
type feedbackJob struct {
	key       string
	workspace string
	signal    *store.FeedbackSignal
	missing   feedback.Missing
	env       feedbackJobEnv
}

// feedbackJobEnv is what a job uses, taken from the server when the job is
// handed off, so a job reads no server field while it runs.
type feedbackJobEnv struct {
	store  store.Store
	forge  feedback.Forge
	apps   github.AppStore
	client *github.AppClient
	clock  func() time.Time
}

func (s *Server) feedbackJobEnv() feedbackJobEnv {
	env := feedbackJobEnv{store: s.Store, forge: s.feedbackForge, client: s.appClient(), clock: time.Now}
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
}

func (s *Server) feedbackWorker() *feedbackWorker {
	s.feedbackWorkOnce.Do(func() {
		worker := &feedbackWorker{jobs: make(chan feedbackJob, feedbackWorkQueue), pending: map[string]bool{}}
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
	key := "signal:" + workspace + ":" + signal.EnvironmentID + ":" + signal.ArrivedAt.Format(time.RFC3339Nano)
	s.feedbackWorker().enqueue(feedbackJob{key: key, workspace: workspace, signal: &copied, env: s.feedbackJobEnv()})
}

// requestFeedbackAnswers asks the background work for the answers a read
// found missing, at most one minute's allowance of comparisons per job
// (component-production-feedback invariant 20). The read does not wait.
func (s *Server) requestFeedbackAnswers(workspace string, missing feedback.Missing) {
	comparisons := store.NormalizeFeedbackComparisonKeys(missing.Comparisons)
	if len(comparisons) > feedbackJobComparison {
		comparisons = comparisons[:feedbackJobComparison]
	}
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
	key := "read:" + workspace + ":" + strings.Join(parts, ",")
	s.feedbackWorker().enqueue(feedbackJob{key: key, workspace: workspace, missing: feedback.Missing{Comparisons: comparisons, Branches: branches}, env: s.feedbackJobEnv()})
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
	forge, noApp, err := job.env.forgeFor(ctx, job.workspace)
	if err != nil {
		log.Printf("feedback_work workspace=%s outcome=deferred category=%s", job.workspace, github.ErrorCategory(github.CategorizeError(err)))
		return
	}
	work := feedback.Work{Store: backend, Forge: forge, Now: job.env.clock, Logf: log.Printf}
	if job.signal == nil {
		if noApp {
			return
		}
		if err := work.RefreshBranches(ctx, job.missing.Branches); err != nil {
			log.Printf("feedback_work workspace=%s outcome=failed step=branches error=%v", job.workspace, err)
			return
		}
		if err := work.CompareDirect(ctx, job.missing.Comparisons); err != nil {
			log.Printf("feedback_work workspace=%s outcome=failed step=compare error=%v", job.workspace, err)
		}
		return
	}
	request, err := feedbackSignalRequest(ctx, job.env.store, *job.signal)
	if err != nil {
		log.Printf("feedback_work workspace=%s outcome=failed step=load error=%v", job.workspace, err)
		return
	}
	request.NoApp = noApp
	if err := work.ProcessSignal(ctx, request); err != nil {
		log.Printf("feedback_work workspace=%s outcome=failed step=signal error=%v", job.workspace, err)
	}
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

// forgeFor binds the workspace App's installation token to the GitHub reads
// (component-production-feedback invariant 9; DEC-59(3)). noApp is true when
// the workspace has no usable App.
func (env feedbackJobEnv) forgeFor(ctx context.Context, workspace string) (feedback.Forge, bool, error) {
	if env.forge != nil {
		return env.forge, false, nil
	}
	token, err := env.client.WorkspaceToken(ctx, env.apps, workspace)
	if err != nil {
		if github.ErrorCategory(err) == github.ForgePermission {
			return nil, true, nil
		}
		return nil, false, err
	}
	return githubFeedbackForge{token: token, identity: github.AppIdentity(workspace)}, false, nil
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
