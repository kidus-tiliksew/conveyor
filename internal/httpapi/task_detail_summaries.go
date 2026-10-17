package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Bounded task detail and history pages present events in a bounded wire
// form, and current facts come from summaries folded over the complete ledger
// by (at, id) order (task 261015-3721c6; component-http-api "Bounded detail
// view"; req-task-centric-operations-view AC-1.2, AC-2.2).

// humanTimelineEventKinds is the effective dashboard timeline rule: every kind
// noteFor can render, minus attemptEventKinds and the kinds noteFor always
// hides. TestHumanTimelineEventKindsMatchDashboard compares it with the
// humanTimelineEventKinds export of web/src/lib/activity.ts.
var humanTimelineEventKinds = []string{
	"task.assignee.set",
	"task.assignee.cleared",
	"pull_request.opened",
	"pipeline.bounce_limit",
	"job.timeout",
	"work_order.plan_revision_requested",
	"spec.version_created",
	"blueprint.materialized",
	"blueprint.closed",
	"task.dependency_unsatisfiable",
	"task.dependency_removed",
	"github_issue.publication_queued",
	"github_issue.publication_retry",
	"github_issue.publication_published",
	"github_issue.associated",
	"github_issue.publication_failed",
	"review.publication_retry",
	"review.publication_failed",
	"github.review_redirected",
	"review.round_completed",
	"work_order.preempted",
	"merge.requested",
	"merge.confirmed",
	"merge.reconciled",
	"merge.failed",
	"merge.blocked",
	"merge.conflict_fix_dispatched",
	"approval.stale",
	"review.refresh_head_advanced",
	"review.refresh_round_created",
	"review.refresh_skipped",
	"dispatch.failed",
	"job.log_stream_degraded",
	"pipeline.bounced",
	"task.state_changed",
	"task.dependency_added",
	// Task-context decisions are audited operator acts (req-operator-next-action
	// AC-2.2; task 261016-37e290).
	"task.context_proposal_confirmed",
	"task.context_proposal_dismissed",
	"work_order.child_failed",
	"work_order.stalled",
}

const (
	// taskEventPayloadLimit omits a stored payload above this many bytes from
	// a presented event; the audit read returns it.
	taskEventPayloadLimit = 8192
	// taskEventViewMaxBytes bounds one serialized presented event, so a
	// 65,536-byte page or event section always holds its first event.
	taskEventViewMaxBytes = 16384
	// taskEventSectionMaxBytes bounds the bounded view's events plus
	// event_page and a history page envelope.
	taskEventSectionMaxBytes = 65536
	// taskSummaryTextMaxBytes bounds each summary text value.
	taskSummaryTextMaxBytes  = 16384
	payloadIndexMaxFields    = 16
	payloadIndexMaxReviewIDs = 64
	payloadIndexMaxIDBytes   = 128
)

// taskEventTimeLayout is RFC 3339 UTC with microseconds, the persisted
// precision of every backend.
const taskEventTimeLayout = "2006-01-02T15:04:05.000000Z07:00"

func formatTaskEventTime(at time.Time) string {
	return at.UTC().Format(taskEventTimeLayout)
}

func formatTaskEventID(id int64) string { return strconv.FormatInt(id, 10) }

// taskEventView is the presented event of the bounded view and history pages.
// The ID is a canonical decimal string so IDs above 2^53 survive JavaScript.
type taskEventView struct {
	ID                    string                     `json:"id"`
	TaskID                string                     `json:"task_id"`
	JobID                 string                     `json:"job_id,omitempty"`
	Kind                  string                     `json:"kind"`
	ActorID               string                     `json:"actor_id"`
	ActorRole             core.ActorRole             `json:"actor_role"`
	Payload               json.RawMessage            `json:"payload"`
	At                    string                     `json:"at"`
	AuditAvailable        bool                       `json:"audit_available,omitempty"`
	PayloadOmitted        bool                       `json:"payload_omitted,omitempty"`
	PayloadBytes          int                        `json:"payload_bytes,omitempty"`
	PayloadIndex          map[string]json.RawMessage `json:"payload_index,omitempty"`
	PayloadIndexTruncated bool                       `json:"payload_index_truncated,omitempty"`
}

