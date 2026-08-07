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
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/planning"
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
	ServingTasks         []core.Task                  `json:"serving_tasks"`
	RequirementLinks     []core.RequirementServesLink `json:"requirement_links"`
	PlanningSessions     []core.PlanningSession       `json:"planning_sessions"`
	Artifacts            []core.Artifact              `json:"artifacts"`
	Lineage              []core.Event                 `json:"lineage"`
	LineageGraph         core.LineageTraversal        `json:"lineage_graph"`
	Staleness            requirementStaleness         `json:"staleness"`
	MigratedSeed         bool                         `json:"migrated_seed"`
	ConfirmationEligible bool                         `json:"confirmation_eligible"`
}

type requirementStaleness struct {
	DeliveryAfterIntent bool            `json:"delivery_after_intent"`
	PartialEvaluation   bool            `json:"partial_evaluation"`
	LatestDelivery      string          `json:"latest_delivery,omitempty"`
	LatestDeliveryAt    *time.Time      `json:"latest_delivery_at,omitempty"`
	ActiveDrift         []monitor.Drift `json:"active_drift"`
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

type requirementMutation struct {
	ID              string                      `json:"id"`
	Title           string                      `json:"title"`
	Content         string                      `json:"content"`
	DerivedFrom     *core.RequirementDerivation `json:"derived_from,omitempty"`
	Origin          core.RequirementOrigin      `json:"origin"`
	OriginSessionID string                      `json:"origin_session_id"`
	OriginTaskID    string                      `json:"origin_task_id"`
	OriginDriftID   string                      `json:"origin_drift_id"`
}

func validateOperatorRequirementMutation(input requirementMutation) error {
	if input.Origin != "" && input.Origin != core.RequirementOriginOperator {
		return fmt.Errorf("REST requirement proposals support only operator origin")
	}
	if strings.TrimSpace(input.OriginSessionID) != "" || strings.TrimSpace(input.OriginTaskID) != "" || strings.TrimSpace(input.OriginDriftID) != "" {
		return fmt.Errorf("REST requirement proposals cannot name an origin session, task, or drift")
	}
	return nil
}

func (s *Server) operatorRequirementVersion(r *http.Request, id string, input requirementMutation) (core.RequirementVersion, error) {
	if err := validateOperatorRequirementMutation(input); err != nil {
		return core.RequirementVersion{}, err
	}
	document, err := pipeline.ParseRequirementDocument(input.Content)
	if err != nil {
		return core.RequirementVersion{}, err
	}
	version := core.RequirementVersion{
		RequirementID: id, Content: document.Markdown, Statements: document.Statements,
		Origin: core.RequirementOriginOperator, DerivedFrom: input.DerivedFrom,
	}
	if input.DerivedFrom != nil {
		validator := s.Planning
		if validator == nil {
			validator = &planning.Service{Store: s.Store}
		}
		if err = validator.ValidateRequirementDerivation(r.Context(), input.DerivedFrom, document.Statements); err != nil {
			return core.RequirementVersion{}, err
		}
	}
	return version, nil
}

func (s *Server) createRequirement(w http.ResponseWriter, r *http.Request) {
	var input requirementMutation
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	id, title := strings.TrimSpace(input.ID), strings.TrimSpace(input.Title)
	if id == "" || title == "" {
		http.Error(w, "requirement id and title are required", http.StatusBadRequest)
		return
	}
	version, err := s.operatorRequirementVersion(r, id, input)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	requirement, version, err := s.Store.CreateRequirement(r.Context(), core.Requirement{ID: id, Title: title}, version)
	if err != nil {
		http.Error(w, err.Error(), requirementMutationStatus(err))
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"requirement": requirement, "version": version})
}

