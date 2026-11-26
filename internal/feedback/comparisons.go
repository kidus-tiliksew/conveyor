package feedback

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// Work is the background resolving and comparing of one workspace. Its
// context carries the workspace, and its forge carries that workspace's App
// token and nothing from a request (component-production-feedback invariants
// 8, 9, and 14). Every comparison request passes the store's admission, which
// keeps the workspace at 60 a minute and never runs one pair twice at once.
type Work struct {
	Store store.FeedbackStore
	Forge Forge
	// Now is the clock. Tests set it.
	Now func() time.Time
	// Logf records each forge failure with its category. It is never nil
	// after defaults.
	Logf func(format string, args ...any)
}

// OutcomeWriteTimeout bounds each save of an outcome that a forge call
// produced.
const OutcomeWriteTimeout = 15 * time.Second

// outcomeContext is the context that saves an outcome a forge call produced.
// It keeps the job context's values, so the workspace and the system actor
// stay the same. It ignores the job's deadline and cancellation, and it has
// its own bound. A forge call that returns when the job's time runs out
// therefore still saves its outcome, and a claimed version never stays
// pending because the save ran on a cancelled context (component-production-
// feedback invariants 7 and 8; req-production-feedback AC-1.4).
func outcomeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), OutcomeWriteTimeout)
}

// recordOutcome runs one save on an outcome context.
func recordOutcome(ctx context.Context, save func(context.Context) error) error {
	writeCtx, cancel := outcomeContext(ctx)
	defer cancel()
	return save(writeCtx)
}

func (w Work) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w Work) logf(format string, args ...any) {
	if w.Logf != nil {
		w.Logf(format, args...)
	}
}

// SignalRequest is one accepted signal, as the background work reads it: the
// immutable signal, its environment, and the workspace's repositories.
type SignalRequest struct {
	Signal       store.FeedbackSignal
	Environment  store.Environment
	Repositories []Repository
	// NoApp is true when the workspace has no usable GitHub App, so every
	// claimed version stays unresolved with that reason.
	NoApp bool
}

// ProcessSignal resolves each version the signal names that has no stored
// answer and that a claim allows, then compares each resolved version the
// signal names (invariants 7, 8, and 14; req-production-feedback AC-1.4).
func (w Work) ProcessSignal(ctx context.Context, request SignalRequest) error {
	signal := request.Signal
	refs := []store.FeedbackVersionRef{}
	seen := map[store.FeedbackVersionRef]bool{}
	for _, service := range signal.Services {
		for _, entry := range service.Versions {
			ref := store.FeedbackVersionRef{EnvironmentID: signal.EnvironmentID, Service: service.Name, FeedbackVersion: store.FeedbackVersion{Repository: service.Repository, Commit: entry.Commit, Version: entry.Version}}
			if !seen[ref] {
				seen[ref] = true
				refs = append(refs, ref)
			}
		}
	}
	for _, ref := range refs {
		// Once the job's time is over, no further version is claimed. Each
		// version not claimed stays eligible for a later signal.
		if ctx.Err() != nil {
			return nil
		}
		now := w.now()
		claimed, err := w.Store.ClaimFeedbackResolution(ctx, ref, signal.ArrivedAt, now)
		if err != nil {
			return err
		}
		if !claimed {
			continue
		}
		outcome := Resolution{Reason: store.FeedbackReasonNoGitHubApp}
		if !request.NoApp {
			outcome = Resolve(ctx, w.Forge, ResolveRequest{Version: ref.FeedbackVersion, Service: ref.Service, Rule: request.Environment.VersionRule, Repositories: request.Repositories})
		}
		if outcome.Reason != "" && outcome.Reason != store.FeedbackReasonNoMatch && outcome.Reason != store.FeedbackReasonAmbiguous {
			w.logf("feedback_resolve environment_id=%s service_digest=%s outcome=unresolved reason=%s", ref.EnvironmentID, store.FeedbackServiceDigest(ref.Service), outcome.Reason)
		}
		resolution := store.FeedbackResolution{FeedbackVersionRef: ref, ResolvedRepository: outcome.Repository, ResolvedCommit: outcome.Commit, Reason: outcome.Reason, AttemptedAt: now, NextAttemptAt: outcome.RetryAt}
		if err := recordOutcome(ctx, func(writeCtx context.Context) error { return w.Store.RecordFeedbackResolution(writeCtx, resolution) }); err != nil {
			return err
		}
	}
	if request.NoApp || ctx.Err() != nil {
		return nil
	}
	return w.compareSignal(ctx, request, refs)
}