// minimalTaskEventView is presented when an event's other fields alone would
// exceed taskEventViewMaxBytes; traversal still progresses past it.
type minimalTaskEventView struct {
	ID             string          `json:"id"`
	Kind           string          `json:"kind"`
	Payload        json.RawMessage `json:"payload"`
	At             string          `json:"at"`
	AuditAvailable bool            `json:"audit_available"`
	PayloadOmitted bool            `json:"payload_omitted"`
}

// renderTaskEventView presents one event. omittedBytes is the stored payload
// length a store PayloadLimit omitted (event.Payload is then nil); full is
// the complete stored payload when known, used only for payload_index.
func renderTaskEventView(event core.Event, omittedBytes int, full json.RawMessage) (json.RawMessage, error) {
	view := taskEventView{
		ID: formatTaskEventID(event.ID), TaskID: event.TaskID, JobID: event.JobID, Kind: event.Kind,
		ActorID: event.ActorID, ActorRole: event.ActorRole, At: formatTaskEventTime(event.At),
	}
	if omittedBytes == 0 && len(event.Payload) > taskEventPayloadLimit {
		omittedBytes, full = len(event.Payload), event.Payload
	}
	if omittedBytes == 0 {
		payload, stripped, err := omitAuditSnapshots(event.Payload)
		if err != nil {
			return nil, err
		}
		view.Payload, view.AuditAvailable = payload, stripped
		data, err := json.Marshal(view)
		if err != nil {
			return nil, err
		}
		if len(data) <= taskEventViewMaxBytes {
			return data, nil
		}
		// Escaping can grow a small stored payload past the event bound.
		omittedBytes, full = len(event.Payload), event.Payload
	}
	view.Payload, view.AuditAvailable, view.PayloadOmitted, view.PayloadBytes = nil, true, true, omittedBytes
	view.PayloadIndex, view.PayloadIndexTruncated = taskEventPayloadIndex(event.Kind, full)
	data, err := json.Marshal(view)
	if err != nil {
		return nil, err
	}
	if len(data) <= taskEventViewMaxBytes {
		return data, nil
	}
	return json.Marshal(minimalTaskEventView{ID: view.ID, Kind: event.Kind, At: view.At, AuditAvailable: true, PayloadOmitted: true})
}

