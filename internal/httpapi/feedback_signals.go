package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// feedbackSignalBodyLimit bounds the body of a signal
// (component-production-feedback, Refusals).
const feedbackSignalBodyLimit = 256 << 10

// feedbackKeyCheckTimeout bounds the key check of a signal. Until it ends the
// limiter keeps every window for the request, so a stalled store must not hold
// it.
const feedbackKeyCheckTimeout = 10 * time.Second

// feedbackScope is what a verified environment key decides about a request,
// with the server's time of arrival (component-production-feedback invariants
// 2 and 4). It holds no user, role, or capability.
type feedbackScope struct {
	key     store.VerifiedEnvironmentKey
	arrived time.Time
}

type feedbackScopeKey struct{}

func (s *Server) feedbackNow() time.Time {
	if s.feedbackClock != nil {
		return s.feedbackClock()
	}
	return time.Now()
}

func (s *Server) feedbackKeyCheckLimit() time.Duration {
	if s.feedbackKeyTimeout > 0 {
		return s.feedbackKeyTimeout
	}
	return feedbackKeyCheckTimeout
}

func (s *Server) feedbackSignals() *feedbackLimiter {
	s.feedbackLimiterOnce.Do(func() {
		if s.feedbackLimiter == nil {
			s.feedbackLimiter = newFeedbackLimiter(feedbackLimiterMaxKeys)
		}
	})
	return s.feedbackLimiter
}

// refuseFeedbackCredential is the one answer to every credential the signal
// route does not admit. It does not say which condition failed.
func refuseFeedbackCredential(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// requireFeedbackKey admits one environment key and nothing else: a request
// with no bearer, a user's credential, a session cookie, or two Authorization
// headers is refused (component-production-feedback invariant 2;
// req-security-boundaries AC-1.5; DEC-148(6)). The check writes nothing, so a
// refused signal leaves no trace. The accepted signal records the key's last
// use. Only the verified key sets the workspace of the request.
func (s *Server) requireFeedbackKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limiter := s.feedbackSignals()
		arrival := limiter.arrive(s.feedbackNow)
		defer limiter.leave(arrival)
		headers := r.Header.Values("Authorization")
		if len(headers) != 1 {
			refuseFeedbackCredential(w)
			return
		}
		value, ok := bearerValue(headers[0])
		if !ok || !strings.HasPrefix(value, store.EnvironmentKeyPrefix) {
			refuseFeedbackCredential(w)
			return
		}
		backend, ok := s.environmentStore(w)
		if !ok {
			return
		}
		checkCtx, cancel := context.WithTimeout(r.Context(), s.feedbackKeyCheckLimit())
		key, err := backend.CheckEnvironmentKey(checkCtx, value)
		cancel()
		if errors.Is(err, store.ErrInvalidEnvironmentKey) {
			refuseFeedbackCredential(w)
			return
		}
		if err != nil {
			log.Printf("httpapi: environment key verification failed: request_id=%q error=%q", middleware.GetReqID(r.Context()), redactedErrorLogText(err))
			http.Error(w, "credential verification failed", http.StatusInternalServerError)
			return
		}
		limiter.identify(arrival, feedbackLimiterKey{workspace: key.Workspace, keyID: key.KeyID})
		ctx := store.WithWorkspace(store.WithActor(r.Context(), store.SystemActor()), key.Workspace)
		ctx = context.WithValue(ctx, feedbackScopeKey{}, feedbackScope{key: key, arrived: arrival.at})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// receiveFeedbackSignal serves POST /v1/feedback/signals. Its answer holds
// the arrival time and no stored content (req-production-feedback AC-1.1;
// req-security-boundaries AC-1.5).
func (s *Server) receiveFeedbackSignal(w http.ResponseWriter, r *http.Request) {
	scope, ok := r.Context().Value(feedbackScopeKey{}).(feedbackScope)
	if !ok {
		refuseFeedbackCredential(w)
		return
	}
	backend, ok := s.Store.(store.FeedbackStore)
	if !ok {
		http.Error(w, "production feedback is not available on this store", http.StatusNotImplemented)
		return
	}
	var raw json.RawMessage
	if err := decodeStrictBoundedJSON(w, r, feedbackSignalBodyLimit, &raw); err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writeBodyTooLarge(w)
			return
		}
		writeFeedbackRefusal(w, feedbackRefusal("body", "must be one JSON object"))
		return
	}
	signal, err := decodeFeedbackSignal(raw)
	if err == nil {
		signal.EnvironmentID, signal.KeyID, signal.ArrivedAt = scope.key.EnvironmentID, scope.key.KeyID, scope.arrived
		signal, err = store.NormalizeFeedbackSignal(signal)
	}
	// Every rule is checked before the reservation, so an invalid signal gets
	// its own refusal inside a window and never holds the key against a valid
	// one. The store checks the repositories again when it records.
	if err == nil && slices.ContainsFunc(signal.Services, func(service store.FeedbackSignalService) bool { return service.Repository != "" }) {
		err = backend.CheckFeedbackRepositories(r.Context(), signal)
	}
	if err != nil {
		writeFeedbackError(w, r, err)
		return
	}
	limiter, key := s.feedbackSignals(), feedbackLimiterKey{workspace: scope.key.Workspace, keyID: scope.key.KeyID}
	decision := limiter.reserve(key, scope.arrived, s.feedbackNow())
	for decision.busy != nil {
		// The store takes the signals of an environment one at a time, so this
		// signal would wait there too.
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
		http.Error(w, "too many signals", http.StatusTooManyRequests)
		return
	}
	accepted := false
	defer func() { limiter.settle(key, accepted) }()
	if err := backend.RecordFeedbackSignal(r.Context(), signal); err != nil {
		writeFeedbackError(w, r, err)
		return
	}
	accepted = true
	writeJSON(w, http.StatusAccepted, map[string]time.Time{"accepted_at": signal.ArrivedAt})
}

