package store

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// Linking bounds (component-feedback-reports invariant 15;
// feature-production-feedback-listening FR-2).
const (
	// FeedbackReportLinkingRetry is the least time between two claimed
	// linking attempts of one report.
	FeedbackReportLinkingRetry = time.Minute
	// FeedbackReportLinkingDeadline is the time after the report was first
	// stored when a rule with no answer ends with the note not_checked.
	FeedbackReportLinkingDeadline = 24 * time.Hour
)

// The states of a link (component-feedback-reports invariants 16 and 17).
const (
	FeedbackReportLinkPossible  = "possible"
	FeedbackReportLinkConfirmed = "confirmed"
	FeedbackReportLinkRemoved   = "removed"
)

// The kinds of a piece of evidence.
const (
	FeedbackEvidenceLine   = "line"
	FeedbackEvidenceTiming = "timing"
)

// The kinds of a note (component-feedback-reports invariants 12 to 15).
const (
	FeedbackNoteTooManyTasks     = "too_many_tasks"
	FeedbackNoteNoTaskDeploy     = "no_task_deploy"
	FeedbackNoteDeployNotCompare = "deploy_not_compared"
	FeedbackNoteFileNotFound     = "file_not_found"
	FeedbackNoteCommitUnresolved = "commit_unresolved"
	FeedbackNoteNotChecked       = "not_checked"
)

// The rules a not_checked note names.
const (
	FeedbackRuleLine   = "line"
	FeedbackRuleTiming = "timing"
)

// FeedbackReportDeploy is a deploy as a piece of evidence or a note names
// it: its record ID, the service, the version exactly as reported, its time,
// and the source of its record.
type FeedbackReportDeploy struct {
	ID         string                `json:"id"`
	Service    string                `json:"service"`
	Version    FeedbackReportVersion `json:"version"`
	DeployedAt time.Time             `json:"deployed_at"`
	Source     string                `json:"source"`
}

// FeedbackReportEvidence is one piece of evidence of a link. A line piece
// holds File, Line, and Commit. A timing piece holds Deploy and
// ElapsedMinutes, and PriorSignalAt when the deploy qualified because
// first_seen fell between the signal before it and the deploy
// (component-feedback-reports invariants 12 and 13; Public surfaces).
type FeedbackReportEvidence struct {
	Kind           string                `json:"kind"`
	File           string                `json:"file,omitempty"`
	Line           int                   `json:"line,omitempty"`
	Commit         string                `json:"commit,omitempty"`
	Deploy         *FeedbackReportDeploy `json:"deploy,omitempty"`
	ElapsedMinutes *int64                `json:"elapsed_minutes,omitempty"`
	PriorSignalAt  *time.Time            `json:"prior_signal_at,omitempty"`
}

// FeedbackReportNote is one note of a report and what it names
// (component-feedback-reports invariants 12 to 15).
type FeedbackReportNote struct {
	Kind string `json:"kind"`
	// Deploy is set for too_many_tasks, no_task_deploy, and
	// deploy_not_compared.
	Deploy *FeedbackReportDeploy `json:"deploy,omitempty"`
	// TaskCount is set for too_many_tasks.
	TaskCount int `json:"task_count,omitempty"`
	// Rule is set for not_checked: line or timing.
	Rule string `json:"rule,omitempty"`
	// Reason is set for commit_unresolved and not_checked.
	Reason string `json:"reason,omitempty"`
	// File and Commit are set for file_not_found: the path GitHub did not
	// find and the resolved commit it read at.
	File   string `json:"file,omitempty"`
	Commit string `json:"commit,omitempty"`
}

// FeedbackReportLink is one stored link of a report to a task or a crate.
// A removed link stays stored, so no rule links that task again
// (component-feedback-reports invariants 11 and 17).
type FeedbackReportLink struct {
	ID          string
	ReportID    string
	TaskID      string
	CrateID     string
	State       string
	Evidence    []FeedbackReportEvidence
	CreatedAt   time.Time
	CreatedBy   string
	ConfirmedBy string
	ConfirmedAt *time.Time
	RemovedBy   string
	RemovedAt   *time.Time
}

// FeedbackReportLineAnswer is the stored answer of one line read: whether
// GitHub found the file at the resolved commit, the commit that last changed
// the line, and the merged pull requests GitHub lists for that commit.
type FeedbackReportLineAnswer struct {
	FileFound    bool   `json:"file_found"`
	Commit       string `json:"commit"`
	PullRequests []int  `json:"pull_requests"`
}

