package reports

import (
	"slices"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/feedback"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// MaxTimingTasks is the most newly introduced tasks a qualifying deploy can
// link. A deploy that newly introduces more links none and gives the note
// too_many_tasks (component-feedback-reports invariant 14;
// req-production-feedback AC-5.3).
const MaxTimingTasks = 10

// TimingApplies reports whether the timing rule applies to a report: its
// first entry states a service (component-feedback-reports invariant 13).
func TimingApplies(report store.FeedbackReport) bool {
	return report.Service != ""
}

// TimingInput is what the timing rule reads for one report.
type TimingInput struct {
	Report store.FeedbackReport
	// TimingWindowMinutes is the environment's timing window.
	TimingWindowMinutes int
	// Deploys is the environment's deploy history.
	Deploys []store.FeedbackDeploy
	// PriorSignals holds, by deploy ID, the newest accepted watcher signal
	// before a deploy recorded from a signal that named the deploy's
	// service. SignalCandidates names the deploys the reader must look up.
	PriorSignals map[string]store.FeedbackBoundary
	// Resolutions are the stored answers of the environment's versions.
	Resolutions []store.FeedbackResolution
	// Slugs maps each configured repository name to its GitHub slug.
	Slugs map[string]string
	// Merged lists the merged tasks of the workspace.
	Merged []store.FeedbackMergedTask
	// Comparisons are the stored comparisons among the keys Missing named.
	Comparisons []store.FeedbackComparison
	// SignalAfterReport is true when a watcher signal that named the service
	// arrived after the report was first stored, and Listening is true when
	// a watcher is listening for the environment
	// (component-feedback-reports invariant 15).
	SignalAfterReport bool
	Listening         bool
}

// serviceDeploys returns the deploys of the report's service in the order of
// their places.
func serviceDeploys(deploys []store.FeedbackDeploy, service string) []store.FeedbackDeploy {
	out := []store.FeedbackDeploy{}
	for _, deploy := range deploys {
		if deploy.Service == service {
			out = append(out, deploy)
		}
	}
	slices.SortStableFunc(out, func(a, b store.FeedbackDeploy) int { return a.Start().Compare(b.Start()) })
	return out
}

// SignalCandidates returns the deploys recorded from a signal whose prior
// watcher signal the timing rule can need, in the order of their places: the
// deploys of the report's service at or after first_seen. A reader looks up
// each in turn and can stop after the first whose prior signal arrived after
// first_seen, because the prior signal of every later deploy arrived no
// earlier than that deploy (component-feedback-reports invariant 13).
func SignalCandidates(report store.FeedbackReport, deploys []store.FeedbackDeploy) []store.FeedbackDeploy {
	out := []store.FeedbackDeploy{}
	for _, deploy := range serviceDeploys(deploys, report.Service) {
		if deploy.Origin.Source == store.FeedbackSourceSignal && !deploy.DeployedAt.Before(report.FirstEntrySeen) {
			out = append(out, deploy)
		}
	}
	return out
}

// qualifying is one deploy that qualifies, with its evidence.
type qualifying struct {
	deploy  store.FeedbackDeploy
	elapsed int64
	prior   *time.Time
}

// qualify decides whether a deploy qualifies for the report's first_seen
// time, comparing exact stored times. In the first case first_seen is at or
// after the deploy's time and no more than the timing window after it, and
// elapsed_minutes is the whole minutes between them. In the second case the
// deploy was recorded from a signal, and first_seen falls between the
// arrival of the watcher signal before it that named the service and the
// deploy's time, both included. Then elapsed_minutes is 0 and the prior
// signal's arrival is kept (component-feedback-reports invariant 13;
// req-production-feedback AC-5.2; req-environments AC-1.3).
func qualify(deploy store.FeedbackDeploy, firstSeen time.Time, window time.Duration, priors map[string]store.FeedbackBoundary) (qualifying, bool) {
	if !firstSeen.Before(deploy.DeployedAt) && !firstSeen.After(deploy.DeployedAt.Add(window)) {
		return qualifying{deploy: deploy, elapsed: int64(firstSeen.Sub(deploy.DeployedAt) / time.Minute)}, true
	}
	if deploy.Origin.Source != store.FeedbackSourceSignal {
		return qualifying{}, false
	}
	prior, found := priors[deploy.ID]
	if !found || firstSeen.Before(prior.At) || firstSeen.After(deploy.DeployedAt) {
		return qualifying{}, false
	}
	at := prior.At
	return qualifying{deploy: deploy, elapsed: 0, prior: &at}, true
}

// evidenceDeploy is the deploy as evidence and notes name it.
func evidenceDeploy(deploy store.FeedbackDeploy) *store.FeedbackReportDeploy {
	return &store.FeedbackReportDeploy{ID: deploy.ID, Service: deploy.Service,
		Version:    store.FeedbackReportVersion{Repository: deploy.Repository, Commit: deploy.Commit, Version: deploy.Version},
		DeployedAt: deploy.DeployedAt, Source: deploy.Origin.Source}
}

// Timing runs the timing rule (component-feedback-reports invariants 13 to
// 15; req-production-feedback AC-5.2 to AC-5.4).
//
//   - A report the rule does not apply to ends with no link and no note.
//   - The rule waits until a watcher signal that names the service arrived
//     after the report was first stored, unless no watcher is listening.
//   - A qualifying deploy with no previous version is skipped.
//   - A qualifying deploy whose version or previous version has no resolved
//     commit, or whose two commits are in different repositories, gives the
//     note deploy_not_compared. A version whose resolution is still pending
//     leaves the rule waiting.
//   - The newly introduced tasks of a qualifying deploy are the merged tasks
//     of its repository that its version contains and its previous version
//     does not, by merge commit equality or the stored comparisons. A
//     missing comparison is never taken as an answer: the rule names it in
//     Missing and waits.
//   - More than MaxTimingTasks newly introduced tasks link none and give the
//     note too_many_tasks with the deploy and the number. None gives the
//     note no_task_deploy with the deploy. Otherwise each is linked as
//     possible with the deploy and elapsed_minutes, and prior_signal_at in
//     the second case.
func Timing(input TimingInput) Outcome {
	report := input.Report
	if !TimingApplies(report) {
		return Outcome{Ended: true}
	}
	if input.Listening && !input.SignalAfterReport {
		return Outcome{Waiting: WaitingForSignal}
	}
	window := time.Duration(input.TimingWindowMinutes) * time.Minute
	resolutions := map[store.FeedbackVersionRef]store.FeedbackResolution{}
	for _, resolution := range input.Resolutions {
		resolutions[resolution.FeedbackVersionRef] = resolution
	}
	comparisons := map[store.FeedbackComparisonKey]store.FeedbackComparison{}
	for _, comparison := range input.Comparisons {
		comparisons[comparison.FeedbackComparisonKey] = comparison
	}
	outcome := Outcome{}
	waiting := ""
	links := map[string][]store.FeedbackReportEvidence{}
	missing := map[store.FeedbackComparisonKey]bool{}
	for _, deploy := range serviceDeploys(input.Deploys, report.Service) {
		found, ok := qualify(deploy, report.FirstEntrySeen, window, input.PriorSignals)
		if !ok {
			continue
		}
		previousRef, hasPrevious := feedback.PreviousDeployVersion(input.Deploys, deploy)
		if !hasPrevious {
			continue
		}
		currentRef := store.FeedbackVersionRef{EnvironmentID: deploy.EnvironmentID, Service: deploy.Service, FeedbackVersion: deploy.FeedbackVersion}
		previousRef.EnvironmentID = deploy.EnvironmentID
		current, currentStored := resolutions[currentRef]
		previous, previousStored := resolutions[previousRef]
		if !currentStored || !previousStored || (!current.Resolved() && current.Reason == store.FeedbackReasonPending) || (!previous.Resolved() && previous.Reason == store.FeedbackReasonPending) {
			waiting = WaitingForResolution
			continue
		}
		slug := input.Slugs[current.ResolvedRepository]
		if !current.Resolved() || !previous.Resolved() || current.ResolvedRepository != previous.ResolvedRepository || slug == "" {
			outcome.Notes = append(outcome.Notes, store.FeedbackReportNote{Kind: store.FeedbackNoteDeployNotCompare, Deploy: evidenceDeploy(deploy)})
			continue
		}
		slug = store.FeedbackRepository(slug)
		contains := func(head, mergeCommit string) (bool, bool) {
			if head == mergeCommit {
				return true, true
			}
			key := store.FeedbackComparisonKey{Repository: slug, Base: mergeCommit, Head: head}
			answer, stored := comparisons[key]
			if !stored {
				missing[key] = true
				return false, false
			}
			return answer.Contains(), true
		}
		newly := []string{}
		complete := true
		seen := map[string]bool{}
		for _, task := range input.Merged {
			if task.Repository != current.ResolvedRepository || task.MergeCommit == "" || seen[task.TaskID] {
				continue
			}
			seen[task.TaskID] = true
			inCurrent, currentKnown := contains(current.ResolvedCommit, task.MergeCommit)
			inPrevious, previousKnown := contains(previous.ResolvedCommit, task.MergeCommit)
			if !currentKnown || !previousKnown {
				complete = false
				continue
			}
			if inCurrent && !inPrevious {
				newly = append(newly, task.TaskID)
			}
		}
		if !complete {
			waiting = WaitingForComparisons
			continue
		}
		switch {
		case len(newly) > MaxTimingTasks:
			outcome.Notes = append(outcome.Notes, store.FeedbackReportNote{Kind: store.FeedbackNoteTooManyTasks, Deploy: evidenceDeploy(deploy), TaskCount: len(newly)})
		case len(newly) == 0:
			outcome.Notes = append(outcome.Notes, store.FeedbackReportNote{Kind: store.FeedbackNoteNoTaskDeploy, Deploy: evidenceDeploy(deploy)})
		default:
			elapsed := found.elapsed
			piece := store.FeedbackReportEvidence{Kind: store.FeedbackEvidenceTiming, Deploy: evidenceDeploy(deploy), ElapsedMinutes: &elapsed, PriorSignalAt: found.prior}
			for _, taskID := range newly {
				links[taskID] = append(links[taskID], piece)
			}
		}
	}
	for key := range missing {
		outcome.Missing = append(outcome.Missing, key)
	}
	outcome.Missing = store.NormalizeFeedbackComparisonKeys(outcome.Missing)
	slices.SortFunc(outcome.Missing, func(a, b store.FeedbackComparisonKey) int {
		return slices.Compare([]string{a.Repository, a.Base, a.Head}, []string{b.Repository, b.Base, b.Head})
	})
	if waiting != "" {
		return Outcome{Waiting: waiting, Missing: outcome.Missing}
	}
	outcome.Ended = true
	for taskID, evidence := range links {
		outcome.Links = append(outcome.Links, store.FeedbackReportLinkCandidate{TaskID: taskID, Evidence: evidence})
	}
	slices.SortFunc(outcome.Links, func(a, b store.FeedbackReportLinkCandidate) int { return compareStrings(a.TaskID, b.TaskID) })
	return outcome
}
