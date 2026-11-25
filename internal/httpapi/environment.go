package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/kidus-tiliksew/conveyor/internal/redact"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// environmentBodyLimit bounds the JSON body of an environment route
// (component-http-api invariant 17).
const environmentBodyLimit = 256 << 10

// environmentSignalPath is the one route that will admit an environment key
// (component-environments invariant 15). It is not mounted yet.
const environmentSignalPath = "/v1/feedback/signals"

// environmentKeyPresented reports whether any Authorization header of the
// request presents an environment key as its bearer. Only the prefix is read,
// and no lookup happens.
func environmentKeyPresented(r *http.Request) bool {
	return slices.ContainsFunc(r.Header.Values("Authorization"), func(header string) bool {
		provided, ok := bearerValue(header)
		return ok && strings.HasPrefix(provided, store.EnvironmentKeyPrefix)
	})
}

// refuseEnvironmentKeyOutsideSignals refuses an environment key on every
// route but the signal route, before routing, so no open route, REST route,
// claim-bound delivery route, /mcp, or dashboard path can admit it, even
// beside a session cookie (component-environments invariant 15;
// req-security-boundaries AC-1.5; DEC-148(6)). The exception compares the
// escaped path, which is what the router matches, so an encoded form of the
// signal path that the router would send elsewhere is refused too.
func (s *Server) refuseEnvironmentKeyOutsideSignals(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() == environmentSignalPath || !environmentKeyPresented(r) {
			next.ServeHTTP(w, r)
			return
		}
		if oauthLoggedPath(r.URL.Path) {
			// An OAuth route keeps its privacy headers on this early refusal
			// (component-identity-membership invariant 52).
			setOAuthNoStoreHeaders(w)
		}
		challenge := "Bearer"
		if provider, enabled := s.oauth(); enabled && r.URL.EscapedPath() == "/mcp" {
			// /mcp keeps the challenge of every refused bearer
			// (component-http-api invariant 4).
			challenge = `Bearer resource_metadata="` + provider.resourceMetadataURL() + `"`
		}
		w.Header().Set("WWW-Authenticate", challenge)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

func (s *Server) environmentStore(w http.ResponseWriter) (store.EnvironmentStore, bool) {
	backend, ok := s.Store.(store.EnvironmentStore)
	if !ok {
		http.Error(w, "environments are not available on this store", http.StatusNotImplemented)
		return nil, false
	}
	return backend, true
}

// listEnvironments serves GET /v1/environments. archived is false (the
// default) or true (component-environments, Public surfaces; req-environments
// AC-1.8).
func (s *Server) listEnvironments(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.environmentStore(w)
	if !ok {
		return
	}
	page, ok := readListPage(w, r, 0)
	if !ok {
		return
	}
	archived := false
	switch r.URL.Query().Get("archived") {
	case "", "false":
	case "true":
		archived = true
	default:
		writeEnvironmentError(w, r, environmentFieldRefusal("archived", "archived must be true or false"))
		return
	}
	records, err := backend.ListEnvironmentRecords(r.Context(), store.EnvironmentListQuery{Archived: &archived})
	if err != nil {
		writeEnvironmentError(w, r, err)
		return
	}
	writeEnvironmentPage(w, page, records)
}

// createEnvironment serves POST /v1/environments. It answers 201 with the
// environment row (req-environments AC-1.1).
func (s *Server) createEnvironment(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.environmentStore(w)
	if !ok {
		return
	}
	patch, ok := decodeEnvironmentPatch(w, r)
	if !ok {
		return
	}
	if patch.Name == nil {
		writeEnvironmentError(w, r, environmentFieldRefusal("name", "name is required"))
		return
	}
	input := store.EnvironmentInput{Name: *patch.Name, VersionRule: patch.VersionRule, TimingWindowMinutes: patch.TimingWindowMinutes}
	if patch.TrackedBranches != nil {
		input.TrackedBranches = *patch.TrackedBranches
	}
	if patch.GitHubEnvironments != nil {
		input.GitHubEnvironments = *patch.GitHubEnvironments
	}
	if patch.Archived != nil {
		input.Archived = *patch.Archived
	}
	record, err := backend.CreateEnvironment(r.Context(), input)
	if err != nil {
		writeEnvironmentError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, record)
}

// patchEnvironment serves PATCH /v1/environments/{id}. An omitted field stays
// unchanged, and an explicit null version_rule clears the rule
// (req-environments AC-1.2 to AC-1.5, AC-1.9).
func (s *Server) patchEnvironment(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.environmentStore(w)
	if !ok {
		return
	}
	patch, ok := decodeEnvironmentPatch(w, r)
	if !ok {
		return
	}
	record, err := backend.UpdateEnvironment(r.Context(), chi.URLParam(r, "id"), patch)
	if err != nil {
		writeEnvironmentError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, record)
}

// issueEnvironmentKey serves POST /v1/environments/{id}/keys. Its 201 answer
// is the only one that ever holds the value, so no cache keeps it
// (component-environments invariant 11; req-environments AC-2.1).
func (s *Server) issueEnvironmentKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	backend, ok := s.environmentStore(w)
	if !ok {
		return
	}
	label, ok := decodeEnvironmentText(w, r, "label")
	if !ok {
		return
	}
	issued, err := backend.IssueEnvironmentKey(r.Context(), chi.URLParam(r, "id"), label)
	if err != nil {
		writeEnvironmentError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, issued)
}

