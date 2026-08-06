package httpapi

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// blueprintView is the dashboard read model for one blueprint anchor
// (spec §21.49). The anchor is presentation, not a persisted entity: every
// field below is derived from the parent task, its approved spec, and the
// children that spec's decomposition materialized. Delivery is reported in
// blueprint vocabulary so the surface never leaks a raw pipeline state.
type blueprintView struct {
	Task core.Task         `json:"task"`
	Spec *core.SpecVersion `json:"spec,omitempty"`
	// GoverningVersion is the approved spec version delivery answers to. It
	// cannot be read off the children: materialization reuses an existing
	// child whenever a revision keeps its sub id, so a child created at v1
	// still serves an approved v3, and the newest child origin can name a
	// version that never governed anything. A later unapproved draft has
	// materialized nothing and never displaces it.
	GoverningVersion int `json:"governing_version"`
	// Children are the materialized child tasks in dependency order.
	Children         []blueprintChild             `json:"children"`
	Delivery         blueprintDelivery            `json:"delivery"`
	Serves           []blueprintRequirementRef    `json:"serves"`
	RequirementLinks []core.RequirementServesLink `json:"requirement_links"`
	Events           []core.Event                 `json:"events"`
	Artifacts        []core.Artifact              `json:"artifacts"`
	PlanningSession  *core.PlanningSession        `json:"planning_session,omitempty"`
}

// blueprintChild pairs a materialized child with the decomposition item that
// produced it, so the surface can show declared dependencies without
// re-deriving them from the child's own dependency edges.
type blueprintChild struct {
	core.TaskRelation
	Repo      string   `json:"repo,omitempty"`
	Summary   string   `json:"summary,omitempty"`
	DependsOn []string `json:"depends_on,omitempty"`
}

// blueprintDeliveryState is the anchor's lifecycle in blueprint vocabulary.
type blueprintDeliveryState string

const (
	blueprintInDelivery blueprintDeliveryState = "in_delivery"
	blueprintCompleted  blueprintDeliveryState = "completed"
	blueprintCancelled  blueprintDeliveryState = "cancelled"
)

// blueprintDelivery keeps merged and closed as separate quantities: a child
// that closed without merging did not deliver, and a rollup that folds the
// two together would overstate progress.
type blueprintDelivery struct {
	State  blueprintDeliveryState `json:"state"`
	Total  int                    `json:"total"`
	Merged int                    `json:"merged"`
	Closed int                    `json:"closed"`
	Open   int                    `json:"open"`
}

// blueprintRequirementRef is an operator-confirmed serves link rendered as a
// compact requirement reference. Proposal history is exposed separately.
type blueprintRequirementRef struct {
	ID    string `json:"id"`
	Slug  string `json:"slug"`
	Title string `json:"title"`
}

