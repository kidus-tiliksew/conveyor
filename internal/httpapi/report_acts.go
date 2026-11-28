package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// The acts a person records on a report (component-feedback-reports
// invariants 17 to 20; req-production-feedback REQ-7, AC-7.1, AC-7.2, AC-7.4,
// and AC-7.5; DEC-149(2)). Each route mounts in the workspace group behind
// the capability to create tasks. The checks run in this order, and every
// refusal for bad input is decided before any write:
//
//  1. The credential, the workspace, and the capability (the group's
//     middleware): 401, or 404 workspace_not_found.
//  2. The body: 413 over feedbackReportActBodyLimit, then 400
//     invalid_report_act for the shape and each field rule.
//  3. In one store transaction under the report lock: 404 report_not_found,
//     409 report_closed for every act but reopening on a closed report, then
//     the targets: 404 task_not_found, crate_not_found, or link_not_found, and
//     409 task_not_merged.
//  4. A store failure: 503 retryable, or a generic 500.
//
// A success answers 200 with the served report as it stands after the act.
// An act that changes nothing records nothing and answers the same way.

// feedbackReportActBodyLimit bounds the body of every act route: an act
// reads a body of at most 16 KiB, and a larger body answers 413 and records
// nothing (component-feedback-reports v3, Public surfaces, routes for people;
// component-http-api invariant 17).
const feedbackReportActBodyLimit = 16 << 10

// writeFeedbackReportActRefusal answers a refused body with 400
// invalid_report_act naming the field. The message repeats no submitted
// value, so a close reason is never echoed.
func writeFeedbackReportActRefusal(w http.ResponseWriter, err error) {
	var field *store.FeedbackFieldError
	if !errors.As(err, &field) {
		field = &store.FeedbackFieldError{Field: "body", Message: "must be one JSON object"}
	}
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_report_act", "field": field.Field, "message": field.Message})
}

// writeFeedbackReportActError answers an error of the act's store call.
func writeFeedbackReportActError(w http.ResponseWriter, r *http.Request, err error) {
	var target *store.FeedbackReportActError
	switch {
	case errors.Is(err, store.ErrFeedbackReportClosed):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "report_closed", "message": "The report is closed. Reopen it first."})
	case errors.As(err, &target):
		status := http.StatusNotFound
		if target.Code == store.FeedbackActTaskNotMerged {
			status = http.StatusConflict
		}
		writeJSON(w, status, map[string]string{"error": target.Code, "field": target.Field, "message": target.Message})
	case errors.As(err, new(*store.FeedbackFieldError)):
		writeFeedbackReportActRefusal(w, err)
	default:
		writeFeedbackReportStoreError(w, r, err)
	}
}

// readFeedbackReportActBody reads the bounded body of an act as one JSON
// object that holds only the allowed fields, each at most once and none
// null. An empty body reads as an empty object, so a route whose act names
// nothing accepts no body. It answers the refusal and reports false.
func readFeedbackReportActBody(w http.ResponseWriter, r *http.Request, allowed ...string) (map[string]json.RawMessage, bool) {
	body, err := readBoundedBody(w, r, feedbackReportActBodyLimit)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writeBodyTooLarge(w)
			return nil, false
		}
		writeFeedbackReportActRefusal(w, feedbackRefusal("body", "must be readable"))
		return nil, false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return map[string]json.RawMessage{}, true
	}
	// encoding/json replaces invalid UTF-8, which could change an ID.
	if !utf8.Valid(body) {
		writeFeedbackReportActRefusal(w, feedbackRefusal("body", "must be UTF-8 text"))
		return nil, false
	}
	fields, err := feedbackObject("body", body, allowed...)
	if err != nil && len(allowed) == 0 {
		err = feedbackRefusal("body", "must be empty or an empty JSON object")
	}
	if err != nil {
		writeFeedbackReportActRefusal(w, err)
		return nil, false
	}
	return fields, true
}

// recordFeedbackReportAct records one act and answers the served report.
//
// An answer says "send it again" (503 retryable) only when nothing was
// written, and once an act has committed the caller always learns that it
// succeeded (component-persistence invariant 8; component-feedback-reports v4
// invariants 17 to 20). So the store reads the report as it stands after the
// act, with its details, in the act's own transaction, and every failure is
// decided before the commit. After the commit the handler only builds the
// served form from that result, which reads nothing and cannot fail because
// the store refused a report without its environment before it wrote,
// writes the 200 answer, and then asks the linking job for a pending
// report, which cannot change the answer. An act that changed nothing
// answers the same way from the same result, and a refused act answers its
// refusal; neither depends on a later step.
func (s *Server) recordFeedbackReportAct(w http.ResponseWriter, r *http.Request, backend store.ReportStore, act store.FeedbackReportAct) {
	act.ReportID, act.At = chi.URLParam(r, "id"), s.feedbackNow()
	// The rules that need no stored state are decided here, before any
	// read or write. The store decides them again.
	if _, err := store.NormalizeFeedbackReportAct(act); err != nil {
		writeFeedbackReportActRefusal(w, err)
		return
	}
	result, err := backend.RecordFeedbackReportAct(s.feedbackReadContext(r.Context()), act)
	if err != nil {
		writeFeedbackReportActError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, buildServedFeedbackReport(result.Report, result.Details, servedFeedbackActEnvironment(result), servedFeedbackActSignal(result), result.Details.ReadAt))
	s.requestPendingFeedbackLinking(r.Context(), []store.FeedbackReport{result.Report})
}

