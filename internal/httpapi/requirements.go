package httpapi

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// requirementView is the dashboard read model for one living requirement.
// It deliberately exposes immutable versions and pipeline-owned lineage
// together so the UI never has to reconstruct authority from feature-tree
// assignments (spec §4.2, §13.3).
type requirementView struct {
	Requirement       core.Requirement          `json:"requirement"`
	CurrentVersion    *core.RequirementVersion  `json:"current_version,omitempty"`
	PendingVersions   []core.RequirementVersion `json:"pending_versions"`
	ServingBlueprints []blueprintLineage        `json:"serving_blueprints"`
	PlanningSessions  []core.PlanningSession    `json:"planning_sessions"`
	Artifacts         []core.Artifact           `json:"artifacts"`
	Lineage           []core.Event              `json:"lineage"`
	Stale             bool                      `json:"stale"`
}

type blueprintLineage struct {
	Task   core.Task         `json:"task"`
	Spec   *core.SpecVersion `json:"spec,omitempty"`
	Events []core.Event      `json:"events"`
}

func (s *Server) listRequirements(w http.ResponseWriter, r *http.Request) {
	requirements, err := s.Store.ListRequirements(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	views, err := s.requirementViews(r, requirements)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) getRequirement(w http.ResponseWriter, r *http.Request) {
	requirement, err := s.Store.GetRequirement(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	views, err := s.requirementViews(r, []core.Requirement{requirement})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, views[0])
}

func (s *Server) listRequirementVersions(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := s.Store.GetRequirement(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	versions, err := s.Store.ListRequirementVersions(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if versions == nil {
		versions = []core.RequirementVersion{}
	}
	writeJSON(w, http.StatusOK, versions)
}

func (s *Server) confirmRequirementVersion(w http.ResponseWriter, r *http.Request) {
	version, err := strconv.Atoi(chi.URLParam(r, "version"))
	if err != nil || version < 1 {
		http.Error(w, "requirement version must be a positive integer", http.StatusBadRequest)
		return
	}
	requirement, confirmed, err := s.Store.ConfirmRequirementVersion(
		r.Context(), chi.URLParam(r, "id"), version,
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"requirement": requirement,
		"version":     confirmed,
	})
}

func (s *Server) requirementViews(r *http.Request, requirements []core.Requirement) ([]requirementView, error) {
	sessions, err := s.Store.ListPlanningSessions(r.Context())
	if err != nil {
		return nil, err
	}
	artifacts, err := s.Store.ListArtifacts(r.Context())
	if err != nil {
		return nil, err
	}
	requirementEvents, err := s.Store.ListEvents(r.Context(), "")
	if err != nil {
		return nil, err
	}

	views := make([]requirementView, 0, len(requirements))
	for _, requirement := range requirements {
		versions, listErr := s.Store.ListRequirementVersions(r.Context(), requirement.ID)
		if listErr != nil {
			return nil, listErr
		}
		view := requirementView{
			Requirement:       requirement,
			PendingVersions:   []core.RequirementVersion{},
			ServingBlueprints: []blueprintLineage{},
			PlanningSessions:  []core.PlanningSession{},
			Artifacts:         []core.Artifact{},
			Lineage:           []core.Event{},
		}
		originSessionIDs := map[string]bool{}
		var confirmedAt time.Time
		for index := range versions {
			version := versions[index]
			if version.OriginSessionID != "" {
				originSessionIDs[version.OriginSessionID] = true
			}
			if version.Version == requirement.CurrentVersion && version.Confirmed {
				current := version
				view.CurrentVersion = &current
				confirmedAt = version.ConfirmedAt
			}
			if !version.Confirmed {
				view.PendingVersions = append(view.PendingVersions, version)
			}
		}
		for _, artifact := range artifacts {
			if artifact.RequirementID == requirement.ID {
				view.Artifacts = append(view.Artifacts, artifact)
			}
		}
		for _, event := range requirementEvents {
			if eventReferencesRequirement(event, requirement.ID, requirement.Workspace) {
				view.Lineage = append(view.Lineage, event)
			}
		}
		blueprints := map[string]bool{}
		for _, session := range sessions {
			if session.RequirementContextID != requirement.ID &&
				session.ProducedRequirementID != requirement.ID &&
				!originSessionIDs[session.ID] {
				continue
			}
			view.PlanningSessions = append(view.PlanningSessions, session)
			if session.RequirementContextID != requirement.ID ||
				session.ProducedTaskID == "" || blueprints[session.ProducedTaskID] {
				continue
			}
			blueprints[session.ProducedTaskID] = true
			task, getErr := s.Store.GetTask(r.Context(), session.ProducedTaskID)
			if getErr != nil {
				return nil, getErr
			}
			events, eventsErr := s.Store.ListEvents(r.Context(), task.ID)
			if eventsErr != nil {
				return nil, eventsErr
			}
			item := blueprintLineage{Task: task, Events: events}
			if spec, exists, specErr := s.Store.GetLatestSpecVersion(r.Context(), task.ID); specErr != nil {
				return nil, specErr
			} else if exists {
				item.Spec = &spec
			}
			view.ServingBlueprints = append(view.ServingBlueprints, item)
			view.Lineage = append(view.Lineage, events...)
			if !confirmedAt.IsZero() && mergedAfter(events, confirmedAt) {
				view.Stale = true
			}
		}
		// An unconfirmed revision is itself visible alignment debt.
		view.Stale = view.Stale || len(view.PendingVersions) > 0
		sort.SliceStable(view.Lineage, func(i, j int) bool {
			if view.Lineage[i].At.Equal(view.Lineage[j].At) {
				return view.Lineage[i].ID < view.Lineage[j].ID
			}
			return view.Lineage[i].At.Before(view.Lineage[j].At)
		})
		views = append(views, view)
	}
	return views, nil
}

func eventReferencesRequirement(event core.Event, requirementID, workspace string) bool {
	var payload map[string]any
	if json.Unmarshal(event.Payload, &payload) != nil {
		return false
	}
	value, _ := payload["requirement_id"].(string)
	eventWorkspace, _ := payload["workspace_id"].(string)
	return strings.TrimSpace(value) == requirementID &&
		(eventWorkspace == "" || strings.TrimSpace(eventWorkspace) == workspace)
}

func mergedAfter(events []core.Event, at time.Time) bool {
	for _, event := range events {
		if (event.Kind == "merge.confirmed" || event.Kind == "merge.reconciled") && event.At.After(at) {
			return true
		}
	}
	return false
}
