package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
)

// The acts a person records on a report (component-feedback-reports
// invariants 17 and 18; req-production-feedback REQ-7).
const (
	FeedbackReportActLink       = "link"
	FeedbackReportActRemoveLink = "remove_link"
	FeedbackReportActClose      = "close"
	FeedbackReportActReopen     = "reopen"
	FeedbackReportActFixTask    = "fix_task"
)

// Act bounds. The close reason is 1 to 500 characters
// (component-feedback-reports invariant 17).
//
// MaxFeedbackReportCauseTasks is provisional. The operator's recovery
// direction of 2026-11-28 set it until a confirmed version of
// component-feedback-reports (version 3, drafted) states it:
// cause_task_ids names at most 50 tasks, each of them once, and a request
// that breaks this is refused whole and records nothing.
const (
	FeedbackReportCloseReasonMax = 500
	MaxFeedbackReportCauseTasks  = 50
)

// FeedbackSourceUser is the source kind of a reopening that a user recorded.
// Its label is empty and its actor is the user (component-feedback-reports
// invariant 17).
const FeedbackSourceUser = "user"

// ErrFeedbackReportClosed refuses every act on a closed report but reopening
// (component-feedback-reports invariant 17).
var ErrFeedbackReportClosed = errors.New("feedback report closed")

// The codes of an act's target refusal.
const (
	FeedbackActTaskNotFound  = "task_not_found"
	FeedbackActTaskNotMerged = "task_not_merged"
	FeedbackActCrateNotFound = "crate_not_found"
	FeedbackActLinkNotFound  = "link_not_found"
)

// FeedbackReportActError refuses an act whose target the workspace does not
// hold in the state the act needs. Field names the request member and Code
// the refusal. Neither holds a submitted value.
type FeedbackReportActError struct {
	Code    string
	Field   string
	Message string
}

func (e *FeedbackReportActError) Error() string { return e.Field + ": " + e.Message }

// FeedbackReportAct is one act of a person on one report. At is the server's
// time of the request. Reason is the close reason as submitted; the store
// redacts it before it stores it.
type FeedbackReportAct struct {
	Kind         string
	ReportID     string
	TaskID       string
	CrateID      string
	LinkID       string
	Reason       string
	CauseTaskIDs []string
	At           time.Time
}

// FeedbackReportActResult is what an act did. Changed is false for an act
// that changed nothing, which records nothing.
type FeedbackReportActResult struct {
	Changed bool
}

// FeedbackReportQuery selects reports for a list. State is open or closed.
// Each other filter is empty when unset. Limit is the page size and Offset
// the rows to skip (component-feedback-reports, Public surfaces).
type FeedbackReportQuery struct {
	State         string
	EnvironmentID string
	TaskID        string
	CrateID       string
	NoCrate       bool
	Limit         int
	Offset        int
}

// MaxFeedbackReportQueryLimit bounds one store page: the HTTP list takes at
// most 200 rows, and the MCP read asks for the read framework's 1,000 items
// and refuses a larger selection from the count.
const MaxFeedbackReportQueryLimit = 1000

// FeedbackReportPage is one page of a list, the number of reports the
// filters select, and the details of the page's reports, read together so
// the page, the total, and the details agree.
type FeedbackReportPage struct {
	Reports []FeedbackReport
	Total   int
	Details FeedbackReportDetails
}

// FeedbackReportTaskRef is a task that a report names: a linked task or the
// fix task, with its title and the crate it is a member of at the read.
type FeedbackReportTaskRef struct {
	ID        string
	Title     string
	CrateID   string
	CrateName string
}

// FeedbackReportCrateRef is a crate that a report link names.
type FeedbackReportCrateRef struct {
	ID   string
	Name string
}

