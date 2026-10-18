package postgres

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

const taskMapScaleBody = "TASK-MAP-SCALE-BODY-SENTINEL"

// taskMapScaleFixture is one seeded workspace and its expected map.
type taskMapScaleFixture struct {
	workspace    string
	open         []string
	merged       []string
	closed       []string
	dependencies map[string][]string
	defaultNodes map[string]bool
	requirements map[string][3]int // open, merged, total
}

// seedTaskMapScale seeds open, merged, and closed tasks with dependencies and
// requirement attachments. Every fixture carries the exceptional next-action
// states: plan gates with plans, parked tasks with park commands, approved
// tasks, claim-window review orders with task-authored proposals, and closed
// blockers, so a query that runs per task in any of those paths shows up as
// a count difference between fixture sizes.
func seedTaskMapScale(t *testing.T, st *Store, openCount, mergedCount, closedCount, exceptional int) taskMapScaleFixture {
	t.Helper()
	workspace := "task-map-scale-" + core.NewTaskID()
	system := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), workspace)
	human := store.WithWorkspace(store.WithActor(t.Context(), store.Actor{ID: "operator", Role: core.ActorHuman}), workspace)
	if _, err := st.BootstrapWorkspaceConfig(system, &config.Config{Workspace: workspace, Repos: []config.Repo{
		{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"},
		{Name: "web", URL: "https://example.test/web", Base: "main"},
	}}); err != nil {
		t.Fatal(err)
	}
	f := taskMapScaleFixture{workspace: workspace, dependencies: map[string][]string{}, defaultNodes: map[string]bool{}, requirements: map[string][3]int{}}
	requirements := make([]string, 5)
	for i := range requirements {
		requirements[i] = fmt.Sprintf("req-scale-%d", i)
		_, version, err := st.CreateRequirement(human, core.Requirement{ID: requirements[i], Title: fmt.Sprintf("Scale %d", i)}, core.RequirementVersion{Content: fmt.Sprintf("# Scale %d", i), Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "The map scales."}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.ConfirmRequirementVersion(human, requirements[i], version.Version); err != nil {
			t.Fatal(err)
		}
	}
	created := time.Now().UTC().Add(-48 * time.Hour)
	type seed struct {
		task core.Task
	}
	var seeds []seed
	suffix := core.NewTaskID()
	add := func(prefix string, i int, state core.TaskState) string {
		id := fmt.Sprintf("%s-%04d-%s", prefix, i, suffix)
		repo := "conveyor"
		if i%3 == 0 {
			repo = "web"
		}
		seeds = append(seeds, seed{task: core.Task{ID: id, Workspace: workspace, Repo: repo, BaseBranch: "main", Branch: "conveyor/task-" + id, Title: "Scale " + id, Body: taskMapScaleBody + " " + id, State: state, NextStage: core.StageImplement, CreatedAt: created.Add(time.Duration(len(seeds)) * time.Second)}})
		return id
	}
	for i := 0; i < mergedCount; i++ {
		f.merged = append(f.merged, add("merged", i, core.TaskMerged))
	}
	for i := 0; i < closedCount; i++ {
		f.closed = append(f.closed, add("closed", i, core.TaskClosed))
	}
	for i := 0; i < openCount; i++ {
		state := core.TaskQueued
		if i%2 == 1 {
			state = core.TaskRunning
		}
		f.open = append(f.open, add("open", i, state))
	}
	// Each exceptional state has `exceptional` tasks; fixtures of different
	// sizes use different counts, so a per-task read in any exceptional path
	// changes the query count.
	awaiting, parked, approved, reviewing := f.open[0:exceptional], f.open[exceptional:2*exceptional], f.open[2*exceptional:3*exceptional], f.open[3*exceptional:4*exceptional]
	for index := range seeds {
		id := seeds[index].task.ID
		switch {
		case taskMapContains(parked, id):
			seeds[index].task.State = core.TaskParked
		case taskMapContains(approved, id):
			seeds[index].task.State = core.TaskApproved
		case taskMapContains(awaiting, id), taskMapContains(reviewing, id):
			seeds[index].task.State = core.TaskRunning
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(seeds))
	work := make(chan core.Task)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range work {
				if err := st.CreateTask(system, task); err != nil {
					errs <- fmt.Errorf("create %s: %w", task.ID, err)
				}
			}
		}()
	}
	for _, item := range seeds {
		work <- item.task
	}
	close(work)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	plane := taskops.New(st)
	for _, id := range awaiting {
		if _, err := plane.Perform(system, id, taskops.Command{Kind: core.TaskGateSpec}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateSpecVersion(system, core.SpecVersion{TaskID: id, Content: "plan", Acceptance: core.JSONPayload([]any{}), Decomposition: core.JSONPayload([]any{})}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range parked {
		if err := st.AppendEvent(system, core.Event{TaskID: id, Kind: "task.state_changed", ActorRole: core.ActorSystem, Payload: core.JSONPayload(map[string]any{"command": "triage.park"})}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range reviewing {
		job := core.Job{ID: id + "-review", TaskID: id, Stage: core.StageReview, StartedAt: time.Now().UTC()}
		if err := st.CreateJob(system, job); err != nil {
			t.Fatal(err)
		}
		if err := storetest.For(st).CreateWorkOrder(system, core.WorkOrder{ID: job.ID, TaskID: id, JobID: job.ID, Stage: core.StageReview, State: core.WorkOrderQueued, ReviewRound: 1, ReviewSeat: 1, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ProposeRequirementVersion(human, core.RequirementVersion{RequirementID: requirements[0], Content: "# Scale 0", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "The map scales for " + id + "."}}, Origin: core.RequirementOriginImplementation, OriginTaskID: id}); err != nil {
			t.Fatal(err)
		}
	}
	// Dependencies and attachments are written in bulk: the fixture measures
	// the read, and dependency creation validates open dependencies only.
	var edgeTasks, edgeDependencies []string
	edge := func(task, dependency string) {
		edgeTasks, edgeDependencies = append(edgeTasks, task), append(edgeDependencies, dependency)
		f.dependencies[task] = append(f.dependencies[task], dependency)
	}
	for i, id := range f.open {
		f.defaultNodes[id] = true
		if i%4 == 0 && i < len(f.merged) {
			edge(id, f.merged[i])
			f.defaultNodes[f.merged[i]] = true
		}
		if i%10 == 0 && i < len(f.closed) {
			edge(id, f.closed[i])
			f.defaultNodes[f.closed[i]] = true
		}
		if i%7 == 1 && i+1 < len(f.open) {
			edge(id, f.open[i+1])
		}
	}
	if _, err := st.pool.Exec(system, `INSERT INTO task_dependencies (workspace_id, task_id, depends_on_task_id)
SELECT $1, task_id, depends_on_task_id FROM unnest($2::text[], $3::text[]) AS edge(task_id, depends_on_task_id)`, workspace, edgeTasks, edgeDependencies); err != nil {
		t.Fatal(err)
	}
	var attachTasks, attachRequirements, attachKinds []string
	attach := func(task, requirement, kind string) {
		attachTasks, attachRequirements, attachKinds = append(attachTasks, task), append(attachRequirements, requirement), append(attachKinds, kind)
	}
	count := func(requirement string, column int) {
		counts := f.requirements[requirement]
		counts[column]++
		counts[2] = counts[0] + counts[1]
		f.requirements[requirement] = counts
	}
	for i, id := range f.open {
		requirement := requirements[i%len(requirements)]
		attach(id, requirement, store.TaskContextRequirementAdded)
		count(requirement, 0)
		if i%9 == 0 {
			// A removed attachment is not served.
			attach(id, requirements[(i+1)%len(requirements)], store.TaskContextRequirementAdded)
			attach(id, requirements[(i+1)%len(requirements)], store.TaskContextRequirementRemoved)
		}
	}
	for i, id := range f.merged {
		requirement := requirements[i%len(requirements)]
		attach(id, requirement, store.TaskContextRequirementAdded)
		count(requirement, 1)
	}
	for i, id := range f.closed {
		attach(id, requirements[i%len(requirements)], store.TaskContextRequirementAdded)
	}
	if _, err := st.pool.Exec(system, `INSERT INTO events (task_id, kind, actor_id, actor_role, payload_json, at, workspace_id)
SELECT task_id, kind, 'operator', 'human', jsonb_build_object('id', requirement_id), now() - interval '1 hour' + ordinality * interval '1 millisecond', $1
FROM unnest($2::text[], $3::text[], $4::text[]) WITH ORDINALITY AS attachment(task_id, requirement_id, kind, ordinality)`, workspace, attachTasks, attachRequirements, attachKinds); err != nil {
		t.Fatal(err)
	}
	return f
}

func taskMapContains(values []string, value string) bool {
	return slices.Contains(values, value)
}

type taskMapScaleResponse struct {
	Tasks []struct {
		ID                   string   `json:"id"`
		State                string   `json:"state"`
		DependsOn            []string `json:"depends_on"`
		BlockingTaskIDs      []string `json:"blocking_task_ids"`
		UnsatisfiableTaskIDs []string `json:"unsatisfiable_task_ids"`
		NextAction           struct {
			Kind  string `json:"kind"`
			Label string `json:"label"`
		} `json:"next_action"`
		Requirements []struct {
			ID string `json:"id"`
		} `json:"requirements"`
	} `json:"tasks"`
	Requirements []struct {
		ID              string `json:"id"`
		OpenTaskCount   int    `json:"open_task_count"`
		MergedTaskCount int    `json:"merged_task_count"`
		TotalTaskCount  int    `json:"total_task_count"`
	} `json:"requirements"`
}

func taskMapScaleRead(t *testing.T, st *Store, workspace, query string) (*httptest.ResponseRecorder, time.Duration) {
	t.Helper()
	server := httpapi.NewServer(st)
	server.Credentials = nil
	server.Workspace, server.BearerToken = workspace, "token"
	request := httptest.NewRequest(http.MethodGet, "/v1/task-map?workspace_id="+workspace+query, nil)
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	started := time.Now()
	server.Handler().ServeHTTP(response, request)
	return response, time.Since(started)
}

// The map read answers 500 open and 2,000 finished tasks in one authenticated
// response under two seconds on the CI PostgreSQL service, with correct
// nodes, edges, next actions, and workspace requirement counts and no task
// body, and its query count does not grow with task count (req-board-map
// AC-3.1; component-http-api; component-persistence).
func TestTaskMapReadScalesWithBoundedQueriesIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	recorder := &queryRecorder{}
	cfg.ConnConfig.Tracer = recorder
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = Migrate(store.WithActor(t.Context(), store.SystemActor()), pool); err != nil {
		t.Fatal(err)
	}
	st := newStore(pool)

	small := seedTaskMapScale(t, st, 40, 40, 40, 2)
	large := seedTaskMapScale(t, st, 500, 1000, 1000, 6)

	recorder.reset()
	if response, _ := taskMapScaleRead(t, st, small.workspace, ""); response.Code != http.StatusOK {
		t.Fatalf("small status=%d body=%s", response.Code, response.Body.String())
	}
	smallQueries := recorder.snapshot()
	recorder.reset()
	response, elapsed := taskMapScaleRead(t, st, large.workspace, "")
	largeQueries := recorder.snapshot()
	if response.Code != http.StatusOK {
		t.Fatalf("large status=%d body=%s", response.Code, response.Body.String())
	}
	t.Logf("task map: %d tasks, %d bytes, %s, %d queries", len(large.defaultNodes), response.Body.Len(), elapsed, len(largeQueries))
	if elapsed >= 2*time.Second {
		t.Fatalf("task map read took %s, want under 2s", elapsed)
	}
	if len(largeQueries) != len(smallQueries) || len(largeQueries) > 30 {
		t.Fatalf("task map queries small=%d large=%d, want an equal fixed count of at most 30:\n%s", len(smallQueries), len(largeQueries), strings.Join(largeQueries, "\n---\n"))
	}
	if strings.Contains(response.Body.String(), taskMapScaleBody) {
		t.Fatal("task map response carried task body text")
	}
	for _, sql := range largeQueries {
		if strings.Contains(sql, "t.body") || strings.Contains(sql, "SELECT t.*") || strings.Contains(sql, "e.* FROM events") {
			t.Fatalf("task map read selected task bodies or full histories:\n%s", sql)
		}
	}
	var decoded taskMapScaleResponse
	if err = json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Tasks) != len(large.defaultNodes) {
		t.Fatalf("default tasks=%d want %d", len(decoded.Tasks), len(large.defaultNodes))
	}
	seen := map[string]bool{}
	kinds := map[string]int{}
	for _, item := range decoded.Tasks {
		if !large.defaultNodes[item.ID] || seen[item.ID] {
			t.Fatalf("unexpected or repeated task %s", item.ID)
		}
		seen[item.ID] = true
		if item.NextAction.Kind == "" || item.NextAction.Label == "" {
			t.Fatalf("task %s has no next action", item.ID)
		}
		kinds[item.NextAction.Kind]++
		want := append([]string{}, large.dependencies[item.ID]...)
		if strings.Join(sortedCopy(item.DependsOn), ",") != strings.Join(sortedCopy(want), ",") {
			t.Fatalf("task %s depends_on=%v want %v", item.ID, item.DependsOn, want)
		}
		for _, dependency := range want {
			blocked := !strings.HasPrefix(dependency, "merged-")
			if taskMapContains(item.BlockingTaskIDs, dependency) != blocked {
				t.Fatalf("task %s blocking=%v dependency %s", item.ID, item.BlockingTaskIDs, dependency)
			}
			if taskMapContains(item.UnsatisfiableTaskIDs, dependency) != strings.HasPrefix(dependency, "closed-") {
				t.Fatalf("task %s unsatisfiable=%v dependency %s", item.ID, item.UnsatisfiableTaskIDs, dependency)
			}
		}
		if len(item.Requirements) != 1 {
			t.Fatalf("task %s requirements=%+v", item.ID, item.Requirements)
		}
	}
	for _, kind := range []string{"plan_approval", "parked"} {
		if kinds[kind] == 0 {
			t.Fatalf("next-action kinds %v missing %s", kinds, kind)
		}
	}
	if len(decoded.Requirements) != len(large.requirements) {
		t.Fatalf("requirements=%+v want %v", decoded.Requirements, large.requirements)
	}
	for _, requirement := range decoded.Requirements {
		want := large.requirements[requirement.ID]
		if requirement.OpenTaskCount != want[0] || requirement.MergedTaskCount != want[1] || requirement.TotalTaskCount != want[2] {
			t.Fatalf("requirement %+v want open/merged/total %v", requirement, want)
		}
	}

	finished, _ := taskMapScaleRead(t, st, large.workspace, "&include_finished=true")
	var all taskMapScaleResponse
	if finished.Code != http.StatusOK || json.Unmarshal(finished.Body.Bytes(), &all) != nil {
		t.Fatalf("include_finished status=%d", finished.Code)
	}
	if want := len(large.open) + len(large.merged) + len(large.closed); len(all.Tasks) != want {
		t.Fatalf("include_finished tasks=%d want %d", len(all.Tasks), want)
	}
	if fmt.Sprint(all.Requirements) != fmt.Sprint(decoded.Requirements) {
		t.Fatalf("include_finished changed requirement counts: %+v vs %+v", all.Requirements, decoded.Requirements)
	}
	filtered, _ := taskMapScaleRead(t, st, large.workspace, "&repository=web")
	var web taskMapScaleResponse
	if filtered.Code != http.StatusOK || json.Unmarshal(filtered.Body.Bytes(), &web) != nil {
		t.Fatalf("repository status=%d", filtered.Code)
	}
	if fmt.Sprint(web.Requirements) != fmt.Sprint(decoded.Requirements) || len(web.Tasks) == 0 || len(web.Tasks) >= len(decoded.Tasks) {
		t.Fatalf("repository filter tasks=%d requirements=%+v", len(web.Tasks), web.Requirements)
	}
}

func sortedCopy(values []string) []string {
	out := append([]string(nil), values...)
	slices.Sort(out)
	return out
}