// taskEventPayloadIndex keeps, for an omitted payload, at most 16 top-level
// number and boolean fields in sorted key order and, for
// review.round_completed, at most 64 review work-order IDs of at most 128
// bytes, so the dashboard can still evaluate review-panel absorption.
func taskEventPayloadIndex(kind string, payload json.RawMessage) (map[string]json.RawMessage, bool) {
	if len(payload) == 0 {
		return nil, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil || fields == nil {
		return nil, false
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	index, truncated := map[string]json.RawMessage{}, false
	added := 0
	for _, key := range keys {
		value := bytes.TrimSpace(fields[key])
		if !jsonScalarNumberOrBool(value) {
			continue
		}
		if added == payloadIndexMaxFields {
			truncated = true
			break
		}
		index[key] = append(json.RawMessage(nil), value...)
		added++
	}
	if kind == "review.round_completed" {
		var reviews []json.RawMessage
		if raw, ok := fields["reviews"]; ok && json.Unmarshal(raw, &reviews) == nil {
			ids := []string{}
			for _, review := range reviews {
				var entry map[string]json.RawMessage
				if json.Unmarshal(review, &entry) != nil || entry == nil {
					continue
				}
				var id string
				if raw, ok := entry["review_work_order_id"]; !ok || json.Unmarshal(raw, &id) != nil {
					continue
				}
				if len(id) > payloadIndexMaxIDBytes || len(ids) == payloadIndexMaxReviewIDs {
					truncated = true
					continue
				}
				ids = append(ids, id)
			}
			if len(ids) > 0 {
				encoded, _ := json.Marshal(ids)
				index["review_work_order_ids"] = encoded
			}
		}
	}
	if len(index) == 0 {
		return nil, truncated
	}
	return index, truncated
}

func jsonScalarNumberOrBool(value []byte) bool {
	if len(value) == 0 {
		return false
	}
	if bytes.Equal(value, []byte("true")) || bytes.Equal(value, []byte("false")) {
		return true
	}
	return value[0] == '-' || value[0] >= '0' && value[0] <= '9'
}

// truncateSummaryText cuts text to at most taskSummaryTextMaxBytes at a rune
// boundary.
func truncateSummaryText(text string) (string, bool) {
	if len(text) <= taskSummaryTextMaxBytes {
		return text, false
	}
	cut := taskSummaryTextMaxBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut], true
}

// taskDetailSummaries are the current facts the dashboard previously folded
// from the complete event list. Each "latest" is the maximum (at, id) tuple
// among the events satisfying that rule's own predicate, never a numeric ID.
type taskDetailSummaries struct {
	Creator                      *taskCreatorSummary       `json:"creator,omitempty"`
	PullRequest                  *taskPullRequestSummary   `json:"pull_request,omitempty"`
	ReviewRound                  *taskReviewRoundSummary   `json:"review_round,omitempty"`
	ReturnedForChanges           *taskReturnedForChanges   `json:"returned_for_changes,omitempty"`
	RunImplementation            bool                      `json:"run_implementation"`
	PlanRevisionRequest          *taskPlanRevisionRequest  `json:"plan_revision_request,omitempty"`
	LatestReviewCompletedEventID string                    `json:"latest_review_completed_event_id,omitempty"`
	TriageFailure                *taskTriageFailure        `json:"triage_failure,omitempty"`
	JobResults                   map[string]*taskJobResult `json:"job_results"`
	ReviewResults                taskReviewResults         `json:"review_results"`
	ReviewRounds                 taskReviewRounds          `json:"review_rounds"`
	ReviewBounces                map[string]jsNumber       `json:"review_bounces"`
}

type taskCreatorSummary struct {
	ActorID   string         `json:"actor_id"`
	ActorRole core.ActorRole `json:"actor_role"`
}

type taskPullRequestSummary struct {
	URL        string    `json:"url,omitempty"`
	Number     *jsNumber `json:"number,omitempty"`
	Repository string    `json:"repository,omitempty"`
	HeadSHA    string    `json:"head_sha,omitempty"`
	BaseSHA    string    `json:"base_sha,omitempty"`
	EventID    string    `json:"event_id"`
	At         string    `json:"at"`
	Truncated  bool      `json:"truncated,omitempty"`
}

type taskReviewRoundSummary struct {
	Verdict     string  `json:"verdict"`
	Summary     *string `json:"summary,omitempty"`
	ReviewCount int     `json:"review_count"`
	EventID     string  `json:"event_id"`
	At          string  `json:"at"`
	Truncated   bool    `json:"truncated,omitempty"`
}

type taskReturnedForChanges struct {
	Feedback  string `json:"feedback"`
	At        string `json:"at"`
	EventID   string `json:"event_id"`
	Truncated bool   `json:"truncated,omitempty"`
}

type taskPlanRevisionRequest struct {
	Rationale   string   `json:"rationale"`
	PlanVersion jsNumber `json:"plan_version"`
	EventID     string   `json:"event_id"`
	At          string   `json:"at"`
	Truncated   bool     `json:"truncated,omitempty"`
}

type taskTriageDiagnostic struct {
	Phase        string `json:"phase,omitempty"`
	ProviderCode string `json:"provider_code,omitempty"`
}

type taskTriageFailure struct {
	JobID      string                `json:"job_id"`
	EventID    string                `json:"event_id,omitempty"`
	Error      string                `json:"error"`
	Diagnostic *taskTriageDiagnostic `json:"diagnostic,omitempty"`
	Superseded bool                  `json:"superseded"`
	At         string                `json:"at,omitempty"`
	Truncated  bool                  `json:"truncated,omitempty"`
}

type taskSummaryText struct {
	Text      string `json:"text"`
	EventID   string `json:"event_id"`
	Truncated bool   `json:"truncated,omitempty"`
}

type taskStageSummaryText struct {
	Text           string `json:"text"`
	ReviewFeedback string `json:"review_feedback,omitempty"`
	EventID        string `json:"event_id"`
	Truncated      bool   `json:"truncated,omitempty"`
}

type taskOutputRejection struct {
	EventID   string `json:"event_id"`
	Error     string `json:"error"`
	Truncated bool   `json:"truncated,omitempty"`
}

type taskJobResult struct {
	Summary         *taskSummaryText      `json:"summary,omitempty"`
	StageSummary    *taskStageSummaryText `json:"stage_summary,omitempty"`
	OutputRejection *taskOutputRejection  `json:"output_rejection,omitempty"`
	FailureError    *taskSummaryText      `json:"failure_error,omitempty"`
}

type taskReviewResult struct {
	Verdict   string `json:"verdict"`
	Summary   string `json:"summary"`
	Feedback  string `json:"feedback"`
	At        string `json:"at"`
	EventID   string `json:"event_id"`
	Truncated bool   `json:"truncated,omitempty"`
}

type taskReviewResults struct {
	ByOrder map[string]taskReviewResult `json:"by_order"`
	ByJob   map[string]taskReviewResult `json:"by_job"`
}

type taskReviewRoundItem struct {
	EventID   string  `json:"event_id"`
	Verdict   string  `json:"verdict"`
	Summary   *string `json:"summary,omitempty"`
	At        string  `json:"at"`
	Truncated bool    `json:"truncated,omitempty"`
}

type taskReviewRounds struct {
	ByRound map[string]taskReviewRoundItem `json:"by_round"`
	ByOrder map[string]taskReviewRoundItem `json:"by_order"`
}

// summaryPayload is a payload decoded with JSON.parse semantics: numbers stay
// json.Number, and a non-object payload reads as empty like `payload ?? {}`.
type summaryPayload map[string]any

func decodeSummaryPayload(raw json.RawMessage) summaryPayload {
	if len(raw) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var fields map[string]any
	if decoder.Decode(&fields) != nil {
		return nil
	}
	return fields
}

// str reports a JSON string value (typeof === 'string').
func (p summaryPayload) str(key string) (string, bool) {
	value, ok := p[key].(string)
	return value, ok
}

// num reports a JSON number value (typeof === 'number').
func (p summaryPayload) num(key string) (float64, bool) {
	return jsonNumberValue(p[key])
}

func jsonNumberValue(value any) (float64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(string(number), 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	return f, true
}

// jsNumber marshals a float64 the way JavaScript prints it, so keys and
// values match what the dashboard computed from JSON.parse.
type jsNumber float64

func (n jsNumber) MarshalJSON() ([]byte, error) {
	f := float64(n)
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return []byte("null"), nil
	}
	return []byte(jsNumberString(f)), nil
}

// jsNumberString is ECMAScript Number::toString for radix 10.
func jsNumberString(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
		return "0"
	}
	sign := ""
	if f < 0 {
		sign, f = "-", -f
	}
	mantissa, exponent, _ := strings.Cut(strconv.FormatFloat(f, 'e', -1, 64), "e")
	digits := strings.Replace(mantissa, ".", "", 1)
	exp, _ := strconv.Atoi(exponent)
	k, n := len(digits), exp+1
	switch {
	case k <= n && n <= 21:
		return sign + digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		return sign + digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		return sign + "0." + strings.Repeat("0", -n) + digits
	}
	e, expSign := n-1, "+"
	if e < 0 {
		e, expSign = -e, "-"
	}
	m := digits[:1]
	if k > 1 {
		m += "." + digits[1:]
	}
	return sign + m + "e" + expSign + strconv.Itoa(e)
}

