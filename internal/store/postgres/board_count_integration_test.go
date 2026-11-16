package postgres

import (
	"fmt"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

func TestBoardStatementCountIntegration(t *testing.T) {
	st := openServerTimingStore(t, serverTimingSchemaURL(t), nil)
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "board-count")
	if _, err := st.BootstrapIdentity(ctx, config.FirstOperatorIdentity{OrganizationName: "Timing", Email: "owner@example.test", DisplayName: "Owner"}, serverTimingToken); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: "board-count", Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var counts []uint64
	for _, size := range []int{10, 100} {
		start := 0
		if size == 100 {
			start = 10
		}
		for i := start; i < size; i++ {
			id := core.NewTaskID()
			state := core.TaskQueued
			if i%2 == 1 {
				state = core.TaskParked
			}
			task := core.Task{ID: id, Workspace: "board-count", Title: fmt.Sprintf("task %d", i), Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, State: state, NextStage: core.StageImplement, CreatedAt: now}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
		}
		st.ReadStats(0)
		collector := dbmetrics.NewCollector()
		board, err := store.ReadBoard(dbmetrics.WithCollector(ctx, collector), st, store.BoardOptions{Limit: 100, NextAction: store.NextActionOptions{Now: now}})
		if err != nil {
			t.Fatal(err)
		}
		if len(board.Items) != size || board.Total != size {
			t.Fatalf("size %d: items=%d total=%d", size, len(board.Items), board.Total)
		}
		kinds := map[string]int{}
		for _, item := range board.Items {
			kinds[string(item.NextAction.Kind)]++
		}
		if len(kinds) != 2 {
			t.Fatalf("size %d: expected two next-action kinds, got %v", size, kinds)
		}
		for _, count := range kinds {
			if count != size/2 {
				t.Fatalf("size %d: expected an even kind mix, got %v", size, kinds)
			}
		}
		rows := st.ReadStats(0).Rows
		if rows != uint64(4*size) {
			t.Fatalf("size %d: database rows=%d, want %d", size, rows, 4*size)
		}
		counts = append(counts, collector.Snapshot().Queries)
		t.Logf("board size=%d items=%d database rows=%d statements=%d", size, len(board.Items), rows, counts[len(counts)-1])
	}
	if counts[0] == 0 || counts[0] != counts[1] {
		t.Fatalf("board statements: 10=%d 100=%d; want equal nonzero counts", counts[0], counts[1])
	}
}
