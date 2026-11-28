package httpapi

import (
	"context"
	"fmt"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// The served report (component-feedback-reports, Public surfaces, and
// invariants 16, 21, and 22; req-production-feedback AC-5.5, AC-6.1, and
// AC-10.2). The routes for people, the MCP read, and the Inbox item take a
// served report from loadServedFeedbackReports, and every surface prints what
// it returns. Each type below holds exactly the members the design lists:
// no removed link, no linking progress, no source, no redaction count, and no
// linking times are served.

// servedFeedbackReport is one served report.
type servedFeedbackReport struct {
	ID           string                       `json:"id"`
	Kind         string                       `json:"kind"`
	Key          string                       `json:"key"`
	Summary      string                       `json:"summary"`
	Environment  servedFeedbackEnvironment    `json:"environment"`
	Service      *string                      `json:"service"`
	Count        int64                        `json:"count"`
	FirstSeen    time.Time                    `json:"first_seen"`
	LastSeen     time.Time                    `json:"last_seen"`
	File         *string                      `json:"file"`
	Line         *int                         `json:"line"`
	First        servedFeedbackVersion        `json:"first"`
	Latest       servedFeedbackVersion        `json:"latest"`
	State        string                       `json:"state"`
	Closed       *servedFeedbackClosed        `json:"closed"`
	ReopenedAt   *time.Time                   `json:"reopened_at"`
	InInbox      bool                         `json:"in_inbox"`
	Links        []servedFeedbackLink         `json:"links"`
	LinksPending bool                         `json:"links_pending"`
	Notes        []servedFeedbackNote         `json:"notes"`
	FixTask      *servedFeedbackTaskReference `json:"fix_task"`
	CreatedAt    time.Time                    `json:"created_at"`
}

// servedFeedbackEnvironment is the report's environment with whether a
// watcher is listening and the last signal time (component-production-feedback
// invariant 13; component-feedback-reports invariant 22).
type servedFeedbackEnvironment struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Listening    bool       `json:"listening"`
	LastSignalAt *time.Time `json:"last_signal_at"`
}

// servedFeedbackVersion is the repository, commit, and version an entry
// stated. A member that was not stated is null.
type servedFeedbackVersion struct {
	Repository *string `json:"repository"`
	Commit     *string `json:"commit"`
	Version    *string `json:"version"`
}

// servedFeedbackClosed is the closing of a closed report.
type servedFeedbackClosed struct {
	At     time.Time `json:"at"`
	By     string    `json:"by"`
	Reason string    `json:"reason"`
}

// servedFeedbackCrate names a crate.
type servedFeedbackCrate struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// servedFeedbackLinkTask is a linked task with its crate at the read, or
// null when it is a member of none.
type servedFeedbackLinkTask struct {
	ID    string               `json:"id"`
	Title string               `json:"title"`
	Crate *servedFeedbackCrate `json:"crate"`
}

// servedFeedbackTaskReference is the fix task.
type servedFeedbackTaskReference struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// servedFeedbackLink is a possible or confirmed link with its evidence. A
// possible link is evidence, never a cause (invariant 16).
type servedFeedbackLink struct {
	ID          string                   `json:"id"`
	State       string                   `json:"state"`
	Task        *servedFeedbackLinkTask  `json:"task,omitempty"`
	Crate       *servedFeedbackCrate     `json:"crate,omitempty"`
	Evidence    []servedFeedbackEvidence `json:"evidence"`
	ConfirmedBy *string                  `json:"confirmed_by,omitempty"`
	ConfirmedAt *time.Time               `json:"confirmed_at,omitempty"`
}

// servedFeedbackEvidence is one piece of evidence: line with file, line, and
// commit, or timing with the deploy's service, version, deployed_at, and
// source, elapsed_minutes, and prior_signal_at.
type servedFeedbackEvidence struct {
	Kind           string                 `json:"kind"`
	File           string                 `json:"file,omitempty"`
	Line           int                    `json:"line,omitempty"`
	Commit         string                 `json:"commit,omitempty"`
	Service        string                 `json:"service,omitempty"`
	Version        *servedFeedbackVersion `json:"version,omitempty"`
	DeployedAt     *time.Time             `json:"deployed_at,omitempty"`
	Source         string                 `json:"source,omitempty"`
	ElapsedMinutes *int64                 `json:"elapsed_minutes,omitempty"`
	PriorSignalAt  *time.Time             `json:"prior_signal_at,omitempty"`
}

