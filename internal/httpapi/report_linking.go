package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"slices"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/feedback"
	"github.com/kidus-tiliksew/conveyor/internal/reports"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// feedbackReportsRequest asks for the linking of an environment's pending
// reports. A nil key list asks for every pending report, as an accepted
// signal does. Otherwise only the reports of the listed problem keys are
// asked for, as the report route asks for the reports it stored.
type feedbackReportsRequest struct {
	environmentID string
	keys          []string
}

// feedbackLineReadPrefix starts the admission key of a line read. A Git ref
// name cannot hold a colon, and a comparison key holds a full commit ID as
// its base, so the key is never a branch read or a comparison, and nothing
// stores it as a comparison (component-feedback-reports invariant 15;
// component-production-feedback invariant 28).
const feedbackLineReadPrefix = "line-read:"

// The reasons a rule waits that the linking work names itself.
const (
	// feedbackWaitingAllowance: the workspace's 60 requests of the minute
	// are spent, or the line read is in flight elsewhere.
	feedbackWaitingAllowance = "github_allowance"
	// feedbackWaitingNoRepository: the read's repository is not in the
	// workspace configuration any more.
	feedbackWaitingNoRepository = "repository_not_configured"
)

// enqueueFeedbackReports asks for the linking of the reports a report request
// stored. The route calls it only after its answer is written and flushed, so
// the route answers before any link is worked out and before any forge call
// (component-feedback-reports invariant 15).
func (s *Server) enqueueFeedbackReports(workspace string, batch store.FeedbackReportBatch) {
	keys := make([]string, 0, len(batch.Entries))
	for _, entry := range batch.Entries {
		keys = append(keys, entry.Key)
	}
	s.enqueueFeedbackPendingReports(workspace, batch.EnvironmentID, keys)
}

// enqueueFeedbackPendingReports queues one job that reads the environment's
// pending reports and queues the linking job of each that the request names.
func (s *Server) enqueueFeedbackPendingReports(workspace, environmentID string, keys []string) {
	request := &feedbackReportsRequest{environmentID: environmentID}
	key := "reports:" + workspace + ":" + environmentID
	if keys != nil {
		request.keys = slices.Clone(keys)
		sorted := slices.Clone(keys)
		slices.Sort(sorted)
		digests := make([]string, len(sorted))
		for i, problem := range sorted {
			digests[i] = store.FeedbackReportKeyDigest(problem)
		}
		sum := sha256.Sum256([]byte(strings.Join(digests, ",")))
		key += ":" + hex.EncodeToString(sum[:])
	}
	s.feedbackWorker().enqueue(feedbackJob{key: key, workspace: workspace, reports: request, env: s.feedbackJobEnv()})
}

// enqueueFeedbackReportLinking queues the linking job of one report. The job
// is deduplicated by report: a report whose job is queued or running is not
// queued again.
func (s *Server) enqueueFeedbackReportLinking(workspace, reportID string) {
	s.feedbackWorker().enqueue(feedbackJob{key: "report:" + workspace + ":" + reportID, workspace: workspace, report: reportID, env: s.feedbackJobEnv()})
}

// feedbackReportErrorClass names a failure of the linking work for a log
// line: a forge failure category, or the kind of a store failure. It holds no
// text of the failure, so no report text reaches a log
// (component-feedback-reports invariant 29).
func feedbackReportErrorClass(err error) string {
	switch {
	case errors.Is(err, feedback.ErrNoGitHubApp):
		return store.FeedbackReasonNoGitHubApp
	case github.ErrorCategory(err) != "":
		return string(github.ErrorCategory(err))
	case errors.Is(err, store.ErrRetryable):
		return "store_retryable"
	case errors.Is(err, store.ErrNotFound):
		return "not_found"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "timeout"
	}
	return "store_failure"
}