// FeedbackReportDetails holds what a served report needs beyond its record:
// every stored link of each report, removed links included and ordered by ID,
// the notes of each report in order, and the tasks and crates the links and
// fix tasks name that the workspace holds. A task or crate the workspace does
// not hold has no entry.
type FeedbackReportDetails struct {
	Links  map[string][]FeedbackReportLink
	Notes  map[string][]FeedbackReportNote
	Tasks  map[string]FeedbackReportTaskRef
	Crates map[string]FeedbackReportCrateRef
}

// ReportReadStore keeps the reads and the acts of reports for people
// (component-feedback-reports invariants 17 to 23;
// feature-production-feedback-listening FR-3). Every operation is scoped to
// the context's workspace. No act changes a task, work order, gate, or next
// action (invariant 1; DEC-149(1); req-production-feedback AC-11.1).
type ReportReadStore interface {
	// ListFeedbackReports answers one page of the reports the query
	// selects, newest last_seen first and then by ID, the number the filters
	// select, and the details of the page's reports. Every filter applies before the page and the count,
	// and a report that matches twice is counted once (FeedbackReportServed
	// and FeedbackReportInCrate state what a filter matches).
	ListFeedbackReports(ctx context.Context, query FeedbackReportQuery) (FeedbackReportPage, error)
	// GetFeedbackReport answers one report, or ErrNotFound.
	GetFeedbackReport(ctx context.Context, reportID string) (FeedbackReport, error)
	// ReadFeedbackReport answers one report with its details, read
	// together, or ErrNotFound.
	ReadFeedbackReport(ctx context.Context, reportID string) (FeedbackReport, FeedbackReportDetails, error)
	// ReadFeedbackReportDetails answers the links, notes, tasks, and crates
	// of the given reports.
	ReadFeedbackReportDetails(ctx context.Context, reports []FeedbackReport) (FeedbackReportDetails, error)
	// CountCrateOpenFeedbackReports answers, by environment ID, the number
	// of open reports that belong to the crate (invariant 23). An
	// environment with none has no entry.
	CountCrateOpenFeedbackReports(ctx context.Context, crateID string) (map[string]int, error)
	// RecordFeedbackReportAct records one act in one transaction, as
	// PlanFeedbackReportAct plans it, under the lock that serializes the
	// report's writes with report entries of its environment and with the
	// linking work. A report the workspace does not hold answers
	// ErrNotFound. Every refusal writes nothing.
	RecordFeedbackReportAct(ctx context.Context, act FeedbackReportAct) (FeedbackReportActResult, error)
}

// NormalizeFeedbackReportQuery checks a query before a backend reads it.
func NormalizeFeedbackReportQuery(query FeedbackReportQuery) (FeedbackReportQuery, error) {
	if query.State != FeedbackReportOpen && query.State != FeedbackReportClosed {
		return FeedbackReportQuery{}, fmt.Errorf("a report query needs the state open or closed")
	}
	if query.Limit < 1 || query.Limit > MaxFeedbackReportQueryLimit || query.Offset < 0 || query.Offset > MaxTaskOperationsOffset {
		return FeedbackReportQuery{}, fmt.Errorf("a report query needs a limit from 1 to %d and an offset from 0 to %d", MaxFeedbackReportQueryLimit, MaxTaskOperationsOffset)
	}
	return query, nil
}

// SortFeedbackReports orders reports newest last_seen first, then by ID, the
// order of every list (component-feedback-reports, Public surfaces).
func SortFeedbackReports(reports []FeedbackReport) {
	slices.SortStableFunc(reports, func(a, b FeedbackReport) int {
		if n := b.LastSeen.Compare(a.LastSeen); n != 0 {
			return n
		}
		return strings.Compare(a.ID, b.ID)
	})
}

// FeedbackReportServed reports whether a stored link is served: it is
// possible or confirmed, and the task or crate it names is a record of the
// workspace. A removed link, a link in another state, and a link whose
// target the workspace does not hold prove nothing and are not served
// (component-feedback-reports invariant 21). Every backend's filters use the
// same rule.
func FeedbackReportServed(link FeedbackReportLink, tasks map[string]FeedbackReportTaskRef, crates map[string]FeedbackReportCrateRef) bool {
	if link.State != FeedbackReportLinkPossible && link.State != FeedbackReportLinkConfirmed {
		return false
	}
	switch {
	case link.TaskID != "" && link.CrateID == "":
		_, ok := tasks[link.TaskID]
		return ok
	case link.CrateID != "" && link.TaskID == "":
		_, ok := crates[link.CrateID]
		return ok
	}
	return false
}

