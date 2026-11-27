package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// feedbackReportBodyLimit bounds the body of a report request
// (component-feedback-reports, Refusals).
const feedbackReportBodyLimit = 256 << 10

// feedbackReportEntryFields are the only fields of an entry
// (component-feedback-reports, Public surfaces).
var feedbackReportEntryFields = []string{"kind", "key", "summary", "service", "repository", "commit", "version", "count", "first_seen", "last_seen", "file", "line"}

func (s *Server) feedbackReports() *feedbackLimiter {
	s.feedbackReportLimiterOnce.Do(func() {
		if s.feedbackReportLimiter == nil {
			s.feedbackReportLimiter = newFeedbackLimiter(feedbackLimiterMaxKeys)
		}
	})
	return s.feedbackReportLimiter
}

// receiveFeedbackReports serves POST /v1/feedback/reports. The key decides
// the workspace and the environment, and the answer holds the arrival time
// and no stored content (component-feedback-reports invariants 2 to 5 and
// 10; req-production-feedback AC-4.1; req-security-boundaries AC-1.5). It
// calls no forge, enqueues no work, and changes no task (invariant 1;
// DEC-149(1)).
func (s *Server) receiveFeedbackReports(w http.ResponseWriter, r *http.Request) {
	scope, ok := r.Context().Value(feedbackScopeKey{}).(feedbackScope)
	if !ok {
		refuseFeedbackCredential(w)
		return
	}
	backend, ok := s.Store.(store.ReportStore)
	if !ok {
		http.Error(w, "feedback reports are not available on this store", http.StatusNotImplemented)
		return
	}
	var raw json.RawMessage
	if err := decodeStrictBoundedJSON(w, r, feedbackReportBodyLimit, &raw); err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writeBodyTooLarge(w)
			return
		}
		writeFeedbackReportRefusal(w, feedbackRefusal("body", "must be one JSON object"))
		return
	}
	batch, err := decodeFeedbackReports(raw)
	if err == nil {
		batch.EnvironmentID, batch.KeyID, batch.ArrivedAt = scope.key.EnvironmentID, scope.key.KeyID, scope.arrived
		batch, err = store.NormalizeFeedbackReports(batch)
	}
	// Every rule is checked before the reservation, so an invalid request
	// gets its own refusal inside a window and never holds the key against a
	// valid one. The store checks the repositories again when it records.
	if err == nil && slices.ContainsFunc(batch.Entries, func(entry store.FeedbackReportEntry) bool { return entry.Repository != "" }) {
		err = backend.CheckFeedbackReportRepositories(r.Context(), batch)
	}
	if err != nil {
		writeFeedbackReportError(w, r, err)
		return
	}
	limiter, key := s.feedbackReports(), feedbackLimiterKey{workspace: scope.key.Workspace, keyID: scope.key.KeyID}
	decision := limiter.reserve(key, scope.arrived, s.feedbackNow())
	for decision.busy != nil {
		// The store takes the requests of an environment one at a time, so
		// this request would wait there too.
		select {
		case <-decision.busy:
			decision = limiter.reserve(key, scope.arrived, s.feedbackNow())
		case <-r.Context().Done():
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	if decision.full {
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	if decision.wait > 0 {
		w.Header().Set("Retry-After", strconv.FormatInt(int64((decision.wait+time.Second-1)/time.Second), 10))
		http.Error(w, "too many reports", http.StatusTooManyRequests)
		return
	}
	accepted := false
	defer func() { limiter.settle(key, accepted) }()
	if err := backend.RecordFeedbackReports(r.Context(), batch); err != nil {
		// A store that asks for the request to be sent again recorded
		// nothing, and the deferred settle opens no window, so an immediate
		// resend is admitted (component-feedback-reports, Refusals;
		// component-persistence invariant 8).
		if errors.Is(err, store.ErrRetryable) {
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "retryable", "message": storeRetryableMessage})
			return
		}
		writeFeedbackReportError(w, r, err)
		return
	}
	accepted = true
	writeJSON(w, http.StatusAccepted, map[string]time.Time{"accepted_at": batch.ArrivedAt})
}

func writeFeedbackReportError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrInvalidEnvironmentKey):
		// The key was revoked or its environment archived after the check.
		refuseFeedbackCredential(w)
	case errors.Is(err, store.ErrOpenReportLimit):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "open_report_limit", "message": fmt.Sprintf("the environment holds %d open reports, so the request would store a new report beyond the limit", store.MaxOpenFeedbackReports)})
	case errors.As(err, new(*store.FeedbackFieldError)):
		writeFeedbackReportRefusal(w, err)
	default:
		writeStoreFailure(w, r, err)
	}
}

