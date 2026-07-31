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
	// GoverningVersion is the spec version the children were materialized
	// from. The children themselves record it, so it stays authoritative even
	// if a later draft version exists on the anchor.
	GoverningVersion int `json:"governing_version"`
	// Children are the materialized child tasks in dependency order.
	Children  []blueprintChild          `json:"children"`
	Delivery  blueprintDelivery         `json:"delivery"`
	Serves    []blueprintRequirementRef `json:"serves"`
	Events    []core.Event              `json:"events"`
	Artifacts []core.Artifact           `json:"artifacts"`
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

// blueprintRequirementRef is a confirmed serves link rendered as a reference.
// Until the §21.46 6.3 links table exists, the durable evidence that a
// blueprint serves a requirement is the planning session it was produced in
// (the same relation requirementView.ServingBlueprints reads forward).
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
	views := make([]blueprintView, 0, len(anchors))
	for _, task := range anchors {
		view := blueprintView{
			Task:      task,
			Children:  []blueprintChild{},
			Serves:    served[task.ID],
			Events:    []core.Event{},
			Artifacts: []core.Artifact{},
		}
		if view.Serves == nil {
			view.Serves = []blueprintRequirementRef{}
		}
		events, eventsErr := s.Store.ListEvents(r.Context(), task.ID)
		if eventsErr != nil {
			return nil, eventsErr
		}
		view.Events = append(view.Events, events...)
		spec, exists, specErr := s.Store.GetLatestSpecVersion(r.Context(), task.ID)
		if specErr != nil {
			return nil, specErr
		}
		var governing *core.SpecVersion
		if exists {
			view.Spec = &spec
		}
		governing, view.GoverningVersion = governingSpec(view.Spec, events)
		if governing != nil {
			// Reused children keep the origin version that created them, so
			// the governing decomposition claims them by sub id — matching on
			// version would leave a revision's own children unlinked.
			governing.MaterializedChildren = childrenForDecomposition(task.Children, decompositionItems(governing))
		}
		view.Children = blueprintChildren(task.Children, governing)
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

// governingSpec is the approved blueprint the anchor's children answer to,
// with its version. It cannot be derived from the children: materialization
// reuses an existing child whenever a revision keeps its sub id, so a child
// created at v1 still serves an approved v3 and the newest child origin can
// name a version that never governed anything.
//
// Approval only ever lands on the newest spec version, so the latest version
// governs whenever it is approved. A newer unapproved draft is a proposal
// that has materialized nothing; the version that did is the last one the
// spec.version_approved audit trail records, and the projection reports that
// number even though this checkout cannot read that older body.
func governingSpec(spec *core.SpecVersion, events []core.Event) (*core.SpecVersion, int) {
	if spec != nil && spec.Approved {
		return spec, spec.Version
	}
	approved := 0
	for _, event := range events {
		if event.Kind != "spec.version_approved" {
			continue
		}
		var payload struct {
			Version int `json:"version"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err == nil && payload.Version > 0 {
			approved = payload.Version
		}
	}
	return nil, approved
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

// servedRequirements inverts the requirement → serving-blueprint relation:
// a session opened in a requirement's context and finalized into a blueprint
// is that blueprint's confirmed serves link. Absence is a normal empty state.
func (s *Server) servedRequirements(r *http.Request) (map[string][]blueprintRequirementRef, error) {
	sessions, err := s.Store.ListPlanningSessions(r.Context())
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
	seen := map[string]bool{}
	for _, session := range sessions {
		if session.RequirementContextID == "" || session.ProducedTaskID == "" {
			continue
		}
		requirement, exists := byID[session.RequirementContextID]
		if !exists || seen[session.ProducedTaskID+"/"+requirement.ID] {
			continue
		}
		seen[session.ProducedTaskID+"/"+requirement.ID] = true
		served[session.ProducedTaskID] = append(served[session.ProducedTaskID], blueprintRequirementRef{
			ID: requirement.ID, Slug: requirement.Slug, Title: requirement.Title,
		})
	}
	return served, nil
}