// FeedbackReportLineRead is the stored line read of one report: the
// repository name and the resolved commit it read at, the file and line,
// and the answer. An answered read is never asked again
// (component-feedback-reports invariants 12 and 15).
type FeedbackReportLineRead struct {
	ReportID       string
	Repository     string
	ResolvedCommit string
	File           string
	Line           int
	Answer         FeedbackReportLineAnswer
	AnsweredAt     time.Time
}

// FeedbackReportLinkCandidate is one task that a rule links, with the
// evidence the rules found for it.
type FeedbackReportLinkCandidate struct {
	TaskID   string
	Evidence []FeedbackReportEvidence
}

// FeedbackReportLinking is the end of linking of one report: the tasks the
// two rules link, the notes, and the time linking ended.
type FeedbackReportLinking struct {
	ReportID string
	Links    []FeedbackReportLinkCandidate
	Notes    []FeedbackReportNote
	EndedAt  time.Time
}

// ReportLinkingStore keeps what the background linking work of reports
// learns and writes (component-feedback-reports invariants 11 to 16;
// feature-production-feedback-listening FR-2). Every operation is scoped
// to the context's workspace. It changes no task, work order, gate, or next
// action (invariant 1; DEC-149(1); req-production-feedback AC-11.1).
type ReportLinkingStore interface {
	// ClaimFeedbackReportLinking takes one linking attempt of a report at
	// now, under the lock that serializes the writes of the report. It
	// answers the report as stored and whether the claim succeeded. The
	// claim succeeds only when linking has not ended and the stored last
	// attempt time is empty or at least FeedbackReportLinkingRetry before
	// the exact now (FeedbackReportLinkingClaimable). A successful claim
	// stores now, cut to the stored precision, as the last attempt time. A
	// refused claim writes nothing. A report the workspace does not hold
	// answers ErrNotFound.
	ClaimFeedbackReportLinking(ctx context.Context, reportID string, now time.Time) (FeedbackReport, bool, error)
	// ReadFeedbackReportLineRead answers the stored line read of a report,
	// and false when none is stored.
	ReadFeedbackReportLineRead(ctx context.Context, reportID string) (FeedbackReportLineRead, bool, error)
	// RecordFeedbackReportLineRead stores the answer of a line read. A
	// stored answer is kept and never replaced. A report the workspace does
	// not hold answers ErrNotFound.
	RecordFeedbackReportLineRead(ctx context.Context, read FeedbackReportLineRead) error
	// FinishFeedbackReportLinking ends the linking of a report in one
	// transaction under the lock that serializes the writes of the report,
	// as PlanFeedbackReportLinking plans it. It writes the links and the
	// notes, sets the time linking ended, and appends one
	// feedback_report.linked event with the system actor that names no task.
	// It answers false and writes nothing when linking had already ended. A
	// report the workspace does not hold answers ErrNotFound.
	FinishFeedbackReportLinking(ctx context.Context, linking FeedbackReportLinking) (bool, error)
	// SaveFeedbackLineProgress stores one definitive result of resolving
	// the first entry of a report, under the report write lock, as
	// ApplyFeedbackLineStep merges it, and answers the progress as stored. A
	// stored answer is never replaced. A report whose linking ended is left
	// as it is. A report the workspace does not hold answers ErrNotFound.
	SaveFeedbackLineProgress(ctx context.Context, reportID string, step FeedbackLineStep) (FeedbackLineProgress, error)
	// SaveFeedbackTimingResult stores the timing rule's answer of a report
	// once, under the report write lock, and answers the result as stored: a
	// stored result is kept. A report whose linking ended is left as it is,
	// and the answer is then the stored result or nil. A report the
	// workspace does not hold answers ErrNotFound.
	SaveFeedbackTimingResult(ctx context.Context, reportID string, result FeedbackTimingResult) (*FeedbackTimingResult, error)
	// ListPendingFeedbackReports returns the reports of one environment
	// whose linking has not ended, open and closed, ordered by ID.
	ListPendingFeedbackReports(ctx context.Context, environmentID string) ([]FeedbackReport, error)
	// ListFeedbackReportLinks returns every stored link of a report, removed
	// links included, ordered by ID.
	ListFeedbackReportLinks(ctx context.Context, reportID string) ([]FeedbackReportLink, error)
	// ListFeedbackReportNotes returns the notes of a report in the order
	// they were written.
	ListFeedbackReportNotes(ctx context.Context, reportID string) ([]FeedbackReportNote, error)
}

