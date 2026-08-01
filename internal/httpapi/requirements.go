package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// requirementView is the dashboard read model for one living requirement.
// It deliberately exposes immutable versions and pipeline-owned lineage
// together so the UI never has to reconstruct authority from feature-tree
// assignments (spec §4.2, §13.3).
type requirementView struct {
	Requirement          core.Requirement             `json:"requirement"`
	CurrentVersion       *core.RequirementVersion     `json:"current_version,omitempty"`
	PendingVersions      []core.RequirementVersion    `json:"pending_versions"`
	ServingBlueprints    []blueprintLineage           `json:"serving_blueprints"`
	RequirementLinks     []core.RequirementServesLink `json:"requirement_links"`
	PlanningSessions     []core.PlanningSession       `json:"planning_sessions"`
	Artifacts            []core.Artifact              `json:"artifacts"`
	Lineage              []core.Event                 `json:"lineage"`
	ShippedPastIntent    string                       `json:"shipped_past_intent,omitempty"`
	MigratedSeed         bool                         `json:"migrated_seed"`
	ConfirmationEligible bool                         `json:"confirmation_eligible"`
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
	var expected []int
	if value := r.Header.Get("If-Match"); value != "" {
		parsed, parseErr := parseRequirementIfMatch(value)
		if parseErr != nil || parsed > int64(^uint(0)>>1) {
			http.Error(w, "If-Match must contain a non-negative current requirement version", http.StatusBadRequest)
			return
		}
		expected = append(expected, int(parsed))
	}
	requirement, confirmed, err := s.Store.ConfirmRequirementVersion(
		r.Context(), chi.URLParam(r, "id"), version, expected...,
	)
	if err != nil {
		var conflict *store.RequirementVersionConflict
		if errors.As(err, &conflict) {
			code := "requirement_version_superseded"
			if conflict.Expected != nil {
				code = "requirement_current_version_mismatch"
			}
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": code, "message": conflict.Error(),
				"requirement_id":    conflict.RequirementID,
				"requested_version": conflict.Requested,
				"current_version":   conflict.Current,
			})
			return
		}
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"requirement": requirement,
		"version":     confirmed,
	})
}

func parseRequirementIfMatch(value string) (int64, error) {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "W/")
	value = strings.Trim(value, "\"")
	version, err := strconv.ParseInt(value, 10, 64)
	if err != nil || version < 0 {
		return 0, fmt.Errorf("If-Match must contain a non-negative current requirement version")
	}
	return version, nil
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
	servesLinks, err := s.Store.ListRequirementServes(r.Context())
	if err != nil {
		return nil, err
	}
	linksByRequirement := make(map[string][]core.RequirementServesLink)
	for _, link := range servesLinks {
		linksByRequirement[link.RequirementID] = append(linksByRequirement[link.RequirementID], link)
	}
	views := make([]requirementView, 0, len(requirements))
	for _, requirement := range requirements {
		versions, listErr := s.Store.ListRequirementVersions(r.Context(), requirement.ID)
		if listErr != nil {
			return nil, listErr
		}
		requirementEvents, listErr := s.Store.ListRequirementEvents(r.Context(), requirement.ID)
		if listErr != nil {
			return nil, listErr
		}
		view := requirementView{
			Requirement:       requirement,
			PendingVersions:   []core.RequirementVersion{},
			ServingBlueprints: []blueprintLineage{},
			RequirementLinks:  linksByRequirement[requirement.ID],
			PlanningSessions:  []core.PlanningSession{},
			Artifacts:         []core.Artifact{},
			Lineage:           []core.Event{},
		}
		if view.RequirementLinks == nil {
			view.RequirementLinks = []core.RequirementServesLink{}
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
		if len(view.PendingVersions) > 0 {
			latest := view.PendingVersions[len(view.PendingVersions)-1]
			view.ConfirmationEligible = core.ConfirmableRequirementVersion(latest) == nil
			view.MigratedSeed = requirement.CurrentVersion == 0 && len(versions) == 1 &&
				latest.Origin == core.RequirementOriginFeatureMigration
		}
		for _, artifact := range artifacts {
			if artifact.RequirementID == requirement.ID {
				view.Artifacts = append(view.Artifacts, artifact)
			}
		}
		view.Lineage = append(view.Lineage, annotateBackfilledEvents(requirementEvents)...)
		for _, session := range sessions {
			if session.RequirementContextID != requirement.ID &&
				session.ProducedRequirementID != requirement.ID &&
				!originSessionIDs[session.ID] {
				continue
			}
			view.PlanningSessions = append(view.PlanningSessions, session)
		}
		for _, link := range view.RequirementLinks {
			if link.State != core.RequirementServesConfirmed {
				continue
			}
			task, getErr := s.Store.GetTask(r.Context(), link.BlueprintTaskID)
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
			view.Lineage = append(view.Lineage, annotateBackfilledEvents(events)...)
			if !confirmedAt.IsZero() {
				if shipped := mergedAfter(events, confirmedAt); shipped != "" {
					view.ShippedPastIntent = shipped
				}
			}
		}
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

func mergedAfter(events []core.Event, at time.Time) string {
	var latest core.Event
	for _, event := range events {
		if (event.Kind == "merge.confirmed" || event.Kind == "merge.reconciled") && event.At.After(at) && event.At.After(latest.At) {
			latest = event
		}
	}
	if latest.Kind == "" {
		return ""
	}
	var payload map[string]any
	_ = json.Unmarshal(latest.Payload, &payload)
	for _, key := range []string{"title", "task_title"} {
		if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
			return value
		}
	}
	if latest.TaskID != "" {
		return latest.TaskID
	}
	return "a serving blueprint merge"
}

func annotateBackfilledEvents(events []core.Event) []core.Event {
	annotated := append([]core.Event(nil), events...)
	for index := range annotated {
		event := &annotated[index]
		if event.ActorID != "migration-050" || (event.Kind != "requirement.created" && event.Kind != "requirement.version_proposed") {
			continue
		}
		var payload map[string]any
		if json.Unmarshal(event.Payload, &payload) == nil {
			payload["backfilled"] = true
			event.Payload = core.JSONPayload(payload)
		}
	}
	return annotated
}