// FeedbackReportInCrate reports whether a served link puts its report in a
// crate: a link to the crate, or to a task that is a member of the crate at
// the read (component-feedback-reports invariant 21). An empty crateID asks
// whether the link puts the report in any crate.
func FeedbackReportInCrate(link FeedbackReportLink, tasks map[string]FeedbackReportTaskRef, crates map[string]FeedbackReportCrateRef, crateID string) bool {
	if !FeedbackReportServed(link, tasks, crates) {
		return false
	}
	target := link.CrateID
	if link.TaskID != "" {
		target = tasks[link.TaskID].CrateID
		if _, ok := crates[target]; !ok {
			return false
		}
	}
	if target == "" {
		return false
	}
	return crateID == "" || target == crateID
}

// FeedbackReportMatches reports whether a report's served links meet the
// task, crate, and no_crate filters of a query. The memory backend uses it,
// and the durable backends' queries state the same rule.
func FeedbackReportMatches(query FeedbackReportQuery, links []FeedbackReportLink, tasks map[string]FeedbackReportTaskRef, crates map[string]FeedbackReportCrateRef) bool {
	if query.TaskID != "" && !slices.ContainsFunc(links, func(link FeedbackReportLink) bool {
		return link.TaskID == query.TaskID && FeedbackReportServed(link, tasks, crates)
	}) {
		return false
	}
	if query.CrateID != "" && !slices.ContainsFunc(links, func(link FeedbackReportLink) bool {
		return FeedbackReportInCrate(link, tasks, crates, query.CrateID)
	}) {
		return false
	}
	if query.NoCrate && slices.ContainsFunc(links, func(link FeedbackReportLink) bool {
		return FeedbackReportInCrate(link, tasks, crates, "")
	}) {
		return false
	}
	return true
}

// FeedbackReportActTask is what an act needs to know of a task it names:
// whether the workspace holds it and whether it is merged.
type FeedbackReportActTask struct {
	Exists bool
	Merged bool
}

// FeedbackReportActTargets holds what a backend read, under the report
// lock, of the tasks and crates an act names.
type FeedbackReportActTargets struct {
	Tasks  map[string]FeedbackReportActTask
	Crates map[string]bool
}