// FeedbackReportLinkingClaimable decides a linking claim
// (component-feedback-reports invariant 15). It compares the exact now with
// the stored last attempt time and rounds neither.
func FeedbackReportLinkingClaimable(report FeedbackReport, now time.Time) bool {
	if report.LinkingEndedAt != nil {
		return false
	}
	return report.LinkingLastAttemptAt == nil || !now.Before(report.LinkingLastAttemptAt.Add(FeedbackReportLinkingRetry))
}

// FeedbackReportLinkID is the ID of the n-th link candidate of a report,
// from 1. A report ID has the fixed form rpt-<yymmdd>-<6 hex> and is unique
// in the workspace, so a link ID names its report. PlanFeedbackReportLinking
// numbers the new links of a report under the lock that serializes the
// writes of the report and skips every number that a stored link of the
// report holds, so no two links of a workspace share an ID. The ID fits the
// 64-character link ID column of every backend.
func FeedbackReportLinkID(reportID string, n int) string {
	return fmt.Sprintf("lnk:%s:%09d", reportID, n)
}

// FeedbackReportNoteID is the ID of the n-th note of a report, from 1. The
// number has at least nine digits, so the IDs of one report sort in the
// order of their notes. Linking writes the notes of a report once, in the
// transaction that ends its linking, so no two notes of a workspace can share
// an ID.
func FeedbackReportNoteID(reportID string, n int) string {
	return fmt.Sprintf("note:%s:%09d", reportID, n)
}

// NormalizeFeedbackReportLineRead checks a line read before a backend
// stores it, and cuts its time to the stored precision.
func NormalizeFeedbackReportLineRead(read FeedbackReportLineRead) (FeedbackReportLineRead, error) {
	if read.ReportID == "" || read.Repository == "" || !FeedbackCommitID(read.ResolvedCommit) || read.File == "" || read.Line < 1 || read.Line > MaxFeedbackReportLine || read.AnsweredAt.IsZero() {
		return FeedbackReportLineRead{}, fmt.Errorf("a line read needs a report, a repository, a full resolved commit, a file, a line, and its time")
	}
	answer := read.Answer
	if answer.FileFound != FeedbackCommitID(answer.Commit) {
		return FeedbackReportLineRead{}, fmt.Errorf("a line read that found the file needs the full changing commit, and one that did not holds none")
	}
	pulls := slices.Clone(answer.PullRequests)
	if pulls == nil {
		pulls = []int{}
	}
	sort.Ints(pulls)
	pulls = slices.Compact(pulls)
	for _, number := range pulls {
		if number < 1 || !answer.FileFound {
			return FeedbackReportLineRead{}, fmt.Errorf("a line read holds pull request numbers only when it found the file, and each is positive")
		}
	}
	read.Answer.PullRequests = pulls
	read.AnsweredAt = EnvironmentTime(read.AnsweredAt)
	return read, nil
}

// FeedbackReportLineAnswerJSON renders a line answer for a durable
// backend's row, and ParseFeedbackReportLineAnswer reads it back.
func FeedbackReportLineAnswerJSON(answer FeedbackReportLineAnswer) string {
	if answer.PullRequests == nil {
		answer.PullRequests = []int{}
	}
	return string(core.JSONPayload(answer))
}

func ParseFeedbackReportLineAnswer(raw []byte) (FeedbackReportLineAnswer, error) {
	var answer FeedbackReportLineAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		return FeedbackReportLineAnswer{}, fmt.Errorf("decode line read answer: %w", err)
	}
	if answer.PullRequests == nil {
		answer.PullRequests = []int{}
	}
	return answer, nil
}

// FeedbackReportEvidenceJSON renders the evidence of a link for a durable
// backend's row, and ParseFeedbackReportEvidence reads it back.
func FeedbackReportEvidenceJSON(evidence []FeedbackReportEvidence) string {
	if evidence == nil {
		evidence = []FeedbackReportEvidence{}
	}
	return string(core.JSONPayload(evidence))
}

func ParseFeedbackReportEvidence(raw []byte) ([]FeedbackReportEvidence, error) {
	evidence := []FeedbackReportEvidence{}
	if err := json.Unmarshal(raw, &evidence); err != nil {
		return nil, fmt.Errorf("decode link evidence: %w", err)
	}
	for i := range evidence {
		normalizeFeedbackEvidenceTimes(&evidence[i])
	}
	return evidence, nil
}

