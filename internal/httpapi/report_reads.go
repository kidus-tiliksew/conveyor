package httpapi

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// The routes for people that read reports (component-feedback-reports,
// Public surfaces, and invariants 21 and 22; req-production-feedback AC-6.1
// to AC-6.3). They mount in the workspace group, which requires the
// capability to view the workspace, and change no report: a read that serves
// a report with links_pending asks the linking job again (invariant 15).

// defaultFeedbackReportPageLimit is the default page of the report list,
// whose rows grow with workspace history (component-http-api invariant 19).
const defaultFeedbackReportPageLimit = defaultHistoryPageLimit

// writeFeedbackReportReadRefusal answers a refused query parameter with 400
// invalid_report_query naming the parameter. The message repeats no value.
func writeFeedbackReportReadRefusal(w http.ResponseWriter, field, message string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_report_query", "field": field, "message": message})
}

// feedbackReportQueryValue reads one optional query parameter that appears
// at most once and is not empty when it appears.
func feedbackReportQueryValue(values url.Values, name string) (string, bool, string) {
	list, present := values[name]
	switch {
	case !present:
		return "", true, ""
	case len(list) != 1:
		return "", false, "must be supplied once"
	case list[0] == "":
		return "", false, "must not be empty"
	}
	return list[0], true, ""
}

// parseFeedbackReportQuery reads the filters of the report list. state is
// open, the default, or closed; no_crate is true or false.
func parseFeedbackReportQuery(values url.Values) (store.FeedbackReportQuery, string, string) {
	query := store.FeedbackReportQuery{State: store.FeedbackReportOpen}
	state, ok, message := feedbackReportQueryValue(values, "state")
	if !ok {
		return query, "state", message
	}
	switch state {
	case "", store.FeedbackReportOpen:
	case store.FeedbackReportClosed:
		query.State = store.FeedbackReportClosed
	default:
		return query, "state", "must be open or closed"
	}
	for _, filter := range []struct {
		name   string
		target *string
	}{{"environment_id", &query.EnvironmentID}, {"task_id", &query.TaskID}, {"crate_id", &query.CrateID}} {
		value, ok, message := feedbackReportQueryValue(values, filter.name)
		if !ok {
			return query, filter.name, message
		}
		*filter.target = value
	}
	noCrate, ok, message := feedbackReportQueryValue(values, "no_crate")
	if !ok {
		return query, "no_crate", message
	}
	switch noCrate {
	case "", "false":
	case "true":
		query.NoCrate = true
	default:
		return query, "no_crate", "must be true or false"
	}
	return query, "", ""
}

// reportStore answers the store's report capability, or answers 501.
func (s *Server) reportStore(w http.ResponseWriter) (store.ReportStore, bool) {
	backend, ok := s.Store.(store.ReportStore)
	if !ok {
		http.Error(w, "feedback reports are not available on this store", http.StatusNotImplemented)
	}
	return backend, ok
}

// writeFeedbackReportStoreError answers a store error of a route for
// people: 404 report_not_found for a report the workspace does not hold,
// 503 retryable for a store call that asks to be sent again, and otherwise
// a generic 500 that carries no store text (DEC-105(1)). Every route for
// people answers a store error here, so a retryable failure answers the same
// way on every path.
func writeFeedbackReportStoreError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case writeFeedbackReportRetryable(w, err):
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "report_not_found", "message": "The report was not found."})
	default:
		writeStoreFailure(w, r, err)
	}
}

// listFeedbackReports serves GET /v1/feedback-reports. The filters apply
// before the page and the count, the order is newest last_seen first and
// then by ID, and the list headers state the page.
func (s *Server) listFeedbackReports(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.reportStore(w)
	if !ok {
		return
	}
	// A query whose encoding is malformed is refused, so a dropped pair never
	// reads as an omitted filter.
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeFeedbackReportReadRefusal(w, "query", "must be a well-formed query string")
		return
	}
	query, field, message := parseFeedbackReportQuery(values)
	if field != "" {
		writeFeedbackReportReadRefusal(w, field, message)
		return
	}
	page, ok := readListPage(w, r, defaultFeedbackReportPageLimit)
	if !ok {
		return
	}
	query.Limit, query.Offset = page.Limit, page.Offset
	listed, err := backend.ListFeedbackReports(s.feedbackReadContext(r.Context()), query)
	if err != nil {
		writeFeedbackReportStoreError(w, r, err)
		return
	}
	served, err := s.servedFeedbackReports(r.Context(), listed.Reports, listed.Details)
	if err != nil {
		writeFeedbackReportStoreError(w, r, err)
		return
	}
	page.writeHeaders(w, listed.Total)
	writeJSON(w, http.StatusOK, served)
	s.requestPendingFeedbackLinking(r.Context(), listed.Reports)
}

// getFeedbackReport serves GET /v1/feedback-reports/{id}. A report of
// another workspace answers exactly as a missing one (component-http-api
// invariant 13).
func (s *Server) getFeedbackReport(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.reportStore(w)
	if !ok {
		return
	}
	s.writeServedFeedbackReport(w, r, backend, chi.URLParam(r, "id"))
}

// writeServedFeedbackReport reads one report and answers its served form.
func (s *Server) writeServedFeedbackReport(w http.ResponseWriter, r *http.Request, backend store.ReportStore, reportID string) {
	report, details, err := backend.ReadFeedbackReport(s.feedbackReadContext(r.Context()), reportID)
	if err != nil {
		writeFeedbackReportStoreError(w, r, err)
		return
	}
	served, err := s.servedFeedbackReports(r.Context(), []store.FeedbackReport{report}, details)
	if err != nil {
		writeFeedbackReportStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, served[0])
	s.requestPendingFeedbackLinking(r.Context(), []store.FeedbackReport{report})
}