func feedbackRefusal(field, message string) error {
	return &store.FeedbackFieldError{Field: field, Message: message}
}

func writeFeedbackError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrInvalidEnvironmentKey):
		// The key was revoked or its environment archived after the check.
		refuseFeedbackCredential(w)
	case errors.As(err, new(*store.FeedbackFieldError)):
		writeFeedbackRefusal(w, err)
	default:
		writeStoreFailure(w, r, err)
	}
}

// writeFeedbackRefusal names the field by its path. Neither the path nor the
// message holds a submitted value.
func writeFeedbackRefusal(w http.ResponseWriter, err error) {
	var field *store.FeedbackFieldError
	errors.As(err, &field)
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_signal", "field": field.Field, "message": field.Message})
}

// decodeFeedbackSignal reads the shape of a signal. It reads each object
// field by field, so an omitted field differs from an explicit null or empty
// one, and an unknown or repeated field is refused at every level. The bounds
// are store.NormalizeFeedbackSignal's.
func decodeFeedbackSignal(raw json.RawMessage) (store.FeedbackSignal, error) {
	var signal store.FeedbackSignal
	// encoding/json replaces invalid UTF-8, which could make two names equal.
	if !utf8.Valid(raw) {
		return signal, feedbackRefusal("body", "must be UTF-8 text")
	}
	fields, err := feedbackObject("body", raw, "next_signal_seconds", "services")
	if err != nil {
		return signal, err
	}
	interval, stated := fields["next_signal_seconds"]
	if !stated || environmentNull(interval) || json.Unmarshal(interval, &signal.NextSignalSeconds) != nil {
		return signal, feedbackRefusal("next_signal_seconds", fmt.Sprintf("must be a whole number from %d to %d", store.MinFeedbackSignalSeconds, store.MaxFeedbackSignalSeconds))
	}
	services, err := feedbackArray("services", fields["services"], store.MaxFeedbackServices)
	if err != nil {
		return signal, err
	}
	for i, entry := range services {
		service, err := decodeFeedbackService(i, entry)
		if err != nil {
			return signal, err
		}
		signal.Services = append(signal.Services, service)
	}
	return signal, nil
}

func decodeFeedbackService(i int, raw json.RawMessage) (store.FeedbackSignalService, error) {
	var service store.FeedbackSignalService
	fields, err := feedbackObject(fmt.Sprintf("services[%d]", i), raw, "name", "repository", "versions")
	if err != nil {
		return service, err
	}
	if fields["name"] == nil {
		return service, feedbackRefusal(store.FeedbackServiceField(i, "name"), "is required")
	}
	if service.Name, err = feedbackText(store.FeedbackServiceField(i, "name"), fields["name"]); err != nil {
		return service, err
	}
	if service.Repository, err = feedbackText(store.FeedbackServiceField(i, "repository"), fields["repository"]); err != nil {
		return service, err
	}
	versions, err := feedbackArray(store.FeedbackServiceField(i, "versions"), fields["versions"], store.MaxFeedbackVersions)
	if err != nil {
		return service, err
	}
	for j, entry := range versions {
		fields, err := feedbackObject(fmt.Sprintf("services[%d].versions[%d]", i, j), entry, "commit", "version")
		if err != nil {
			return service, err
		}
		var version store.FeedbackSignalVersion
		if version.Commit, err = feedbackText(store.FeedbackVersionField(i, j, "commit"), fields["commit"]); err != nil {
			return service, err
		}
		if version.Version, err = feedbackText(store.FeedbackVersionField(i, j, "version"), fields["version"]); err != nil {
			return service, err
		}
		service.Versions = append(service.Versions, version)
	}
	return service, nil
}

