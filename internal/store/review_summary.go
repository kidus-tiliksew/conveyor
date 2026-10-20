package store

// Review round summaries (req-review-gates-evidence REQ-10, AC-10.1, AC-10.2,
// AC-10.4, AC-10.7; DEC-79; feature-review-round-summary SU-2). A summary is
// one append-only review.summary_recorded task event over a settled round.
// It is informative: recording one changes no task row, work order, job,
// intervention, bounce count, approved head, gate, or review publication.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// ReviewSummaryRecordedEvent is the task event kind of one summary version.
const ReviewSummaryRecordedEvent = "review.summary_recorded"

// SU-2 bounds, counted in characters (runes), not bytes.
const (
	ReviewSummaryMaxSummary  = 4000
	ReviewSummaryMaxFindings = 50
	ReviewSummaryMaxProblem  = 1000
	ReviewSummaryMaxLocation = 300
	ReviewSummaryMaxFix      = 1000
	ReviewSummaryMaxModel    = 200
)

// Review summary severities.
const (
	ReviewSummarySeverityBlocking    = "blocking"
	ReviewSummarySeverityNonBlocking = "non-blocking"
)

// ReviewSummaryModelEnforcement is the only author model label: the model is
// reported by the caller and never enforced (SU-2).
const ReviewSummaryModelEnforcement = "self-reported"

var (
	// ErrReviewSummaryRoundNotSettled refuses a round below 1 or a round with no
	// review.round_completed event (HTTP 409).
	ErrReviewSummaryRoundNotSettled = errors.New("review_summary_round_not_settled: the review round has not settled")
	// ErrReviewSummaryInvalid refuses a field outside its bound, an unknown
	// severity, an unknown seat, or an uncited changes-requested seat (HTTP 400).
	ErrReviewSummaryInvalid = errors.New("review_summary_invalid")
	// ErrReviewSummaryConflict refuses a reused request_id whose content differs
	// from the recorded summary (HTTP 409).
	ErrReviewSummaryConflict = errors.New("review_summary_conflict: request_id was already recorded with different content")
)

// ReviewSummaryStore records review round summaries on every backend.
type ReviewSummaryStore interface {
	// RecordReviewSummaryCommand validates the request against the task's
	// event history and appends one review.summary_recorded event in a single
	// task-serialized transaction. An identical replay of a request_id returns
	// the recorded event and appends nothing.
	RecordReviewSummaryCommand(ctx context.Context, request ReviewSummaryRequest) (ReviewSummaryResult, error)
}

// ReviewSummaryFinding is one consolidated finding and the seats that raised it.
type ReviewSummaryFinding struct {
	Severity string `json:"severity"`
	Problem  string `json:"problem"`
	Location string `json:"location,omitempty"`
	Fix      string `json:"fix,omitempty"`
	Seats    []int  `json:"seats"`
}

// ReviewSummaryAuthor is the credential-derived author of one summary. The
// actor fields come from the authenticated context; work_order_id and
// session_id are present for agent callers; model is reported text.
type ReviewSummaryAuthor struct {
	ActorID          string         `json:"actor_id"`
	ActorRole        core.ActorRole `json:"actor_role"`
	WorkOrderID      string         `json:"work_order_id,omitempty"`
	SessionID        string         `json:"session_id,omitempty"`
	Model            string         `json:"model,omitempty"`
	ModelEnforcement string         `json:"model_enforcement"`
}

// ReviewSummaryRequest is one admitted request to record a summary.
type ReviewSummaryRequest struct {
	TaskID      string
	ReviewRound int
	RequestID   string
	Summary     string
	Findings    []ReviewSummaryFinding
	Author      ReviewSummaryAuthor
}

// ReviewSummaryPayload is the review.summary_recorded event payload (SU-2).
type ReviewSummaryPayload struct {
	WorkspaceID string                 `json:"workspace_id"`
	ReviewRound int                    `json:"review_round"`
	Version     int                    `json:"version"`
	RequestID   string                 `json:"request_id"`
	Summary     string                 `json:"summary"`
	Findings    []ReviewSummaryFinding `json:"findings"`
	Author      ReviewSummaryAuthor    `json:"author"`
}

// ReviewSummaryResult identifies the recorded summary version. Replayed is
// internal: REST answers 200 for a replay and 201 for a new version.
type ReviewSummaryResult struct {
	TaskID      string    `json:"task_id"`
	ReviewRound int       `json:"review_round"`
	Version     int       `json:"version"`
	EventID     int64     `json:"event_id"`
	RecordedAt  time.Time `json:"recorded_at"`
	Replayed    bool      `json:"-"`
}

// ReviewSummaryPlan is a backend's decision for one request under its task
// lock: either the recorded event a replay returns, or the event to append.
type ReviewSummaryPlan struct {
	Replay *ReviewSummaryResult
	Event  core.Event
}