// FeedbackReportNoteJSON renders a note for a durable backend's detail
// column, and ParseFeedbackReportNote reads it back.
func FeedbackReportNoteJSON(note FeedbackReportNote) string {
	return string(core.JSONPayload(note))
}

func ParseFeedbackReportNote(raw []byte) (FeedbackReportNote, error) {
	var note FeedbackReportNote
	if err := json.Unmarshal(raw, &note); err != nil {
		return FeedbackReportNote{}, fmt.Errorf("decode report note: %w", err)
	}
	if note.Deploy != nil {
		note.Deploy.DeployedAt = note.Deploy.DeployedAt.UTC()
	}
	return note, nil
}

func normalizeFeedbackEvidenceTimes(evidence *FeedbackReportEvidence) {
	if evidence.Deploy != nil {
		evidence.Deploy.DeployedAt = evidence.Deploy.DeployedAt.UTC()
	}
	if evidence.PriorSignalAt != nil {
		at := evidence.PriorSignalAt.UTC()
		evidence.PriorSignalAt = &at
	}
}

// FeedbackReportLinkingWrite is what one end of linking writes, as
// PlanFeedbackReportLinking plans it.
type FeedbackReportLinkingWrite struct {
	// New are the links to insert, each possible.
	New []FeedbackReportLink
	// Extended are stored links whose evidence grew. Their state is kept.
	Extended []FeedbackReportLink
	// Notes are the notes to insert with their IDs, in order.
	Notes []FeedbackReportNoteRow
	// Event is the feedback_report.linked event.
	Event core.Event
}

// FeedbackReportNoteRow is one note with its ID.
type FeedbackReportNoteRow struct {
	ID   string
	Note FeedbackReportNote
}

// PlanFeedbackReportLinking plans the end of linking of a report from the
// stored links of the report (component-feedback-reports invariants 11, 15,
// 16, and 20; req-production-feedback AC-5.6 and AC-7.2). Every backend
// writes exactly this plan, under the lock that serializes the writes of the
// report, after it checks that linking has not ended.
//
//   - A task whose stored link a user removed is not linked again, and its
//     evidence is dropped.
//   - A task with a stored possible or confirmed link keeps that link and its
//     state. Each piece of evidence that the link does not already hold is
//     added to it.
//   - Every other task gets a new possible link with the system as the
//     creator, and an ID that no stored link of the report holds.
//   - The notes are stored in their order.
//   - One linked event names the report, its environment, and the tasks
//     whose links were added or extended, with the system actor and no task.
func PlanFeedbackReportLinking(report FeedbackReport, stored []FeedbackReportLink, linking FeedbackReportLinking) FeedbackReportLinkingWrite {
	at := EnvironmentTime(linking.EndedAt)
	byTask := map[string]FeedbackReportLink{}
	held := map[string]bool{}
	for _, link := range stored {
		held[link.ID] = true
		if link.TaskID != "" {
			byTask[link.TaskID] = link
		}
	}
	next := 0
	nextID := func() string {
		for {
			next++
			if id := FeedbackReportLinkID(report.ID, next); !held[id] {
				held[id] = true
				return id
			}
		}
	}
	system := SystemActor()
	write := FeedbackReportLinkingWrite{}
	named := []string{}
	candidates := slices.Clone(linking.Links)
	slices.SortStableFunc(candidates, func(a, b FeedbackReportLinkCandidate) int { return strings.Compare(a.TaskID, b.TaskID) })
	for _, candidate := range candidates {
		if candidate.TaskID == "" {
			continue
		}
		evidence := distinctFeedbackEvidence(nil, candidate.Evidence)
		link, exists := byTask[candidate.TaskID]
		switch {
		case exists && link.State == FeedbackReportLinkRemoved:
			continue
		case exists:
			grown := distinctFeedbackEvidence(link.Evidence, evidence)
			if len(grown) == len(link.Evidence) {
				continue
			}
			link.Evidence = grown
			write.Extended = append(write.Extended, link)
		default:
			link = FeedbackReportLink{ID: nextID(), ReportID: report.ID, TaskID: candidate.TaskID,
				State: FeedbackReportLinkPossible, Evidence: evidence, CreatedAt: at, CreatedBy: system.ID}
			write.New = append(write.New, link)
		}
		byTask[candidate.TaskID] = link
		named = append(named, candidate.TaskID)
	}
	for i, note := range linking.Notes {
		write.Notes = append(write.Notes, FeedbackReportNoteRow{ID: FeedbackReportNoteID(report.ID, i+1), Note: note})
	}
	write.Event = core.Event{Kind: FeedbackReportLinkedEvent, ActorID: system.ID, ActorRole: system.Role, At: at,
		Payload: core.JSONPayload(map[string]any{"report_id": report.ID, "environment_id": report.EnvironmentID, "task_ids": named})}
	return write
}