// ready reports whether the forge can start a request. A forge that cannot,
// such as one whose installation token request failed, is logged once, and
// no comparison is charged for it. The comparisons stay eligible for a later
// signal or read.
func (w Work) ready(ctx context.Context) error {
	forge, ok := w.Forge.(ReadyForge)
	if !ok {
		return nil
	}
	err := forge.Ready(ctx)
	if err != nil && !errors.Is(err, ErrNoGitHubApp) {
		w.logf("feedback_work outcome=deferred category=%s", github.ErrorCategory(github.CategorizeError(err)))
	}
	return err
}

// compareSignal compares each resolved version the signal names with the
// merged tasks of its repository.
func (w Work) compareSignal(ctx context.Context, request SignalRequest, refs []store.FeedbackVersionRef) error {
	inputs, err := w.Store.ReadFeedbackInputs(ctx, []string{request.Signal.EnvironmentID})
	if err != nil {
		return err
	}
	resolved := map[store.FeedbackVersionRef]store.FeedbackResolution{}
	for _, resolution := range inputs.Resolutions {
		if resolution.Resolved() {
			resolved[resolution.FeedbackVersionRef] = resolution
		}
	}
	slugs := map[string]string{}
	for _, repository := range request.Repositories {
		slugs[repository.Name] = repository.Slug
	}
	for _, ref := range refs {
		current, ok := resolved[ref]
		if !ok || slugs[current.ResolvedRepository] == "" {
			continue
		}
		previous := ""
		if before, found := PreviousVersion(inputs.Deploys[ref.EnvironmentID], ref); found {
			if prior, ok := resolved[before]; ok && prior.ResolvedRepository == current.ResolvedRepository {
				previous = prior.ResolvedCommit
			}
		}
		if err := w.CompareVersion(ctx, Repository{Name: current.ResolvedRepository, Slug: slugs[current.ResolvedRepository]}, current.ResolvedCommit, previous); err != nil {
			return err
		}
	}
	return nil
}

// PreviousVersion returns the version the service deployed last before the
// newest deploy of ref, from an environment's deploy history.
func PreviousVersion(deploys []store.FeedbackDeploy, ref store.FeedbackVersionRef) (store.FeedbackVersionRef, bool) {
	var current *store.FeedbackDeploy
	for i := range deploys {
		deploy := &deploys[i]
		if deploy.Service == ref.Service && deploy.FeedbackVersion == ref.FeedbackVersion && (current == nil || deploy.Start().Compare(current.Start()) > 0) {
			current = deploy
		}
	}
	if current == nil {
		return store.FeedbackVersionRef{}, false
	}
	var previous *store.FeedbackDeploy
	for i := range deploys {
		deploy := &deploys[i]
		if deploy.Service != ref.Service || deploy.FeedbackVersion == ref.FeedbackVersion || deploy.Start().Compare(current.Start()) >= 0 {
			continue
		}
		if previous == nil || deploy.Start().Compare(previous.Start()) > 0 {
			previous = deploy
		}
	}
	if previous == nil {
		return store.FeedbackVersionRef{}, false
	}
	return store.FeedbackVersionRef{EnvironmentID: ref.EnvironmentID, Service: ref.Service, FeedbackVersion: previous.FeedbackVersion}, true
}