// servedFeedbackActEnvironment is the environment of an act's report from
// the act's result; the store refused the act before it wrote if the
// workspace did not hold it.
func servedFeedbackActEnvironment(result store.FeedbackReportActResult) store.Environment {
	environment := result.Details.Environments[result.Report.EnvironmentID]
	return store.Environment{ID: result.Report.EnvironmentID, Name: environment.Name}
}

// servedFeedbackActSignal is the newest signal of the act's environment from
// the act's result, or nil.
func servedFeedbackActSignal(result store.FeedbackReportActResult) *store.FeedbackSignalState {
	return result.Details.Environments[result.Report.EnvironmentID].Signal
}

// linkFeedbackReport serves POST /v1/feedback-reports/{id}/links: it
// confirms a possible link, or adds a confirmed one, to task_id or crate_id.
func (s *Server) linkFeedbackReport(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.reportStore(w)
	if !ok {
		return
	}
	fields, ok := readFeedbackReportActBody(w, r, "task_id", "crate_id")
	if !ok {
		return
	}
	act := store.FeedbackReportAct{Kind: store.FeedbackReportActLink}
	var err error
	if act.TaskID, err = feedbackText("task_id", fields["task_id"]); err == nil {
		act.CrateID, err = feedbackText("crate_id", fields["crate_id"])
	}
	if err != nil {
		writeFeedbackReportActRefusal(w, err)
		return
	}
	s.recordFeedbackReportAct(w, r, backend, act)
}

// removeFeedbackReportLink serves DELETE
// /v1/feedback-reports/{id}/links/{link_id}.
func (s *Server) removeFeedbackReportLink(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.reportStore(w)
	if !ok {
		return
	}
	if _, ok := readFeedbackReportActBody(w, r); !ok {
		return
	}
	s.recordFeedbackReportAct(w, r, backend, store.FeedbackReportAct{Kind: store.FeedbackReportActRemoveLink, LinkID: chi.URLParam(r, "link_id")})
}

// closeFeedbackReport serves POST /v1/feedback-reports/{id}/close with a
// reason of 1 to 500 characters, which the store redacts with the stored
// secrets before it stores it.
func (s *Server) closeFeedbackReport(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.reportStore(w)
	if !ok {
		return
	}
	fields, ok := readFeedbackReportActBody(w, r, "reason")
	if !ok {
		return
	}
	if fields["reason"] == nil {
		writeFeedbackReportActRefusal(w, feedbackRefusal("reason", "is required"))
		return
	}
	reason, err := feedbackText("reason", fields["reason"])
	if err != nil {
		writeFeedbackReportActRefusal(w, err)
		return
	}
	s.recordFeedbackReportAct(w, r, backend, store.FeedbackReportAct{Kind: store.FeedbackReportActClose, Reason: reason})
}

// reopenFeedbackReport serves POST /v1/feedback-reports/{id}/reopen.
func (s *Server) reopenFeedbackReport(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.reportStore(w)
	if !ok {
		return
	}
	if _, ok := readFeedbackReportActBody(w, r); !ok {
		return
	}
	s.recordFeedbackReportAct(w, r, backend, store.FeedbackReportAct{Kind: store.FeedbackReportActReopen})
}

// setFeedbackReportFixTask serves PUT /v1/feedback-reports/{id}/fix-task:
// task_id is the fix task, and cause_task_ids names at most
// store.MaxFeedbackReportCauseTasks merged tasks, each once.
func (s *Server) setFeedbackReportFixTask(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.reportStore(w)
	if !ok {
		return
	}
	fields, ok := readFeedbackReportActBody(w, r, "task_id", "cause_task_ids")
	if !ok {
		return
	}
	if fields["task_id"] == nil {
		writeFeedbackReportActRefusal(w, feedbackRefusal("task_id", "is required"))
		return
	}
	act := store.FeedbackReportAct{Kind: store.FeedbackReportActFixTask}
	var err error
	if act.TaskID, err = feedbackText("task_id", fields["task_id"]); err == nil {
		act.CauseTaskIDs, err = feedbackCauseTaskIDs(fields["cause_task_ids"])
	}
	if err != nil {
		writeFeedbackReportActRefusal(w, err)
		return
	}
	s.recordFeedbackReportAct(w, r, backend, act)
}

// feedbackCauseTaskIDs reads the optional cause_task_ids array. Its size is
// checked before any entry is read, and each entry is a string that is not
// empty. An omitted array names no cause.
func feedbackCauseTaskIDs(raw json.RawMessage) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	var entries []json.RawMessage
	if environmentNull(raw) || json.Unmarshal(raw, &entries) != nil || entries == nil {
		return nil, feedbackRefusal("cause_task_ids", "must be an array of task IDs")
	}
	if len(entries) > store.MaxFeedbackReportCauseTasks {
		return nil, feedbackRefusal("cause_task_ids", fmt.Sprintf("must name at most %d tasks", store.MaxFeedbackReportCauseTasks))
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		id, err := feedbackText("cause_task_ids", entry)
		if err != nil || entry == nil {
			return nil, feedbackRefusal("cause_task_ids", "must hold task IDs that are not empty")
		}
		ids = append(ids, id)
	}
	if err := store.CheckFeedbackReportCauseTasks(ids); err != nil {
		return nil, err
	}
	return ids, nil
}
