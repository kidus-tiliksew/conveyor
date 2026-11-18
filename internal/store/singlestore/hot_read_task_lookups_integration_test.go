package singlestore

import (
	"context"
	"database/sql"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

const unknownNonterminalState core.TaskState = "legacy-future-open"

// TestHotReadTaskLookupsIntegration compares public ListTaskPage States/Open
// results and startup review selection across an isolated legacy database,
// a partial snapshot, a prepared reopen, and the running stores that must
// not observe preparation until they reopen. EXPLAIN uses the actual
// ListTaskPage count and list shapes from s.taskPredicate.
func TestHotReadTaskLookupsIntegration(t *testing.T) {
	ctx := t.Context()
	db, dsn := hotReadIntegrationDatabase(t, true)
	openStore := func() *Store {
		t.Helper()
		s, err := Open(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Close)
		return s
	}
	legacy := openStore()
	for _, index := range hotReadIndexes {
		if index.column != "" && legacy.hotReadIndexReady(index.name) {
			t.Fatalf("legacy lookup %s ready", index.name)
		}
	}

	ws := "hot-task-lookups-" + core.NewTaskID()
	wsCtx := store.WithActor(store.WithWorkspace(ctx, ws), store.SystemActor())
	if _, err := legacy.BootstrapWorkspaceConfig(wsCtx, &config.Config{
		Workspace: ws,
		Repos:     []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}},
	}); err != nil {
		t.Fatal(err)
	}

	body := strings.Repeat("UNUSED-TASK-LOOKUP-BODY", 2048)
	queuedID, unknownID, mergedOpenID, closedID, anchorID, childID := seedHotReadTaskLookupFixture(t, legacy, wsCtx, ws, body)

	index := hotReadIndexes[0]
	if _, err := db.ExecContext(ctx, "ALTER TABLE "+index.table+" ADD COLUMN "+index.column+" AS ("+index.expression+") PERSISTED VARCHAR(64)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "ALTER TABLE "+index.table+" ADD KEY "+index.name+" ("+strings.Join(index.columns, ",")+") USING HASH"); err != nil {
		t.Fatal(err)
	}
	partial := openStore()
	if !partial.hotReadIndexReady(index.name) || partial.hotReadIndexReady(hotReadIndexes[1].name) || partial.hotReadIndexReady("tasks_workspace_state_created_idx") {
		t.Fatal("partial schema was not discovered per lookup")
	}
	if legacy.hotReadIndexReady(index.name) {
		t.Fatal("legacy store changed after preparation")
	}

	applySSPrepareScript(t, ctx, db, loadHotReadPrepareScript(t, "prepare-singlestore.sql", ws, anchorID))
	if partial.hotReadIndexReady(hotReadIndexes[1].name) || partial.hotReadIndexReady("tasks_workspace_state_created_idx") || legacy.hotReadIndexReady(index.name) || legacy.hotReadIndexReady("tasks_workspace_state_created_idx") {
		t.Fatal("running store changed after preparation")
	}

	prepared := openStore()
	reopen := openStore()
	for _, candidate := range []*Store{prepared, reopen} {
		for _, lookup := range hotReadIndexes {
			if lookup.column != "" && !candidate.hotReadIndexReady(lookup.name) {
				t.Errorf("prepared lookup %s not ready", lookup.name)
			}
		}
	}

	stores := []struct {
		name string
		st   *Store
	}{
		{"legacy", legacy},
		{"partial", partial},
		{"prepared", prepared},
		{"reopen", reopen},
	}
	queries := []struct {
		name   string
		filter store.TaskFilter
		empty  bool
	}{
		{"states queued", store.TaskFilter{States: []core.TaskState{core.TaskQueued}}, false},
		{"states unknown", store.TaskFilter{States: []core.TaskState{unknownNonterminalState}}, false},
		{"states terminal", store.TaskFilter{States: []core.TaskState{core.TaskMerged, core.TaskClosed}}, false},
		{"states mixed", store.TaskFilter{States: []core.TaskState{core.TaskQueued, unknownNonterminalState, core.TaskMerged}}, false},
		{"open", store.TaskFilter{Open: true}, false},
		{"open queued", store.TaskFilter{Open: true, States: []core.TaskState{core.TaskQueued}}, false},
		{"open terminal", store.TaskFilter{Open: true, States: []core.TaskState{core.TaskMerged, core.TaskClosed}}, true},
	}

	t.Run("list page matches legacy across snapshots", func(t *testing.T) {
		for _, q := range queries {
			baseline, err := legacy.ListTaskPage(wsCtx, store.TaskOperationsQuery{TaskFilter: q.filter})
			if err != nil {
				t.Fatalf("%s legacy: %v", q.name, err)
			}
			if q.empty {
				if baseline.Total != 0 || len(baseline.Tasks) != 0 {
					t.Fatalf("%s: legacy page = %+v, want empty", q.name, baseline)
				}
			} else if baseline.Total == 0 || len(baseline.Tasks) == 0 {
				t.Fatalf("%s: legacy page empty", q.name)
			}
			assertHotReadTaskPageBodies(t, q.name+" legacy", baseline, body)
			for _, candidate := range stores[1:] {
				got, err := candidate.st.ListTaskPage(wsCtx, store.TaskOperationsQuery{TaskFilter: q.filter})
				if err != nil {
					t.Fatalf("%s %s: %v", q.name, candidate.name, err)
				}
				assertHotReadTaskPageBodies(t, q.name+" "+candidate.name, got, body)
				if !reflect.DeepEqual(got, baseline) {
					t.Fatalf("%s %s: prepared=%+v legacy=%+v", q.name, candidate.name, got, baseline)
				}
			}
		}

		openPage, err := legacy.ListTaskPage(wsCtx, store.TaskOperationsQuery{TaskFilter: store.TaskFilter{Open: true}})
		if err != nil {
			t.Fatal(err)
		}
		if !hotReadTaskPageHasID(openPage, unknownID) || !hotReadTaskPageHasID(openPage, queuedID) || !hotReadTaskPageHasID(openPage, childID) {
			t.Fatalf("open dropped a nonterminal candidate: %+v", taskPageIDs(openPage))
		}
		if hotReadTaskPageHasID(openPage, mergedOpenID) || hotReadTaskPageHasID(openPage, closedID) || hotReadTaskPageHasID(openPage, anchorID) {
			t.Fatalf("open included a terminal or blueprint anchor: %+v", taskPageIDs(openPage))
		}
	})

	t.Run("hot reads carry no metadata after Open", func(t *testing.T) {
		prepared.ReadStats(0)
		if _, err := prepared.ListTaskPage(wsCtx, store.TaskOperationsQuery{TaskFilter: store.TaskFilter{Open: true}}); err != nil {
			t.Fatal(err)
		}
		stats := prepared.ReadStats(1 << 16)
		for _, entry := range stats.Top {
			if strings.Contains(strings.ToLower(entry.Label), "metadata") {
				t.Fatalf("unexpected metadata read: %s", entry.Label)
			}
		}
	})

	t.Run("explain names the prepared state key", func(t *testing.T) {
		positive := store.TaskFilter{States: []core.TaskState{core.TaskQueued, unknownNonterminalState}}
		countSQL, listSQL, args := hotReadTaskPageQueryShape(prepared, wsCtx, positive)
		hotReadExplainContains(t, prepared, "task page count", "task_state_key", countSQL, args...)
		hotReadExplainContains(t, prepared, "task page list", "task_state_key", listSQL, args...)

		negative := store.TaskFilter{Open: true}
		countSQL, listSQL, args = hotReadTaskPageQueryShape(prepared, wsCtx, negative)
		hotReadExplainStateKeyOrBlocker(t, prepared, "task page open count", countSQL, args...)
		hotReadExplainStateKeyOrBlocker(t, prepared, "task page open list", listSQL, args...)
	})

	t.Run("startup review matches ListTasks open-status oracle", func(t *testing.T) {
		want := hotReadReviewOracle(t, legacy, wsCtx)
		if !hotReadTasksHaveID(want, unknownID) || !hotReadTasksHaveID(want, queuedID) || !hotReadTasksHaveID(want, anchorID) || !hotReadTasksHaveID(want, childID) || !hotReadTasksHaveID(want, mergedOpenID) {
			t.Fatalf("oracle dropped a live or open-status task: %+v", hotReadTaskIDs(want))
		}
		if hotReadTasksHaveID(want, closedID) {
			t.Fatalf("oracle included an irrelevant terminal: %+v", hotReadTaskIDs(want))
		}
		prepared.ReadStats(0)
		got, err := prepared.ListReviewPolicyReconciliationTasks(wsCtx)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("startup tasks = %+v, want ListTasks+open-status oracle %+v", got, want)
		}
		for _, task := range got {
			if task.Body != "" {
				t.Fatalf("startup task %s transferred body", task.ID)
			}
			if task.ID == mergedOpenID && task.SetupContract.MaxBounces != 7 {
				t.Fatalf("startup dropped SetupContract: %+v", task.SetupContract)
			}
		}
		received := uint64(0)
		for _, entry := range prepared.ReadStats(1 << 16).Top {
			if entry.Label == "ListReviewPolicyReconciliationTasks" {
				received = entry.Bytes
			}
		}
		if received >= uint64(len(body)) {
			t.Fatalf("startup task read received %d bytes including an unused task body", received)
		}
		for _, candidate := range []*Store{legacy, partial, reopen} {
			compareHotLookup(t, "startup "+storeName(candidate, legacy, partial, reopen), func() ([]core.Task, error) {
				return want, nil
			}, func() ([]core.Task, error) {
				return candidate.ListReviewPolicyReconciliationTasks(wsCtx)
			})
		}
	})
}

