package httpapi

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/kidus-tiliksew/conveyor/internal/stageprefs"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// maxStagePreferenceBody bounds a whole-set PUT. Thirteen entries at their byte
// limits fit in a few kilobytes.
const maxStagePreferenceBody = 64 << 10

// stagePreferenceService returns the service over the server's backend, or nil
// when the backend keeps no stage preferences. The service holds no state, so
// every surface reads and writes the same table (component-stage-preferences
// invariant 6).
func (s *Server) stagePreferenceService() *stageprefs.Service {
	if s.StagePreferences == nil {
		return nil
	}
	return stageprefs.New(s.StagePreferences)
}

// getStagePreferences serves GET /v1/me/stage-preferences in the self-service
// class: the subject is the credential's owner, and no path, query, or body
// value names another person (req-stage-preferences AC-1.6, AC-2.4).
func (s *Server) getStagePreferences(w http.ResponseWriter, r *http.Request) {
	service := s.stagePreferenceService()
	if service == nil {
		http.Error(w, "stage preferences unavailable", http.StatusNotFound)
		return
	}
	credential, _ := store.CredentialFromContext(r.Context())
	set, err := service.Get(r.Context(), credential.OwnerUserID)
	if err != nil {
		writeStagePreferenceStoreError(w, r, "read stage preferences", err)
		return
	}
	writeStagePreferences(w, http.StatusOK, set)
}

// putStagePreferences serves PUT /v1/me/stage-preferences. It accepts a
// dashboard session, with its mutation proof, or a personal access token,
// because an agent session saves its person's answers (DEC-116(2);
// component-stage-preferences invariant 7). If-Match carries the version the
// caller read; 0 is the never-saved set (invariant 4; AC-1.7).
func (s *Server) putStagePreferences(w http.ResponseWriter, r *http.Request) {
	service := s.stagePreferenceService()
	if service == nil {
		http.Error(w, "stage preferences unavailable", http.StatusNotFound)
		return
	}
	expected, err := parseStagePreferenceIfMatch(r.Header.Get("If-Match"))
	if err != nil {
		writeJSON(w, http.StatusPreconditionRequired, map[string]any{"error": "if_match_required", "message": err.Error()})
		return
	}
	// A body over the bound answers 413 before any store access, so nothing
	// is written (component-http-api invariant 17; DEC-105(3)). Field
	// validation stays 422.
	body, err := readBoundedBody(w, r, maxStagePreferenceBody)
	if errors.Is(err, errBodyTooLarge) {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "body_too_large", "message": "The stage preferences body must be at most 64 KiB."})
		return
	}
	if err != nil {
		writeStagePreferenceValidation(w, &stageprefs.ValidationError{Field: "body", Message: "must be one readable JSON object"})
		return
	}
	credential, _ := store.CredentialFromContext(r.Context())
	set, err := service.Replace(r.Context(), credential.OwnerUserID, expected, body)
	var invalid *stageprefs.ValidationError
	switch {
	case errors.As(err, &invalid):
		writeStagePreferenceValidation(w, invalid)
	case errors.Is(err, stageprefs.ErrVersionConflict):
		writeJSON(w, http.StatusPreconditionFailed, map[string]any{"error": "version_conflict", "message": "Your stage preferences changed since you read them. Read them again and repeat your change."})
	case err != nil:
		writeStagePreferenceStoreError(w, r, "replace stage preferences", err)
	default:
		writeStagePreferences(w, http.StatusOK, set)
	}
}

// parseStagePreferenceIfMatch reads the version a PUT is based on. Unlike
// workspace configuration, version 0 is valid: it is the never-saved set.
func parseStagePreferenceIfMatch(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, errors.New("If-Match must hold the stage preferences version you read; use 0 before the first save")
	}
	value = strings.Trim(strings.TrimPrefix(value, "W/"), "\"")
	version, err := strconv.ParseInt(value, 10, 64)
	if err != nil || version < 0 {
		return 0, errors.New("If-Match must hold a stage preferences version: a whole number of 0 or more")
	}
	return version, nil
}

func writeStagePreferences(w http.ResponseWriter, status int, set store.StagePreferences) {
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(set.Version, 10)))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, set)
}

// writeStagePreferenceValidation answers 422 validation_failed naming the
// field and, for a review seat entry, its 1-based position (invariant 2).
func writeStagePreferenceValidation(w http.ResponseWriter, invalid *stageprefs.ValidationError) {
	field := map[string]any{"field": invalid.Field, "message": invalid.Message}
	if invalid.Position > 0 {
		field["position"] = invalid.Position
	}
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "validation_failed", "fields": []map[string]any{field}})
}

// writeStagePreferenceStoreError logs the cause with the request ID and
// answers a generic 500, so no surface reports a cached set as current
// (DEC-105(1); component-stage-preferences, State and failure).
func writeStagePreferenceStoreError(w http.ResponseWriter, r *http.Request, action string, err error) {
	log.Printf("httpapi: %s: request_id=%q error=%q", action, middleware.GetReqID(r.Context()), redactedErrorLogText(err))
	http.Error(w, "internal server error", http.StatusInternalServerError)
}