// revokeEnvironmentKey serves DELETE /v1/environments/{id}/keys/{key_id}. A
// repeat of a revocation answers 204 and records nothing (req-environments
// AC-2.2).
func (s *Server) revokeEnvironmentKey(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.environmentStore(w)
	if !ok {
		return
	}
	if err := backend.RevokeEnvironmentKey(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "key_id")); err != nil {
		writeEnvironmentError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listEnvironmentSuggestions serves GET /v1/environments/suggestions
// (component-environments invariant 16; req-environments AC-1.6).
func (s *Server) listEnvironmentSuggestions(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.environmentStore(w)
	if !ok {
		return
	}
	page, ok := readListPage(w, r, 0)
	if !ok {
		return
	}
	suggestions, err := backend.ListEnvironmentSuggestions(r.Context())
	if err != nil {
		writeEnvironmentError(w, r, err)
		return
	}
	writeEnvironmentPage(w, page, suggestions)
}

// ignoreEnvironmentSuggestion serves POST /v1/environments/suggestions/ignore
// (component-environments invariant 16; req-environments AC-1.6).
func (s *Server) ignoreEnvironmentSuggestion(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.environmentStore(w)
	if !ok {
		return
	}
	name, ok := decodeEnvironmentText(w, r, "github_environment")
	if !ok {
		return
	}
	if err := backend.IgnoreEnvironmentSuggestion(r.Context(), name); err != nil {
		writeEnvironmentError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeEnvironmentPage[T any](w http.ResponseWriter, page listPage, items []T) {
	total := len(items)
	if page.Paged {
		start := min(page.Offset, total)
		items = items[start:min(start+page.Limit, total)]
	}
	page.writeHeaders(w, total)
	writeJSON(w, http.StatusOK, items)
}

func environmentFieldRefusal(field, message string) error {
	return &store.EnvironmentFieldError{Field: field, Message: message}
}

// strictEnvironmentObject reads one JSON object field by field, so a caller
// can tell an omitted field from an explicit null. A repeated key is refused
// through duplicate, because either value could be the one the caller meant.
func strictEnvironmentObject(raw []byte) (fields map[string]json.RawMessage, duplicate string, err error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, "", errors.New("expected a JSON object")
	}
	fields = map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, "", err
		}
		key, _ := token.(string)
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, "", err
		}
		if _, repeated := fields[key]; repeated {
			duplicate = key
		}
		fields[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, "", err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, "", errors.New("expected one JSON value")
	}
	return fields, duplicate, nil
}

