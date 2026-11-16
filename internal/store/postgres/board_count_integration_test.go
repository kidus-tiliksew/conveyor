package postgres

import (
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
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
	for _, state := range []core.TaskState{core.TaskMerged, core.TaskClosed} {
		id := core.NewTaskID()
		if err := st.CreateTask(ctx, core.Task{ID: id, Workspace: "board-count", Title: "terminal", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, State: state, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	for _, size := range []int{10, 100} {
		start := 0
		if size == 100 {
			start = 10
		}
		storetest.SeedBoardCount(t, ctx, st, start, size, now)
		st.ReadStats(0)
		collector := dbmetrics.NewCollector()
		board, err := store.ReadBoard(dbmetrics.WithCollector(ctx, collector), st, store.BoardOptions{Limit: 100, NextAction: store.NextActionOptions{Now: now}})
		if err != nil {
			t.Fatal(err)
		}
		if len(board.Items) != size || board.Total != size {
			t.Fatalf("size %d: items=%d total=%d", size, len(board.Items), board.Total)
		}
		stats := st.ReadStats(100)
		wantRows := map[int]uint64{10: 49, 100: 490}
		if stats.Rows != wantRows[size] || collector.Snapshot().Queries != 25 {
			t.Errorf("size %d: rows=%d statements=%d, want rows=%d statements=25", size, stats.Rows, collector.Snapshot().Queries, wantRows[size])
		}
		labels := make(map[string]bool)
		for _, entry := range stats.Top {
			labels[entry.Label] = entry.Queries > 0
		}
		for _, label := range strings.Fields("ListRiskRatingsForTasks ListClaimEventsForTasks ListTaskApprovalsForTasks AuthorizeWorkspaceUsers ListRiskOrdersForTasks ListLatestPullRequestEventsForTasks") {
			if !labels[label] {
				t.Errorf("size %d: missing batched read %s; labels=%v", size, label, stats.Top)
			}
		}
		t.Logf("board size=%d rows=%d statements=%d", size, stats.Rows, collector.Snapshot().Queries)
	}
}
