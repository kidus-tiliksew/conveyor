package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

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

func (s *Server) systemDesignView(r *http.Request, document core.SystemDesign) (systemDesignView, error) {
	versions, err := s.Store.ListSystemDesignVersions(r.Context(), document.ID)
	if err != nil {
		return systemDesignView{}, err
	}
	events, err := s.Store.ListSystemDesignEvents(r.Context(), document.ID)
	if err != nil {
		return systemDesignView{}, err
	}
	view := systemDesignView{Document: document, PendingVersions: []core.SystemDesignVersion{}, Versions: versions, Lineage: events, Drift: []monitor.Drift{}}
	if s.Monitor != nil {
		status, statusErr := s.Monitor.Status(r.Context())
		if statusErr != nil {
			return systemDesignView{}, statusErr
		}
		for _, drift := range status.Drift {
			if drift.SystemDesignID == document.ID {
				view.Drift = append(view.Drift, drift)
			}
		}
	}
	for i := range versions {
		if versions[i].Version == document.CurrentVersion {
			copy := versions[i]
			view.CurrentVersion = &copy
		}
		if !versions[i].Confirmed {
			view.PendingVersions = append(view.PendingVersions, versions[i])
		}
	}
	return view, nil
}

func (s *Server) listSystemDesigns(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.ListSystemDesigns(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	views := make([]systemDesignView, 0, len(items))
	for _, item := range items {
		view, viewErr := s.systemDesignView(r, item)
		if viewErr != nil {
			http.Error(w, viewErr.Error(), http.StatusInternalServerError)
			return
		}
		views = append(views, view)
	}
	writeJSON(w, http.StatusOK, views)
}
func (s *Server) getSystemDesign(w http.ResponseWriter, r *http.Request) {
	item, err := s.Store.GetSystemDesign(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	view, err := s.systemDesignView(r, item)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
	return core.SystemDesignVersion{DocumentID: id, Content: input.Content, Origin: input.Origin, OriginSessionID: input.OriginSessionID, OriginTaskID: input.OriginTaskID}
}
func (s *Server) createSystemDesign(w http.ResponseWriter, r *http.Request) {
	var input systemDesignMutation
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if input.Origin == "" {
		input.Origin = core.SystemDesignOriginOperator
	}
	document, version, err := s.Store.CreateSystemDesign(r.Context(), core.SystemDesign{ID: strings.TrimSpace(input.ID), Title: strings.TrimSpace(input.Title), Category: strings.TrimSpace(input.Category)}, input.version(input.ID))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
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
	if input.Origin == "" {
		input.Origin = core.SystemDesignOriginOperator
	}
	version, err := s.Store.ProposeSystemDesignVersion(r.Context(), input.version(chi.URLParam(r, "id")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
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
		var conflict *store.SystemDesignVersionConflict
		if errors.As(err, &conflict) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "system_design_version_conflict", "message": conflict.Error(), "document_id": conflict.DocumentID, "requested_version": conflict.Requested, "current_version": conflict.Current})
			return
		}
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"document": document, "version": confirmed})
}

func (s *Server) listDecisions(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.ListDecisions(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
	if input.Origin == "" {
		input.Origin = core.DecisionOriginOperator
	}
	item, err := s.Store.ProposeDecision(r.Context(), input)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}
func (s *Server) confirmDecision(w http.ResponseWriter, r *http.Request) {
	item, err := s.Store.ConfirmDecision(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, fmt.Sprintf("confirm decision: %v", err), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, item)
}