// decodeEnvironmentObject reads a request body that holds only the allowed
// fields, each at most once.
func decodeEnvironmentObject(w http.ResponseWriter, r *http.Request, allowed ...string) (map[string]json.RawMessage, bool) {
	raw, err := readBoundedBody(w, r, environmentBodyLimit)
	if err != nil {
		writeBodyDecodeError(w, err)
		return nil, false
	}
	fields, duplicate, err := strictEnvironmentObject(raw)
	if err != nil {
		writeBodyDecodeError(w, fmt.Errorf("invalid JSON body: %w", err))
		return nil, false
	}
	if duplicate != "" {
		writeEnvironmentError(w, r, environmentFieldRefusal(duplicate, fmt.Sprintf("%s appears more than once", duplicate)))
		return nil, false
	}
	for name := range fields {
		if !slices.Contains(allowed, name) {
			writeEnvironmentError(w, r, environmentFieldRefusal(name, fmt.Sprintf("%s is not a field of this request", name)))
			return nil, false
		}
	}
	return fields, true
}

func environmentNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// environmentValue decodes one non-null value into target.
func environmentValue(raw json.RawMessage, field, shape string, target any) error {
	if environmentNull(raw) || json.Unmarshal(raw, target) != nil {
		return environmentFieldRefusal(field, field+" must be "+shape)
	}
	return nil
}

// decodeEnvironmentText reads a body whose one field is a required text.
func decodeEnvironmentText(w http.ResponseWriter, r *http.Request, field string) (string, bool) {
	fields, ok := decodeEnvironmentObject(w, r, field)
	if !ok {
		return "", false
	}
	raw, present := fields[field]
	var value string
	err := environmentFieldRefusal(field, field+" is required")
	if present {
		err = environmentValue(raw, field, "a string", &value)
	}
	if err != nil {
		writeEnvironmentError(w, r, err)
		return "", false
	}
	return value, true
}

// environmentBranchesValue reads a branch map and refuses a second branch
// for one repository (req-environments AC-1.4).
func environmentBranchesValue(raw json.RawMessage) (map[string]string, error) {
	const field, shape = "tracked_branches", "an object from repository name to branch name"
	fields, duplicate, err := strictEnvironmentObject(raw)
	if err != nil {
		return nil, environmentFieldRefusal(field, field+" must be "+shape)
	}
	if duplicate != "" {
		return nil, environmentFieldRefusal(field, fmt.Sprintf("tracked_branches names repository %q more than once", duplicate))
	}
	branches := make(map[string]string, len(fields))
	for repository, value := range fields {
		var branch string
		if err := environmentValue(value, field, shape, &branch); err != nil {
			return nil, err
		}
		branches[repository] = branch
	}
	return branches, nil
}

func environmentNamesValue(raw json.RawMessage) ([]string, error) {
	const field, shape = "github_environments", "a list of strings"
	var values []json.RawMessage
	if err := environmentValue(raw, field, shape, &values); err != nil {
		return nil, err
	}
	names := make([]string, len(values))
	for i, value := range values {
		if err := environmentValue(value, field, shape, &names[i]); err != nil {
			return nil, err
		}
	}
	return names, nil
}

func environmentRuleValue(raw json.RawMessage) (*store.EnvironmentVersionRule, error) {
	const field, shape = "version_rule", "null or an object with the strings kind and text"
	if environmentNull(raw) {
		return nil, nil
	}
	refusal := environmentFieldRefusal(field, field+" must be "+shape)
	fields, duplicate, err := strictEnvironmentObject(raw)
	if err != nil || duplicate != "" || len(fields) != 2 {
		return nil, refusal
	}
	var rule store.EnvironmentVersionRule
	for name, target := range map[string]*string{"kind": &rule.Kind, "text": &rule.Text} {
		value, present := fields[name]
		if !present || environmentValue(value, field, shape, target) != nil {
			return nil, refusal
		}
	}
	return &rule, nil
}