func logFeedbackReport(workspace, environmentID, reportID, outcome, step string, err error) {
	if err != nil {
		log.Printf("feedback_report_link workspace=%s environment_id=%s report_id=%s outcome=%s step=%s class=%s", workspace, environmentID, reportID, outcome, step, feedbackReportErrorClass(err))
		return
	}
	log.Printf("feedback_report_link workspace=%s environment_id=%s report_id=%s outcome=%s step=%s", workspace, environmentID, reportID, outcome, step)
}

// runFeedbackPendingReports reads the environment's pending reports and
// queues the linking job of each that the request names. A read that fails
// is logged and queues nothing. A later signal, a later entry, or a read asks
// again.
func runFeedbackPendingReports(ctx context.Context, job feedbackJob) {
	backend, ok := job.env.store.(store.ReportStore)
	if !ok || job.env.linkReport == nil {
		return
	}
	pending, err := backend.ListPendingFeedbackReports(ctx, job.reports.environmentID)
	if err != nil {
		logFeedbackReport(job.workspace, job.reports.environmentID, "", "failed", "pending", err)
		return
	}
	for _, report := range pending {
		if job.reports.keys != nil && !slices.Contains(job.reports.keys, report.Key) {
			continue
		}
		job.env.linkReport(job.workspace, report.ID)
	}
}

// runFeedbackReportLinking runs one linking attempt of one report
// (component-feedback-reports invariants 11 to 15; feature-production-
// feedback-listening FR-2). The attempt is claimed in the store first, at
// most once a minute for the report, and a report whose linking ended is
// skipped there, before any forge call. The line rule reuses a stored line
// read, and otherwise resolves the first entry's commit or version and asks
// GitHub once, through the workspace allowance. The timing rule reads stored
// records only and asks the background work for missing comparisons. When
// both rules have ended, or 24 hours have passed since the report was first
// stored, the links, the notes, and the linked event are written in one
// transaction. Every store failure stops the attempt and is logged by class.
func runFeedbackReportLinking(ctx context.Context, job feedbackJob) {
	reportStore, ok := job.env.store.(store.ReportStore)
	feedbackStore, isFeedback := job.env.store.(store.FeedbackStore)
	if !ok || !isFeedback {
		return
	}
	report, claimed, err := reportStore.ClaimFeedbackReportLinking(ctx, job.report, job.env.clock())
	if err != nil {
		logFeedbackReport(job.workspace, "", job.report, "failed", "claim", err)
		return
	}
	if !claimed {
		return
	}
	attempt := feedbackReportAttempt{job: job, reports: reportStore, feedback: feedbackStore, report: report}
	if err := attempt.run(ctx); err != nil {
		logFeedbackReport(job.workspace, report.EnvironmentID, report.ID, "failed", attempt.step, err)
	}
}

// feedbackReportAttempt is one claimed linking attempt.
type feedbackReportAttempt struct {
	job      feedbackJob
	reports  store.ReportStore
	feedback store.FeedbackStore
	report   store.FeedbackReport
	// step names the step that failed, for the log line.
	step string
}

func (a *feedbackReportAttempt) run(ctx context.Context) error {
	a.step = "load"
	environment, found, err := feedbackReportEnvironment(ctx, a.job.env.store, a.report.EnvironmentID)
	if err != nil {
		return err
	}
	if !found {
		return store.ErrNotFound
	}
	repositories, err := feedbackRepositories(ctx, a.job.env.store)
	if err != nil {
		return err
	}
	a.step = "merged"
	merged, err := a.feedback.ListFeedbackMergedTasks(ctx, store.FeedbackMergedTaskQuery{})
	if err != nil {
		return err
	}
	a.step = "line"
	line, err := a.lineRule(ctx, environment, repositories, merged)
	if err != nil {
		return err
	}
	a.step = "timing"
	timing, err := a.timingRule(ctx, environment, repositories, merged)
	if err != nil {
		return err
	}
	linking, ended := reports.Decide(a.report, line, timing, a.job.env.clock())
	if !ended {
		logFeedbackReport(a.job.workspace, a.report.EnvironmentID, a.report.ID, "pending", "decide", nil)
		return nil
	}
	a.step = "finish"
	var finished bool
	if err := recordFeedbackReportOutcome(ctx, func(writeCtx context.Context) error {
		var err error
		finished, err = a.reports.FinishFeedbackReportLinking(writeCtx, linking)
		return err
	}); err != nil {
		return err
	}
	if finished {
		logFeedbackReport(a.job.workspace, a.report.EnvironmentID, a.report.ID, "linked", "finish", nil)
	}
	return nil
}

