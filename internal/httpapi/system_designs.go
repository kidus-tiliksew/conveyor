package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type systemDesignView struct {
	Document        core.SystemDesign          `json:"document"`
	CurrentVersion  *core.SystemDesignVersion  `json:"current_version,omitempty"`
	PendingVersions []core.SystemDesignVersion `json:"pending_versions"`
	Versions        []core.SystemDesignVersion `json:"versions"`
	Lineage         []core.Event               `json:"lineage"`
	Drift           []monitor.Drift            `json:"drift"`
}

// systemDesignSummary is the collection read model. Immutable document
// content, governed paths, version history, lineage, and drift records belong
// to the per-document endpoint and must not be repeated in the navigation tree.
type systemDesignSummary struct {
	Document            core.SystemDesign            `json:"document"`
	CurrentVersion      *systemDesignVersionSummary  `json:"current_version,omitempty"`
	PendingVersions     []systemDesignVersionSummary `json:"pending_versions"`
	PendingVersionCount int                          `json:"pending_version_count"`
	DriftCount          int                          `json:"drift_count"`
}

type systemDesignVersionSummary struct {
	DocumentID      string                  `json:"document_id"`
	Version         int                     `json:"version"`
	Origin          core.SystemDesignOrigin `json:"origin"`
	OriginSessionID string                  `json:"origin_session_id,omitempty"`
	OriginTaskID    string                  `json:"origin_task_id,omitempty"`
	Confirmed       bool                    `json:"confirmed"`
	ConfirmedBy     string                  `json:"confirmed_by,omitempty"`
	ConfirmedAt     time.Time               `json:"confirmed_at,omitempty"`
	Dismissed       bool                    `json:"dismissed"`
	DismissedBy     string                  `json:"dismissed_by,omitempty"`
	DismissedAt     time.Time               `json:"dismissed_at,omitempty"`
	Workspace       string                  `json:"workspace"`
	CreatedAt       time.Time               `json:"created_at"`
}

func summarizeSystemDesignVersion(version core.SystemDesignVersion) systemDesignVersionSummary {
	return systemDesignVersionSummary{
		DocumentID: version.DocumentID, Version: version.Version, Origin: version.Origin,
		OriginSessionID: version.OriginSessionID, OriginTaskID: version.OriginTaskID,
		Confirmed: version.Confirmed, ConfirmedBy: version.ConfirmedBy, ConfirmedAt: version.ConfirmedAt,
		Dismissed: version.Dismissed, DismissedBy: version.DismissedBy, DismissedAt: version.DismissedAt,
		Workspace: version.Workspace, CreatedAt: version.CreatedAt,
	}
}

func (s *Server) systemDesignView(r *http.Request, document core.SystemDesign, drift []monitor.Drift) (systemDesignView, error) {
	versions, err := s.Store.ListSystemDesignVersions(r.Context(), document.ID)
	if err != nil {
		return systemDesignView{}, err
	}
	events, err := s.Store.ListSystemDesignEvents(r.Context(), document.ID)
	if err != nil {
		return systemDesignView{}, err
	}
	return buildSystemDesignView(document, versions, events, drift), nil
}

func buildSystemDesignView(document core.SystemDesign, versions []core.SystemDesignVersion, events []core.Event, drift []monitor.Drift) systemDesignView {
	view := systemDesignView{Document: document, PendingVersions: []core.SystemDesignVersion{}, Versions: versions, Lineage: events, Drift: []monitor.Drift{}}
	for _, item := range drift {
		if item.SystemDesignID == document.ID {
			view.Drift = append(view.Drift, item)
		}
	}
	for i := range versions {
		if versions[i].Version == document.CurrentVersion {
			copy := versions[i]
			view.CurrentVersion = &copy
		}
		if !versions[i].Confirmed && !versions[i].Dismissed {
			view.PendingVersions = append(view.PendingVersions, versions[i])
		}
	}
	return view
}