// Result returns the result for the appended plan event once the backend
// knows its committed identity and time.
func (p ReviewSummaryPlan) Result(taskID string, eventID int64, recordedAt time.Time) ReviewSummaryResult {
	var payload ReviewSummaryPayload
	_ = json.Unmarshal(p.Event.Payload, &payload)
	return ReviewSummaryResult{TaskID: taskID, ReviewRound: payload.ReviewRound, Version: payload.Version, EventID: eventID, RecordedAt: recordedAt.UTC()}
}

// reviewRoundSettlement is the part of a review.round_completed payload a
// summary reads: each settled seat and its verdict.
type reviewRoundSettlement struct {
	ReviewRound int `json:"review_round"`
	Reviews     []struct {
		ReviewSeat int    `json:"review_seat"`
		Verdict    string `json:"verdict"`
	} `json:"reviews"`
}

// settledReviewRound finds the round's review.round_completed event and
// returns its seats and the seats that requested changes. Only that event
// settles a round; task and order state are never consulted.
func settledReviewRound(round int, events []core.Event) (seats map[int]bool, changesRequested []int, settled bool) {
	if round < 1 {
		return nil, nil, false
	}
	for _, event := range events {
		if event.Kind != "review.round_completed" {
			continue
		}
		var settlement reviewRoundSettlement
		if json.Unmarshal(event.Payload, &settlement) != nil || settlement.ReviewRound != round {
			continue
		}
		seats = map[int]bool{}
		blocking := map[int]bool{}
		for _, review := range settlement.Reviews {
			seats[review.ReviewSeat] = true
			if review.Verdict == "changes_requested" {
				blocking[review.ReviewSeat] = true
			}
		}
		for seat := range blocking {
			changesRequested = append(changesRequested, seat)
		}
		sort.Ints(changesRequested)
		return seats, changesRequested, true
	}
	return nil, nil, false
}

func reviewSummaryInvalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrReviewSummaryInvalid, fmt.Sprintf(format, args...))
}

func runeBounded(field, value string, minimum, maximum int) error {
	if count := utf8.RuneCountInString(value); count < minimum || count > maximum {
		if minimum > 0 {
			return reviewSummaryInvalid("%s must be %d to %d characters", field, minimum, maximum)
		}
		return reviewSummaryInvalid("%s must be at most %d characters", field, maximum)
	}
	return nil
}

// normalizeReviewSummaryRequest applies SU-2's only normalization, the
// trimmed summary, and represents absent findings as an empty list.
func normalizeReviewSummaryRequest(request ReviewSummaryRequest) ReviewSummaryRequest {
	request.Summary = strings.TrimSpace(request.Summary)
	findings := make([]ReviewSummaryFinding, 0, len(request.Findings))
	for _, finding := range request.Findings {
		finding.Seats = append([]int{}, finding.Seats...)
		findings = append(findings, finding)
	}
	request.Findings = findings
	request.Author.ModelEnforcement = ReviewSummaryModelEnforcement
	return request
}

// ValidateReviewSummary is the one SU-2 validation every backend calls with
// the task's complete event history. A missing or unsettled round is
// ErrReviewSummaryRoundNotSettled; every other rule is ErrReviewSummaryInvalid,
// and a coverage refusal names the uncited changes-requested seats.
func ValidateReviewSummary(request ReviewSummaryRequest, events []core.Event) error {
	seats, changesRequested, settled := settledReviewRound(request.ReviewRound, events)
	if !settled {
		return ErrReviewSummaryRoundNotSettled
	}
	if strings.TrimSpace(request.RequestID) == "" {
		return reviewSummaryInvalid("request_id is required")
	}
	if err := runeBounded("summary", strings.TrimSpace(request.Summary), 1, ReviewSummaryMaxSummary); err != nil {
		return err
	}
	if len(request.Findings) > ReviewSummaryMaxFindings {
		return reviewSummaryInvalid("findings must have at most %d items", ReviewSummaryMaxFindings)
	}
	if err := runeBounded("author model", request.Author.Model, 0, ReviewSummaryMaxModel); err != nil {
		return err
	}
	cited := map[int]bool{}
	for i, finding := range request.Findings {
		if finding.Severity != ReviewSummarySeverityBlocking && finding.Severity != ReviewSummarySeverityNonBlocking {
			return reviewSummaryInvalid("findings[%d].severity must be %q or %q", i, ReviewSummarySeverityBlocking, ReviewSummarySeverityNonBlocking)
		}
		if err := runeBounded(fmt.Sprintf("findings[%d].problem", i), finding.Problem, 1, ReviewSummaryMaxProblem); err != nil {
			return err
		}
		if err := runeBounded(fmt.Sprintf("findings[%d].location", i), finding.Location, 0, ReviewSummaryMaxLocation); err != nil {
			return err
		}
		if err := runeBounded(fmt.Sprintf("findings[%d].fix", i), finding.Fix, 0, ReviewSummaryMaxFix); err != nil {
			return err
		}
		if len(finding.Seats) == 0 {
			return reviewSummaryInvalid("findings[%d].seats must name at least one seat", i)
		}
		seen := map[int]bool{}
		for _, seat := range finding.Seats {
			if seen[seat] {
				return reviewSummaryInvalid("findings[%d].seats repeats seat %d", i, seat)
			}
			seen[seat] = true
			if !seats[seat] {
				return reviewSummaryInvalid("findings[%d].seats names seat %d, which is not a seat of review round %d", i, seat, request.ReviewRound)
			}
			cited[seat] = true
		}
	}
	var uncited []string
	for _, seat := range changesRequested {
		if !cited[seat] {
			uncited = append(uncited, fmt.Sprint(seat))
		}
	}
	if len(uncited) > 0 {
		return reviewSummaryInvalid("every seat that requested changes must be cited by at least one finding; uncited seats: %s", strings.Join(uncited, ", "))
	}
	return nil
}

