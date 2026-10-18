package httpapi

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// taskMapResponse is the workspace task map envelope (req-board-map AC-3.1;
// component-http-api). It carries no task body, policy contract, child
// rollup, plan, governance document, or event list.
type taskMapResponse struct {
	Tasks        []taskMapItem        `json:"tasks"`
	Requirements []taskMapRequirement `json:"requirements"`
}

// taskMapItem is one map node: identity, the stated compact next action,
// dependency relations, and served requirements only.
type taskMapItem struct {
	ID                   string                  `json:"id"`
	Title                string                  `json:"title"`
	Repo                 string                  `json:"repo"`
	State                core.TaskState          `json:"state"`
	LastEventAt          time.Time               `json:"last_event_at"`
	NextAction           nextaction.Compact      `json:"next_action"`
	DependsOn            []string                `json:"depends_on"`
	BlockingTaskIDs      []string                `json:"blocking_task_ids"`
	UnsatisfiableTaskIDs []string                `json:"unsatisfiable_task_ids"`
	Requirements         []taskMapRequirementRef `json:"requirements"`
}

type taskMapRequirementRef struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// taskMapRequirement is one workspace-wide served-requirement aggregate. The
// map's progress ring is merged_task_count / total_task_count; closed tasks
// count in neither (req-board-map AC-1.3).
type taskMapRequirement struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	OpenTaskCount   int    `json:"open_task_count"`
	MergedTaskCount int    `json:"merged_task_count"`
	TotalTaskCount  int    `json:"total_task_count"`
}

// parseTaskMapQuery reads the shared repository filter, with its repeated
// values and repo alias, and the include_finished Boolean. A malformed
// filter or Boolean is rejected rather than dropped.
func parseTaskMapQuery(values url.Values) (store.TaskMapQuery, error) {
	filter, err := parseTaskFilter(values)
	if err != nil {
		return store.TaskMapQuery{}, err
	}
	query := store.TaskMapQuery{Repositories: filter.Repositories}
	if values.Has("include_finished") {
		query.IncludeFinished, err = strconv.ParseBool(values.Get("include_finished"))
		if err != nil {
			return store.TaskMapQuery{}, fmt.Errorf("include_finished must be a Boolean")
		}
	}
	return query, nil
}

// getTaskMap serves the Board map's one workspace-wide read
// (req-board-map AC-1.2, AC-1.3, AC-1.7, AC-2.1-AC-2.4, AC-3.1, AC-3.4;
// component-http-api). The store answers the nodes, edges, served
// requirements, open set, and aggregates in a fixed number of set-based
// queries; markers, blockers, and the shared next-action projection add a
// fixed number more, independent of task count. The next action is
// nextaction.CompactFrom over store.ReadNextActions, the projection every
// other surface states (DEC-61). The response is never paginated.
func (s *Server) getTaskMap(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query, err := parseTaskMapQuery(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	taskMap, err := s.Store.ListTaskMap(ctx, query)
	if err != nil {
		log.Printf("read task map: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	response := taskMapResponse{Tasks: make([]taskMapItem, 0, len(taskMap.Tasks)), Requirements: make([]taskMapRequirement, 0, len(taskMap.Requirements))}
	for _, requirement := range taskMap.Requirements {
		response.Requirements = append(response.Requirements, taskMapRequirement{
			ID: requirement.ID, Title: requirement.Title, OpenTaskCount: requirement.OpenTaskCount,
			MergedTaskCount: requirement.MergedTaskCount, TotalTaskCount: requirement.TotalTaskCount,
		})
	}
	if len(taskMap.Tasks) == 0 {
		writeJSON(w, http.StatusOK, response)
		return
	}
	tasks := make([]core.Task, len(taskMap.Tasks))
	ids := make([]string, len(taskMap.Tasks))
	for i, node := range taskMap.Tasks {
		tasks[i], ids[i] = node.Task, node.Task.ID
	}
	markers, err := s.Store.ListActivityMarkersForTasks(ctx, ids)
	if err != nil {
		log.Printf("read task map markers: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	opts, err := s.nextActionOptions(r)
	if err != nil {
		log.Printf("read task map next-action options: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	opts.MarkersLoaded, opts.Markers = true, markers
	opts.OpenTasksLoaded, opts.OpenTasks = true, taskMap.OpenTasks
	opts.CompactOnly = true
	projected, err := store.ReadNextActions(ctx, s.Store, tasks, opts)
	if err != nil {
		log.Printf("read task map next actions: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	blockers, err := s.Store.ListDependencyBlockers(ctx, ids)
	if err != nil {
		log.Printf("read task map blockers: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	lastEvent := make(map[string]time.Time, len(markers))
	for _, marker := range markers {
		lastEvent[marker.TaskID] = marker.LastEventAt
	}
	for _, node := range taskMap.Tasks {
		task := node.Task
		// Activity markers fall back to creation time for a task without
		// events; the same fallback applies if a marker is absent.
		at, ok := lastEvent[task.ID]
		if !ok || at.IsZero() {
			at = task.CreatedAt
		}
		refs := make([]taskMapRequirementRef, 0, len(node.Requirements))
		for _, requirement := range node.Requirements {
			refs = append(refs, taskMapRequirementRef{ID: requirement.ID, Title: requirement.Title})
		}
		response.Tasks = append(response.Tasks, taskMapItem{
			ID: task.ID, Title: task.Title, Repo: task.Repo, State: task.State, LastEventAt: at,
			NextAction:           nextaction.CompactFrom(projected[task.ID]),
			DependsOn:            append([]string{}, node.DependsOn...),
			BlockingTaskIDs:      append([]string{}, blockers[task.ID].BlockingTaskIDs...),
			UnsatisfiableTaskIDs: append([]string{}, blockers[task.ID].UnsatisfiableTaskIDs...),
			Requirements:         refs,
		})
	}
	// Newest activity first, the Board's last-event order; IDs break ties.
	sort.SliceStable(response.Tasks, func(i, j int) bool {
		if !response.Tasks[i].LastEventAt.Equal(response.Tasks[j].LastEventAt) {
			return response.Tasks[i].LastEventAt.After(response.Tasks[j].LastEventAt)
		}
		return response.Tasks[i].ID < response.Tasks[j].ID
	})
	writeJSON(w, http.StatusOK, response)
}
