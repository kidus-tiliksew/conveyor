package store

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Resolving and comparing bounds (component-production-feedback invariants 8
// and 14; feature-production-feedback-live FB-3).
const (
	// FeedbackResolutionRetry is the least time between two attempts to
	// resolve one version (req-production-feedback AC-1.4).
	FeedbackResolutionRetry = 10 * time.Minute
	// FeedbackComparisonsPerMinute bounds the comparisons that the
	// background work asks GitHub for in one workspace in any minute.
	FeedbackComparisonsPerMinute = 60
	// FeedbackComparisonWindow is the window of that bound.
	FeedbackComparisonWindow = time.Minute
	// FeedbackAdmissionLease is how long an admitted comparison or branch
	// read stays in flight. A second admission of the same request waits for
	// it, so concurrent signals, reads, and replicas never ask twice.
	FeedbackAdmissionLease = 2 * time.Minute
	// FeedbackBranchHeadAge is the age after which a read asks for a new
	// observation of a tracked branch head.
	FeedbackBranchHeadAge = 10 * time.Minute
)

// Unresolved reasons. A forge failure keeps its category from the GitHub
// boundary (component-git-delivery) as its reason.
const (
	FeedbackReasonPending       = "pending"
	FeedbackReasonNoGitHubApp   = "no_github_app"
	FeedbackReasonNoMatch       = "no_match"
	FeedbackReasonAmbiguous     = "ambiguous"
	FeedbackReasonNoVersionRule = "no_version_rule"
	FeedbackReasonRuleMismatch  = "version_rule_mismatch"
	FeedbackReasonNoRepository  = "no_repository"
)

// Comparison statuses. ahead, identical, behind, and diverged are GitHub's
// answer for base...head. missing records that GitHub reports the base commit
// missing from the repository.
const (
	FeedbackCompareAhead     = "ahead"
	FeedbackCompareIdentical = "identical"
	FeedbackCompareBehind    = "behind"
	FeedbackCompareDiverged  = "diverged"
	FeedbackCompareMissing   = "missing"
)

// FeedbackVersionRef names one version of a service in an environment by the
// identity FB-2 stores: the environment, the service, and the exact reported
// repository, commit, and version.
type FeedbackVersionRef struct {
	EnvironmentID string
	Service       string
	FeedbackVersion
}

// FeedbackResolution is the stored answer for one version. A resolved version
// holds the workspace repository and the full commit ID, and Reason is empty.
// An unresolved version keeps its reason and the earliest time a later
// signal can try it again (component-production-feedback invariants 7 and 8).
type FeedbackResolution struct {
	FeedbackVersionRef
	ResolvedRepository string
	ResolvedCommit     string
	Reason             string
	AttemptedAt        time.Time
	NextAttemptAt      time.Time
}

// Resolved reports whether the version holds a repository and a commit.
func (r FeedbackResolution) Resolved() bool { return r.ResolvedCommit != "" }

// FeedbackComparisonKey names one comparison of base with head in one forge
// repository. Repository is the canonical lower-case GitHub slug, and base
// and head are full commit IDs. A branch read uses Base "" and the branch
// name as Head; it is admitted like a comparison and never stored as one.
type FeedbackComparisonKey struct {
	Repository string
	Base       string
	Head       string
}

// FeedbackComparison is GitHub's stored answer for one comparison. It is
// never asked again (component-production-feedback invariant 14).
type FeedbackComparison struct {
	FeedbackComparisonKey
	Status     string
	ComparedAt time.Time
}

// Contains reports whether head holds base.
func (c FeedbackComparison) Contains() bool {
	return c.Status == FeedbackCompareAhead || c.Status == FeedbackCompareIdentical
}

// FeedbackBranchHead is the newest observed head of a tracked branch, read in
// the background through the forge boundary.
type FeedbackBranchHead struct {
	Repository string
	Branch     string
	Commit     string
	ObservedAt time.Time
}

// FeedbackMergedTask is one merged task with its configured repository and
// the merge commit and time of its latest merge.confirmed or merge.reconciled
// event that records a usable merge commit, so a later event without one does
// not hide it. MergeCommit is empty and MergedAt is zero when the ledger holds
// no usable merge commit for it.
type FeedbackMergedTask struct {
	TaskID      string
	Repository  string
	MergeCommit string
	MergedAt    time.Time
	EventID     int64
}

// FeedbackMergedTaskQuery narrows the merged-task read. Every set member
// narrows it; an empty query reads every merged task of the workspace.
type FeedbackMergedTaskQuery struct {
	TaskIDs    []string
	CrateID    string
	Repository string
}

// FeedbackInputs is the stored state of a set of environments that the
// status function reads.
type FeedbackInputs struct {
	Signals     map[string]FeedbackSignalState
	Running     map[string][]FeedbackRunningVersion
	Deploys     map[string][]FeedbackDeploy
	Resolutions []FeedbackResolution
}

// FeedbackStatusStore keeps what resolving and comparing learn, and reads
// what the status function needs (component-production-feedback; component-
// persistence). Every operation is scoped to the context's workspace. It
// appends no event and changes no task (invariant 1).
type FeedbackStatusStore interface {
	// ClaimFeedbackResolution takes one attempt to resolve a version for an
	// accepted signal that arrived at signalAt. It answers false and writes
	// nothing when the version is resolved, or when signalAt is earlier than
	// the stored next attempt time. Otherwise it records the attempt at now
	// with reason pending, so concurrent signals and replicas never both run
	// it, and answers true.
	ClaimFeedbackResolution(ctx context.Context, ref FeedbackVersionRef, signalAt, now time.Time) (bool, error)
	// RecordFeedbackResolution stores the outcome of a claimed attempt. It
	// never changes a resolved answer. A resolved answer also marks the
	// version's running and deploy rows resolved.
	RecordFeedbackResolution(ctx context.Context, resolution FeedbackResolution) error
	// ReadFeedbackInputs returns the newest signal, running versions, deploys,
	// and stored resolutions of the named environments.
	ReadFeedbackInputs(ctx context.Context, environmentIDs []string) (FeedbackInputs, error)
	// AdmitFeedbackComparisons admits, in the given order, the requests that
	// are neither stored nor in flight, up to the workspace's remaining
	// allowance in the minute before now. It records each admitted request at
	// now and returns them.
	AdmitFeedbackComparisons(ctx context.Context, keys []FeedbackComparisonKey, now time.Time) ([]FeedbackComparisonKey, error)
	// RecordFeedbackComparisons stores answers. A stored answer is kept.
	RecordFeedbackComparisons(ctx context.Context, comparisons []FeedbackComparison) error
	// ListFeedbackComparisons returns the stored answers among keys.
	ListFeedbackComparisons(ctx context.Context, keys []FeedbackComparisonKey) ([]FeedbackComparison, error)
	// RecordFeedbackBranchHead stores a branch head observation that is newer
	// than the stored one.
	RecordFeedbackBranchHead(ctx context.Context, head FeedbackBranchHead) error
	// ListFeedbackBranchHeads returns the workspace's branch head observations.
	ListFeedbackBranchHeads(ctx context.Context) ([]FeedbackBranchHead, error)
	// ListFeedbackMergedTasks returns merged tasks, newest merge first, then by
	// event ID and task ID.
	ListFeedbackMergedTasks(ctx context.Context, query FeedbackMergedTaskQuery) ([]FeedbackMergedTask, error)
}

// FeedbackRepository is the canonical form of a GitHub slug in a comparison
// key.
func FeedbackRepository(slug string) string { return strings.ToLower(strings.TrimSpace(slug)) }

// FeedbackCommitID reports whether commit is a full lower-case commit ID.
func FeedbackCommitID(commit string) bool {
	return len(commit) == 40 && strings.Trim(commit, "0123456789abcdef") == ""
}

// NormalizeFeedbackResolution checks an outcome before a backend stores it.
func NormalizeFeedbackResolution(resolution FeedbackResolution) (FeedbackResolution, error) {
	if resolution.EnvironmentID == "" || resolution.Service == "" {
		return FeedbackResolution{}, fmt.Errorf("a feedback resolution needs its environment and service")
	}
	resolution.AttemptedAt = EnvironmentTime(resolution.AttemptedAt)
	if resolution.Resolved() {
		if !FeedbackCommitID(resolution.ResolvedCommit) || resolution.ResolvedRepository == "" {
			return FeedbackResolution{}, fmt.Errorf("a resolved version needs a repository and a full commit ID")
		}
		resolution.Reason, resolution.NextAttemptAt = "", time.Time{}
		return resolution, nil
	}
	if resolution.Reason == "" {
		return FeedbackResolution{}, fmt.Errorf("an unresolved version needs its reason")
	}
	resolution.ResolvedRepository = ""
	least := resolution.AttemptedAt.Add(FeedbackResolutionRetry)
	if resolution.NextAttemptAt.Before(least) {
		resolution.NextAttemptAt = least
	}
	resolution.NextAttemptAt = EnvironmentTime(resolution.NextAttemptAt)
	return resolution, nil
}

// FeedbackClaimable reports whether a stored answer lets a signal that
// arrived at signalAt take a new attempt.
func FeedbackClaimable(stored FeedbackResolution, found bool, signalAt time.Time) bool {
	if !found {
		return true
	}
	return !stored.Resolved() && !signalAt.Before(stored.NextAttemptAt)
}

// FeedbackPendingResolution is the row a claim records.
func FeedbackPendingResolution(ref FeedbackVersionRef, now time.Time) FeedbackResolution {
	now = EnvironmentTime(now)
	return FeedbackResolution{FeedbackVersionRef: ref, Reason: FeedbackReasonPending, AttemptedAt: now, NextAttemptAt: now.Add(FeedbackResolutionRetry)}
}

// NormalizeFeedbackComparison checks an answer before a backend stores it.
func NormalizeFeedbackComparison(comparison FeedbackComparison) (FeedbackComparison, error) {
	comparison.Repository = FeedbackRepository(comparison.Repository)
	if comparison.Repository == "" || !FeedbackCommitID(comparison.Base) || !FeedbackCommitID(comparison.Head) {
		return FeedbackComparison{}, fmt.Errorf("a feedback comparison needs a repository and two full commit IDs")
	}
	switch comparison.Status {
	case FeedbackCompareAhead, FeedbackCompareIdentical, FeedbackCompareBehind, FeedbackCompareDiverged, FeedbackCompareMissing:
	default:
		return FeedbackComparison{}, fmt.Errorf("unknown feedback comparison status %q", comparison.Status)
	}
	comparison.ComparedAt = EnvironmentTime(comparison.ComparedAt)
	return comparison, nil
}

// NormalizeFeedbackComparisonKeys canonicalizes keys and drops repeats,
// keeping the first.
func NormalizeFeedbackComparisonKeys(keys []FeedbackComparisonKey) []FeedbackComparisonKey {
	out := make([]FeedbackComparisonKey, 0, len(keys))
	seen := make(map[FeedbackComparisonKey]bool, len(keys))
	for _, key := range keys {
		key.Repository = FeedbackRepository(key.Repository)
		if key.Repository == "" || key.Head == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out
}

// FeedbackAdmission is one admitted request as a backend stores it.
type FeedbackAdmission struct {
	FeedbackComparisonKey
	AdmittedAt time.Time
}

// PlanFeedbackAdmission decides which requests a backend admits, from the
// stored answers and admissions of the workspace. Every backend admits
// exactly this result.
func PlanFeedbackAdmission(keys []FeedbackComparisonKey, stored map[FeedbackComparisonKey]bool, admissions []FeedbackAdmission, now time.Time) []FeedbackComparisonKey {
	now = EnvironmentTime(now)
	used := 0
	inFlight := map[FeedbackComparisonKey]bool{}
	for _, admission := range admissions {
		if admission.AdmittedAt.After(now.Add(-FeedbackComparisonWindow)) {
			used++
		}
		if admission.AdmittedAt.After(now.Add(-FeedbackAdmissionLease)) {
			inFlight[admission.FeedbackComparisonKey] = true
		}
	}
	admitted := []FeedbackComparisonKey{}
	for _, key := range NormalizeFeedbackComparisonKeys(keys) {
		if used >= FeedbackComparisonsPerMinute {
			break
		}
		if stored[key] || inFlight[key] {
			continue
		}
		admitted = append(admitted, key)
		used++
	}
	return admitted
}

// SortFeedbackMergedTasks orders merged tasks newest merge first, then by
// event ID and task ID. A task without a merge commit sorts last.
func SortFeedbackMergedTasks(tasks []FeedbackMergedTask) {
	slices.SortFunc(tasks, func(a, b FeedbackMergedTask) int {
		if order := b.MergedAt.Compare(a.MergedAt); order != 0 {
			return order
		}
		if order := cmp.Compare(b.EventID, a.EventID); order != 0 {
			return order
		}
		return strings.Compare(a.TaskID, b.TaskID)
	})
}

// SortFeedbackResolutions orders resolutions by environment, service, and
// version.
func SortFeedbackResolutions(resolutions []FeedbackResolution) {
	slices.SortFunc(resolutions, func(a, b FeedbackResolution) int {
		if order := strings.Compare(a.EnvironmentID, b.EnvironmentID); order != 0 {
			return order
		}
		return compareFeedbackVersions(a.Service, a.FeedbackVersion, b.Service, b.FeedbackVersion)
	})
}

// SortFeedbackComparisons orders comparisons by repository, base, and head.
func SortFeedbackComparisons(comparisons []FeedbackComparison) {
	slices.SortFunc(comparisons, func(a, b FeedbackComparison) int {
		return slices.Compare([]string{a.Repository, a.Base, a.Head}, []string{b.Repository, b.Base, b.Head})
	})
}

// FeedbackMergeCommit returns the merge commit of a merge event payload when
// it is a commit ID. The event's commit is kept as recorded, lower-cased.
func FeedbackMergeCommit(sha string) string {
	sha = strings.ToLower(strings.TrimSpace(sha))
	if !FeedbackCommitID(sha) {
		return ""
	}
	return sha
}

// NormalizeFeedbackBranchHead checks an observation before a backend stores
// it.
func NormalizeFeedbackBranchHead(head FeedbackBranchHead) (FeedbackBranchHead, error) {
	head.Repository = FeedbackRepository(head.Repository)
	if head.Repository == "" || head.Branch == "" || !FeedbackCommitID(head.Commit) || head.ObservedAt.IsZero() {
		return FeedbackBranchHead{}, fmt.Errorf("a branch head needs a repository, a branch, a full commit ID, and its time")
	}
	head.ObservedAt = EnvironmentTime(head.ObservedAt)
	return head, nil
}

// SortFeedbackBranchHeads orders observations by repository and branch.
func SortFeedbackBranchHeads(heads []FeedbackBranchHead) {
	slices.SortFunc(heads, func(a, b FeedbackBranchHead) int {
		return slices.Compare([]string{a.Repository, a.Branch}, []string{b.Repository, b.Branch})
	})
}