// jsSpace is the ECMAScript WhiteSpace and LineTerminator set that
// String.prototype.trim and parseInt skip.
func jsSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xA0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}

func jsTrim(text string) string { return strings.TrimFunc(text, jsSpace) }

// jsLineTerminator is the set `.` does not match in a JavaScript RegExp.
func jsLineTerminator(r rune) bool {
	return r == '\n' || r == '\r' || r == 0x2028 || r == 0x2029
}

// jsToString is String(value) for a JSON.parse result, with null mapped to
// the empty string as `value ?? ”` and array joins do.
func jsToString(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case bool:
		if v {
			return "true"
		}
		return "false"
	case json.Number:
		f, _ := jsonNumberValue(v)
		return jsNumberString(f)
	case []any:
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = jsToString(item)
		}
		return strings.Join(parts, ",")
	default:
		return "[object Object]"
	}
}

// jsParseInt is Number.parseInt(text, 10).
func jsParseInt(text string) float64 {
	text = strings.TrimLeftFunc(text, jsSpace)
	sign := 1.0
	if strings.HasPrefix(text, "-") {
		sign, text = -1, text[1:]
	} else if strings.HasPrefix(text, "+") {
		text = text[1:]
	}
	end := 0
	for end < len(text) && text[end] >= '0' && text[end] <= '9' {
		end++
	}
	if end == 0 {
		return math.NaN()
	}
	f, _ := strconv.ParseFloat(text[:end], 64)
	return sign * f
}