// TaskIDs are the tasks an act names: a link's task, or the fix task and the
// causes.
func (a FeedbackReportAct) TaskIDs() []string {
	ids := []string{}
	if a.TaskID != "" {
		ids = append(ids, a.TaskID)
	}
	for _, id := range a.CauseTaskIDs {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids
}

// NormalizeFeedbackReportAct applies every rule of an act that needs no
// stored state, before any read or write. A refusal is a FeedbackFieldError
// that names the request member, and its message repeats no submitted value.
// It does not redact the reason; PrepareFeedbackReportAct does.
func NormalizeFeedbackReportAct(act FeedbackReportAct) (FeedbackReportAct, error) {
	if act.ReportID == "" {
		return FeedbackReportAct{}, fmt.Errorf("an act needs its report")
	}
	if act.At.IsZero() {
		return FeedbackReportAct{}, fmt.Errorf("an act needs its time")
	}
	act.At = act.At.UTC()
	switch act.Kind {
	case FeedbackReportActLink:
		if (act.TaskID == "") == (act.CrateID == "") {
			return FeedbackReportAct{}, feedbackFieldError("body", "must hold exactly one of task_id and crate_id")
		}
		if act.LinkID != "" || act.Reason != "" || act.CauseTaskIDs != nil {
			return FeedbackReportAct{}, fmt.Errorf("a link act names only its target")
		}
	case FeedbackReportActRemoveLink:
		if act.LinkID == "" || act.TaskID != "" || act.CrateID != "" || act.Reason != "" || act.CauseTaskIDs != nil {
			return FeedbackReportAct{}, fmt.Errorf("a removal names only its link")
		}
	case FeedbackReportActClose:
		if err := CheckFeedbackReportCloseReason(act.Reason); err != nil {
			return FeedbackReportAct{}, err
		}
		if act.TaskID != "" || act.CrateID != "" || act.LinkID != "" || act.CauseTaskIDs != nil {
			return FeedbackReportAct{}, fmt.Errorf("a close act names only its reason")
		}
	case FeedbackReportActReopen:
		if act.TaskID != "" || act.CrateID != "" || act.LinkID != "" || act.Reason != "" || act.CauseTaskIDs != nil {
			return FeedbackReportAct{}, fmt.Errorf("a reopen act names nothing")
		}
	case FeedbackReportActFixTask:
		if act.TaskID == "" {
			return FeedbackReportAct{}, feedbackFieldError("task_id", "must name a task")
		}
		if act.CrateID != "" || act.LinkID != "" || act.Reason != "" {
			return FeedbackReportAct{}, fmt.Errorf("a fix-task act names only the fix task and the causes")
		}
		if err := CheckFeedbackReportCauseTasks(act.CauseTaskIDs); err != nil {
			return FeedbackReportAct{}, err
		}
	default:
		return FeedbackReportAct{}, feedbackFieldError("kind", "must be link, remove_link, close, reopen, or fix_task")
	}
	return act, nil
}

// CheckFeedbackReportCloseReason bounds a close reason: valid UTF-8 text
// without a NUL character, of 1 to 500 characters counted as Unicode code
// points (component-feedback-reports invariant 17).
func CheckFeedbackReportCloseReason(reason string) error {
	if !utf8.ValidString(reason) || strings.ContainsRune(reason, 0) {
		return feedbackFieldError("reason", "must be UTF-8 text without a NUL character")
	}
	if count := utf8.RuneCountInString(reason); count < 1 || count > FeedbackReportCloseReasonMax {
		return feedbackFieldError("reason", "must be 1 to %d characters", FeedbackReportCloseReasonMax)
	}
	return nil
}

// CheckFeedbackReportCauseTasks bounds cause_task_ids: at most
// MaxFeedbackReportCauseTasks entries, each a task ID that is not empty and
// appears once. The bound is provisional (MaxFeedbackReportCauseTasks).
func CheckFeedbackReportCauseTasks(ids []string) error {
	if len(ids) > MaxFeedbackReportCauseTasks {
		return feedbackFieldError("cause_task_ids", "must name at most %d tasks", MaxFeedbackReportCauseTasks)
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" {
			return feedbackFieldError("cause_task_ids", "must hold task IDs that are not empty")
		}
		if seen[id] {
			return feedbackFieldError("cause_task_ids", "must name each task once")
		}
		seen[id] = true
	}
	return nil
}

// PrepareFeedbackReportAct normalizes an act and redacts a close reason with
// the current stored secrets, as a summary is redacted. A backend calls it
// before it takes a lock, so the secret read never waits on the write
// (component-feedback-reports invariants 5 and 17; req-security-boundaries
// REQ-3).
func PrepareFeedbackReportAct(ctx context.Context, source redact.SecretSource, act FeedbackReportAct) (FeedbackReportAct, error) {
	act, err := NormalizeFeedbackReportAct(act)
	if err != nil {
		return FeedbackReportAct{}, err
	}
	if act.Kind != FeedbackReportActClose {
		return act, nil
	}
	redactor, err := redact.WithSecrets(ctx, source, nil)
	if err != nil {
		return FeedbackReportAct{}, err
	}
	act.Reason, _ = redactor.Redact(act.Reason)
	return act, nil
}

