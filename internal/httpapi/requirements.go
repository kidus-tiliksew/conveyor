package httpapi

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
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
// assignments (design-document-corpus; design-web-dashboard).
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

// requirementSummary is the compact read model used by the document tree.
// Content, histories, graph data, and action payloads stay on the detail route
// (design-http-api; design-web-dashboard).
type requirementSummary struct {
	Requirement          core.Requirement           `json:"requirement"`
	CurrentVersion       *requirementVersionSummary `json:"current_version,omitempty"`
	PendingVersionCount  int                        `json:"pending_version_count"`
	ServingTasks         []requirementTaskSummary   `json:"serving_tasks"`
	Staleness            requirementStaleness       `json:"staleness"`
	ConfirmationEligible bool                       `json:"confirmation_eligible"`
}

type requirementTaskSummary struct {
	ID    string         `json:"id"`
	Title string         `json:"title"`
	State core.TaskState `json:"state"`
}

type requirementVersionSummary struct {
	RequirementID string                 `json:"requirement_id"`
	Version       int                    `json:"version"`
	Origin        core.RequirementOrigin `json:"origin"`
	Confirmed     bool                   `json:"confirmed"`
	ConfirmedBy   string                 `json:"confirmed_by,omitempty"`
	ConfirmedAt   time.Time              `json:"confirmed_at,omitempty"`
	CreatedAt     time.Time              `json:"created_at"`
}

type requirementStaleness struct {
	DeliveryAfterIntent bool                  `json:"delivery_after_intent"`
	PartialEvaluation   bool                  `json:"partial_evaluation"`
	Deliveries          []requirementDelivery `json:"deliveries"`
	ActiveDrift         []monitor.Drift       `json:"active_drift"`
}

type requirementDelivery struct {
	SignalID        string               `json:"signal_id"`
	TaskID          string               `json:"task_id"`
	DeliveryEventID int64                `json:"delivery_event_id"`
	EventKind       string               `json:"event_kind"`
	Label           string               `json:"label"`
	URL             string               `json:"url,omitempty"`
	At              time.Time            `json:"at"`
	PinnedVersion   int                  `json:"pinned_version,omitempty"`
	CurrentVersion  int                  `json:"current_version,omitempty"`
	NeedsAttention  bool                 `json:"needs_attention"`
	Reasons         []string             `json:"reasons"`
	FollowUp        *requirementFollowUp `json:"follow_up,omitempty"`
}

type requirementFollowUp struct {
	TaskID string         `json:"task_id"`
	Title  string         `json:"title"`
	State  core.TaskState `json:"state"`
}

type requirementDeliveryWatermark struct {
	At      time.Time
	EventID int64
}

type blueprintLineage struct {
	Task   core.Task         `json:"task"`
	Spec   *core.SpecVersion `json:"spec,omitempty"`
	Events []core.Event      `json:"events"`
}