func jsIsInteger(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0) && f == math.Trunc(f)
}

func reviewVerdict(payload summaryPayload) string {
	if verdict, _ := payload.str("verdict"); verdict == "changes_requested" {
		return "changes_requested"
	}
	return "approve"
}

// sortedTaskEvents orders a copy of the ledger by (at, id), the persistence
// order every summary rule folds over.
func sortedTaskEvents(events []core.Event) []core.Event {
	sorted := append([]core.Event(nil), events...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return store.TaskEventAfter(sorted[j], store.TaskEventPosition{At: sorted[i].At, ID: sorted[i].ID})
	})
	return sorted
}

// summaryKinds are the event kinds any summary rule reads a payload from.
var summaryKinds = map[string]bool{
	"task.created": true, "pull_request.opened": true, "review.round_completed": true,
	"pipeline.bounced": true, "work_order.claimed": true, "work_order.plan_revision_requested": true,
	"review.completed": true, "job.failed": true, "job.summary": true, "triage.completed": true,
	"triage.output_invalid": true, "spec.output_invalid": true, "review.output_invalid": true,
}

// deriveTaskDetailSummaries reproduces the dashboard's selection rules in
// web/src/lib/activity.ts (taskCreator, mergeGateReview,
// pendingUserRequestChanges, userRunImplementation, latestPlanRevisionRequest,
// failedTriage, buildReviewPanels, preferredJobSummary, rejectedOutputEvent,
// jobSummary) over the complete ledger in (at, id) order.
func deriveTaskDetailSummaries(events []core.Event, jobs []core.Job, orders []core.WorkOrder) taskDetailSummaries {
	summaries := taskDetailSummaries{
		JobResults:    map[string]*taskJobResult{},
		ReviewResults: taskReviewResults{ByOrder: map[string]taskReviewResult{}, ByJob: map[string]taskReviewResult{}},
		ReviewRounds:  taskReviewRounds{ByRound: map[string]taskReviewRoundItem{}, ByOrder: map[string]taskReviewRoundItem{}},
		ReviewBounces: map[string]jsNumber{},
	}
	ordered := sortedTaskEvents(events)
	jobResult := func(jobID string) *taskJobResult {
		result := summaries.JobResults[jobID]
		if result == nil {
			result = &taskJobResult{}
			summaries.JobResults[jobID] = result
		}
		return result
	}
	var lastPlanRevision *core.Event
	var lastPlanRevisionPayload summaryPayload
	for i := range ordered {
		event := ordered[i]
		if !summaryKinds[event.Kind] {
			continue
		}
		payload := decodeSummaryPayload(event.Payload)
		eventID, at := formatTaskEventID(event.ID), formatTaskEventTime(event.At)
		switch event.Kind {
		case "task.created":
			if summaries.Creator == nil {
				summaries.Creator = &taskCreatorSummary{ActorID: event.ActorID, ActorRole: event.ActorRole}
			}
		case "pull_request.opened":
			pr := &taskPullRequestSummary{EventID: eventID, At: at}
			pr.HeadSHA, _ = payload.str("head_sha")
			pr.BaseSHA, _ = payload.str("base_sha")
			if url, _ := payload.str("url"); url != "" {
				pr.URL = url
				if number, ok := payload.num("number"); ok {
					value := jsNumber(number)
					pr.Number = &value
				}
				pr.Repository, _ = payload.str("repository")
			}
			var cut [5]bool
			pr.URL, cut[0] = truncateSummaryText(pr.URL)
			pr.Repository, cut[1] = truncateSummaryText(pr.Repository)
			pr.HeadSHA, cut[2] = truncateSummaryText(pr.HeadSHA)
			pr.BaseSHA, cut[3] = truncateSummaryText(pr.BaseSHA)
			pr.Truncated = cut[0] || cut[1] || cut[2] || cut[3]
			summaries.PullRequest = pr
		case "review.round_completed":
			round := &taskReviewRoundSummary{Verdict: reviewVerdict(payload), EventID: eventID, At: at}
			item := taskReviewRoundItem{EventID: eventID, Verdict: round.Verdict, At: at}
			if text, ok := payload.str("summary"); ok {
				cut, truncated := truncateSummaryText(text)
				round.Summary, round.Truncated = &cut, truncated
				item.Summary, item.Truncated = &cut, truncated
			}
			reviews, _ := payload["reviews"].([]any)
			round.ReviewCount = len(reviews)
			summaries.ReviewRound = round
			if number, ok := payload.num("review_round"); ok && number > 0 {
				summaries.ReviewRounds.ByRound[jsNumberString(number)] = item
			}
			for _, review := range reviews {
				entry, _ := review.(map[string]any)
				if orderID, ok := entry["review_work_order_id"].(string); ok {
					summaries.ReviewRounds.ByOrder[orderID] = item
				}
			}
		case "pipeline.bounced":
			if source, _ := payload.str("source"); source == userRequestChangesSource {
				feedback, _ := payload.str("feedback")
				pending := &taskReturnedForChanges{At: at, EventID: eventID}
				pending.Feedback, pending.Truncated = truncateSummaryText(jsTrim(feedback))
				summaries.ReturnedForChanges = pending
			}
			round, roundOK := payload.num("review_round")
			count, countOK := payload.num("count")
			if roundOK && countOK {
				summaries.ReviewBounces[jsNumberString(round)] = jsNumber(count)
			}
		case "work_order.claimed":
			if stage, _ := payload.str("stage"); stage == string(core.StageImplement) {
				summaries.ReturnedForChanges = nil
				claimant, _ := payload.str("claimant_id")
				rest, isRun := strings.CutPrefix(claimant, "run:")
				first, _ := utf8.DecodeRuneInString(rest)
				summaries.RunImplementation = isRun && rest != "" && !jsLineTerminator(first)
			}
		case "work_order.plan_revision_requested":
			lastPlanRevision, lastPlanRevisionPayload = &ordered[i], payload
		case "review.completed":
			summaries.LatestReviewCompletedEventID = eventID
			review := taskReviewResult{Verdict: reviewVerdict(payload), At: at, EventID: eventID}
			var cutSummary, cutFeedback bool
			text, _ := payload.str("summary")
			review.Summary, cutSummary = truncateSummaryText(text)
			feedback, _ := payload.str("feedback")
			review.Feedback, cutFeedback = truncateSummaryText(feedback)
			review.Truncated = cutSummary || cutFeedback
			if orderID, ok := payload.str("review_work_order_id"); ok {
				summaries.ReviewResults.ByOrder[orderID] = review
			}
			if event.JobID != "" {
				summaries.ReviewResults.ByJob[event.JobID] = review
			}
		}
		if event.JobID == "" {
			continue
		}
		switch event.Kind {
		case "job.summary":
			if text, ok := payload.str("summary"); ok {
				cut, truncated := truncateSummaryText(text)
				jobResult(event.JobID).Summary = &taskSummaryText{Text: cut, EventID: eventID, Truncated: truncated}
			}
		case "triage.completed", "review.completed":
			if text, _ := payload.str("summary"); text != "" {
				stage := &taskStageSummaryText{EventID: eventID}
				var cutText, cutFeedback bool
				stage.Text, cutText = truncateSummaryText(text)
				if event.Kind == "review.completed" {
					feedback, _ := payload.str("feedback")
					stage.ReviewFeedback, cutFeedback = truncateSummaryText(jsTrim(feedback))
				}
				stage.Truncated = cutText || cutFeedback
				jobResult(event.JobID).StageSummary = stage
			}
		case "triage.output_invalid", "spec.output_invalid", "review.output_invalid":
			text, _ := payload.str("error")
			rejection := &taskOutputRejection{EventID: eventID}
			rejection.Error, rejection.Truncated = truncateSummaryText(jsTrim(text))
			jobResult(event.JobID).OutputRejection = rejection
		case "job.failed":
			if text, _ := payload.str("error"); text != "" {
				cut, truncated := truncateSummaryText(text)
				jobResult(event.JobID).FailureError = &taskSummaryText{Text: cut, EventID: eventID, Truncated: truncated}
			}
		}
	}
	if lastPlanRevision != nil {
		summaries.PlanRevisionRequest = planRevisionRequestFrom(*lastPlanRevision, lastPlanRevisionPayload)
	}
	summaries.TriageFailure = deriveTriageFailure(ordered, jobs, orders)
	return summaries
}

