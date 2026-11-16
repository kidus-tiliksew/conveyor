package storetest

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func runBoardConformance(t *testing.T, x Fixture) {
	ctx, st := x.Context, x.Backend
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	opts := store.NextActionOptions{Now: now, Capabilities: map[core.Capability]bool{core.CapabilityOperateGates: true, core.CapabilityConfirmDocuments: true, core.CapabilityRecoverWork: true}}
	expected := make([]core.Task, 0, 500)
	for i := range 502 {
		id := fixtureTaskID()
		state := core.TaskQueued
		if i == 500 {
			state = core.TaskMerged
		}
		if i == 501 {
			state = core.TaskClosed
		}
		task := core.Task{ID: id, Workspace: x.Workspace, Title: fmt.Sprintf("%03d ", i) + strings.Repeat("𐐀", 100), Repo: "app", BaseBranch: "main", Branch: "conveyor/task-" + id, State: state, NextStage: core.StageImplement, CreatedAt: now.Add(time.Duration(i/2) * time.Second)}
		requireOK(t, st.CreateTask(ctx, task))
		if i < 500 {
			expected = append(expected, task)
		}
	}
	anchorID := fixtureTaskID()
	requireOK(t, st.CreateTask(ctx, core.Task{ID: anchorID, Workspace: x.Workspace, Title: "blueprint anchor", Repo: "app", BaseBranch: "main", Branch: "conveyor/task-" + anchorID, State: core.TaskQueued, CreatedAt: now}))
	childID := fixtureTaskID()
	requireOK(t, st.CreateTask(ctx, core.Task{ID: childID, Workspace: x.Workspace, Title: "closed child", Repo: "app", BaseBranch: "main", Branch: "conveyor/task-" + childID, ParentTaskID: anchorID, State: core.TaskClosed, CreatedAt: now}))
	slices.SortFunc(expected, func(a, b core.Task) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	offset := 0
	for offset < len(expected) {
		board, err := store.ReadBoard(ctx, st, store.BoardOptions{Offset: offset, Limit: 1000, NextAction: opts})
		requireOK(t, err)
		if board.Total != len(expected) {
			t.Fatalf("total %d, want %d", board.Total, len(expected))
		}
		if len(board.Items) == 0 || len(board.Items) > 100 {
			t.Fatalf("page length %d", len(board.Items))
		}
		if offset == 0 && len(board.Items) == 100 {
			t.Fatal("fixture did not exercise the byte limit before the item limit")
		}
		encoded, err := json.Marshal(board)
		requireOK(t, err)
		if len(encoded) >= 32768 {
			t.Fatalf("page has %d bytes", len(encoded))
		}
		for i, item := range board.Items {
			want := expected[offset+i]
			if item.ID != want.ID {
				t.Fatalf("item %d = %s, want %s", offset+i, item.ID, want.ID)
			}
			if item.Title != string([]rune(want.Title)[:80]) {
				t.Errorf("title %s not cut to 80 runes", item.ID)
			}
			task, err := st.GetTask(ctx, item.ID)
			requireOK(t, err)
			orders, err := st.ListTaskWorkOrders(ctx, item.ID)
			requireOK(t, err)
			detail, err := store.ReadNextActionForTask(ctx, st, task, orders, opts)
			requireOK(t, err)
			if wantAction := nextaction.CompactFrom(detail); !reflect.DeepEqual(item.NextAction, wantAction) {
				t.Errorf("task %s board action %+v, detail %+v", item.ID, item.NextAction, wantAction)
			}
			raw, err := json.Marshal(item)
			requireOK(t, err)
			var obj map[string]json.RawMessage
			requireOK(t, json.Unmarshal(raw, &obj))
			var action map[string]json.RawMessage
			requireOK(t, json.Unmarshal(obj["next_action"], &action))
			for _, field := range []string{"deadline", "blocked_by", "blocks"} {
				if _, ok := action[field]; ok {
					t.Errorf("empty next_action.%s included for %s", field, item.ID)
				}
			}
			for _, field := range []string{"stage", "current_order", "pull_request", "stage_elapsed_seconds"} {
				if _, ok := obj[field]; ok {
					t.Errorf("empty %s included for %s", field, item.ID)
				}
			}
		}
		offset += len(board.Items)
	}
}