func (s *Server) listRequirements(w http.ResponseWriter, r *http.Request) {
	requirements, err := s.Store.ListRequirements(r.Context())
	if err != nil {
		log.Printf("handle requirement request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	views, err := s.requirementViews(r, requirements, false)
	if err != nil {
		log.Printf("handle requirement request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	summaries := make([]requirementSummary, 0, len(views))
	for _, view := range views {
		staleness := view.Staleness
		staleness.Deliveries = slices.DeleteFunc(staleness.Deliveries, func(delivery requirementDelivery) bool {
			return !delivery.NeedsAttention
		})
		var current *requirementVersionSummary
		if view.CurrentVersion != nil {
			current = &requirementVersionSummary{
				RequirementID: view.CurrentVersion.RequirementID,
				Version:       view.CurrentVersion.Version,
				Origin:        view.CurrentVersion.Origin,
				Confirmed:     view.CurrentVersion.Confirmed,
				ConfirmedBy:   view.CurrentVersion.ConfirmedBy,
				ConfirmedAt:   view.CurrentVersion.ConfirmedAt,
				CreatedAt:     view.CurrentVersion.CreatedAt,
			}
		}
		servingTasks := make([]requirementTaskSummary, 0, len(view.ServingTasks))
		for _, task := range view.ServingTasks {
			servingTasks = append(servingTasks, requirementTaskSummary{ID: task.ID, Title: task.Title, State: task.State})
		}
		summaries = append(summaries, requirementSummary{
			Requirement:          view.Requirement,
			CurrentVersion:       current,
			PendingVersionCount:  len(view.PendingVersions),
			ServingTasks:         servingTasks,
			Staleness:            staleness,
			ConfirmationEligible: view.ConfirmationEligible,
		})
	}
	writeJSON(w, http.StatusOK, summaries)
}

func (s *Server) getRequirement(w http.ResponseWriter, r *http.Request) {
	requirement, err := s.Store.GetRequirement(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	views, err := s.requirementViews(r, []core.Requirement{requirement}, true)
	if err != nil {
		log.Printf("handle requirement request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
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
		log.Printf("handle requirement request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if versions == nil {
		versions = []core.RequirementVersion{}
	}
	writeJSON(w, http.StatusOK, versions)
}

func (s *Server) acknowledgeRequirementStaleness(w http.ResponseWriter, r *http.Request) {
	requirementID, signalID := chi.URLParam(r, "id"), chi.URLParam(r, "signal")
	delivery, err := s.currentRequirementDelivery(r, requirementID, signalID, false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	acknowledgment, err := s.Store.AcknowledgeRequirementStaleness(r.Context(), core.RequirementStalenessAcknowledgment{
		RequirementID: requirementID, SignalID: signalID, DeliveryTaskID: delivery.TaskID,
		DeliveryEventID: delivery.DeliveryEventID, AcknowledgedThrough: delivery.At,
	})
	if err != nil {
		http.Error(w, err.Error(), requirementMutationStatus(err))
		return
	}
	writeJSON(w, http.StatusCreated, acknowledgment)
}

func (s *Server) createRequirementStalenessFollowUp(w http.ResponseWriter, r *http.Request) {
	requirementID, signalID := chi.URLParam(r, "id"), chi.URLParam(r, "signal")
	delivery, err := s.currentRequirementDelivery(r, requirementID, signalID, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	deliveryTask, err := s.Store.GetTask(r.Context(), delivery.TaskID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	comparison := "No served-version skew was recorded."
	if delivery.PinnedVersion > 0 || delivery.CurrentVersion > 0 {
		comparison = fmt.Sprintf("Served version: v%d; current at delivery: v%d.", delivery.PinnedVersion, delivery.CurrentVersion)
	}
	link := delivery.URL
	if link == "" {
		link = "No pull-request URL was recorded."
	}
	body := fmt.Sprintf("# Investigate requirement delivery-staleness signal\n\nRequirement: `%s`\nSignal: `%s`\nDelivery task: `%s`\nDelivery event: `%s` (%d) at %s\nDelivery link: %s\n\n## Firing condition\n\n- %s\n\n## Version context\n\n%s\n", requirementID, signalID, delivery.TaskID, delivery.EventKind, delivery.DeliveryEventID, delivery.At.UTC().Format(time.RFC3339Nano), link, strings.Join(delivery.Reasons, "\n- "), comparison)
	result, err := s.createTaskRecord(r.Context(), createTaskReq{
		Body: body, Repo: deliveryTask.Repo, BaseBranch: deliveryTask.BaseBranch,
		Source: "requirement-staleness", RequirementIDs: []string{requirementID},
	}, requirementStalenessIntakeKey(signalID), "requirement-staleness")
	if err != nil {
		writeTaskCreateError(w, err)
		return
	}
	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"task": result.Task, "created": result.Created})
}

func (s *Server) currentRequirementDelivery(r *http.Request, requirementID, signalID string, allowLinked bool) (requirementDelivery, error) {
	requirement, err := s.Store.GetRequirement(r.Context(), requirementID)
	if err != nil {
		return requirementDelivery{}, err
	}
	views, err := s.requirementViews(r, []core.Requirement{requirement}, true)
	if err != nil {
		return requirementDelivery{}, err
	}
	for _, delivery := range views[0].Staleness.Deliveries {
		if delivery.SignalID == signalID && (delivery.NeedsAttention || (allowLinked && delivery.FollowUp != nil)) {
			return delivery, nil
		}
	}
	return requirementDelivery{}, fmt.Errorf("staleness signal %s is no longer actionable", signalID)
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
		var superseded *store.RequirementVersionSuperseded
		if errors.As(err, &superseded) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": "requirement_version_superseded", "message": superseded.Error(),
				"requirement_id":        superseded.RequirementID,
				"requested_version":     superseded.Requested,
				"current_version":       superseded.Current,
				"superseded_by_version": superseded.SupersededBy,
			})
			return
		}
		var conflict *store.RequirementVersionConflict
		if errors.As(err, &conflict) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": "requirement_current_version_mismatch", "message": conflict.Error(),
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

func (s *Server) listCheckpointContextCandidates(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := s.Store.GetRequirement(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	candidates, err := s.Store.ListCheckpointContextCandidates(r.Context(), id)
	if err != nil {
		log.Printf("handle requirement request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, candidates)
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

func (s *Server) requirementViews(r *http.Request, requirements []core.Requirement, includeDetail bool) ([]requirementView, error) {
	sessions := []core.PlanningSession{}
	var err error
	if includeDetail {
		sessions, err = s.Store.ListPlanningSessions(r.Context())
		if err != nil {
			return nil, err
		}
	}
	workspace, _ := store.WorkspaceFromContext(r.Context())
	// Staleness is an authority decision, not prompt rendering. Keep enough
	// fixed delivery-chain depth even when operators tune agent context lower.
	deliveryBudget := core.LineageTraversalBudget{MaxDepth: 3, MaxNodes: 256, MaxLinks: 1024, Workspace: workspace}
	artifactNodes := map[core.LineageNode]bool{}
	graphs := make(map[string]core.LineageTraversal, len(requirements))
	deliveryGraphs := make(map[string]core.LineageTraversal, len(requirements))
	lineageByRequirement := make(map[string][]core.LineageLink, len(requirements))
	deliveryLineageByRequirement := map[string][]core.LineageLink{}
	if !includeDetail {
		requirementIDs := make([]string, 0, len(requirements))
		for _, requirement := range requirements {
			requirementIDs = append(requirementIDs, requirement.ID)
		}
		deliveryLineageByRequirement, err = s.Store.ListRequirementDeliveryLineageByRequirement(r.Context(), requirementIDs, deliveryBudget)
		if err != nil {
			return nil, err
		}
	}
	for _, requirement := range requirements {
		root := core.LineageNode{Type: core.LineageRequirement, ID: requirement.ID}
		if includeDetail {
			lineageBudget := core.LineageTraversalBudget{MaxDepth: 5, MaxNodes: 256, MaxLinks: 1024, Workspace: workspace}
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
		deliveryLineage := deliveryLineageByRequirement[requirement.ID]
		if includeDetail {
			var deliveryErr error
			deliveryLineage, deliveryErr = s.Store.ListRequirementDeliveryLineage(r.Context(), requirement.ID, deliveryBudget)
			if deliveryErr != nil {
				return nil, deliveryErr
			}
		}
		deliveryGraph, deliveryErr := core.TraverseLineage(deliveryLineage, []core.LineageNode{root}, deliveryBudget)
		if deliveryErr != nil {
			return nil, deliveryErr
		}
		deliveryGraphs[root.ID] = deliveryGraph
	}
	nodes := make([]core.LineageNode, 0, len(artifactNodes))
	artifacts := []core.Artifact{}
	lineageLabels := map[core.LineageNode]string{}
	if includeDetail {
		for node := range artifactNodes {
			nodes = append(nodes, node)
		}
		artifacts, err = s.Store.ListArtifactsForLineage(r.Context(), nodes)
		if err != nil {
			return nil, err
		}
		lineageLabels, err = s.lineageNodeLabels(r, nodes, artifacts, sessions)
		if err != nil {
			return nil, err
		}
	}
	servesLinks := []core.RequirementServesLink{}
	if includeDetail {
		servesLinks, err = s.Store.ListRequirementServes(r.Context())
		if err != nil {
			return nil, err
		}
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
	if !includeDetail {
		taskSet := map[string]bool{}
		for _, requirement := range requirements {
			deliveryGraph := deliveryGraphs[requirement.ID]
			for taskID := range deliveryReachableTasks(deliveryGraph.Links, requirement.ID) {
				taskSet[taskID] = true
			}
			for taskID := range directServingTaskIDs(deliveryGraph.Links, requirement.ID) {
				taskSet[taskID] = true
			}
		}
		taskIDs := make([]string, 0, len(taskSet))
		for taskID := range taskSet {
			taskIDs = append(taskIDs, taskID)
		}
		sort.Strings(taskIDs)
		eventsByTask, err = s.Store.ListRequirementDeliveryEventsForTasks(r.Context(), taskIDs)
		if err != nil {
			return nil, err
		}
	}
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
	versionsByRequirement := map[string][]core.RequirementVersion{}
	requirementEventsByRequirement := map[string][]core.Event{}
	if !includeDetail {
		versionsByRequirement, err = s.Store.ListRequirementVersionsByRequirement(r.Context())
		if err != nil {
			return nil, err
		}
		requirementEventsByRequirement, err = s.Store.ListRequirementEventsByRequirement(r.Context())
		if err != nil {
			return nil, err
		}
	}
	tasksByID := map[string]core.Task{}
	if !includeDetail {
		tasks, listErr := s.Store.ListTasks(r.Context())
		if listErr != nil {
			return nil, listErr
		}
		for _, task := range tasks {
			tasksByID[task.ID] = task
		}
	}
	loadTask := func(taskID string) (core.Task, error) {
		if task, ok := tasksByID[taskID]; ok {
			return task, nil
		}
		task, getErr := s.Store.GetTask(r.Context(), taskID)
		if getErr == nil {
			tasksByID[taskID] = task
		}
		return task, getErr
	}
	views := make([]requirementView, 0, len(requirements))
	for _, requirement := range requirements {
		versions := versionsByRequirement[requirement.ID]
		requirementEvents := requirementEventsByRequirement[requirement.ID]
		if includeDetail {
			var listErr error
			versions, listErr = s.Store.ListRequirementVersions(r.Context(), requirement.ID)
			if listErr != nil {
				return nil, listErr
			}
			requirementEvents, listErr = s.Store.ListRequirementEvents(r.Context(), requirement.ID)
			if listErr != nil {
				return nil, listErr
			}
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
			Staleness:         requirementStaleness{Deliveries: []requirementDelivery{}, ActiveDrift: []monitor.Drift{}},
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
			if !version.Confirmed && !version.Retired {
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
		if includeDetail {
			view.Lineage = append(view.Lineage, annotateBackfilledEvents(requirementEvents)...)
		}
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
			task, getErr := loadTask(link.BlueprintTaskID)
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
		if includeDetail {
			graph := graphs[requirement.ID]
			applyLineageLabels(&graph, lineageLabels)
			view.LineageGraph = graph
		}
		deliveryGraph := deliveryGraphs[requirement.ID]
		view.Staleness.PartialEvaluation = deliveryGraph.Truncated
		reachableTasks := deliveryReachableTasks(deliveryGraph.Links, requirement.ID)
		deliveryServingTaskIDs := directServingTaskIDs(deliveryGraph.Links, requirement.ID)
		servingTaskIDs := deliveryServingTaskIDs
		if includeDetail {
			servingTaskIDs = directServingTaskIDs(lineageByRequirement[requirement.ID], requirement.ID)
		}
		for taskID := range servingTaskIDs {
			task, getErr := loadTask(taskID)
			if getErr != nil {
				return nil, getErr
			}
			if !core.BlueprintAnchor(task) {
				view.ServingTasks = append(view.ServingTasks, task)
			}
		}
		sort.Slice(view.ServingTasks, func(i, j int) bool { return view.ServingTasks[i].ID < view.ServingTasks[j].ID })
		effectiveBoundary := requirementAcknowledgedThrough(requirementEvents, confirmedAt)
		if !effectiveBoundary.At.IsZero() && !view.Staleness.PartialEvaluation {
			for taskID := range reachableTasks {
				events, eventsErr := loadTaskEvents(taskID)
				if eventsErr != nil {
					return nil, eventsErr
				}
				for _, delivery := range classifyRequirementDeliveries(taskID, events, versions, requirement.ID, effectiveBoundary, deliveryServingTaskIDs[taskID]) {
					if delivery.NeedsAttention {
						if followUp, found, lookupErr := s.Store.GetTaskByIntakeKey(r.Context(), requirementStalenessIntakeKey(delivery.SignalID)); lookupErr != nil {
							return nil, lookupErr
						} else if found && requirementFollowUpOpen(followUp.State) {
							delivery.FollowUp = &requirementFollowUp{TaskID: followUp.ID, Title: followUp.Title, State: followUp.State}
							delivery.NeedsAttention = false
						}
					}
					view.Staleness.Deliveries = append(view.Staleness.Deliveries, delivery)
					view.Staleness.DeliveryAfterIntent = view.Staleness.DeliveryAfterIntent || delivery.NeedsAttention
				}
			}
			sort.Slice(view.Staleness.Deliveries, func(i, j int) bool {
				if view.Staleness.Deliveries[i].At.Equal(view.Staleness.Deliveries[j].At) {
					return view.Staleness.Deliveries[i].TaskID < view.Staleness.Deliveries[j].TaskID
				}
				return view.Staleness.Deliveries[i].At.After(view.Staleness.Deliveries[j].At)
			})
		}
		for _, drift := range activeDrift {
			if drift.RequirementID == requirement.ID || reachableTasks[drift.TaskID] {
				view.Staleness.ActiveDrift = append(view.Staleness.ActiveDrift, drift)
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

// deliveryReachableTasks finds the delivery candidates that classification
// evaluates. It walks delivery edges at task level, so it follows `serves`
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

func classifyRequirementDeliveries(taskID string, events []core.Event, versions []core.RequirementVersion, requirementID string, after requirementDeliveryWatermark, directlyServing bool) []requirementDelivery {
	deliveries := []requirementDelivery{}
	for _, event := range events {
		if (event.Kind != "merge.confirmed" && event.Kind != "merge.reconciled") || !deliveryAfterWatermark(event, after) {
			continue
		}
		currentVersion := confirmedRequirementVersionAt(versions, event.At)
		pinnedVersion := taskRequirementVersionAt(events, versions, requirementID, event.At)
		reasons := []string{}
		if pinnedVersion > 0 && currentVersion > pinnedVersion {
			reasons = append(reasons, fmt.Sprintf("planned against v%d; v%d was current at merge", pinnedVersion, currentVersion))
		}
		if event.Kind == "merge.reconciled" {
			reasons = append(reasons, "merged outside factory review")
		}
		if !directlyServing {
			reasons = append(reasons, "delivered through related work without serving this requirement")
		}
		var payload struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(event.Payload, &payload)
		signalMaterial := fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%d\x00%s", requirementID, taskID, event.ID, pinnedVersion, currentVersion, strings.Join(reasons, "\x00"))
		signalID := fmt.Sprintf("%x", sha256.Sum256([]byte(signalMaterial)))
		deliveries = append(deliveries, requirementDelivery{
			SignalID: signalID, TaskID: taskID, DeliveryEventID: event.ID, EventKind: event.Kind,
			Label: deliveryLabel(event), URL: payload.URL, At: event.At,
			PinnedVersion: pinnedVersion, CurrentVersion: currentVersion,
			NeedsAttention: len(reasons) > 0, Reasons: reasons,
		})
	}
	return deliveries
}

func requirementAcknowledgedThrough(events []core.Event, confirmedAt time.Time) requirementDeliveryWatermark {
	boundary := requirementDeliveryWatermark{At: confirmedAt}
	for _, event := range events {
		if event.Kind != "requirement.staleness_acknowledged" {
			continue
		}
		var payload struct {
			AcknowledgedThrough time.Time `json:"acknowledged_through"`
			DeliveryEventID     int64     `json:"delivery_event_id"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil {
			continue
		}
		candidate := requirementDeliveryWatermark{At: payload.AcknowledgedThrough, EventID: payload.DeliveryEventID}
		if watermarkAfter(candidate, boundary) {
			boundary = candidate
		}
	}
	return boundary
}

func deliveryAfterWatermark(event core.Event, watermark requirementDeliveryWatermark) bool {
	return event.At.After(watermark.At) || (event.At.Equal(watermark.At) && event.ID > watermark.EventID)
}

func watermarkAfter(candidate, boundary requirementDeliveryWatermark) bool {
	return candidate.At.After(boundary.At) || (candidate.At.Equal(boundary.At) && candidate.EventID > boundary.EventID)
}

func requirementStalenessIntakeKey(signalID string) string {
	return "requirement-staleness:" + signalID
}

func requirementFollowUpOpen(state core.TaskState) bool {
	return state != core.TaskMerged && state != core.TaskClosed
}

func confirmedRequirementVersionAt(versions []core.RequirementVersion, at time.Time) int {
	current := 0
	var confirmedAt time.Time
	for _, version := range versions {
		if !version.Confirmed || version.ConfirmedAt.IsZero() || version.ConfirmedAt.After(at) {
			continue
		}
		if confirmedAt.IsZero() || version.ConfirmedAt.After(confirmedAt) || (version.ConfirmedAt.Equal(confirmedAt) && version.Version > current) {
			current, confirmedAt = version.Version, version.ConfirmedAt
		}
	}
	return current
}

func taskRequirementVersionAt(events []core.Event, versions []core.RequirementVersion, requirementID string, at time.Time) int {
	active, pinned := false, 0
	for _, event := range events {
		if event.At.After(at) {
			continue
		}
		var payload struct {
			ID          string `json:"id"`
			Version     int    `json:"version"`
			Unconfirmed bool   `json:"unconfirmed"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil || payload.ID != requirementID {
			continue
		}
		switch event.Kind {
		case store.TaskContextRequirementAdded:
			if payload.Unconfirmed {
				active, pinned = false, 0
				continue
			}
			active, pinned = true, payload.Version
			if pinned == 0 {
				pinned = confirmedRequirementVersionAt(versions, event.At)
			}
		case store.TaskContextRequirementActive:
			active, pinned = true, payload.Version
		case store.TaskContextRequirementRemoved:
			active, pinned = false, 0
		}
	}
	if !active {
		return 0
	}
	return pinned
}

func deliveryLabel(event core.Event) string {
	var payload map[string]any
	_ = json.Unmarshal(event.Payload, &payload)
	for _, key := range []string{"title", "task_title"} {
		if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
			return value
		}
	}
	if event.TaskID != "" {
		return event.TaskID
	}
	return "a serving delivery"
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