// sameReviewSummaryContent compares every caller-controlled field of a
// replay with the recorded payload as typed values, never as JSON bytes and
// never including the recorded version or time.
func sameReviewSummaryContent(recorded ReviewSummaryPayload, request ReviewSummaryRequest) bool {
	recordedFindings := recorded.Findings
	if recordedFindings == nil {
		recordedFindings = []ReviewSummaryFinding{}
	}
	for i := range recordedFindings {
		if recordedFindings[i].Seats == nil {
			recordedFindings[i].Seats = []int{}
		}
	}
	return recorded.Summary == request.Summary &&
		reflect.DeepEqual(recordedFindings, request.Findings) &&
		recorded.Author == request.Author
}

// PlanReviewSummary decides one request under the backend's task lock from
// the task's complete event history: an identical request_id replay returns
// the recorded event, a different one is a conflict, and otherwise the
// validated request becomes the next version of the round. Versions count
// the round's earlier summary events, never global event IDs.
func PlanReviewSummary(workspace string, request ReviewSummaryRequest, events []core.Event) (ReviewSummaryPlan, error) {
	request = normalizeReviewSummaryRequest(request)
	earlier := 0
	for _, event := range events {
		if event.Kind != ReviewSummaryRecordedEvent {
			continue
		}
		var recorded ReviewSummaryPayload
		if json.Unmarshal(event.Payload, &recorded) != nil || recorded.ReviewRound != request.ReviewRound {
			continue
		}
		earlier++
		if recorded.RequestID != request.RequestID {
			continue
		}
		if !sameReviewSummaryContent(recorded, request) {
			return ReviewSummaryPlan{}, ErrReviewSummaryConflict
		}
		return ReviewSummaryPlan{Replay: &ReviewSummaryResult{
			TaskID: event.TaskID, ReviewRound: recorded.ReviewRound, Version: recorded.Version,
			EventID: event.ID, RecordedAt: event.At.UTC(), Replayed: true,
		}}, nil
	}
	if err := ValidateReviewSummary(request, events); err != nil {
		return ReviewSummaryPlan{}, err
	}
	payload := ReviewSummaryPayload{
		WorkspaceID: workspace, ReviewRound: request.ReviewRound, Version: earlier + 1, RequestID: request.RequestID,
		Summary: request.Summary, Findings: request.Findings, Author: request.Author,
	}
	return ReviewSummaryPlan{Event: core.Event{TaskID: request.TaskID, Kind: ReviewSummaryRecordedEvent, Payload: core.JSONPayload(payload)}}, nil
}

// RecordReviewSummaryCommand is the memory backend's SU-2 command. The store
// mutex serializes it with every other task command.
func (m *memory) RecordReviewSummaryCommand(ctx context.Context, request ReviewSummaryRequest) (ReviewSummaryResult, error) {
	if _, err := RequireActor(ctx); err != nil {
		return ReviewSummaryResult{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[request.TaskID]
	if selected, present := WorkspaceFromContext(ctx); !ok || (present && task.Workspace != selected) {
		return ReviewSummaryResult{}, fmt.Errorf("task %s: %w", request.TaskID, ErrNotFound)
	}
	plan, err := PlanReviewSummary(task.Workspace, request, m.events[task.ID])
	if err != nil {
		return ReviewSummaryResult{}, err
	}
	if plan.Replay != nil {
		return *plan.Replay, nil
	}
	m.appendEventLocked(ctx, plan.Event)
	recorded := m.events[task.ID][len(m.events[task.ID])-1]
	return plan.Result(task.ID, recorded.ID, recorded.At), nil
}