func (s *Server) proposeRequirementVersion(w http.ResponseWriter, r *http.Request) {
	var input requirementMutation
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	id := chi.URLParam(r, "id")
	if _, err := s.Store.GetRequirement(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	version, err := s.operatorRequirementVersion(r, id, input)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	version, err = s.Store.ProposeRequirementVersion(r.Context(), version)
	if err != nil {
		// The store owns content coherence, high-water, and additive AC rules;
		// violations are safe, specific client errors rather than SQL details.
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusCreated, version)
}

func requirementMutationStatus(err error) int {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, store.ErrRequirementSlugConflict), strings.Contains(err.Error(), "already exists"):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
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
	workspace, _ := store.WorkspaceFromContext(r.Context())
	// Staleness is an authority decision, not prompt rendering. Keep enough
	// fixed delivery-chain depth even when operators tune agent context lower.
	lineageBudget := core.LineageTraversalBudget{MaxDepth: 5, MaxNodes: 256, MaxLinks: 1024, Workspace: workspace}
	artifactNodes := map[core.LineageNode]bool{}
	graphs := make(map[string]core.LineageTraversal, len(requirements))
	lineageByRequirement := make(map[string][]core.LineageLink, len(requirements))
	for _, requirement := range requirements {
		root := core.LineageNode{Type: core.LineageRequirement, ID: requirement.ID}
		lineage, listErr := s.Store.ListLineageNeighborhood(r.Context(), []core.LineageNode{root}, lineageBudget)
		if listErr != nil {
			return nil, listErr
		}
		graph, graphErr := core.TraverseLineage(lineage, []core.LineageNode{root}, lineageBudget)
		if graphErr != nil {
			return nil, graphErr
		}
		for _, node := range graph.Nodes {
			artifactNodes[node] = true
		}
		graphs[root.ID] = graph
		lineageByRequirement[root.ID] = lineage
	}
	nodes := make([]core.LineageNode, 0, len(artifactNodes))
	for node := range artifactNodes {
		nodes = append(nodes, node)
	}
	artifacts, err := s.Store.ListArtifactsForLineage(r.Context(), nodes)
	if err != nil {
		return nil, err
	}
	lineageLabels, err := s.lineageNodeLabels(r, nodes, artifacts, sessions)
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
	activeDrift := []monitor.Drift{}
	if s.Monitor != nil {
		status, statusErr := s.Monitor.Status(r.Context())
		if statusErr != nil {
			return nil, fmt.Errorf("resolve requirement drift: %w", statusErr)
		}
		activeDrift = status.Drift
	}
	eventsByTask := map[string][]core.Event{}
	loadTaskEvents := func(taskID string) ([]core.Event, error) {
		if events, ok := eventsByTask[taskID]; ok {
			return events, nil
		}
		events, listErr := s.Store.ListEvents(r.Context(), taskID)
		if listErr == nil {
			eventsByTask[taskID] = events
		}
		return events, listErr
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
			ServingTasks:      []core.Task{},
			RequirementLinks:  linksByRequirement[requirement.ID],
			PlanningSessions:  []core.PlanningSession{},
			Artifacts:         []core.Artifact{},
			Lineage:           []core.Event{},
			Staleness:         requirementStaleness{ActiveDrift: []monitor.Drift{}},
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
			events, eventsErr := loadTaskEvents(task.ID)
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
		}
		graph := graphs[requirement.ID]
		applyLineageLabels(&graph, lineageLabels)
		view.LineageGraph = graph
		view.Staleness.PartialEvaluation = graph.Truncated
		lineage := lineageByRequirement[requirement.ID]
		reachableTasks := deliveryReachableTasks(lineage, requirement.ID)
		servingTaskIDs := directServingTaskIDs(lineage, requirement.ID)
		for taskID := range servingTaskIDs {
			task, getErr := s.Store.GetTask(r.Context(), taskID)
			if getErr != nil {
				return nil, getErr
			}
			if !core.BlueprintAnchor(task) {
				view.ServingTasks = append(view.ServingTasks, task)
			}
		}
		sort.Slice(view.ServingTasks, func(i, j int) bool { return view.ServingTasks[i].ID < view.ServingTasks[j].ID })
		if !confirmedAt.IsZero() && !view.Staleness.PartialEvaluation {
			for taskID := range reachableTasks {
				events, eventsErr := loadTaskEvents(taskID)
				if eventsErr != nil {
					return nil, eventsErr
				}
				label, at := mergedAfter(events, confirmedAt)
				if label != "" && (view.Staleness.LatestDeliveryAt == nil || at.After(*view.Staleness.LatestDeliveryAt)) {
					atCopy := at
					view.Staleness.LatestDelivery, view.Staleness.LatestDeliveryAt = label, &atCopy
				}
			}
		}
		for _, drift := range activeDrift {
			if drift.RequirementID == requirement.ID || reachableTasks[drift.TaskID] {
				view.Staleness.ActiveDrift = append(view.Staleness.ActiveDrift, drift)
			}
		}
		view.Staleness.DeliveryAfterIntent = !view.Staleness.PartialEvaluation && view.Staleness.LatestDelivery != ""
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

func directServingTaskIDs(links []core.LineageLink, requirementID string) map[string]bool {
	tasks := map[string]bool{}
	for _, link := range links {
		if link.Kind != "serves" {
			continue
		}
		if link.SrcType == core.LineageRequirement && link.SrcID == requirementID && link.DstType == core.LineageTask {
			tasks[link.DstID] = true
		}
		if link.DstType == core.LineageRequirement && link.DstID == requirementID && link.SrcType == core.LineageTask {
			tasks[link.SrcID] = true
		}
	}
	return tasks
}

// deliveryReachableTasks is the single staleness predicate. Staleness walks
// delivery edges at task level (spec §21.58 change 6), so it follows `serves`
// straight onto the task the requirement is attached to (change 3) as well as
// the historical blueprint chain that predates it, which stays readable as
// record. Planning-session and dependency hops remain excluded: they cannot
// import unrelated merges.
func deliveryReachableTasks(links []core.LineageLink, requirementID string) map[string]bool {
	reachable := map[core.LineageNode]bool{{Type: core.LineageRequirement, ID: requirementID}: true}
	tasks := map[string]bool{}
	changed := true
	for changed {
		changed = false
		for _, link := range links {
			src := core.LineageNode{Type: link.SrcType, ID: link.SrcID}
			dst := core.LineageNode{Type: link.DstType, ID: link.DstID}
			if !reachable[src] {
				continue
			}
			allowed := (link.Kind == "serves" && link.SrcType == core.LineageRequirement &&
				(link.DstType == core.LineageTask || link.DstType == core.LineageBlueprint)) ||
				(link.Kind == "versions" && link.SrcType == core.LineageBlueprint && link.DstType == core.LineageBlueprintVersion) ||
				(link.Kind == "materializes" && link.SrcType == core.LineageBlueprintVersion && link.DstType == core.LineageTask)
			if !allowed || reachable[dst] {
				continue
			}
			reachable[dst] = true
			changed = true
			if dst.Type == core.LineageBlueprint || dst.Type == core.LineageTask {
				tasks[dst.ID] = true
			}
		}
	}
	return tasks
}

func mergedAfter(events []core.Event, at time.Time) (string, time.Time) {
	var latest core.Event
	for _, event := range events {
		if (event.Kind == "merge.confirmed" || event.Kind == "merge.reconciled") && event.At.After(at) && event.At.After(latest.At) {
			latest = event
		}
	}
	if latest.Kind == "" {
		return "", time.Time{}
	}
	var payload map[string]any
	_ = json.Unmarshal(latest.Payload, &payload)
	for _, key := range []string{"title", "task_title"} {
		if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
			return value, latest.At
		}
	}
	if latest.TaskID != "" {
		return latest.TaskID, latest.At
	}
	return "a serving blueprint merge", latest.At
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