// FeedbackReportActWrite is what one act writes, as PlanFeedbackReportAct
// plans it. Nothing is written when Changed is false.
type FeedbackReportActWrite struct {
	Changed bool
	// Report is the record to store when Changed is true.
	Report FeedbackReport
	// NewLinks are the links to insert. UpdatedLinks are stored links whose
	// state or actors changed.
	NewLinks     []FeedbackReportLink
	UpdatedLinks []FeedbackReportLink
	// Event is the one workspace event of the act.
	Event core.Event
}

func feedbackActTaskError(field, id string, task FeedbackReportActTask, needMerged bool) error {
	switch {
	case !task.Exists:
		return &FeedbackReportActError{Code: FeedbackActTaskNotFound, Field: field, Message: "must name a task of the workspace"}
	case needMerged && !task.Merged:
		return &FeedbackReportActError{Code: FeedbackActTaskNotMerged, Field: field, Message: "must name a merged task"}
	}
	return nil
}

// PlanFeedbackReportAct plans one act from the report, every stored link of
// the report, and the targets a backend read under the report lock
// (component-feedback-reports invariants 17 to 20; req-production-feedback
// AC-7.1, AC-7.2, AC-7.4, and AC-7.5). Every backend writes exactly this
// plan. The checks run in this order: a closed report refuses every act but
// reopening with ErrFeedbackReportClosed; then each target is checked; then
// the act is applied.
//
//   - Confirming or adding a link to a merged task or a crate of the
//     workspace sets it confirmed with the actor and time and keeps its
//     evidence. A removed link becomes confirmed again. A confirmed link is
//     left as it is.
//   - Removing a served link sets it removed with the actor and time, and the
//     report stays open. A removed link is left as it is. A link of another
//     report is not found.
//   - Closing stores the time, the actor, and the redacted reason.
//   - Reopening a closed report restores the open state and records the
//     reopening with the user as its actor. An open report is left as it is.
//   - The fix task replaces an earlier one with the actor and time, and each
//     cause gets a confirmed link as a link act gives it.
//
// An act that changes something clears the Inbox flag and appends one
// workspace event with IDs only. An act that changes nothing writes nothing
// and appends no event.
func PlanFeedbackReportAct(report FeedbackReport, stored []FeedbackReportLink, act FeedbackReportAct, targets FeedbackReportActTargets, actor Actor) (FeedbackReportActWrite, error) {
	if report.State == FeedbackReportClosed && act.Kind != FeedbackReportActReopen {
		return FeedbackReportActWrite{}, ErrFeedbackReportClosed
	}
	at := EnvironmentTime(act.At)
	write := FeedbackReportActWrite{Report: report}
	payload := map[string]any{"report_id": report.ID, "environment_id": report.EnvironmentID}
	kind := ""
	held := map[string]bool{}
	for _, link := range stored {
		held[link.ID] = true
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
	links := slices.Clone(stored)
	// confirm sets the link to a task or crate confirmed, and reports whether
	// it changed anything.
	confirm := func(taskID, crateID string) bool {
		for i, link := range links {
			if link.ReportID != report.ID || link.TaskID != taskID || link.CrateID != crateID {
				continue
			}
			if link.State == FeedbackReportLinkConfirmed {
				return false
			}
			link.State, link.ConfirmedBy, link.ConfirmedAt = FeedbackReportLinkConfirmed, actor.ID, &at
			link.RemovedBy, link.RemovedAt = "", nil
			links[i] = link
			write.UpdatedLinks = append(write.UpdatedLinks, link)
			return true
		}
		link := FeedbackReportLink{ID: nextID(), ReportID: report.ID, TaskID: taskID, CrateID: crateID, State: FeedbackReportLinkConfirmed,
			Evidence: []FeedbackReportEvidence{}, CreatedAt: at, CreatedBy: actor.ID, ConfirmedBy: actor.ID, ConfirmedAt: &at}
		links = append(links, link)
		write.NewLinks = append(write.NewLinks, link)
		return true
	}
	switch act.Kind {
	case FeedbackReportActLink:
		if act.TaskID != "" {
			if err := feedbackActTaskError("task_id", act.TaskID, targets.Tasks[act.TaskID], true); err != nil {
				return FeedbackReportActWrite{}, err
			}
			payload["task_id"] = act.TaskID
		} else {
			if !targets.Crates[act.CrateID] {
				return FeedbackReportActWrite{}, &FeedbackReportActError{Code: FeedbackActCrateNotFound, Field: "crate_id", Message: "must name a crate of the workspace"}
			}
			payload["crate_id"] = act.CrateID
		}
		write.Changed = confirm(act.TaskID, act.CrateID)
		kind = FeedbackReportLinkConfirmedEvent
	case FeedbackReportActRemoveLink:
		index := slices.IndexFunc(links, func(link FeedbackReportLink) bool { return link.ID == act.LinkID && link.ReportID == report.ID })
		if index < 0 {
			return FeedbackReportActWrite{}, &FeedbackReportActError{Code: FeedbackActLinkNotFound, Field: "link_id", Message: "must name a link of the report"}
		}
		link := links[index]
		payload["link_id"] = link.ID
		if link.TaskID != "" {
			payload["task_id"] = link.TaskID
		} else {
			payload["crate_id"] = link.CrateID
		}
		if link.State != FeedbackReportLinkRemoved {
			link.State, link.RemovedBy, link.RemovedAt = FeedbackReportLinkRemoved, actor.ID, &at
			write.UpdatedLinks = append(write.UpdatedLinks, link)
			write.Changed = true
		}
		kind = FeedbackReportLinkRemovedEvent
	case FeedbackReportActClose:
		write.Report.State = FeedbackReportClosed
		write.Report.ClosedAt, write.Report.ClosedBy, write.Report.CloseReason = &at, actor.ID, act.Reason
		write.Changed = true
		kind = FeedbackReportClosedEvent
	case FeedbackReportActReopen:
		if report.State == FeedbackReportClosed {
			origin := FeedbackReportOrigin{Source: FeedbackSourceUser, Label: "", Actor: actor.ID}
			write.Report.State = FeedbackReportOpen
			write.Report.ClosedAt, write.Report.ClosedBy, write.Report.CloseReason = nil, "", ""
			write.Report.ReopenedAt, write.Report.ReopenOrigin = &at, &origin
			write.Changed = true
		}
		kind = FeedbackReportReopenedEvent
	case FeedbackReportActFixTask:
		if err := feedbackActTaskError("task_id", act.TaskID, targets.Tasks[act.TaskID], false); err != nil {
			return FeedbackReportActWrite{}, err
		}
		for _, id := range act.CauseTaskIDs {
			if err := feedbackActTaskError("cause_task_ids", id, targets.Tasks[id], true); err != nil {
				return FeedbackReportActWrite{}, err
			}
		}
		if report.FixTaskID != act.TaskID {
			write.Report.FixTaskID, write.Report.FixTaskSetBy, write.Report.FixTaskSetAt = act.TaskID, actor.ID, &at
			write.Changed = true
		}
		confirmed := []string{}
		for _, id := range act.CauseTaskIDs {
			if confirm(id, "") {
				confirmed = append(confirmed, id)
				write.Changed = true
			}
		}
		payload["task_id"] = act.TaskID
		payload["cause_task_ids"] = confirmed
		kind = FeedbackReportFixTaskSetEvent
	default:
		return FeedbackReportActWrite{}, fmt.Errorf("unknown report act %q", act.Kind)
	}
	if !write.Changed {
		return FeedbackReportActWrite{Report: report}, nil
	}
	// Any act of a person takes the report out of the Inbox
	// (component-feedback-reports invariant 19).
	write.Report.InInbox = false
	write.Event = core.Event{Kind: kind, ActorID: actor.ID, ActorRole: actor.Role, At: at, Payload: core.JSONPayload(payload)}
	return write, nil
}