func seedHotReadTaskLookupFixture(t *testing.T, st *Store, ctx context.Context, ws, body string) (queuedID, unknownID, mergedOpenID, closedID, anchorID, childID string) {
	t.Helper()
	contract := config.PolicyContract{
		MaxBounces: 7,
		Risk:       &riskpolicy.Policy{Mode: riskpolicy.ModeEnforce, Tiers: riskpolicy.DefaultTable()},
		Review:     config.ReviewPanel{Seats: []config.ReviewSeat{{}}},
	}
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	n := 0
	file := func(state core.TaskState, parent string) string {
		t.Helper()
		n++
		id := core.NewTaskID()
		task := core.Task{
			ID: id, Workspace: ws, Title: "lookup-" + id, Body: body,
			Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id,
			State: state, NextStage: core.StageImplement, ParentTaskID: parent,
			CreatedAt: now.Add(time.Duration(n) * time.Second), SetupContract: contract,
		}
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		return id
	}
	queuedID = file(core.TaskQueued, "")
	n++
	unknownID = core.NewTaskID()
	if _, err := st.db.ExecContext(ctx, `INSERT INTO tasks
		(id,workspace_id,source,title,body,class,escalation_level,repo_name,base_branch,branch,state,created_at,mode,spec_approval,merge_approval,next_stage)
		VALUES(?,?,'test',?,?,'','L2','conveyor','main',?,?,?,'',true,true,?)`,
		unknownID, ws, "lookup-"+unknownID, body, "conveyor/task-"+unknownID, unknownNonterminalState, now.Add(time.Duration(n)*time.Second), core.StageImplement); err != nil {
		t.Fatalf("historical unknown state: %v", err)
	}
	mergedOpenID = file(core.TaskMerged, "")
	closedID = file(core.TaskClosed, "")
	anchorID = file(core.TaskQueued, "")
	childID = file(core.TaskRunning, anchorID)
	if _, _, err := st.QueueReviewPolicyStatus(ctx, mergedOpenID, "head-open", core.PolicyStatusPending); err != nil {
		t.Fatal(err)
	}
	return queuedID, unknownID, mergedOpenID, closedID, anchorID, childID
}