// userRequestChangesSource is the bounce source the merge-gate
// request-changes command records (store.UserRequestChangesPending).
const userRequestChangesSource = "user-request-changes"

// planRevisionRequestFrom accepts only a decidable request: a non-blank
// rationale and a positive integer plan version (activity.ts
// planRevisionRequestFrom).
func planRevisionRequestFrom(event core.Event, payload summaryPayload) *taskPlanRevisionRequest {
	rationale, _ := payload.str("rationale")
	rationale = jsTrim(rationale)
	version, ok := payload.num("plan_version")
	if !ok {
		version = jsParseInt(jsToString(payload["plan_version"]))
	}
	if rationale == "" || !jsIsInteger(version) || version <= 0 {
		return nil
	}
	request := &taskPlanRevisionRequest{PlanVersion: jsNumber(version), EventID: formatTaskEventID(event.ID), At: formatTaskEventTime(event.At)}
	request.Rationale, request.Truncated = truncateSummaryText(rationale)
	return request
}

// deriveTriageFailure finds the latest triage job's failure. Jobs are
// ordered as failedTriage orders them: a stable sort on the millisecond
// started_at, else ended_at, else zero. The dashboard keeps the task-state
// conditions; superseded compares by (at, id), never by numeric ID.
func deriveTriageFailure(ordered []core.Event, jobs []core.Job, orders []core.WorkOrder) *taskTriageFailure {
	if len(jobs) == 0 {
		return nil
	}
	sortedJobs := append([]core.Job(nil), jobs...)
	key := func(job core.Job) int64 {
		switch {
		case !job.StartedAt.IsZero():
			return job.StartedAt.UnixMilli()
		case !job.EndedAt.IsZero():
			return job.EndedAt.UnixMilli()
		}
		return 0
	}
	sort.SliceStable(sortedJobs, func(i, j int) bool { return key(sortedJobs[i]) < key(sortedJobs[j]) })
	latest := sortedJobs[len(sortedJobs)-1]
	if latest.Stage != core.StageTriage {
		return nil
	}
	var failure *core.Event
	for i := len(ordered) - 1; i >= 0; i-- {
		if ordered[i].JobID == latest.ID && ordered[i].Kind == "job.failed" {
			failure = &ordered[i]
			break
		}
	}
	if latest.State != core.JobFailed && failure == nil {
		return nil
	}
	result := &taskTriageFailure{JobID: latest.ID}
	var failureTime time.Time
	switch {
	case failure != nil:
		failureTime = failure.At
		result.EventID = formatTaskEventID(failure.ID)
	case !latest.EndedAt.IsZero():
		failureTime = latest.EndedAt
	case !latest.StartedAt.IsZero():
		failureTime = latest.StartedAt
	}
	if !failureTime.IsZero() {
		result.At = formatTaskEventTime(failureTime)
	}
	superseding := func(kind string) bool {
		return strings.HasPrefix(kind, "intervention.") || kind == "pipeline.dispatched" || kind == "job.created" || kind == "work_order.claimed"
	}
	for _, event := range ordered {
		if !superseding(event.Kind) {
			continue
		}
		if failure != nil && store.TaskEventAfter(event, store.TaskEventPosition{At: failure.At, ID: failure.ID}) ||
			failure == nil && !failureTime.IsZero() && event.At.After(failureTime) {
			result.Superseded = true
			break
		}
	}
	if !failureTime.IsZero() {
		for _, order := range orders {
			entered := order.CreatedAt
			if entered.IsZero() {
				entered = order.QueueEnteredAt
			}
			if entered.After(failureTime) {
				result.Superseded = true
				break
			}
		}
	}
	var payload summaryPayload
	if failure != nil {
		payload = decodeSummaryPayload(failure.Payload)
	}
	recorded, _ := payload.str("error")
	var cutError, cutPhase, cutCode bool
	result.Error, cutError = truncateSummaryText(jsTrim(recorded))
	if diagnostic, ok := payload["diagnostic"].(map[string]any); ok {
		phase, _ := diagnostic["phase"].(string)
		code, _ := diagnostic["provider_code"].(string)
		if phase != "" || code != "" {
			result.Diagnostic = &taskTriageDiagnostic{}
			result.Diagnostic.Phase, cutPhase = truncateSummaryText(phase)
			result.Diagnostic.ProviderCode, cutCode = truncateSummaryText(code)
		}
	}
	result.Truncated = cutError || cutPhase || cutCode
	return result
}