func (s *Server) listBlueprints(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.Store.ListTasks(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	anchors := make([]core.Task, 0)
	for _, task := range tasks {
		if core.BlueprintAnchor(task) {
			anchors = append(anchors, task)
		}
	}
	sort.SliceStable(anchors, func(i, j int) bool {
		return anchors[i].CreatedAt.After(anchors[j].CreatedAt)
	})
	views, err := s.blueprintViews(r, anchors)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) blueprintViews(r *http.Request, anchors []core.Task) ([]blueprintView, error) {
	if len(anchors) == 0 {
		return []blueprintView{}, nil
	}
	served, err := s.servedRequirements(r)
	if err != nil {
		return nil, err
	}
	artifacts, err := s.Store.ListArtifacts(r.Context())
	if err != nil {
		return nil, err
	}
	sessions, err := s.Store.ListPlanningSessions(r.Context())
	if err != nil {
		return nil, err
	}
	links, err := s.Store.ListRequirementServes(r.Context())
	if err != nil {
		return nil, err
	}
	linksByTask := make(map[string][]core.RequirementServesLink)
	for _, link := range links {
		linksByTask[link.BlueprintTaskID] = append(linksByTask[link.BlueprintTaskID], link)
	}
	sessionByTask := make(map[string]core.PlanningSession, len(sessions))
	for _, session := range sessions {
		if session.ProducedTaskID != "" {
			sessionByTask[session.ProducedTaskID] = session
		}
	}
	views := make([]blueprintView, 0, len(anchors))
	for _, task := range anchors {
		view := blueprintView{
			Task:             task,
			Children:         []blueprintChild{},
			Serves:           served[task.ID],
			Events:           []core.Event{},
			Artifacts:        []core.Artifact{},
			RequirementLinks: linksByTask[task.ID],
		}
		if view.Serves == nil {
			view.Serves = []blueprintRequirementRef{}
		}
		if view.RequirementLinks == nil {
			view.RequirementLinks = []core.RequirementServesLink{}
		}
		if session, exists := sessionByTask[task.ID]; exists {
			session := session
			view.PlanningSession = &session
		}
		events, eventsErr := s.Store.ListEvents(r.Context(), task.ID)
		if eventsErr != nil {
			return nil, eventsErr
		}
		view.Events = append(view.Events, events...)
		governing, exists, specErr := s.Store.GetApprovedSpecVersion(r.Context(), task.ID)
		if specErr != nil {
			return nil, specErr
		}
		if exists {
			// Reused children keep the origin version that created them, so
			// the governing decomposition claims them by sub id — matching on
			// version would leave a revision's own children unlinked.
			governing.MaterializedChildren = childrenForDecomposition(task.Children, decompositionItems(&governing))
			view.Spec, view.GoverningVersion = &governing, governing.Version
		}
		view.Children = blueprintChildren(task.Children, view.Spec)
		for _, artifact := range artifacts {
			if artifact.TaskID != task.ID {
				continue
			}
			artifact.DownloadURL = "/v1/artifacts/" + artifact.ID
			view.Artifacts = append(view.Artifacts, artifact)
		}
		view.Delivery = blueprintDeliveryOf(task, events)
		views = append(views, view)
	}
	return views, nil
}

// childrenForDecomposition claims the materialized children the governing
// decomposition names, by sub id rather than by origin version.
func childrenForDecomposition(children []core.TaskRelation, items []core.BlueprintDecompositionItem) []core.TaskRelation {
	bySub := childrenBySubID(children)
	claimed := make([]core.TaskRelation, 0, len(items))
	for _, item := range items {
		if child, exists := bySub[item.ID]; exists {
			claimed = append(claimed, child)
		}
	}
	return claimed
}

// childrenBySubID indexes children by the decomposition item they deliver.
// The store never materializes two children for one sub id, but children
// arrive sorted by origin version, so keeping the first entry preserves the
// same earliest-wins tie-break the store itself applies when reusing them.
func childrenBySubID(children []core.TaskRelation) map[string]core.TaskRelation {
	bySub := make(map[string]core.TaskRelation, len(children))
	for _, child := range children {
		if child.OriginSubID == "" {
			continue
		}
		if _, exists := bySub[child.OriginSubID]; !exists {
			bySub[child.OriginSubID] = child
		}
	}
	return bySub
}

// blueprintChildren orders the materialized children by the governing
// decomposition's dependency order, so a child never renders above something
// it waits on, and pairs each with the governing item's current repo,
// summary, and declared dependencies. A child the governing decomposition
// does not name — one whose sub id a later revision dropped — keeps its
// stored order at the end rather than disappearing.
func blueprintChildren(children []core.TaskRelation, governing *core.SpecVersion) []blueprintChild {
	bySub := childrenBySubID(children)
	ordered := make([]blueprintChild, 0, len(children))
	placed := make(map[string]bool, len(children))
	for _, item := range core.OrderDecompositionByDependency(decompositionItems(governing)) {
		child, exists := bySub[item.ID]
		if !exists || placed[child.ID] {
			continue
		}
		placed[child.ID] = true
		ordered = append(ordered, blueprintChild{
			TaskRelation: child, Repo: item.Repo, Summary: item.Summary, DependsOn: item.DependsOn,
		})
	}
	for _, child := range children {
		if !placed[child.ID] {
			ordered = append(ordered, blueprintChild{TaskRelation: child})
		}
	}
	return ordered
}

func decompositionItems(spec *core.SpecVersion) []core.BlueprintDecompositionItem {
	if spec == nil || len(spec.Decomposition) == 0 {
		return nil
	}
	var items []core.BlueprintDecompositionItem
	if err := json.Unmarshal(spec.Decomposition, &items); err != nil {
		return nil
	}
	return items
}

// blueprintDeliveryOf reports the anchor in blueprint vocabulary. The store
// closes an anchor two ways: blueprint closure once every child is terminal,
// and operator cancellation. Only the first is delivery completing, so the
// blueprint.closed audit event — not the task state — distinguishes them.
func blueprintDeliveryOf(task core.Task, events []core.Event) blueprintDelivery {
	delivery := blueprintDelivery{State: blueprintInDelivery, Total: len(task.Children)}
	for _, child := range task.Children {
		switch child.State {
		case core.TaskMerged:
			delivery.Merged++
		case core.TaskClosed:
			delivery.Closed++
		default:
			delivery.Open++
		}
	}
	if !core.TaskTerminal(task.State) {
		return delivery
	}
	delivery.State = blueprintCancelled
	for _, event := range events {
		if event.Kind == "blueprint.closed" {
			delivery.State = blueprintCompleted
			break
		}
	}
	return delivery
}

// servedRequirements renders only operator-confirmed links as authoritative.
// Proposed and dismissed links remain available through RequirementLinks.
func (s *Server) servedRequirements(r *http.Request) (map[string][]blueprintRequirementRef, error) {
	links, err := s.Store.ListRequirementServes(r.Context())
	if err != nil {
		return nil, err
	}
	requirements, err := s.Store.ListRequirements(r.Context())
	if err != nil {
		return nil, err
	}
	byID := make(map[string]core.Requirement, len(requirements))
	for _, requirement := range requirements {
		byID[requirement.ID] = requirement
	}
	served := map[string][]blueprintRequirementRef{}
	for _, link := range links {
		if link.State != core.RequirementServesConfirmed {
			continue
		}
		requirement, exists := byID[link.RequirementID]
		if !exists {
			continue
		}
		served[link.BlueprintTaskID] = append(served[link.BlueprintTaskID], blueprintRequirementRef{
			ID: requirement.ID, Slug: requirement.Slug, Title: requirement.Title,
		})
	}
	return served, nil
}
