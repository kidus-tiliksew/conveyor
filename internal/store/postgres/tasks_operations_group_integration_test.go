package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strconv"
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

// groupScaleShape counts one fixture's tasks by shape.
type groupScaleShape struct {
	merged, closed, queued, blocked, awaiting, parked, approvedOff, approvedOn, reviewing, running int
}

func (s groupScaleShape) total() int {
	return s.merged + s.closed + s.queued + s.blocked + s.awaiting + s.parked + s.approvedOff + s.approvedOn + s.reviewing + s.running
}

// seedTaskOperationsGroupScale seeds one workspace with every shape the
// grouped read partitions: finished tasks, queued implement orders (agent),
// dependents in the web repository blocked on conveyor tasks (dependency),
// plan gates with plans and parked tasks (operator), approved tasks with the
// merge gate off (factory) and on (operator), claim-window review orders with
// task-authored proposals, and running tasks without an order.
func seedTaskOperationsGroupScale(t *testing.T, st *Store, shape groupScaleShape) string {
	t.Helper()
	workspace := "task-ops-group-" + core.NewTaskID()
	system := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), workspace)
	human := store.WithWorkspace(store.WithActor(t.Context(), store.Actor{ID: "operator", Role: core.ActorHuman}), workspace)
	if _, err := st.BootstrapWorkspaceConfig(system, &config.Config{Workspace: workspace, Repos: []config.Repo{
		{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"},
		{Name: "web", URL: "https://example.test/web", Base: "main"},
	}}); err != nil {
		t.Fatal(err)
	}
	requirement := "req-group-" + core.NewTaskID()
	_, version, err := st.CreateRequirement(human, core.Requirement{ID: requirement, Title: "Grouped read"}, core.RequirementVersion{Content: "# Grouped read", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "The grouped read scales."}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(human, requirement, version.Version); err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC().Add(-48 * time.Hour)
	var seeds []core.Task
	byShape := map[string][]string{}
	add := func(kind string, count int, state core.TaskState, repo string, mergeApproval bool) {
		for i := 0; i < count; i++ {
			id := fmt.Sprintf("%s-%04d-%s", kind, i, workspace[len(workspace)-6:])
			seeds = append(seeds, core.Task{ID: id, Workspace: workspace, Repo: repo, BaseBranch: "main", Branch: "conveyor/task-" + id, Title: "Group " + id, Body: "body " + id, State: state, NextStage: core.StageImplement, MergeApproval: mergeApproval, CreatedAt: created.Add(time.Duration(len(seeds)/2) * time.Second)})
			byShape[kind] = append(byShape[kind], id)
		}
	}
	add("merged", shape.merged, core.TaskMerged, "conveyor", false)
	add("closed", shape.closed, core.TaskClosed, "web", false)
	add("queued", shape.queued, core.TaskQueued, "conveyor", false)
	add("awaiting", shape.awaiting, core.TaskRunning, "web", false)
	add("parked", shape.parked, core.TaskParked, "conveyor", false)
	add("approvedoff", shape.approvedOff, core.TaskApproved, "web", false)
	add("approvedon", shape.approvedOn, core.TaskApproved, "conveyor", true)
	add("reviewing", shape.reviewing, core.TaskRunning, "web", false)
	add("running", shape.running, core.TaskRunning, "conveyor", false)
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
	for _, task := range seeds {
		work <- task
	}
	close(work)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	order := func(id string, stage core.Stage, review bool) {
		t.Helper()
		now := time.Now().UTC()
		job := core.Job{ID: id + "-" + string(stage), TaskID: id, Stage: stage, StartedAt: now}
		if err := st.CreateJob(system, job); err != nil {
			t.Fatal(err)
		}
		wo := core.WorkOrder{ID: job.ID, TaskID: id, JobID: job.ID, Stage: stage, State: core.WorkOrderQueued, CreatedAt: now}
		if review {
			wo.ReviewRound, wo.ReviewSeat = 1, 1
		}
		if err := storetest.For(st).CreateWorkOrder(system, wo); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range byShape["queued"] {
		order(id, core.StageImplement, false)
	}
	// Each blocked dependent lives in the web repository and waits on a
	// queued conveyor task, so a web-only filter hides its blocker.
	for i := 0; i < shape.blocked; i++ {
		id := fmt.Sprintf("blocked-%04d-%s", i, workspace[len(workspace)-6:])
		blocker := byShape["queued"][i%len(byShape["queued"])]
		task := core.Task{ID: id, Workspace: workspace, Repo: "web", BaseBranch: "main", Branch: "conveyor/task-" + id, Title: "Group " + id, Body: "body " + id, State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: created.Add(time.Hour + time.Duration(i)*time.Second)}
		if err := st.CreateTaskWithDependencies(system, task, []string{blocker}); err != nil {
			t.Fatal(err)
		}
		order(id, core.StageImplement, false)
		byShape["blocked"] = append(byShape["blocked"], id)
	}
	plane := taskops.New(st)
	for _, id := range byShape["awaiting"] {
		if _, err := plane.Perform(system, id, taskops.Command{Kind: core.TaskGateSpec}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateSpecVersion(system, core.SpecVersion{TaskID: id, Content: "plan", Acceptance: core.JSONPayload([]any{}), Decomposition: core.JSONPayload([]any{})}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range byShape["parked"] {
		if err := st.AppendEvent(system, core.Event{TaskID: id, Kind: "task.state_changed", ActorRole: core.ActorSystem, Payload: core.JSONPayload(map[string]any{"command": "triage.park"})}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range byShape["reviewing"] {
		order(id, core.StageReview, true)
		if _, err := st.ProposeRequirementVersion(human, core.RequirementVersion{RequirementID: requirement, Content: "# Grouped read", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "The grouped read scales for " + id + "."}}, Origin: core.RequirementOriginImplementation, OriginTaskID: id}); err != nil {
			t.Fatal(err)
		}
	}
	return workspace
}

// groupTimingStore times the grouped read's candidate and detail phases.
type groupTimingStore struct {
	*Store
	mu         sync.Mutex
	candidates time.Duration
	details    time.Duration
}

func (s *groupTimingStore) ListTaskOperationsCandidates(ctx context.Context, filter store.TaskFilter) ([]core.Task, error) {
	started := time.Now()
	defer func() { s.mu.Lock(); s.candidates += time.Since(started); s.mu.Unlock() }()
	return s.Store.ListTaskOperationsCandidates(ctx, filter)
}

func (s *groupTimingStore) ListTaskOperationsForIDs(ctx context.Context, ids []string) (store.TaskOperationsPage, error) {
	started := time.Now()
	defer func() { s.mu.Lock(); s.details += time.Since(started); s.mu.Unlock() }()
	return s.Store.ListTaskOperationsForIDs(ctx, ids)
}

type groupScaleRow struct {
	Task struct {
		ID   string `json:"id"`
		Repo string `json:"repo"`
	} `json:"task"`
	NextAction struct {
		Actor string `json:"actor"`
	} `json:"next_action"`
}

var groupScaleHeaders = map[string]string{"operator": "X-Conveyor-Group-Operator-Total", "agent": "X-Conveyor-Group-Agent-Total", "waiting": "X-Conveyor-Group-Waiting-Total", "terminal": "X-Conveyor-Group-Terminal-Total"}

func groupScaleGroup(actor string) string {
	switch actor {
	case "factory", "dependency":
		return "waiting"
	case "none":
		return "terminal"
	default:
		return actor
	}
}

func groupScaleRead(t *testing.T, st *groupTimingStore, workspace, query string) (*httptest.ResponseRecorder, []groupScaleRow, time.Duration) {
	t.Helper()
	server := httpapi.NewServer(st)
	server.Credentials = nil
	server.Workspace, server.BearerToken = workspace, "token"
	request := httptest.NewRequest(http.MethodGet, "/v1/task-operations?workspace_id="+workspace+query, nil)
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	started := time.Now()
	server.Handler().ServeHTTP(response, request)
	elapsed := time.Since(started)
	if response.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", query, response.Code, response.Body.String())
	}
	var rows []groupScaleRow
	if err := json.Unmarshal(response.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	return response, rows, elapsed
}

// The grouped task-operations read answers about 700 mixed tasks on the CI
// PostgreSQL service with counts equal to the ungrouped read's tallies, and
// its query count does not grow with task count (req-operator-next-action
// AC-6.5; component-http-api, Grouped task-operations read). Latency, phase
// times, and allocations are logged as evidence.
func TestTaskOperationsGroupedReadScalesIntegration(t *testing.T) {
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
	st := &groupTimingStore{Store: newStore(pool)}

	smallShape := groupScaleShape{merged: 20, closed: 10, queued: 4, blocked: 2, awaiting: 2, parked: 2, approvedOff: 2, approvedOn: 2, reviewing: 2, running: 2}
	largeShape := groupScaleShape{merged: 400, closed: 100, queued: 60, blocked: 20, awaiting: 20, parked: 20, approvedOff: 20, approvedOn: 20, reviewing: 20, running: 20}
	small := seedTaskOperationsGroupScale(t, st.Store, smallShape)
	large := seedTaskOperationsGroupScale(t, st.Store, largeShape)
	t.Logf("grouped read fixture: %d tasks %+v; small fixture %d tasks", largeShape.total(), largeShape, smallShape.total())

	// Query counts: the same paged grouped read on both fixtures.
	recorder.reset()
	groupScaleRead(t, st, small, "&group=waiting&limit=25")
	smallQueries := recorder.snapshot()
	recorder.reset()
	groupScaleRead(t, st, large, "&group=waiting&limit=25")
	largeQueries := recorder.snapshot()
	t.Logf("grouped read queries: small %d, large %d", len(smallQueries), len(largeQueries))
	if len(smallQueries) != len(largeQueries) {
		t.Fatalf("grouped read queries small=%d large=%d, want equal:\n%s", len(smallQueries), len(largeQueries), strings.Join(largeQueries, "\n---\n"))
	}

	// Latency: one cold and three repeated reads per group, with phases.
	for _, group := range []string{"operator", "agent", "waiting", "terminal"} {
		for run := 0; run < 4; run++ {
			st.mu.Lock()
			st.candidates, st.details = 0, 0
			st.mu.Unlock()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			response, rows, elapsed := groupScaleRead(t, st, large, "&group="+group+"&limit=25")
			runtime.ReadMemStats(&after)
			st.mu.Lock()
			candidates, details := st.candidates, st.details
			st.mu.Unlock()
			label := "repeated"
			if run == 0 {
				label = "cold"
			}
			t.Logf("grouped read group=%s %s: %d rows, total %s, candidates %s, details %s, projection and assembly %s, %d allocations, %d bytes allocated, %d response bytes, counts %s/%s/%s/%s",
				group, label, len(rows), elapsed, candidates, details, elapsed-candidates-details, after.Mallocs-before.Mallocs, after.TotalAlloc-before.TotalAlloc, response.Body.Len(),
				response.Header().Get(groupScaleHeaders["operator"]), response.Header().Get(groupScaleHeaders["agent"]), response.Header().Get(groupScaleHeaders["waiting"]), response.Header().Get(groupScaleHeaders["terminal"]))
		}
	}
	_, all, unpaged := groupScaleRead(t, st, large, "")
	t.Logf("ungrouped unpaged read: %d rows, %s", len(all), unpaged)

	for _, filter := range []string{"", "&repository=web", "&repository=conveyor&state=queued&state=running"} {
		_, ungrouped, _ := groupScaleRead(t, st, large, filter)
		tally := map[string][]string{}
		for _, row := range ungrouped {
			group := groupScaleGroup(row.NextAction.Actor)
			tally[group] = append(tally[group], row.Task.ID)
		}
		for group := range groupScaleHeaders {
			response, rows, _ := groupScaleRead(t, st, large, filter+"&group="+group)
			if response.Header().Get("X-Conveyor-Total") != "" {
				t.Fatalf("%s %s: unpaginated read carried X-Conveyor-Total", filter, group)
			}
			ids := make([]string, len(rows))
			for i, row := range rows {
				ids[i] = row.Task.ID
				if groupScaleGroup(row.NextAction.Actor) != group {
					t.Fatalf("%s %s: row %s has actor %s", filter, group, row.Task.ID, row.NextAction.Actor)
				}
			}
			if !slices.Equal(ids, tally[group]) && len(ids)+len(tally[group]) > 0 {
				t.Fatalf("%s %s: rows=%d want %d", filter, group, len(ids), len(tally[group]))
			}
			for member, header := range groupScaleHeaders {
				if response.Header().Get(header) != strconv.Itoa(len(tally[member])) {
					t.Fatalf("%s %s: %s=%s want %d", filter, group, header, response.Header().Get(header), len(tally[member]))
				}
			}
		}
		if filter == "&repository=web" {
			dependency := 0
			for _, row := range ungrouped {
				if row.NextAction.Actor == "dependency" {
					dependency++
				}
			}
			if dependency != largeShape.blocked {
				t.Fatalf("web filter dependency-blocked rows=%d want %d", dependency, largeShape.blocked)
			}
		}
	}
	terminal := len(all)
	for _, row := range all {
		if groupScaleGroup(row.NextAction.Actor) != "terminal" {
			terminal--
		}
	}
	_, rows, _ := groupScaleRead(t, st, large, "&group=terminal")
	if len(rows) != terminal || terminal <= 200 {
		t.Fatalf("unpaginated terminal group rows=%d want %d (over 200)", len(rows), terminal)
	}
}