// CompareVersion stores, for each merged task of the repository, whether the
// version's commit head holds the task's merge commit
// (feature-production-feedback-live FB-3; invariant 14). A merge commit equal
// to head is identical. When previous is the service's resolved previous
// commit in the same repository and head descends from it, one comparison of
// previous with head lists the commits that arrived: a merge commit among
// them, a merge commit equal to previous, and a merge commit previous held
// are contained in head. Every pair still unproven, which includes the first
// version and a version that does not descend from the previous one, is
// compared directly, newest merge first, within the workspace allowance.
func (w Work) CompareVersion(ctx context.Context, repository Repository, head, previous string) error {
	merged, err := w.Store.ListFeedbackMergedTasks(ctx, store.FeedbackMergedTaskQuery{Repository: repository.Name})
	if err != nil {
		return err
	}
	slug := store.FeedbackRepository(repository.Slug)
	keys := []store.FeedbackComparisonKey{}
	commits := []string{}
	seen := map[string]bool{}
	for _, task := range merged {
		if task.MergeCommit == "" || seen[task.MergeCommit] {
			continue
		}
		seen[task.MergeCommit] = true
		commits = append(commits, task.MergeCommit)
		keys = append(keys, store.FeedbackComparisonKey{Repository: slug, Base: task.MergeCommit, Head: head})
		if previous != "" {
			keys = append(keys, store.FeedbackComparisonKey{Repository: slug, Base: task.MergeCommit, Head: previous})
		}
	}
	if len(commits) == 0 {
		return nil
	}
	descent := store.FeedbackComparisonKey{Repository: slug, Base: previous, Head: head}
	if previous != "" && previous != head {
		keys = append(keys, descent)
	}
	stored, err := w.storedComparisons(ctx, keys)
	if err != nil {
		return err
	}
	now := w.now()
	found := []store.FeedbackComparison{}
	proven := map[string]bool{}
	prove := func(commit, status string) {
		key := store.FeedbackComparisonKey{Repository: slug, Base: commit, Head: head}
		if _, ok := stored[key]; ok || proven[commit] {
			return
		}
		proven[commit] = true
		found = append(found, store.FeedbackComparison{FeedbackComparisonKey: key, Status: status, ComparedAt: now})
	}
	for _, commit := range commits {
		if commit == head {
			prove(commit, store.FeedbackCompareIdentical)
		}
	}
	if previous != "" && previous != head {
		answer, ok := stored[descent]
		arrived := map[string]bool{}
		if !ok && w.ready(ctx) == nil {
			// The one request is charged just before it starts.
			admitted, err := w.Store.AdmitFeedbackComparisons(ctx, []store.FeedbackComparisonKey{descent}, w.now(), 1)
			if err != nil {
				return err
			}
			if len(admitted) == 1 {
				result, err := w.Forge.Compare(ctx, slug, previous, head, github.MaxArrivedCommits)
				if err == nil {
					answer, ok = store.FeedbackComparison{FeedbackComparisonKey: descent, Status: result.Status, ComparedAt: now}, true
					found = append(found, answer)
					for _, commit := range result.Commits {
						arrived[commit] = true
					}
				} else {
					w.logf("feedback_compare repository=%s outcome=pending category=%s", slug, github.ErrorCategory(github.CategorizeError(err)))
				}
			}
		}
		if ok && answer.Contains() {
			for _, commit := range commits {
				held := stored[store.FeedbackComparisonKey{Repository: slug, Base: commit, Head: previous}]
				if commit == previous || arrived[commit] || held.Contains() {
					prove(commit, store.FeedbackCompareAhead)
				}
			}
		}
	}
	if err := recordOutcome(ctx, func(writeCtx context.Context) error { return w.Store.RecordFeedbackComparisons(writeCtx, found) }); err != nil {
		return err
	}
	remaining := []store.FeedbackComparisonKey{}
	for _, commit := range commits {
		key := store.FeedbackComparisonKey{Repository: slug, Base: commit, Head: head}
		if _, ok := stored[key]; !ok && !proven[commit] {
			remaining = append(remaining, key)
		}
	}
	return w.CompareDirect(ctx, remaining)
}

func (w Work) storedComparisons(ctx context.Context, keys []store.FeedbackComparisonKey) (map[store.FeedbackComparisonKey]store.FeedbackComparison, error) {
	comparisons, err := w.Store.ListFeedbackComparisons(ctx, keys)
	if err != nil {
		return nil, err
	}
	out := make(map[store.FeedbackComparisonKey]store.FeedbackComparison, len(comparisons))
	for _, comparison := range comparisons {
		out[comparison.FeedbackComparisonKey] = comparison
	}
	return out, nil
}

// admitNext charges the next request the store admits among remaining, at
// the time it starts, and removes it from remaining. ok is false when the
// store admits none: every request is stored, in flight, or past the
// workspace allowance. Charging each request when it starts keeps the
// requests that actually start at most 60 in any minute, however long each
// takes and however many workers share the workspace.
func (w Work) admitNext(ctx context.Context, remaining *[]store.FeedbackComparisonKey) (store.FeedbackComparisonKey, bool, error) {
	// Once the job's time is over, no new request starts. Every pair not
	// started stays eligible for a later signal or read.
	if len(*remaining) == 0 || ctx.Err() != nil {
		return store.FeedbackComparisonKey{}, false, nil
	}
	admitted, err := w.Store.AdmitFeedbackComparisons(ctx, *remaining, w.now(), 1)
	if err != nil || len(admitted) == 0 {
		return store.FeedbackComparisonKey{}, false, err
	}
	key := admitted[0]
	*remaining = slices.DeleteFunc(*remaining, func(candidate store.FeedbackComparisonKey) bool {
		return store.NormalizeFeedbackComparisonKeys([]store.FeedbackComparisonKey{candidate})[0] == key
	})
	return key, true, nil
}