// recordFeedbackReportOutcome saves an outcome on a context that keeps the
// job's workspace and actor and has its own bound, so a save that a forge
// call produced still runs when the job's time ran out.
func recordFeedbackReportOutcome(ctx context.Context, save func(context.Context) error) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), feedback.OutcomeWriteTimeout)
	defer cancel()
	return save(writeCtx)
}

// feedbackReportEnvironment reads one environment of the workspace,
// archived ones included.
func feedbackReportEnvironment(ctx context.Context, backend store.Store, environmentID string) (store.Environment, bool, error) {
	environments, ok := backend.(store.EnvironmentStore)
	if !ok {
		return store.Environment{}, false, nil
	}
	listed, err := environments.ListEnvironments(ctx, store.EnvironmentListQuery{})
	if err != nil {
		return store.Environment{}, false, err
	}
	for _, environment := range listed {
		if environment.ID == environmentID {
			return environment, true, nil
		}
	}
	return store.Environment{}, false, nil
}

// lineRule runs the line rule for the attempt. A stored line read is reused
// and GitHub is not asked again. Otherwise the first entry's commit or
// version is resolved as component-production-feedback invariant 7 resolves
// a version, and the read is admitted against the workspace allowance just
// before it starts. A definitive answer, a missing file included, is stored
// before the rule uses it. A forge failure leaves the rule waiting with its
// category and stores nothing.
func (a *feedbackReportAttempt) lineRule(ctx context.Context, environment store.Environment, repositories []feedback.Repository, merged []store.FeedbackMergedTask) (reports.Outcome, error) {
	report := a.report
	if !reports.LineApplies(report) {
		return reports.Line(reports.LineInput{Report: report}), nil
	}
	read, stored, err := a.reports.ReadFeedbackReportLineRead(ctx, report.ID)
	if err != nil {
		return reports.Outcome{}, err
	}
	if stored {
		return reports.Line(reports.LineInput{Report: report, Read: &read, Merged: merged}), nil
	}
	forge := a.job.env.lazyForge(a.job.workspace)
	version := store.FeedbackVersion{Repository: report.First.Repository, Commit: report.First.Commit, Version: report.First.Version}
	resolution := feedback.Resolve(ctx, forge, feedback.ResolveRequest{Version: version, Service: report.Service, Rule: environment.VersionRule, Repositories: repositories})
	if resolution.Commit == "" {
		return reports.Line(reports.LineInput{Report: report, Unresolved: resolution.Reason}), nil
	}
	slug := ""
	for _, repository := range repositories {
		if repository.Name == resolution.Repository {
			slug = repository.Slug
		}
	}
	if slug == "" {
		return reports.Outcome{Waiting: feedbackWaitingNoRepository}, nil
	}
	key := store.FeedbackComparisonKey{Repository: slug, Head: feedbackLineReadPrefix + report.ID}
	admitted, err := a.feedback.AdmitFeedbackComparisons(ctx, []store.FeedbackComparisonKey{key}, a.job.env.clock(), 1)
	if err != nil {
		return reports.Outcome{}, err
	}
	if len(admitted) == 0 {
		return reports.Outcome{Waiting: feedbackWaitingAllowance}, nil
	}
	answer, err := forge.ReadLineChange(ctx, store.FeedbackRepository(slug), resolution.Commit, report.File, report.Line)
	if err != nil {
		reason := feedbackReportErrorClass(err)
		logFeedbackReport(a.job.workspace, report.EnvironmentID, report.ID, "pending", "line_read", err)
		return reports.Outcome{Waiting: reason}, nil
	}
	read = store.FeedbackReportLineRead{ReportID: report.ID, Repository: resolution.Repository, ResolvedCommit: resolution.Commit, File: report.File, Line: report.Line,
		Answer:     store.FeedbackReportLineAnswer{FileFound: answer.FileFound, Commit: answer.Commit, PullRequests: answer.PullRequests},
		AnsweredAt: a.job.env.clock()}
	if err := recordFeedbackReportOutcome(ctx, func(writeCtx context.Context) error { return a.reports.RecordFeedbackReportLineRead(writeCtx, read) }); err != nil {
		return reports.Outcome{}, err
	}
	// The stored answer is the one the rule uses: an answer that another
	// attempt stored first is kept, and this one is dropped.
	read, stored, err = a.reports.ReadFeedbackReportLineRead(ctx, report.ID)
	if err != nil {
		return reports.Outcome{}, err
	}
	if !stored {
		return reports.Outcome{}, store.ErrNotFound
	}
	return reports.Line(reports.LineInput{Report: report, Read: &read, Merged: merged}), nil
}