func hotReadTaskPageQueryShape(st *Store, ctx context.Context, filter store.TaskFilter) (countSQL, listSQL string, args []any) {
	predicate, args := st.taskPredicate(ctx, filter)
	return `SELECT COUNT(*) FROM tasks WHERE ` + predicate,
		`SELECT ` + taskListColumns + ` FROM tasks WHERE ` + predicate + ` ORDER BY created_at DESC,id ASC`,
		args
}

// hotReadExplainStateKeyOrBlocker records the exact EXPLAIN of a negative
// TaskPage shape. A missing task_state_key is a parent blocker, not a skip.
func hotReadExplainStateKeyOrBlocker(t *testing.T, st *Store, name, query string, args ...any) {
	t.Helper()
	text := hotReadExplainText(t, st, name, query, args...)
	if strings.Contains(text, "task_state_key") {
		return
	}
	t.Errorf("blocker: parent: unsupported negative index shape for %s (task_state_key not chosen):\n%s", name, text)
}

func hotReadExplainText(t *testing.T, st *Store, name, query string, args ...any) string {
	t.Helper()
	rows, err := st.db.QueryContext(t.Context(), "EXPLAIN "+query, args...)
	if err != nil {
		t.Fatalf("%s explain: %v", name, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		values := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		for _, v := range values {
			if v.Valid {
				plan = append(plan, v.String)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	text := strings.Join(plan, "\n")
	t.Logf("%s EXPLAIN:\n%s", name, text)
	return text
}

func assertHotReadTaskPageBodies(t *testing.T, name string, page store.TaskPage, body string) {
	t.Helper()
	if page.Total != len(page.Tasks) {
		t.Fatalf("%s count=%d len=%d", name, page.Total, len(page.Tasks))
	}
	for _, task := range page.Tasks {
		if task.Body == "" {
			continue
		}
		if task.Body != body {
			t.Fatalf("%s task %s body length %d, want full %d", name, task.ID, len(task.Body), len(body))
		}
	}
}

func hotReadReviewOracle(t *testing.T, st *Store, ctx context.Context) []core.Task {
	t.Helper()
	tasks, err := st.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The reconciliation projection orders by its own (created_at,id) scan.
	// ListTasks presents newest first, so rebuild the projection's order
	// rather than inheriting that presentation order.
	sort.SliceStable(tasks, func(i, j int) bool {
		if !tasks[i].CreatedAt.Equal(tasks[j].CreatedAt) {
			return tasks[i].CreatedAt.Before(tasks[j].CreatedAt)
		}
		return tasks[i].ID < tasks[j].ID
	})
	statuses, err := st.ListOpenReviewPolicyStatuses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	open := map[string]bool{}
	for _, status := range statuses {
		open[status.TaskID] = true
	}
	out := []core.Task{}
	for _, task := range tasks {
		if !core.TaskTerminal(task.State) || open[task.ID] {
			out = append(out, core.Task{ID: task.ID, State: task.State, SetupContract: task.SetupContract})
		}
	}
	return out
}

func hotReadTaskPageHasID(page store.TaskPage, id string) bool {
	return hotReadTasksHaveID(page.Tasks, id)
}

func hotReadTasksHaveID(tasks []core.Task, id string) bool {
	for _, task := range tasks {
		if task.ID == id {
			return true
		}
	}
	return false
}

func taskPageIDs(page store.TaskPage) []string {
	return hotReadTaskIDs(page.Tasks)
}

func hotReadTaskIDs(tasks []core.Task) []string {
	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.ID)
	}
	return ids
}

func storeName(st, legacy, partial, reopen *Store) string {
	switch st {
	case legacy:
		return "legacy"
	case partial:
		return "partial"
	case reopen:
		return "reopen"
	default:
		return "store"
	}
}
