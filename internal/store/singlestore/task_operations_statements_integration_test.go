package singlestore

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/s2log"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestTaskOperationsStatementBoundIntegration proves the Tasks page read issues
// the same number of statements for 10 and 100 rows: the operations events,
// the latest plan metadata, and the legacy gate markers are read for the page
// in one statement each, so the read count does not grow with the page
// (component-persistence, Database read accounting; req-operator-next-action
// AC-2.7).
func TestTaskOperationsStatementBoundIntegration(t *testing.T) {
	st := integrationStore(t)
	connector, err := mysql.NewConnector(sharedDatabase.cfg)
	taskTestOK(t, err)
	recorder := &selectedBytesRecorder{}
	db := sql.OpenDB(recordingConnector{Connector: connector, recorder: recorder})
	t.Cleanup(func() { _ = db.Close() })
	recorded := &Store{db: db, log: s2log.New(db)}
	statements := func(size int) int {
		t.Helper()
		workspace := fmt.Sprintf("task-operations-statements-%d", size)
		ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), workspace)
		if _, err := st.CreateWorkspace(ctx, workspace, workspace, &config.Config{Workspace: workspace}); err != nil {
			t.Fatal(err)
		}
		for i := range size {
			id := fmt.Sprintf("%s-task-%03d", workspace, i)
			taskTestOK(t, st.CreateTask(ctx, core.Task{ID: id, Workspace: workspace, Branch: "conveyor/" + id, Title: id, State: core.TaskQueued, NextStage: core.StageTriage}))
			if _, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: id, Content: "# Plan for " + id}); err != nil {
				t.Fatal(err)
			}
			taskTestOK(t, st.AppendEvent(ctx, core.Event{TaskID: id, Kind: store.TaskContextRequirementAdded, Payload: core.JSONPayload(map[string]any{"id": "req-statement-bound"})}))
		}
		recorder.start()
		page, err := recorded.ListTaskOperations(ctx, store.TaskOperationsQuery{Limit: 200})
		queries, _ := recorder.stop()
		taskTestOK(t, err)
		if len(page.Tasks) != size || len(page.Plans) != size || len(page.Events) != size {
			t.Fatalf("page of %d rows read %d tasks, %d plans, %d event lists", size, len(page.Tasks), len(page.Plans), len(page.Events))
		}
		// A list read states plan metadata and no plan text.
		for id, plan := range page.Plans {
			if plan.Content != "" || len(plan.Acceptance) != 0 || len(plan.Decomposition) != 0 {
				t.Fatalf("plan %s carried text on a list read", id)
			}
		}
		return len(queries)
	}
	small, large := statements(10), statements(100)
	if small != large {
		t.Fatalf("task operations read %d statements for 10 rows and %d for 100 rows", small, large)
	}
}