// timingRule runs the timing rule for the attempt from stored records. It
// asks the background work for the comparisons the rule still needs.
func (a *feedbackReportAttempt) timingRule(ctx context.Context, environment store.Environment, repositories []feedback.Repository, merged []store.FeedbackMergedTask) (reports.Outcome, error) {
	report := a.report
	if !reports.TimingApplies(report) {
		return reports.Timing(reports.TimingInput{Report: report}), nil
	}
	now := a.job.env.clock()
	signals, err := a.feedback.ListFeedbackSignals(ctx, []string{report.EnvironmentID})
	if err != nil {
		return reports.Outcome{}, err
	}
	state, hasSignal := signals[report.EnvironmentID]
	listening := hasSignal && state.Listening(now)
	created := report.CreatedAt
	_, signalAfter, err := a.feedback.FindFeedbackServiceSignal(ctx, store.FeedbackServiceSignalQuery{EnvironmentID: report.EnvironmentID, Service: report.Service, After: &created})
	if err != nil {
		return reports.Outcome{}, err
	}
	inputs, err := a.feedback.ReadFeedbackInputs(ctx, []string{report.EnvironmentID})
	if err != nil {
		return reports.Outcome{}, err
	}
	deploys := inputs.Deploys[report.EnvironmentID]
	priors := map[string]store.FeedbackBoundary{}
	for _, deploy := range reports.SignalCandidates(report, deploys) {
		before := deploy.Start()
		prior, found, err := a.feedback.FindFeedbackServiceSignal(ctx, store.FeedbackServiceSignalQuery{EnvironmentID: report.EnvironmentID, Service: report.Service, Before: &before})
		if err != nil {
			return reports.Outcome{}, err
		}
		if found {
			priors[deploy.ID] = prior
		}
		// The prior signal of every later deploy arrived no earlier than this
		// deploy, so none of them can qualify by its prior signal.
		if deploy.DeployedAt.After(report.FirstEntrySeen) {
			break
		}
	}
	slugs := map[string]string{}
	for _, repository := range repositories {
		slugs[repository.Name] = repository.Slug
	}
	input := reports.TimingInput{Report: report, TimingWindowMinutes: environment.TimingWindowMinutes, Deploys: deploys, PriorSignals: priors,
		Resolutions: inputs.Resolutions, Slugs: slugs, Merged: merged, SignalAfterReport: signalAfter, Listening: listening}
	outcome := reports.Timing(input)
	if len(outcome.Missing) == 0 {
		return outcome, nil
	}
	if input.Comparisons, err = a.feedback.ListFeedbackComparisons(ctx, outcome.Missing); err != nil {
		return reports.Outcome{}, err
	}
	outcome = reports.Timing(input)
	if len(outcome.Missing) > 0 && a.job.env.requestAnswers != nil {
		// A missing answer is never taken as an answer. The read job asks
		// GitHub within the workspace allowance, and a later attempt reads
		// the stored answer.
		a.job.env.requestAnswers(a.job.workspace, feedback.Missing{Comparisons: outcome.Missing})
	}
	return outcome, nil
}