// servedFeedbackDeploy is the deploy a note names.
type servedFeedbackDeploy struct {
	Service    string                `json:"service"`
	Version    servedFeedbackVersion `json:"version"`
	DeployedAt time.Time             `json:"deployed_at"`
	Source     string                `json:"source"`
}

// servedFeedbackNote is one note with what it names.
type servedFeedbackNote struct {
	Kind      string                `json:"kind"`
	Deploy    *servedFeedbackDeploy `json:"deploy,omitempty"`
	TaskCount int                   `json:"task_count,omitempty"`
	Rule      string                `json:"rule,omitempty"`
	Reason    string                `json:"reason,omitempty"`
	File      string                `json:"file,omitempty"`
	Commit    string                `json:"commit,omitempty"`
}

func optionalServedText(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func servedVersion(version store.FeedbackReportVersion) servedFeedbackVersion {
	return servedFeedbackVersion{Repository: optionalServedText(version.Repository), Commit: optionalServedText(version.Commit), Version: optionalServedText(version.Version)}
}

// servedDeploy is the served form of a stored deploy, and false when the
// stored deploy lacks a member it needs, so it proves nothing.
func servedDeploy(deploy *store.FeedbackReportDeploy) (*servedFeedbackDeploy, bool) {
	if deploy == nil || deploy.Service == "" || deploy.DeployedAt.IsZero() || deploy.Source == "" {
		return nil, false
	}
	return &servedFeedbackDeploy{Service: deploy.Service, Version: servedVersion(deploy.Version), DeployedAt: deploy.DeployedAt.UTC(), Source: deploy.Source}, true
}

// servedEvidence is the served form of one stored piece of evidence. A
// piece that is malformed or lacks a member it needs proves nothing and is
// not served (component-feedback-reports invariants 12, 13, and 16).
func servedEvidence(piece store.FeedbackReportEvidence) (servedFeedbackEvidence, bool) {
	switch piece.Kind {
	case store.FeedbackEvidenceLine:
		if piece.File == "" || piece.Line < 1 || piece.Line > store.MaxFeedbackReportLine || !store.FeedbackCommitID(piece.Commit) || piece.Deploy != nil {
			return servedFeedbackEvidence{}, false
		}
		return servedFeedbackEvidence{Kind: piece.Kind, File: piece.File, Line: piece.Line, Commit: piece.Commit}, true
	case store.FeedbackEvidenceTiming:
		deploy, ok := servedDeploy(piece.Deploy)
		if !ok || piece.ElapsedMinutes == nil || *piece.ElapsedMinutes < 0 || piece.File != "" || piece.Commit != "" {
			return servedFeedbackEvidence{}, false
		}
		elapsed := *piece.ElapsedMinutes
		served := servedFeedbackEvidence{Kind: piece.Kind, Service: deploy.Service, Version: &deploy.Version, DeployedAt: &deploy.DeployedAt, Source: deploy.Source, ElapsedMinutes: &elapsed}
		if piece.PriorSignalAt != nil {
			at := piece.PriorSignalAt.UTC()
			served.PriorSignalAt = &at
		}
		return served, true
	}
	return servedFeedbackEvidence{}, false
}

// servedNote is the served form of one stored note, and false when the note
// is of an unknown kind or lacks what its kind names.
func servedNote(note store.FeedbackReportNote) (servedFeedbackNote, bool) {
	served := servedFeedbackNote{Kind: note.Kind}
	switch note.Kind {
	case store.FeedbackNoteTooManyTasks, store.FeedbackNoteNoTaskDeploy, store.FeedbackNoteDeployNotCompare:
		deploy, ok := servedDeploy(note.Deploy)
		if !ok || (note.Kind == store.FeedbackNoteTooManyTasks && note.TaskCount < 1) {
			return servedFeedbackNote{}, false
		}
		served.Deploy = deploy
		if note.Kind == store.FeedbackNoteTooManyTasks {
			served.TaskCount = note.TaskCount
		}
	case store.FeedbackNoteFileNotFound:
		if note.File == "" {
			return servedFeedbackNote{}, false
		}
		served.File, served.Commit = note.File, note.Commit
	case store.FeedbackNoteCommitUnresolved:
		if note.Reason == "" {
			return servedFeedbackNote{}, false
		}
		served.Reason = note.Reason
	case store.FeedbackNoteNotChecked:
		if (note.Rule != store.FeedbackRuleLine && note.Rule != store.FeedbackRuleTiming) || note.Reason == "" {
			return servedFeedbackNote{}, false
		}
		served.Rule, served.Reason = note.Rule, note.Reason
	default:
		return servedFeedbackNote{}, false
	}
	return served, true
}

// servedLink is the served form of one stored link, and false for a link
// that is not served: removed, of another state, or naming a task or crate
// the workspace does not hold (store.FeedbackReportServed; invariant 21).
func servedLink(link store.FeedbackReportLink, details store.FeedbackReportDetails) (servedFeedbackLink, bool) {
	if !store.FeedbackReportServed(link, details.Tasks, details.Crates) {
		return servedFeedbackLink{}, false
	}
	served := servedFeedbackLink{ID: link.ID, State: link.State, Evidence: []servedFeedbackEvidence{}}
	if link.TaskID != "" {
		task := details.Tasks[link.TaskID]
		served.Task = &servedFeedbackLinkTask{ID: task.ID, Title: task.Title}
		if crate, ok := details.Crates[task.CrateID]; ok && task.CrateID != "" {
			served.Task.Crate = &servedFeedbackCrate{ID: crate.ID, Name: crate.Name}
		}
	} else {
		crate := details.Crates[link.CrateID]
		served.Crate = &servedFeedbackCrate{ID: crate.ID, Name: crate.Name}
	}
	for _, piece := range link.Evidence {
		if evidence, ok := servedEvidence(piece); ok {
			served.Evidence = append(served.Evidence, evidence)
		}
	}
	if link.State == store.FeedbackReportLinkConfirmed && link.ConfirmedAt != nil && link.ConfirmedBy != "" {
		by, at := link.ConfirmedBy, link.ConfirmedAt.UTC()
		served.ConfirmedBy, served.ConfirmedAt = &by, &at
	}
	return served, true
}

// buildServedFeedbackReport builds one served report from its stored
// record, its details, its environment, and the environment's newest signal
// at the read clock now. It reads nothing and writes nothing.
func buildServedFeedbackReport(report store.FeedbackReport, details store.FeedbackReportDetails, environment store.Environment, signal *store.FeedbackSignalState, now time.Time) servedFeedbackReport {
	served := servedFeedbackReport{
		ID: report.ID, Kind: report.Kind, Key: report.Key, Summary: report.Summary,
		Environment: servedFeedbackEnvironment{ID: environment.ID, Name: environment.Name},
		Service:     optionalServedText(report.Service), Count: report.Count,
		FirstSeen: report.FirstSeen.UTC(), LastSeen: report.LastSeen.UTC(),
		File: optionalServedText(report.File), First: servedVersion(report.First), Latest: servedVersion(report.Latest),
		State: report.State, InInbox: report.InInbox && report.State == store.FeedbackReportOpen,
		Links: []servedFeedbackLink{}, LinksPending: report.LinkingEndedAt == nil, Notes: []servedFeedbackNote{},
		CreatedAt: report.CreatedAt.UTC(),
	}
	if signal != nil && !signal.LastSignalAt.IsZero() {
		at := signal.LastSignalAt.UTC()
		served.Environment.Listening, served.Environment.LastSignalAt = signal.Listening(now), &at
	}
	if report.File != "" && report.Line > 0 {
		line := report.Line
		served.Line = &line
	}
	if report.State == store.FeedbackReportClosed && report.ClosedAt != nil {
		served.Closed = &servedFeedbackClosed{At: report.ClosedAt.UTC(), By: report.ClosedBy, Reason: report.CloseReason}
	}
	if report.ReopenedAt != nil {
		at := report.ReopenedAt.UTC()
		served.ReopenedAt = &at
	}
	for _, link := range details.Links[report.ID] {
		if link.ReportID != report.ID {
			continue
		}
		if item, ok := servedLink(link, details); ok {
			served.Links = append(served.Links, item)
		}
	}
	for _, note := range details.Notes[report.ID] {
		if item, ok := servedNote(note); ok {
			served.Notes = append(served.Notes, item)
		}
	}
	if task, ok := details.Tasks[report.FixTaskID]; ok && report.FixTaskID != "" {
		served.FixTask = &servedFeedbackTaskReference{ID: task.ID, Title: task.Title}
	}
	return served
}

// loadServedFeedbackReports is the entry point of a surface that serves
// reports it found by ID, such as the Inbox: it reads the reports of the IDs
// with their details from one consistent view, builds their served form, and
// asks the linking job again for each pending report it returns. The caller
// passes exactly the IDs it serves, so the ask covers no report it does not
// serve (component-feedback-reports invariants 15 and 21).
func (s *Server) loadServedFeedbackReports(ctx context.Context, backend store.ReportStore, reportIDs []string) ([]servedFeedbackReport, error) {
	if len(reportIDs) == 0 {
		return []servedFeedbackReport{}, nil
	}
	reports, details, err := backend.ReadFeedbackReports(ctx, reportIDs)
	if err != nil {
		return nil, err
	}
	served, err := s.servedFeedbackReports(ctx, reports, details)
	if err != nil {
		return nil, err
	}
	s.requestPendingFeedbackLinking(ctx, reports)
	return served, nil
}

// servedFeedbackReports builds the served form of stored reports from the
// details read with them, at one read clock. The environment and its newest
// signal come from the details, which the store reads in the same view as
// the records, so no served value comes from a second read. A report whose
// environment the details do not hold is a store failure, and a failure
// serves no partial list.
func (s *Server) servedFeedbackReports(ctx context.Context, reports []store.FeedbackReport, details store.FeedbackReportDetails) ([]servedFeedbackReport, error) {
	served := make([]servedFeedbackReport, 0, len(reports))
	now := s.feedbackNow()
	for _, report := range reports {
		environment, ok := details.Environments[report.EnvironmentID]
		if !ok {
			return nil, fmt.Errorf("feedback report %s names environment %s that the workspace does not hold", report.ID, report.EnvironmentID)
		}
		served = append(served, buildServedFeedbackReport(report, details, store.Environment{ID: environment.ID, Name: environment.Name}, environment.Signal, now))
	}
	return served, nil
}

// requestPendingFeedbackLinking asks the linking job again for each served
// report whose links are pending, through the job's own entry point. Every
// path that serves reports calls it with exactly the reports it served, after
// it served them: the HTTP list with its page, the HTTP read and every act
// answer with their one report, loadServedFeedbackReports with what it
// returns, and the MCP read with the admitted page (requestServedMCPLinking).
// The job is deduplicated by report and claims an attempt no more than once a
// minute, so a read never works out a link itself and never waits for one
// (component-feedback-reports invariant 15).
func (s *Server) requestPendingFeedbackLinking(ctx context.Context, reports []store.FeedbackReport) {
	ids := []string{}
	for _, report := range reports {
		if report.LinkingEndedAt == nil {
			ids = append(ids, report.ID)
		}
	}
	s.requestFeedbackLinking(ctx, ids)
}

// requestFeedbackLinking asks the linking job for each report ID.
func (s *Server) requestFeedbackLinking(ctx context.Context, reportIDs []string) {
	workspace, ok := store.WorkspaceFromContext(ctx)
	if !ok || workspace == "" {
		return
	}
	for _, id := range reportIDs {
		if s.feedbackLinkingAsked != nil {
			s.feedbackLinkingAsked(workspace, id)
		}
		s.enqueueFeedbackReportLinking(workspace, id)
	}
}