// CompareDirect asks GitHub for each pair the store admits and stores each
// answer. Each pair is admitted just before its request starts. The store
// puts pairs never tried before pairs tried earlier, so pairs that keep
// failing never hold back the others. A pair GitHub cannot compare because a
// commit is missing is stored as missing only when a read of the base commit
// proves it absent. A rate-limited answer stops the pass, and every pair not
// answered stays eligible for a later signal or read.
func (w Work) CompareDirect(ctx context.Context, keys []store.FeedbackComparisonKey) error {
	remaining := store.NormalizeFeedbackComparisonKeys(keys)
	if len(remaining) == 0 || w.ready(ctx) != nil {
		return nil
	}
	for {
		key, ok, err := w.admitNext(ctx, &remaining)
		if err != nil || !ok {
			return err
		}
		result, err := w.Forge.Compare(ctx, key.Repository, key.Base, key.Head, 0)
		status := result.Status
		if err != nil {
			status = ""
			category := github.ErrorCategory(github.CategorizeError(err))
			if github.HTTPStatus(err) == http.StatusNotFound {
				// The base read's own failure decides the category, so a
				// rate-limited base read stops the pass.
				_, readErr := w.Forge.ReadCommit(ctx, key.Repository, key.Base)
				switch {
				case errors.Is(readErr, github.ErrCommitNotFound):
					status = store.FeedbackCompareMissing
				case readErr != nil:
					category = github.ErrorCategory(github.CategorizeError(readErr))
				}
			}
			if status == "" {
				w.logf("feedback_compare repository=%s outcome=pending category=%s", key.Repository, category)
				if category == github.ForgeRateLimited {
					return nil
				}
				continue
			}
		}
		answer := []store.FeedbackComparison{{FeedbackComparisonKey: key, Status: status, ComparedAt: w.now()}}
		if err := recordOutcome(ctx, func(writeCtx context.Context) error { return w.Store.RecordFeedbackComparisons(writeCtx, answer) }); err != nil {
			return err
		}
	}
}

// BranchRef names a tracked branch of a repository.
type BranchRef struct {
	Slug   string
	Branch string
}

// RefreshBranches reads the head of each admitted tracked branch and stores
// it with its time. A branch read passes the same admission as a comparison.
// When GitHub confirms that the branch does not exist, the confirmed absence
// supersedes the stored head, so a read never reports containment from a head
// that GitHub disproved. A forge failure that proves nothing keeps the stored
// head.
func (w Work) RefreshBranches(ctx context.Context, branches []BranchRef) error {
	if len(branches) == 0 {
		return nil
	}
	keys := make([]store.FeedbackComparisonKey, len(branches))
	for i, branch := range branches {
		keys[i] = store.FeedbackComparisonKey{Repository: branch.Slug, Head: branch.Branch}
	}
	remaining := store.NormalizeFeedbackComparisonKeys(keys)
	if w.ready(ctx) != nil {
		return nil
	}
	for {
		key, ok, err := w.admitNext(ctx, &remaining)
		if err != nil || !ok {
			return err
		}
		commit, err := w.Forge.ReadCommit(ctx, key.Repository, "heads/"+key.Head)
		if errors.Is(err, github.ErrCommitNotFound) {
			w.logf("feedback_branch repository=%s outcome=absent", key.Repository)
			commit, err = store.FeedbackBranchAbsent, nil
		}
		if err != nil {
			w.logf("feedback_branch repository=%s outcome=pending category=%s", key.Repository, github.ErrorCategory(github.CategorizeError(err)))
			if github.ErrorCategory(github.CategorizeError(err)) == github.ForgeRateLimited {
				return nil
			}
			continue
		}
		head := store.FeedbackBranchHead{Repository: key.Repository, Branch: key.Head, Commit: commit, ObservedAt: w.now()}
		if err := recordOutcome(ctx, func(writeCtx context.Context) error { return w.Store.RecordFeedbackBranchHead(writeCtx, head) }); err != nil {
			return err
		}
	}
}