// writeFeedbackReportRefusal names the field by its path. Neither the path
// nor the message holds a submitted value (req-security-boundaries AC-1.5).
func writeFeedbackReportRefusal(w http.ResponseWriter, err error) {
	var field *store.FeedbackFieldError
	errors.As(err, &field)
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_report", "field": field.Field, "message": field.Message})
}

// decodeFeedbackReports reads the shape of a report request. It reads each
// object field by field, so an omitted field differs from an explicit null
// or empty one, and an unknown or repeated field is refused at every level.
// The bounds are store.NormalizeFeedbackReports's.
func decodeFeedbackReports(raw json.RawMessage) (store.FeedbackReportBatch, error) {
	var batch store.FeedbackReportBatch
	// encoding/json replaces invalid UTF-8, which could make two keys equal.
	if !utf8.Valid(raw) {
		return batch, feedbackRefusal("body", "must be UTF-8 text")
	}
	fields, err := feedbackObject("body", raw, "reports")
	if err != nil {
		return batch, err
	}
	entries, err := feedbackArray("reports", fields["reports"], store.MaxFeedbackReportEntries)
	if err != nil {
		return batch, err
	}
	for i, rawEntry := range entries {
		entry, err := decodeFeedbackReportEntry(i, rawEntry)
		if err != nil {
			return batch, err
		}
		batch.Entries = append(batch.Entries, entry)
	}
	return batch, nil
}

func decodeFeedbackReportEntry(i int, raw json.RawMessage) (store.FeedbackReportEntry, error) {
	var entry store.FeedbackReportEntry
	fields, err := feedbackObject(fmt.Sprintf("reports[%d]", i), raw, feedbackReportEntryFields...)
	if err != nil {
		return entry, err
	}
	for _, required := range []string{"kind", "key", "summary"} {
		if fields[required] == nil {
			return entry, feedbackRefusal(store.FeedbackReportField(i, required), "is required")
		}
	}
	texts := []struct {
		name   string
		target *string
	}{
		{"kind", &entry.Kind}, {"key", &entry.Key}, {"summary", &entry.Summary}, {"service", &entry.Service},
		{"repository", &entry.Repository}, {"commit", &entry.Commit}, {"version", &entry.Version}, {"file", &entry.File},
	}
	for _, text := range texts {
		if *text.target, err = feedbackText(store.FeedbackReportField(i, text.name), fields[text.name]); err != nil {
			return entry, err
		}
	}
	if raw := fields["count"]; raw != nil {
		if entry.Count, err = feedbackWholeNumber(store.FeedbackReportField(i, "count"), raw, store.MaxFeedbackReportCount); err != nil {
			return entry, err
		}
	}
	if raw := fields["line"]; raw != nil {
		line, err := feedbackWholeNumber(store.FeedbackReportField(i, "line"), raw, store.MaxFeedbackReportLine)
		if err != nil {
			return entry, err
		}
		entry.Line = int(line)
	}
	if entry.FirstSeen, err = feedbackReportTime(store.FeedbackReportField(i, "first_seen"), fields["first_seen"]); err != nil {
		return entry, err
	}
	if entry.LastSeen, err = feedbackReportTime(store.FeedbackReportField(i, "last_seen"), fields["last_seen"]); err != nil {
		return entry, err
	}
	return entry, nil
}

// feedbackWholeNumber reads one stated whole number from 1 to most.
func feedbackWholeNumber(path string, raw json.RawMessage, most int64) (int64, error) {
	var number int64
	if environmentNull(raw) || json.Unmarshal(raw, &number) != nil || number < 1 || number > most {
		return 0, feedbackRefusal(path, fmt.Sprintf("must be a whole number from 1 to %d", most))
	}
	return number, nil
}

// feedbackReportTime reads one optional RFC 3339 time. An omitted field is
// nil, and the store takes it as the arrival (component-feedback-reports
// invariant 4).
func feedbackReportTime(path string, raw json.RawMessage) (*time.Time, error) {
	if raw == nil {
		return nil, nil
	}
	var text string
	if environmentNull(raw) || json.Unmarshal(raw, &text) != nil {
		return nil, feedbackRefusal(path, "must be an RFC 3339 time")
	}
	at, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return nil, feedbackRefusal(path, "must be an RFC 3339 time")
	}
	return &at, nil
}