// decodeEnvironmentPatch reads the fields that PATCH takes, which POST takes
// too.
func decodeEnvironmentPatch(w http.ResponseWriter, r *http.Request) (store.EnvironmentPatch, bool) {
	fields, ok := decodeEnvironmentObject(w, r, "name", "tracked_branches", "github_environments", "version_rule", "timing_window_minutes", "archived")
	if !ok {
		return store.EnvironmentPatch{}, false
	}
	patch, err := environmentPatchFrom(fields)
	if err != nil {
		writeEnvironmentError(w, r, err)
		return store.EnvironmentPatch{}, false
	}
	return patch, true
}

func environmentPatchFrom(fields map[string]json.RawMessage) (store.EnvironmentPatch, error) {
	var patch store.EnvironmentPatch
	if raw, present := fields["name"]; present {
		patch.Name = new(string)
		if err := environmentValue(raw, "name", "a string", patch.Name); err != nil {
			return patch, err
		}
	}
	if raw, present := fields["tracked_branches"]; present {
		branches, err := environmentBranchesValue(raw)
		if err != nil {
			return patch, err
		}
		patch.TrackedBranches = &branches
	}
	if raw, present := fields["github_environments"]; present {
		names, err := environmentNamesValue(raw)
		if err != nil {
			return patch, err
		}
		patch.GitHubEnvironments = &names
	}
	if raw, present := fields["version_rule"]; present {
		rule, err := environmentRuleValue(raw)
		if err != nil {
			return patch, err
		}
		patch.VersionRuleSet, patch.VersionRule = true, rule
	}
	if raw, present := fields["timing_window_minutes"]; present {
		patch.TimingWindowMinutes = new(int)
		if err := environmentValue(raw, "timing_window_minutes", "a whole number", patch.TimingWindowMinutes); err != nil {
			return patch, err
		}
	}
	if raw, present := fields["archived"]; present {
		patch.Archived = new(bool)
		if err := environmentValue(raw, "archived", "true or false", patch.Archived); err != nil {
			return patch, err
		}
	}
	return patch, nil
}

// writeEnvironmentError maps the environment refusals to the codes
// component-environments lists. Any other failure answers 500 through
// writeStoreFailure (DEC-105). A refusal that repeats the request's own text
// is redacted, so a key value pasted into a field or a path is not sent back.
func writeEnvironmentError(w http.ResponseWriter, r *http.Request, err error) {
	var field *store.EnvironmentFieldError
	var notFound *store.EnvironmentNotFoundError
	var archived *store.EnvironmentArchivedError
	var limit *store.EnvironmentKeyLimitError
	switch {
	case errors.As(err, &field):
		writeEnvironmentRefusal(w, http.StatusBadRequest, map[string]string{"error": "invalid_environment", "field": field.Field, "message": field.Message})
	case errors.As(err, &notFound):
		writeEnvironmentRefusal(w, http.StatusNotFound, map[string]string{"error": "environment_not_found", "environment_id": notFound.ID, "message": notFound.Error()})
	case errors.As(err, &archived):
		writeEnvironmentRefusal(w, http.StatusConflict, map[string]string{"error": "environment_archived", "environment_id": archived.ID, "message": archived.Error()})
	case errors.As(err, &limit):
		writeEnvironmentRefusal(w, http.StatusConflict, map[string]string{"error": "environment_key_limit", "environment_id": limit.ID, "message": limit.Error()})
	default:
		writeStoreFailure(w, r, err)
	}
}

func writeEnvironmentRefusal(w http.ResponseWriter, status int, body map[string]string) {
	redactor := redact.New(nil)
	for name, text := range body {
		body[name], _ = redactor.Redact(text)
	}
	writeJSON(w, status, body)
}