func (s *Server) listSystemDesigns(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.ListSystemDesigns(r.Context(), r.URL.Query().Get("include_archived") == "true")
	if err != nil {
		log.Printf("handle system design request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	views := make([]systemDesignSummary, 0, len(items))
	versions, err := s.Store.ListSystemDesignVersionsByDocument(r.Context())
	if err != nil {
		log.Printf("handle system design request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	driftCounts := map[string]int{}
	if s.Monitor != nil {
		driftCounts, err = s.Store.ListActiveSystemDesignDriftCounts(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	for _, item := range items {
		summary := systemDesignSummary{Document: item, PendingVersions: []systemDesignVersionSummary{}}
		for _, version := range versions[item.ID] {
			if version.Version == item.CurrentVersion {
				current := summarizeSystemDesignVersion(version)
				summary.CurrentVersion = &current
			}
			if !version.Confirmed && !version.Dismissed {
				summary.PendingVersions = append(summary.PendingVersions, summarizeSystemDesignVersion(version))
			}
		}
		summary.PendingVersionCount = len(summary.PendingVersions)
		summary.DriftCount = driftCounts[item.ID]
		views = append(views, summary)
	}
	writeJSON(w, http.StatusOK, views)
}
func (s *Server) getSystemDesign(w http.ResponseWriter, r *http.Request) {
	item, err := s.Store.GetSystemDesign(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	var drift []monitor.Drift
	if s.Monitor != nil {
		status, statusErr := s.Monitor.Status(r.Context())
		if statusErr != nil {
			http.Error(w, statusErr.Error(), http.StatusInternalServerError)
			return
		}
		drift = status.Drift
	}
	view, err := s.systemDesignView(r, item, drift)
	if err != nil {
		log.Printf("handle system design request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, view)
}
func (s *Server) listSystemDesignVersions(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.ListSystemDesignVersions(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if items == nil {
		items = []core.SystemDesignVersion{}
	}
	writeJSON(w, http.StatusOK, items)
}

type systemDesignMutation struct {
	ID              string                  `json:"id"`
	Title           string                  `json:"title"`
	Category        string                  `json:"category"`
	Content         string                  `json:"content"`
	Origin          core.SystemDesignOrigin `json:"origin"`
	OriginSessionID string                  `json:"origin_session_id"`
	OriginTaskID    string                  `json:"origin_task_id"`
}

func (input systemDesignMutation) version(id string) core.SystemDesignVersion {
	return core.SystemDesignVersion{DocumentID: id, Content: input.Content, Origin: core.SystemDesignOriginOperator}
}

func validateOperatorSystemDesignMutation(input systemDesignMutation) error {
	if input.Origin != "" && input.Origin != core.SystemDesignOriginOperator {
		return fmt.Errorf("REST system design mutations support only operator origin")
	}
	if strings.TrimSpace(input.OriginSessionID) != "" || strings.TrimSpace(input.OriginTaskID) != "" {
		return fmt.Errorf("REST system design mutations cannot name an origin session or task")
	}
	return nil
}
func (s *Server) createSystemDesign(w http.ResponseWriter, r *http.Request) {
	var input systemDesignMutation
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if err := validateOperatorSystemDesignMutation(input); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(input.ID) == "" || strings.TrimSpace(input.Title) == "" || strings.TrimSpace(input.Category) == "" {
		http.Error(w, "system design id, title, and category are required", http.StatusBadRequest)
		return
	}
	if err := core.ValidateSystemDesignID(input.ID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if version := input.version(input.ID); core.NormalizeSystemDesignVersion(&version) != nil {
		http.Error(w, "invalid system design content", http.StatusBadRequest)
		return
	}
	document, version, err := s.Store.CreateSystemDesign(r.Context(), core.SystemDesign{ID: strings.TrimSpace(input.ID), Title: strings.TrimSpace(input.Title), Category: strings.TrimSpace(input.Category)}, input.version(input.ID))
	if err != nil {
		http.Error(w, err.Error(), systemDesignMutationStatus(err))
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"document": document, "version": version})
}
func (s *Server) proposeSystemDesignVersion(w http.ResponseWriter, r *http.Request) {
	var input systemDesignMutation
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if err := validateOperatorSystemDesignMutation(input); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if version := input.version(chi.URLParam(r, "id")); core.NormalizeSystemDesignVersion(&version) != nil {
		http.Error(w, "invalid system design content", http.StatusBadRequest)
		return
	}
	version, err := s.Store.ProposeSystemDesignVersion(r.Context(), input.version(chi.URLParam(r, "id")))
	if err != nil {
		var archived *store.SystemDesignArchivedError
		if errors.As(err, &archived) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "system_design_archived", "message": archived.Error()})
			return
		}
		http.Error(w, err.Error(), systemDesignMutationStatus(err))
		return
	}
	writeJSON(w, http.StatusCreated, version)
}
func (s *Server) confirmSystemDesignVersion(w http.ResponseWriter, r *http.Request) {
	version, err := strconv.Atoi(chi.URLParam(r, "version"))
	if err != nil || version < 1 {
		http.Error(w, "system design version must be a positive integer", http.StatusBadRequest)
		return
	}
	var expected []int
	if value := r.Header.Get("If-Match"); value != "" {
		parsed, parseErr := parseRequirementIfMatch(value)
		if parseErr != nil {
			http.Error(w, "If-Match must contain a non-negative current system design version", http.StatusBadRequest)
			return
		}
		expected = append(expected, int(parsed))
	}
	document, confirmed, err := s.Store.ConfirmSystemDesignVersion(r.Context(), chi.URLParam(r, "id"), version, expected...)
	if err != nil {
		var archived *store.SystemDesignArchivedError
		if errors.As(err, &archived) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "system_design_archived", "message": archived.Error()})
			return
		}
		var conflict *store.SystemDesignVersionConflict
		if errors.As(err, &conflict) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "system_design_version_conflict", "message": conflict.Error(), "document_id": conflict.DocumentID, "requested_version": conflict.Requested, "current_version": conflict.Current})
			return
		}
		http.Error(w, err.Error(), systemDesignMutationStatus(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"document": document, "version": confirmed})
}

func (s *Server) archiveSystemDesign(w http.ResponseWriter, r *http.Request) {
	s.setSystemDesignArchiveState(w, r, true)
}
func (s *Server) restoreSystemDesign(w http.ResponseWriter, r *http.Request) {
	s.setSystemDesignArchiveState(w, r, false)
}
func (s *Server) setSystemDesignArchiveState(w http.ResponseWriter, r *http.Request, archived bool) {
	id := chi.URLParam(r, "id")
	actor := store.ActorFromContext(r.Context()).ID
	var err error
	if archived {
		err = s.Store.ArchiveSystemDesign(r.Context(), id, actor)
	} else {
		err = s.Store.RestoreSystemDesign(r.Context(), id, actor)
	}
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	document, err := s.Store.GetSystemDesign(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, document)
}

func (s *Server) dismissSystemDesignVersion(w http.ResponseWriter, r *http.Request) {
	version, err := strconv.Atoi(chi.URLParam(r, "version"))
	if err != nil || version < 1 {
		http.Error(w, "system design version must be a positive integer", http.StatusBadRequest)
		return
	}
	document, dismissed, err := s.Store.DismissSystemDesignVersion(r.Context(), chi.URLParam(r, "id"), version)
	if err != nil {
		var conflict *store.SystemDesignVersionDismissalConflict
		if errors.As(err, &conflict) {
			code := "system_design_version_" + string(conflict.Reason)
			body := map[string]any{
				"error": code, "message": conflict.Error(), "document_id": conflict.DocumentID,
				"requested_version": conflict.Requested, "current_version": conflict.Current,
			}
			if conflict.SupersededBy > 0 {
				body["superseded_by_version"] = conflict.SupersededBy
			}
			writeJSON(w, http.StatusConflict, body)
			return
		}
		http.Error(w, err.Error(), systemDesignMutationStatus(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"document": document, "version": dismissed})
}

func (s *Server) listDecisions(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.ListDecisions(r.Context())
	if err != nil {
		log.Printf("handle system design request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if items == nil {
		items = []core.Decision{}
	}
	writeJSON(w, http.StatusOK, items)
}
func (s *Server) getDecision(w http.ResponseWriter, r *http.Request) {
	item, err := s.Store.GetDecision(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, item)
}
func (s *Server) proposeDecision(w http.ResponseWriter, r *http.Request) {
	var input core.Decision
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if input.Origin != "" && input.Origin != core.DecisionOriginOperator {
		http.Error(w, "REST decision proposals support only operator origin", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(input.OriginSessionID) != "" || strings.TrimSpace(input.OriginTaskID) != "" {
		http.Error(w, "REST decision proposals cannot name an origin session or task", http.StatusBadRequest)
		return
	}
	input.Origin, input.OriginSessionID, input.OriginTaskID = core.DecisionOriginOperator, "", ""
	if err := core.ValidateDecision(input); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	item, err := s.Store.ProposeDecision(r.Context(), input)
	if err != nil {
		http.Error(w, err.Error(), systemDesignMutationStatus(err))
		return
	}
	writeJSON(w, http.StatusCreated, item)
}
func (s *Server) confirmDecision(w http.ResponseWriter, r *http.Request) {
	item, err := s.Store.ConfirmDecision(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, fmt.Sprintf("confirm decision: %v", err), systemDesignMutationStatus(err))
		return
	}
	writeJSON(w, http.StatusOK, item)
}
func (s *Server) dismissDecision(w http.ResponseWriter, r *http.Request) {
	item, err := s.Store.DismissDecision(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, fmt.Sprintf("dismiss decision: %v", err), systemDesignMutationStatus(err))
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) dismissDecisionSupersessionSweep(w http.ResponseWriter, r *http.Request) {
	entry, err := s.Store.DismissDecisionSupersessionSweep(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "tier"), chi.URLParam(r, "document_id"))
	if err != nil {
		http.Error(w, fmt.Sprintf("dismiss decision supersession sweep: %v", err), systemDesignMutationStatus(err))
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func systemDesignMutationStatus(err error) int {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, store.ErrSystemDesignIDConflict), errors.Is(err, store.ErrSystemDesignSlugConflict),
		errors.Is(err, store.ErrDecisionIDConflict), errors.Is(err, store.ErrDecisionSupersessionConflict), errors.Is(err, store.ErrDecisionSweepTransition):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}