// distinctFeedbackEvidence appends each piece of added that held does not
// already hold. Two pieces are the same when every member is equal.
func distinctFeedbackEvidence(held, added []FeedbackReportEvidence) []FeedbackReportEvidence {
	out := slices.Clone(held)
	if out == nil {
		out = []FeedbackReportEvidence{}
	}
	for _, piece := range added {
		normalizeFeedbackEvidenceTimes(&piece)
		if !slices.ContainsFunc(out, func(existing FeedbackReportEvidence) bool { return reflect.DeepEqual(existing, piece) }) {
			out = append(out, piece)
		}
	}
	return out
}

// FeedbackMergePullRequest reads the pull request number of a merge event
// payload's pull_request member, which a backend extracts as text: a
// positive whole number, written as a JSON number or a string. Any other
// value is no number, and answers 0.
func FeedbackMergePullRequest(text string) int {
	text = strings.Trim(strings.TrimSpace(text), `"`)
	number, err := strconv.Atoi(text)
	if err != nil || number < 1 {
		return 0
	}
	return number
}

// FeedbackLineAbsent is the stored answer of a candidate repository's commit
// read when GitHub proved that the repository holds no such commit or tag.
const FeedbackLineAbsent = "absent"

// FeedbackLineProgress is the stored progress of a report's line rule, so a
// fresh worker continues from the store alone (component-feedback-reports
// invariants 12 and 15; approved plan version 3). Candidates holds, by
// repository name of the workspace configuration, the definitive answer of
// that repository's commit read: a full commit or FeedbackLineAbsent. Once
// resolving ends, either Repository and Commit hold the resolved repository
// and full commit, or Unresolved holds a definitive unresolved reason. A
// retryable failure is never stored. It holds no report text.
type FeedbackLineProgress struct {
	Candidates map[string]string `json:"candidates,omitempty"`
	Repository string            `json:"repository,omitempty"`
	Commit     string            `json:"commit,omitempty"`
	Unresolved string            `json:"unresolved,omitempty"`
}

// Ended reports whether resolving has ended.
func (p *FeedbackLineProgress) Ended() bool {
	return p != nil && (p.Commit != "" || p.Unresolved != "")
}

// FeedbackLineStep is one definitive result of resolving to store: one
// candidate answer (Candidate and Answer), the resolution (Repository and
// Commit), or a definitive unresolved reason (Unresolved). Exactly one kind
// is set.
type FeedbackLineStep struct {
	Candidate  string
	Answer     string
	Repository string
	Commit     string
	Unresolved string
}

// FeedbackDefinitiveUnresolved lists the unresolved reasons of
// component-production-feedback invariant 7 that are final for the line
// rule. Every other reason, a forge failure, a missing App, and pending
// included, proves nothing and is never stored.
var FeedbackDefinitiveUnresolved = []string{FeedbackReasonNoMatch, FeedbackReasonAmbiguous, FeedbackReasonNoVersionRule, FeedbackReasonRuleMismatch, FeedbackReasonNoRepository}

// ApplyFeedbackLineStep merges one step into stored progress and reports
// whether it changed. A stored answer is never replaced: a second answer for
// a candidate, and a second resolution or reason, change nothing. Every
// backend writes exactly this result under the report write lock.
func ApplyFeedbackLineStep(stored *FeedbackLineProgress, step FeedbackLineStep) (FeedbackLineProgress, bool, error) {
	progress := FeedbackLineProgress{}
	if stored != nil {
		progress = cloneFeedbackLineProgress(*stored)
	}
	kinds := 0
	if step.Candidate != "" || step.Answer != "" {
		kinds++
		if step.Candidate == "" || (step.Answer != FeedbackLineAbsent && !FeedbackCommitID(step.Answer)) {
			return progress, false, fmt.Errorf("a candidate answer needs a repository and a full commit or an absence")
		}
	}
	if step.Repository != "" || step.Commit != "" {
		kinds++
		if step.Repository == "" || !FeedbackCommitID(step.Commit) {
			return progress, false, fmt.Errorf("a resolution needs a repository and a full commit")
		}
	}
	if step.Unresolved != "" {
		kinds++
		if !slices.Contains(FeedbackDefinitiveUnresolved, step.Unresolved) {
			return progress, false, fmt.Errorf("only a definitive unresolved reason is stored")
		}
	}
	if kinds != 1 {
		return progress, false, fmt.Errorf("a line step holds exactly one result")
	}
	switch {
	case step.Candidate != "":
		if _, answered := progress.Candidates[step.Candidate]; answered || progress.Ended() {
			return progress, false, nil
		}
		if progress.Candidates == nil {
			progress.Candidates = map[string]string{}
		}
		progress.Candidates[step.Candidate] = step.Answer
	case progress.Ended():
		return progress, false, nil
	case step.Commit != "":
		progress.Repository, progress.Commit = step.Repository, step.Commit
	default:
		progress.Unresolved = step.Unresolved
	}
	return progress, true, nil
}