// feedbackObject reads one object that holds only the allowed fields, each at
// most once. The refusal names the object and no field of it, because an
// unknown field's name is submitted text.
func feedbackObject(path string, raw json.RawMessage, allowed ...string) (map[string]json.RawMessage, error) {
	fields, duplicate, err := strictEnvironmentObject(raw)
	if err != nil {
		return nil, feedbackRefusal(path, "must be a JSON object")
	}
	for name := range fields {
		if !slices.Contains(allowed, name) {
			return nil, feedbackRefusal(path, "must hold only "+strings.Join(allowed, ", "))
		}
	}
	if duplicate != "" {
		return nil, feedbackRefusal(path, "must hold each field at most once")
	}
	return fields, nil
}

// feedbackArray reads one required array and checks its size before any entry
// is read.
func feedbackArray(path string, raw json.RawMessage, most int) ([]json.RawMessage, error) {
	var entries []json.RawMessage
	if raw == nil || environmentNull(raw) || json.Unmarshal(raw, &entries) != nil || len(entries) < 1 || len(entries) > most {
		return nil, feedbackRefusal(path, fmt.Sprintf("must hold 1 to %d entries", most))
	}
	return entries, nil
}

// feedbackText reads one optional text. An omitted field is empty. A stated
// one is a string with at least one character.
func feedbackText(path string, raw json.RawMessage) (string, error) {
	if raw == nil {
		return "", nil
	}
	var text string
	if environmentNull(raw) || json.Unmarshal(raw, &text) != nil || text == "" {
		return "", feedbackRefusal(path, "must be a string that is not empty")
	}
	if feedbackUnpairedSurrogate(raw) {
		return "", feedbackRefusal(path, "must be valid Unicode text")
	}
	return text, nil
}

// feedbackUnpairedSurrogate reports whether a valid JSON string escapes half
// of a surrogate pair. encoding/json replaces each such half with the same
// character, which could make two names equal.
func feedbackUnpairedSurrogate(raw []byte) bool {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if raw[i] != 'u' {
			continue
		}
		unit := feedbackEscapedUnit(raw[i-1:])
		i += 4
		if !utf16.IsSurrogate(unit) {
			continue
		}
		if utf16.DecodeRune(unit, feedbackEscapedUnit(raw[i+1:])) == utf8.RuneError {
			return true
		}
		i += 6
	}
	return false
}

// feedbackEscapedUnit reads one \uXXXX escape, or 0 when raw opens with
// none.
func feedbackEscapedUnit(raw []byte) rune {
	if len(raw) < 6 || raw[0] != '\\' || raw[1] != 'u' {
		return 0
	}
	unit, err := strconv.ParseUint(string(raw[2:6]), 16, 16)
	if err != nil {
		return 0
	}
	return rune(unit)
}

// environmentListRow is one row of GET /v1/environments: the environment row
// and the fields component-production-feedback adds to it.
type environmentListRow struct {
	store.EnvironmentRecord
	Listening    bool       `json:"listening"`
	LastSignalAt *time.Time `json:"last_signal_at"`
}

// writeEnvironmentRows answers the environment list with the listening state
// of each row, computed now from its newest signal. Nothing schedules it
// (component-production-feedback invariant 13; req-production-feedback
// AC-2.1).
func (s *Server) writeEnvironmentRows(w http.ResponseWriter, r *http.Request, page listPage, records []store.EnvironmentRecord) {
	total := len(records)
	records = environmentPageItems(page, records)
	backend, ok := s.Store.(store.FeedbackStore)
	if !ok {
		http.Error(w, "production feedback is not available on this store", http.StatusNotImplemented)
		return
	}
	ids := make([]string, len(records))
	for i, record := range records {
		ids[i] = record.ID
	}
	signals, err := backend.ListFeedbackSignals(r.Context(), ids)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	now := s.feedbackNow()
	rows := make([]environmentListRow, len(records))
	for i, record := range records {
		rows[i].EnvironmentRecord = record
		if signal, signalled := signals[record.ID]; signalled {
			rows[i].Listening, rows[i].LastSignalAt = signal.Listening(now), &signal.LastSignalAt
		}
	}
	page.writeHeaders(w, total)
	writeJSON(w, http.StatusOK, rows)
}
