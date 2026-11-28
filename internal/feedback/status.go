package feedback

import (
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Live statuses (component-production-feedback, Public surfaces).
const (
	StatusLive         = "live"
	StatusNotLiveYet   = "not_live_yet"
	StatusNoLongerLive = "no_longer_live"
	StatusUnknown      = "unknown"
)

// Inputs is everything the status function reads, all from storage, and the
// read time. Slugs maps each configured repository name to its GitHub slug.
type Inputs struct {
	Now          time.Time
	Environments []store.Environment
	State        store.FeedbackInputs
	Comparisons  []store.FeedbackComparison
	BranchHeads  []store.FeedbackBranchHead
	Slugs        map[string]string
}

// EnvironmentRef names an environment in a served status.
type EnvironmentRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// TaskStatus is one merged task's status in one relevant environment
// (component-production-feedback invariants 16 and 17).
type TaskStatus struct {
	Environment     EnvironmentRef `json:"environment"`
	Status          string         `json:"status"`
	Services        []string       `json:"services"`
	LiveSince       *time.Time     `json:"live_since"`
	OnTrackedBranch bool           `json:"on_tracked_branch"`
	AsOf            *time.Time     `json:"as_of"`
}

// CrateLine is one crate line for one environment (invariant 18).
type CrateLine struct {
	Environment     EnvironmentRef `json:"environment"`
	LiveTaskCount   int            `json:"live_task_count"`
	MergedTaskCount int            `json:"merged_task_count"`
	NewestLiveAt    *time.Time     `json:"newest_live_at"`
	Listening       bool           `json:"listening"`
	LastSignalAt    *time.Time     `json:"last_signal_at"`
	AsOf            *time.Time     `json:"as_of"`
	// OpenReportCount is the number of open reports of the environment that
	// belong to the crate (component-feedback-reports invariant 23;
	// req-production-feedback AC-6.2).
	OpenReportCount int `json:"open_report_count"`
}

// Missing names the stored answers that a status needs and storage lacks.
// A read requests them in the background and leaves that status out
// (invariant 20).
type Missing struct {
	Comparisons []store.FeedbackComparisonKey
	Branches    []BranchRef
}

func (m *Missing) add(other Missing) {
	m.Comparisons = append(m.Comparisons, other.Comparisons...)
	m.Branches = append(m.Branches, other.Branches...)
}

// unique drops repeated requests, keeping the first of each.
func (m Missing) unique() Missing {
	out := Missing{Comparisons: store.NormalizeFeedbackComparisonKeys(m.Comparisons)}
	seen := map[BranchRef]bool{}
	for _, branch := range m.Branches {
		if !seen[branch] {
			seen[branch] = true
			out.Branches = append(out.Branches, branch)
		}
	}
	return out
}

// Empty reports whether nothing is missing.
func (m Missing) Empty() bool { return len(m.Comparisons) == 0 && len(m.Branches) == 0 }

// view indexes the inputs for one derivation.
type view struct {
	in          Inputs
	resolutions map[store.FeedbackVersionRef]store.FeedbackResolution
	comparisons map[store.FeedbackComparisonKey]store.FeedbackComparison
	heads       map[BranchRef]store.FeedbackBranchHead
}

func newView(in Inputs) view {
	v := view{in: in, resolutions: map[store.FeedbackVersionRef]store.FeedbackResolution{}, comparisons: map[store.FeedbackComparisonKey]store.FeedbackComparison{}, heads: map[BranchRef]store.FeedbackBranchHead{}}
	for _, resolution := range in.State.Resolutions {
		if resolution.Resolved() {
			v.resolutions[resolution.FeedbackVersionRef] = resolution
		}
	}
	for _, comparison := range in.Comparisons {
		v.comparisons[comparison.FeedbackComparisonKey] = comparison
	}
	for _, head := range in.BranchHeads {
		v.heads[BranchRef{Slug: head.Repository, Branch: head.Branch}] = head
	}
	return v
}

func (v view) environments() []store.Environment {
	out := []store.Environment{}
	for _, environment := range v.in.Environments {
		if !environment.Archived {
			out = append(out, environment)
		}
	}
	slices.SortFunc(out, func(a, b store.Environment) int {
		if order := strings.Compare(store.EnvironmentFold(a.Name), store.EnvironmentFold(b.Name)); order != 0 {
			return order
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

// deployRepository is the repository a deploy is for: its resolved
// repository, or the repository the signal stated.
func (v view) deployRepository(environmentID, service string, version store.FeedbackVersion) (string, store.FeedbackResolution, bool) {
	resolution, ok := v.resolutions[store.FeedbackVersionRef{EnvironmentID: environmentID, Service: service, FeedbackVersion: version}]
	if ok {
		return resolution.ResolvedRepository, resolution, true
	}
	return version.Repository, store.FeedbackResolution{}, false
}

// relevant reports whether an unarchived environment holds a deploy for the
// repository or tracks a branch for it (invariant 15).
func (v view) relevant(environment store.Environment, repository string) bool {
	if environment.TrackedBranches[repository] != "" {
		return true
	}
	for _, deploy := range v.in.State.Deploys[environment.ID] {
		if name, _, _ := v.deployRepository(environment.ID, deploy.Service, deploy.FeedbackVersion); name == repository {
			return true
		}
	}
	return false
}

// asOf is the time of the newest signal or GitHub deployment report of the
// environment (invariant 17).
func (v view) asOf(environmentID string) *time.Time {
	var newest time.Time
	if signal, ok := v.in.State.Signals[environmentID]; ok {
		newest = signal.LastSignalAt
	}
	for _, deploy := range v.in.State.Deploys[environmentID] {
		if deploy.Origin.Source != store.FeedbackSourceSignal && deploy.DeployedAt.After(newest) {
			newest = deploy.DeployedAt
		}
	}
	if newest.IsZero() {
		return nil
	}
	return &newest
}

// containment is what storage proves about one commit holding a merge
// commit.
type containment int

const (
	pending containment = iota
	contained
	notContained
	mergeMissing
)

func (v view) contains(slug, merge, head string, missing *Missing) containment {
	if merge == head {
		return contained
	}
	key := store.FeedbackComparisonKey{Repository: slug, Base: merge, Head: head}
	comparison, ok := v.comparisons[key]
	switch {
	case !ok:
		missing.Comparisons = append(missing.Comparisons, key)
		return pending
	case comparison.Status == store.FeedbackCompareMissing:
		return mergeMissing
	case comparison.Contains():
		return contained
	}
	return notContained
}

// taskStatus derives one task's status in one relevant environment. ok is
// false when the status is left out. awaiting is true only when it is left
// out because a comparison or a tracked branch head is not stored yet, so a
// read carries environments_pending (component-production-feedback
// invariant 20). A status left out for want of a merge commit or a
// repository slug needs no background work and is not pending.
func (v view) taskStatus(environment store.Environment, task store.FeedbackMergedTask) (status TaskStatus, ok, awaiting bool, missing Missing) {
	slug := store.FeedbackRepository(v.in.Slugs[task.Repository])
	status = TaskStatus{Environment: EnvironmentRef{ID: environment.ID, Name: environment.Name}, Services: []string{}, AsOf: v.asOf(environment.ID)}
	if task.MergeCommit == "" || slug == "" {
		return status, false, false, missing
	}
	waiting, unknown := false, false
	judge := func(service string, version store.FeedbackVersion) containment {
		name, resolution, resolved := v.deployRepository(environment.ID, service, version)
		if !resolved || name != task.Repository {
			// An unresolved version, or one of another repository, contains
			// no task (invariant 14; req-production-feedback AC-3.1).
			return notContained
		}
		answer := v.contains(slug, task.MergeCommit, resolution.ResolvedCommit, &missing)
		switch answer {
		case pending:
			waiting = true
		case mergeMissing:
			unknown = true
		}
		return answer
	}
	// Live is decided from the running versions first. When every running
	// version has its answer and one of them contains the task, the services
	// and live_since are proven, and no past deploy can change them, so a
	// missing answer for a stopped version does not hide the status
	// (component-production-feedback invariants 16, 18, and 20;
	// req-production-feedback AC-3.1).
	var liveSince time.Time
	services := map[string]bool{}
	for _, running := range v.in.State.Running[environment.ID] {
		if judge(running.Service, running.FeedbackVersion) == contained {
			services[running.Service] = true
			if liveSince.IsZero() || running.DeployedAt.Before(liveSince) {
				liveSince = running.DeployedAt
			}
		}
	}
	if len(services) > 0 && !waiting && !unknown {
		status.Status = StatusLive
		status.Services = slices.Sorted(maps.Keys(services))
		status.LiveSince = &liveSince
		return status, true, false, missing
	}
	// No running version proves live, so the past deploys tell the other
	// statuses apart.
	pastContained := false
	for _, deploy := range v.in.State.Deploys[environment.ID] {
		if judge(deploy.Service, deploy.FeedbackVersion) == contained {
			pastContained = true
		}
	}
	switch {
	case unknown:
		status.Status = StatusUnknown
		return status, true, false, Missing{}
	case waiting:
		return status, false, true, missing
	case pastContained:
		status.Status = StatusNoLongerLive
		return status, true, false, missing
	}
	// No deploy contains the task, so the tracked branch is read now
	// (req-production-feedback AC-3.2).
	status.Status = StatusNotLiveYet
	branch := environment.TrackedBranches[task.Repository]
	if branch == "" {
		return status, true, false, missing
	}
	ref := BranchRef{Slug: slug, Branch: branch}
	head, stored := v.heads[ref]
	if !stored {
		missing.Branches = append(missing.Branches, ref)
		return status, false, true, missing
	}
	// A stored head older than its age limit is still served, and a newer
	// one is requested, so it does not make the status pending.
	if v.in.Now.Sub(head.ObservedAt) >= store.FeedbackBranchHeadAge {
		missing.Branches = append(missing.Branches, ref)
	}
	if head.Absent() {
		// GitHub confirmed that the branch does not exist, so nothing is on
		// it. An older head it superseded is never used.
		return status, true, false, missing
	}
	switch v.contains(slug, task.MergeCommit, head.Commit, &missing) {
	case pending:
		return status, false, true, missing
	case mergeMissing:
		status.Status = StatusUnknown
	case contained:
		status.OnTrackedBranch = true
	}
	return status, true, false, missing
}

// TaskStatuses returns a merged task's status in each relevant unarchived
// environment, ordered by environment name, whether a status was left out
// because an answer is not stored yet, and what storage still lacks
// (component-production-feedback invariants 15 to 17 and 20;
// req-production-feedback AC-3.1, AC-3.2, and AC-3.6). It does no I/O.
// pending is separate from missing: a served stale branch head is missing
// and not pending.
func TaskStatuses(in Inputs, task store.FeedbackMergedTask) (statuses []TaskStatus, pending bool, missing Missing) {
	v := newView(in)
	statuses = []TaskStatus{}
	for _, environment := range v.environments() {
		if !v.relevant(environment, task.Repository) {
			continue
		}
		status, ok, waiting, lacking := v.taskStatus(environment, task)
		missing.add(lacking)
		pending = pending || waiting
		if ok {
			statuses = append(statuses, status)
		}
	}
	return statuses, pending, missing.unique()
}

// CrateStatus returns one line for each unarchived environment relevant to at
// least one merged member, the environments where each member is live,
// whether any member's status was left out because an answer is not stored
// yet, and what storage still lacks (invariants 18 and 20;
// req-production-feedback AC-3.4). members are every merged member of the
// crate, whatever their merge commit. A pending member changes no line,
// count, or live_in entry. openReports holds the number of open reports that
// belong to the crate by environment ID, and each line carries its
// environment's number, 0 when it has none; it decides no line
// (component-feedback-reports invariant 23).
func CrateStatus(in Inputs, members []store.FeedbackMergedTask, openReports map[string]int) (lines []CrateLine, liveIn map[string][]string, pending bool, missing Missing) {
	v := newView(in)
	lines = []CrateLine{}
	liveIn = map[string][]string{}
	for _, environment := range v.environments() {
		line := CrateLine{Environment: EnvironmentRef{ID: environment.ID, Name: environment.Name}, MergedTaskCount: len(members), AsOf: v.asOf(environment.ID), OpenReportCount: openReports[environment.ID]}
		relevant := false
		for _, member := range members {
			if !v.relevant(environment, member.Repository) {
				continue
			}
			relevant = true
			status, ok, waiting, lacking := v.taskStatus(environment, member)
			missing.add(lacking)
			pending = pending || waiting
			if !ok || status.Status != StatusLive {
				continue
			}
			line.LiveTaskCount++
			liveIn[member.TaskID] = append(liveIn[member.TaskID], environment.Name)
			if line.NewestLiveAt == nil || status.LiveSince.After(*line.NewestLiveAt) {
				line.NewestLiveAt = status.LiveSince
			}
		}
		if !relevant {
			continue
		}
		if signal, ok := v.in.State.Signals[environment.ID]; ok {
			at := signal.LastSignalAt
			line.Listening, line.LastSignalAt = signal.Listening(v.in.Now), &at
		}
		lines = append(lines, line)
	}
	return lines, liveIn, pending, missing.unique()
}

// RequiredComparisons returns every stored answer that a derivation for the
// tasks can read: each resolved version of the task's repository in a
// relevant environment, and each observed tracked branch head, against the
// task's merge commit. A read loads them before it derives.
func RequiredComparisons(in Inputs, tasks []store.FeedbackMergedTask) []store.FeedbackComparisonKey {
	v := newView(in)
	keys := []store.FeedbackComparisonKey{}
	for _, task := range tasks {
		slug := store.FeedbackRepository(in.Slugs[task.Repository])
		if task.MergeCommit == "" || slug == "" {
			continue
		}
		for _, environment := range v.environments() {
			for _, deploy := range in.State.Deploys[environment.ID] {
				if name, resolution, ok := v.deployRepository(environment.ID, deploy.Service, deploy.FeedbackVersion); ok && name == task.Repository {
					keys = append(keys, store.FeedbackComparisonKey{Repository: slug, Base: task.MergeCommit, Head: resolution.ResolvedCommit})
				}
			}
			for _, running := range in.State.Running[environment.ID] {
				if name, resolution, ok := v.deployRepository(environment.ID, running.Service, running.FeedbackVersion); ok && name == task.Repository {
					keys = append(keys, store.FeedbackComparisonKey{Repository: slug, Base: task.MergeCommit, Head: resolution.ResolvedCommit})
				}
			}
			if branch := environment.TrackedBranches[task.Repository]; branch != "" {
				if head, ok := v.heads[BranchRef{Slug: slug, Branch: branch}]; ok && !head.Absent() {
					keys = append(keys, store.FeedbackComparisonKey{Repository: slug, Base: task.MergeCommit, Head: head.Commit})
				}
			}
		}
	}
	return store.NormalizeFeedbackComparisonKeys(keys)
}

// NewestUnresolved returns the running version of an environment that was
// deployed last among those without a resolved answer, with its reason. A
// version no attempt has reached yet has reason pending
// (component-production-feedback invariant 21; req-production-feedback
// AC-2.3).
func NewestUnresolved(state store.FeedbackInputs, environmentID string) (store.FeedbackRunningVersion, string, bool) {
	answers := map[store.FeedbackVersionRef]store.FeedbackResolution{}
	for _, resolution := range state.Resolutions {
		answers[resolution.FeedbackVersionRef] = resolution
	}
	var newest store.FeedbackRunningVersion
	reason, found := "", false
	for _, running := range state.Running[environmentID] {
		answer, ok := answers[store.FeedbackVersionRef{EnvironmentID: environmentID, Service: running.Service, FeedbackVersion: running.FeedbackVersion}]
		if ok && answer.Resolved() {
			continue
		}
		if found && !running.DeployedAt.After(newest.DeployedAt) {
			continue
		}
		newest, found, reason = running, true, store.FeedbackReasonPending
		if ok {
			reason = answer.Reason
		}
	}
	return newest, reason, found
}