func cloneFeedbackLineProgress(progress FeedbackLineProgress) FeedbackLineProgress {
	if progress.Candidates != nil {
		candidates := make(map[string]string, len(progress.Candidates))
		for name, answer := range progress.Candidates {
			candidates[name] = answer
		}
		progress.Candidates = candidates
	}
	return progress
}

// FeedbackTimingLink is one piece of timing evidence of a stored timing
// result: the task, the deploy by its record ID, the whole minutes, and the
// prior signal's arrival in the second case.
type FeedbackTimingLink struct {
	TaskID         string     `json:"task_id"`
	DeployID       string     `json:"deploy_id"`
	ElapsedMinutes int64      `json:"elapsed_minutes"`
	PriorSignalAt  *time.Time `json:"prior_signal_at,omitempty"`
}

// FeedbackTimingNote is one note of a stored timing result, naming its deploy
// by record ID.
type FeedbackTimingNote struct {
	Kind      string `json:"kind"`
	DeployID  string `json:"deploy_id"`
	TaskCount int    `json:"task_count,omitempty"`
}

// FeedbackTimingResult is the stored answer of a report's timing rule once it
// ended, so it is reused and never worked out again (approved plan version
// 3). Deploys are named by record ID, so it holds no report text. An empty
// result, with no link and no note, is an answer too.
type FeedbackTimingResult struct {
	Links []FeedbackTimingLink `json:"links"`
	Notes []FeedbackTimingNote `json:"notes"`
}

// NormalizeFeedbackTimingResult checks a timing result before a backend
// stores it.
func NormalizeFeedbackTimingResult(result FeedbackTimingResult) (FeedbackTimingResult, error) {
	if result.Links == nil {
		result.Links = []FeedbackTimingLink{}
	}
	if result.Notes == nil {
		result.Notes = []FeedbackTimingNote{}
	}
	for i, link := range result.Links {
		if link.TaskID == "" || link.DeployID == "" || link.ElapsedMinutes < 0 {
			return FeedbackTimingResult{}, fmt.Errorf("a timing link needs a task, a deploy, and whole minutes")
		}
		if link.PriorSignalAt != nil {
			at := link.PriorSignalAt.UTC()
			result.Links[i].PriorSignalAt = &at
		}
	}
	for _, note := range result.Notes {
		if note.DeployID == "" {
			return FeedbackTimingResult{}, fmt.Errorf("a timing note needs a deploy")
		}
		switch note.Kind {
		case FeedbackNoteTooManyTasks, FeedbackNoteNoTaskDeploy, FeedbackNoteDeployNotCompare:
		default:
			return FeedbackTimingResult{}, fmt.Errorf("a timing note is too_many_tasks, no_task_deploy, or deploy_not_compared")
		}
	}
	return result, nil
}

// FeedbackLineProgressJSON and FeedbackTimingResultJSON render the stored
// progress for a durable backend's column, and the Parse functions read it
// back. A NULL column is nil.
func FeedbackLineProgressJSON(progress FeedbackLineProgress) string {
	return string(core.JSONPayload(progress))
}

func ParseFeedbackLineProgress(raw []byte) (*FeedbackLineProgress, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var progress FeedbackLineProgress
	if err := json.Unmarshal(raw, &progress); err != nil {
		return nil, fmt.Errorf("decode line progress: %w", err)
	}
	return &progress, nil
}

func FeedbackTimingResultJSON(result FeedbackTimingResult) string {
	return string(core.JSONPayload(result))
}

func ParseFeedbackTimingResult(raw []byte) (*FeedbackTimingResult, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var result FeedbackTimingResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("decode timing result: %w", err)
	}
	normalized, err := NormalizeFeedbackTimingResult(result)
	if err != nil {
		return nil, err
	}
	return &normalized, nil
}
