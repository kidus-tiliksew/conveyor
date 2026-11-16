package store

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
)

type BoardOptions struct {
	Workspace  string
	Filter     TaskFilter
	Limit      int
	Offset     int
	NextAction NextActionOptions
}

type BoardOrder struct {
	ID    string `json:"id"`
	Stage string `json:"stage"`
	State string `json:"state"`
}

type BoardItem struct {
	ID                  string             `json:"id"`
	Title               string             `json:"title"`
	State               core.TaskState     `json:"state"`
	Stage               core.Stage         `json:"stage,omitempty"`
	CurrentOrder        *BoardOrder        `json:"current_order,omitempty"`
	PullRequest         int                `json:"pull_request,omitempty"`
	NextAction          nextaction.Compact `json:"next_action"`
	LastChange          *time.Time         `json:"last_change,omitempty"`
	StageElapsedSeconds *int64             `json:"stage_elapsed_seconds,omitempty"`
}

// MarshalJSON preserves the shared compact projection while omitting absent fields on the board.
func (item BoardItem) MarshalJSON() ([]byte, error) {
	type itemAlias BoardItem
	type compact struct {
		Label     string                  `json:"label"`
		Actor     string                  `json:"actor"`
		Kind      string                  `json:"kind"`
		Judgment  bool                    `json:"judgment"`
		Summary   string                  `json:"summary"`
		Deadline  *time.Time              `json:"deadline,omitempty"`
		BlockedBy []nextaction.BlockerRef `json:"blocked_by,omitempty"`
		Blocks    []nextaction.BlockerRef `json:"blocks,omitempty"`
	}
	a := item.NextAction
	return json.Marshal(struct {
		itemAlias
		NextAction compact `json:"next_action"`
	}{itemAlias(item), compact{a.Label, a.Actor, a.Kind, a.Judgment, a.Summary, a.Deadline, a.BlockedBy, a.Blocks}})
}

type Board struct {
	Items []BoardItem `json:"items"`
	Total int         `json:"total"`
}

func ReadBoard(ctx context.Context, st Store, opts BoardOptions) (Board, error) {
	if opts.Workspace != "" {
		ctx = WithWorkspace(ctx, opts.Workspace)
	}
	if opts.Limit == 0 || opts.Limit > 100 {
		opts.Limit = 100
	}
	if opts.Limit < 1 || opts.Offset < 0 {
		return Board{}, fmt.Errorf("board limit must be positive and offset must be nonnegative")
	}
	filter := opts.Filter
	filter.Open = true
	filter.ExcludeBlueprintAnchors = true
	all, err := st.ListTasksFilteredWithoutBody(ctx, TaskFilter{})
	if err != nil {
		return Board{}, err
	}
	tasks, err := st.ListTasksFilteredWithoutBody(ctx, filter)
	if err != nil {
		return Board{}, err
	}
	workspace, scoped := WorkspaceFromContext(ctx)
	if scoped && workspace != "" {
		filtered := tasks[:0]
		for _, task := range tasks {
			if task.Workspace == workspace {
				filtered = append(filtered, task)
			}
		}
		tasks = filtered
	}
	result := Board{Items: []BoardItem{}, Total: len(tasks)}
	if opts.Offset >= len(tasks) {
		return result, nil
	}
	end := opts.Offset + min(len(tasks)-opts.Offset, opts.Limit)
	tasks = tasks[opts.Offset:end]
	ids := make([]string, len(tasks))
	for i, task := range tasks {
		ids[i] = task.ID
	}
	orders, err := st.ListWorkOrdersForTasks(ctx, ids)
	if err != nil {
		return Board{}, err
	}
	markers, err := st.ListActivityMarkersForTasks(ctx, ids)
	if err != nil {
		return Board{}, err
	}
	pulls, err := st.ListLatestPullRequestEventsForTasks(ctx, ids)
	if err != nil {
		return Board{}, err
	}
	actionOpts := opts.NextAction
	actionOpts.OrdersLoaded, actionOpts.Orders = true, orders
	actionOpts.MarkersLoaded, actionOpts.Markers = true, markers
	actionOpts.OpenTasksLoaded, actionOpts.OpenTasks = true, all
	actionOpts.CompactOnly = true
	actions, err := ReadNextActions(ctx, st, tasks, actionOpts)
	if err != nil {
		return Board{}, err
	}
	slices.SortFunc(orders, func(a, b core.WorkOrder) int {
		if n := a.CreatedAt.Compare(b.CreatedAt); n != 0 {
			return n
		}
		return strings.Compare(a.ID, b.ID)
	})
	latest := make(map[string]core.WorkOrder)
	stageSince := make(map[string]time.Time)
	for _, order := range orders {
		old, ok := latest[order.TaskID]
		if !ok || old.Stage != order.Stage {
			stageSince[order.TaskID] = order.CreatedAt
		}
		latest[order.TaskID] = order
	}
	markerByID := make(map[string]ActivityMarker, len(markers))
	for _, marker := range markers {
		markerByID[marker.TaskID] = marker
	}
	for _, task := range tasks {
		title := []rune(task.Title)
		if len(title) > 80 {
			title = title[:80]
		}
		action := actions[task.ID]
		item := BoardItem{ID: task.ID, Title: string(title), State: task.State, Stage: latest[task.ID].Stage, NextAction: nextaction.CompactFrom(action)}
		if since := stageSince[task.ID]; !since.IsZero() {
			now := opts.NextAction.Now
			if now.IsZero() {
				now = time.Now().UTC()
			}
			seconds := max(int64(now.Sub(since).Seconds()), 0)
			item.StageElapsedSeconds = &seconds
		}
		if action.CurrentOrder != nil {
			item.CurrentOrder = &BoardOrder{ID: action.CurrentOrder.ID, Stage: action.CurrentOrder.Stage, State: action.CurrentOrder.State}
		}
		at := markerByID[task.ID].LastEventAt
		if at.IsZero() {
			at = task.CreatedAt
		}
		if !at.IsZero() {
			item.LastChange = &at
		}
		if event, ok := pulls[task.ID]; ok {
			var payload struct {
				Number int `json:"number"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return Board{}, err
			}
			item.PullRequest = payload.Number
		}
		result.Items = append(result.Items, item)
		encoded, err := json.Marshal(result)
		if err != nil {
			return Board{}, err
		}
		if len(encoded) >= 32768 {
			result.Items = result.Items[:len(result.Items)-1]
			if len(result.Items) == 0 {
				return Board{}, fmt.Errorf("board item exceeds 32 KiB")
			}
			break
		}
	}
	return result, nil
}
