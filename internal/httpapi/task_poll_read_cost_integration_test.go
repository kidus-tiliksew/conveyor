package httpapi

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres"
	"github.com/kidus-tiliksew/conveyor/internal/store/singlestore"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

// taskPollTasks is the fixture size: the staging workspace that reported the
// regression held about 340 tasks.
const taskPollTasks = 340

// taskPollNow is the fixture's fixed clock. It is later than every time the
// stores stamp from the wall clock, such as a document proposal's proposed
// time, so the pending proposal's age changes each second during a slow
// build, as on a live daemon.
var taskPollNow = time.Date(2030, 1, 1, 0, 0, 5, 0, time.UTC)

// taskPollDatabaseName is the shape of a database the test owns.
var taskPollDatabaseName = regexp.MustCompile(`^[a-z][a-z0-9_]{2,62}_test$`)

// taskPollSeedWorkers writes the fixture tasks concurrently; each task's
// history stays in one worker, so its event order is fixed.
const taskPollSeedWorkers = 8

// taskPollByteLimits bounds the bytes one cold poll of GET /v1/tasks,
// GET /v1/work-orders through the work-order service, and
// GET /v1/next-actions?limit=200&offset=0 receives from each backend on the
// seeded fixture. Measured on SingleStore 10.9 and PostgreSQL 16 with this
// test:
//
//	backend      v0.71.0    main d7639465
//	singlestore  4,103,424  2,469,148
//	postgres     3,615,156  2,194,348
//
// The SingleStore limit is the measured v0.71.0 poll, the last release whose
// task list and inbox reads were cheap on SingleStore. The PostgreSQL limit
// is the measured main poll plus 1% for the width of generated IDs and
// timestamps, so it fails on a regression against main as well as against
// v0.71.0.
var taskPollByteLimits = map[string]uint64{
	"postgres":    2_216_291,
	"singlestore": 4_103_424,
}

type taskPollStore interface {
	store.Store
	BootstrapWorkspaceConfig(context.Context, *config.Config) (bool, error)
	ReadStats(int) dbmetrics.ReadInterval
	Close()
}

// TestTaskPollReadCostIntegration measures what one external poll of the task
// list, the work-order list, and the inbox page costs the database. Each
// request runs against a new server, so no response memo answers it, with no
// If-None-Match and a fixed clock. The test fails when the poll receives more
// bytes than the backend's limit.
func TestTaskPollReadCostIntegration(t *testing.T) {
	for _, backend := range []struct{ name, variable string }{
		{"postgres", "CONVEYOR_TEST_DATABASE_URL"},
		{"singlestore", "CONVEYOR_TEST_SINGLESTORE_URL"},
	} {
		t.Run(backend.name, func(t *testing.T) {
			raw := strings.TrimSpace(os.Getenv(backend.variable))
			if raw == "" {
				t.Skipf("%s is unset", backend.variable)
			}
			ctx := store.WithActor(t.Context(), store.SystemActor())
			st, reused := openTaskPollStore(t, ctx, backend.name, raw)
			const ws = "task-poll"
			ctx = store.WithWorkspace(ctx, ws)
			cfg := &config.Config{Workspace: ws, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}
			now := taskPollNow
			if !reused {
				if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
					t.Fatal(err)
				}
				started := time.Now()
				seedTaskPoll(t, ctx, st, ws, now)
				t.Logf("seeded %d tasks in %s", taskPollTasks, time.Since(started).Round(time.Millisecond))
			}
			previousClock := activityClock
			activityClock = func() time.Time { return now }
			t.Cleanup(func() { activityClock = previousClock })
			newServer := func(memoClock func() time.Time) *Server {
				s := NewServer(st)
				s.Workspace = ws
				membership := &membershipFixture{workspaces: []core.Workspace{{ID: ws, Name: ws}}, roles: map[string]map[string]core.WorkspaceRole{"reader": {ws: core.WorkspaceRoleViewer}}}
				s.Workspaces, s.Memberships = membership, membership
				s.Credentials = staticCredentialVerifier{"reader": {ID: "reader-pat", OwnerUserID: "reader", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}}
				s.ConfigProvider = func(context.Context) (*config.Config, error) { return cfg, nil }
				// GET /v1/work-orders runs the work-order service's list and
				// projection, as conveyord wires it, not the bare store read.
				s.WorkOrders = &workorder.Service{Store: st, ConfigProvider: s.ConfigProvider}
				s.nextActionClock = func() time.Time { return now }
				taskPollMemoClock(s, memoClock)
				return s
			}
			fixed := func() func() time.Time { return func() time.Time { return now } }
			slow := func() func() time.Time { return taskPollSlowClock(now) }
			paths := []string{
				"/v1/tasks?workspace_id=" + ws,
				"/v1/work-orders?workspace_id=" + ws,
				"/v1/next-actions?workspace_id=" + ws + "&limit=200&offset=0",
			}
			// Attribution: the store is opened by this test alone and runs no
			// dispatcher, worker, or sweep, the requests run one after
			// another, and the store's read ledger is drained before and
			// after each request. Each route interval therefore holds only
			// that request's reads. Bytes received outside a labeled
			// operation, such as connection setup or a notice listener,
			// land in the "unlabeled" account; they are reported with the
			// route and counted in the bounded total.
			poll := func(memoClock func() func() time.Time) (dbmetrics.ReadInterval, []dbmetrics.ReadInterval, []string) {
				st.ReadStats(0)
				var routes []dbmetrics.ReadInterval
				var digests []string
				var total dbmetrics.ReadInterval
				for _, path := range paths {
					r := httptest.NewRequest(http.MethodGet, path, nil).WithContext(t.Context())
					r.Header.Set("Authorization", "Bearer reader")
					w := httptest.NewRecorder()
					newServer(memoClock()).Handler().ServeHTTP(w, r)
					route := st.ReadStats(64)
					if w.Code != http.StatusOK {
						t.Fatalf("%s answered %d: %s", path, w.Code, w.Body.String())
					}
					if route.Queries == 0 || route.Bytes == 0 {
						t.Fatalf("%s recorded no database read: %+v", path, route)
					}
					if dir := os.Getenv("CONVEYOR_TASK_POLL_DUMP"); dir != "" {
						name := strings.NewReplacer("/", "_", "?", "_", "&", "_", "=", "_").Replace(strings.SplitN(path, "?", 2)[0])
						if err := os.WriteFile(filepath.Join(dir, backend.name+name+".json"), w.Body.Bytes(), 0o600); err != nil {
							t.Fatal(err)
						}
					}
					digests = append(digests, fmt.Sprintf("%s %d %x %d", path, w.Code, taskPollResponseDigest(w), w.Body.Len()))
					routes = append(routes, route)
					total.Bytes += route.Bytes
					total.Rows += route.Rows
					total.Queries += route.Queries
				}
				return total, routes, digests
			}
			// The first poll also pays the store's one-time schema discovery,
			// which no later poll repeats, so it is reported and not bounded.
			// In the slow sample each request's response memo clock passes
			// the memo lifetime during each of its first builds, as a build
			// slower than the lifetime does, and then stops. Each route must
			// still read once.
			var measured uint64
			for sample, memoClock := range []func() func() time.Time{fixed, fixed, fixed, slow} {
				total, routes, digests := poll(memoClock)
				report, err := json.Marshal(map[string]any{"backend": backend.name, "sample": sample, "slow_builds": sample == 3, "tasks": taskPollTasks, "total": total, "routes": routes, "responses": digests})
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("task poll read cost: %s", report)
				if sample > 0 {
					measured = max(measured, total.Bytes)
				}
			}
			// The byte limit is checked before the concurrent reproduction,
			// whose wall-clock memo can rebuild without end on a memo that
			// does not bound its rebuilds.
			limit := taskPollByteLimits[backend.name]
			if os.Getenv("CONVEYOR_TASK_POLL_REPORT_ONLY") == "1" {
				t.Logf("report only: a cold poll received at most %d bytes; limit %d", measured, limit)
			} else if measured > limit {
				t.Fatalf("one cold poll received %d bytes from %s, above the limit of %d", measured, backend.name, limit)
			}
			taskPollConcurrentReproduction(t, backend.name, st, func() *Server { return newServer(func() time.Time { return time.Now().UTC() }) }, paths)
		})
	}
}

// openTaskPollStore opens the backend on a new database that the test owns
// and drops. CONVEYOR_TASK_POLL_DATABASE names an existing seeded database to
// reuse instead, and CONVEYOR_TASK_POLL_KEEP=1 keeps the new database, so the
// same rows can be read by two builds.
func openTaskPollStore(t *testing.T, ctx context.Context, backend, raw string) (taskPollStore, bool) {
	t.Helper()
	reuse := strings.TrimSpace(os.Getenv("CONVEYOR_TASK_POLL_DATABASE"))
	name := reuse
	if name == "" {
		name = fmt.Sprintf("conveyor_poll_%d_test", time.Now().UnixNano())
	}
	// Only an owned test database is opened, migrated, and seeded
	// (component-testing-strategy invariant 11).
	if !taskPollDatabaseName.MatchString(name) {
		t.Fatalf("task poll database %q is not an owned test database name", name)
	}
	keep := reuse != "" || os.Getenv("CONVEYOR_TASK_POLL_KEEP") == "1"
	if backend == "postgres" {
		admin, err := pgx.Connect(ctx, raw)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = admin.Close(context.Background()) })
		if reuse == "" {
			if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
				t.Fatal(err)
			}
		}
		if !keep {
			t.Cleanup(func() {
				if _, err := admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
					t.Errorf("drop %s: %v", name, err)
				}
			})
		} else {
			t.Logf("task poll database %s", name)
		}
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		u.Path = "/" + name
		st, err := postgres.Open(ctx, u.String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(st.Close)
		return st, reuse != ""
	}
	cfg, err := taskPollSingleStoreConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	admin := sql.OpenDB(connector)
	t.Cleanup(func() { _ = admin.Close() })
	cfg.DBName = name
	if reuse == "" {
		if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+name+"` PARTITIONS 2"); err != nil {
			t.Fatal(err)
		}
	}
	if !keep {
		t.Cleanup(func() {
			if _, err := admin.ExecContext(context.Background(), "DROP DATABASE `"+name+"`"); err != nil {
				t.Errorf("drop %s: %v", name, err)
			}
		})
	} else {
		t.Logf("task poll database %s", name)
	}
	if reuse == "" {
		// A fresh install creates the operator-owned event table before
		// normal startup.
		preparationConnector, err := mysql.NewConnector(cfg)
		if err != nil {
			t.Fatal(err)
		}
		preparation := sql.OpenDB(preparationConnector)
		defer preparation.Close()
		data, err := os.ReadFile(filepath.Join("..", "..", "docs", "sql", "events-columnstore", "01-create.sql"))
		if err != nil {
			t.Fatal(err)
		}
		var lines []string
		for _, line := range strings.Split(string(data), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "--") {
				lines = append(lines, line)
			}
		}
		for _, statement := range strings.Split(strings.Join(lines, "\n"), ";") {
			if statement = strings.TrimSpace(statement); statement != "" {
				if _, err := preparation.ExecContext(ctx, strings.ReplaceAll(statement, "`your_database`", "`"+name+"`")); err != nil {
					t.Fatalf("prepare events table: %v", err)
				}
			}
		}
	}
	st, err := singlestore.Open(ctx, cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st, reuse != ""
}

// taskPollSingleStoreConfig reads the SingleStore fixture address in DSN or
// URL form.
func taskPollSingleStoreConfig(raw string) (*mysql.Config, error) {
	if !strings.Contains(raw, "://") {
		cfg, err := mysql.ParseDSN(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid SingleStore fixture DSN")
		}
		cfg.ParseTime = true
		return cfg, nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "singlestore" && u.Scheme != "mysql") || u.Hostname() == "" {
		return nil, fmt.Errorf("invalid SingleStore fixture URL")
	}
	cfg := mysql.NewConfig()
	cfg.Net = "tcp"
	port := u.Port()
	if port == "" {
		port = "3306"
	}
	cfg.Addr = net.JoinHostPort(u.Hostname(), port)
	cfg.DBName = strings.TrimPrefix(u.Path, "/")
	if u.User != nil {
		cfg.User = u.User.Username()
		cfg.Passwd, _ = u.User.Password()
	}
	cfg.TLSConfig = u.Query().Get("tls")
	cfg.ParseTime = true
	return cfg, nil
}

// seedTaskPoll writes a workspace shaped like the one that reported the
// regression through public store commands only. Each group of ten tasks
// holds four merged tasks, two implementing tasks with a live claim, one task
// with a live review claim, one task with a queued review order, one task at
// the plan approval gate, and one task blocked by that gated task. Every task
// has a large creation snapshot, and the orders have long claim and renewal
// histories with padded fields no marker fold reads.
func seedTaskPoll(t *testing.T, ctx context.Context, st store.Store, ws string, now time.Time) {
	t.Helper()
	// A blocked task depends on the gated task before it, so the blocked
	// tasks are written after every other task.
	for _, blocked := range []bool{false, true} {
		indexes := make(chan int)
		failures := make(chan error, taskPollSeedWorkers)
		var workers sync.WaitGroup
		for range taskPollSeedWorkers {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for index := range indexes {
					if err := seedTaskPollTask(ctx, st, ws, now, index); err != nil {
						failures <- err
						for range indexes {
						}
						return
					}
				}
			}()
		}
		for index := range taskPollTasks {
			if (index%10 == 9) == blocked {
				indexes <- index
			}
		}
		close(indexes)
		workers.Wait()
		close(failures)
		for err := range failures {
			t.Fatal(err)
		}
	}
	// One pending requirement proposal puts a document proposal in the
	// inbox, whose age the inbox states to the second.
	if _, _, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-task-poll", Title: "Task poll proposal"}, core.RequirementVersion{Content: "# Task poll proposal", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "The poll reads once."}}}); err != nil {
		t.Fatal(err)
	}
}

type taskPollSeedError struct{ err error }

// seedTaskPollTask writes one fixture task and its history.
func seedTaskPollTask(ctx context.Context, st store.Store, ws string, now time.Time, index int) (failure error) {
	defer func() {
		if r := recover(); r != nil {
			seedErr, ok := r.(taskPollSeedError)
			if !ok {
				panic(r)
			}
			failure = fmt.Errorf("seed task %d: %w", index, seedErr.err)
		}
	}()
	pad := strings.Repeat("p", 2048)
	body := strings.Repeat("Task body line for the poll fixture.\n", 16)
	{
		taskID := fmt.Sprintf("poll-%03d", index)
		group := index % 10
		created := now.Add(-time.Duration(taskPollTasks-index) * time.Hour)
		task := core.Task{
			ID: taskID, Workspace: ws, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + taskID,
			Title: fmt.Sprintf("Poll fixture task %03d", index), Body: body, CreatedAt: created,
		}
		var dependencies []string
		switch group {
		case 0, 1, 2, 3:
			task.State = core.TaskMerged
		case 4, 5:
			task.State, task.NextStage = core.TaskRunning, core.StageImplement
		case 6, 7:
			task.State, task.NextStage = core.TaskRunning, core.StageReview
		case 8:
			task.State, task.GateKind, task.NextStage = core.TaskAwaiting, core.GateKindPlanApproval, core.StageImplement
		case 9:
			task.State, task.NextStage = core.TaskQueued, core.StageImplement
			dependencies = []string{fmt.Sprintf("poll-%03d", index-1)}
		}
		if err := st.CreateTaskWithDependencies(ctx, task, dependencies); err != nil {
			panic(taskPollSeedError{err})
		}
		at := created
		appendEvent := func(jobID, kind string, payload any) {
			at = at.Add(time.Second)
			encoded, err := json.Marshal(payload)
			if err != nil {
				panic(taskPollSeedError{err})
			}
			if err := st.AppendEvent(ctx, core.Event{TaskID: taskID, JobID: jobID, Kind: kind, At: at, Payload: encoded}); err != nil {
				panic(taskPollSeedError{err})
			}
		}
		appendEvent("", "task.created", map[string]any{"title": task.Title, "body": body, "snapshot": map[string]any{"plan": pad, "context": pad}})
		newOrder := func(suffix string, stage core.Stage) core.WorkOrder {
			id := taskID + "-" + suffix
			if err := st.CreateJob(ctx, core.Job{ID: id, TaskID: taskID, Stage: stage, State: core.JobPending}); err != nil {
				panic(taskPollSeedError{err})
			}
			order := core.WorkOrder{ID: id, TaskID: taskID, JobID: id, Stage: stage, QueueEnteredAt: at, QueueDeadline: now.Add(48 * time.Hour)}
			if err := storetest.CreateWorkOrder(ctx, st, order); err != nil {
				panic(taskPollSeedError{err})
			}
			return order
		}
		history := func(order core.WorkOrder, claims, renewals int, lease time.Time) {
			for claim := range claims {
				appendEvent(order.JobID, "work_order.claimed", map[string]any{
					"id": order.ID, "task_id": taskID, "job_id": order.JobID, "stage": string(order.Stage), "state": "claimed",
					"session_id": fmt.Sprintf("session-%s-%d", order.ID, claim), "attempt_id": fmt.Sprintf("attempt-%s-%d", order.ID, claim),
					"lease_expires_at": lease.Add(-time.Duration(claims-1-claim) * time.Hour).Format(time.RFC3339Nano), "progress": pad,
				})
				for renewal := range renewals {
					appendEvent(order.JobID, "work_order.lease_renewed", map[string]any{
						"attempt_id":       fmt.Sprintf("attempt-%s-%d", order.ID, claim),
						"lease_expires_at": lease.Add(-time.Duration(claims-1-claim)*time.Hour - time.Duration(renewals-1-renewal)*time.Minute).Format(time.RFC3339Nano),
					})
				}
			}
		}
		submit := func(order core.WorkOrder) {
			appendEvent("", "work_order.updated", map[string]any{"id": order.ID, "stage": string(order.Stage), "state": "submitted", "head_sha": "head-" + taskID, "detail": pad})
		}
		switch group {
		case 0, 1, 2, 3:
			spec := newOrder("spec-1", core.StageSpec)
			history(spec, 1, 2, now.Add(-200*time.Hour))
			implement := newOrder("implement-1", core.StageImplement)
			history(implement, 3, 8, now.Add(-100*time.Hour))
			submit(implement)
			appendEvent("", "pipeline.bounced", map[string]any{"source": "operator", "feedback": pad})
			review := newOrder("review-1-seat-1", core.StageReview)
			history(review, 2, 4, now.Add(-90*time.Hour))
			appendEvent(review.JobID, "review.completed", map[string]any{"id": review.ID, "verdict": "approve", "summary": pad})
			appendEvent("", "review.round_completed", map[string]any{"round": 1, "summary": pad})
			appendEvent("", "merge.failed", map[string]any{"forge_error_category": "merge_conflict", "error": "merge conflict", "head_sha": "head-" + taskID, "detail": pad})
			appendEvent("", "pull_request.merged", map[string]any{"head_sha": "head-" + taskID, "detail": pad})
			for _, o := range []core.WorkOrder{spec, implement, review} {
				o.State = core.WorkOrderTimedOut
				if err := storetest.UpdateWorkOrder(ctx, st, o, core.WorkOrderCmdTimeout); err != nil {
					panic(taskPollSeedError{err})
				}
			}
		case 4, 5:
			implement := newOrder("implement-1", core.StageImplement)
			history(implement, 2, 12, now.Add(30*time.Minute))
			if group == 5 {
				appendEvent("", "pipeline.bounced", map[string]any{"source": store.UserRequestChangesSource, "feedback": pad})
			}
		case 6, 7:
			implement := newOrder("implement-1", core.StageImplement)
			history(implement, 2, 8, now.Add(-20*time.Hour))
			submit(implement)
			implement.State = core.WorkOrderTimedOut
			if err := storetest.UpdateWorkOrder(ctx, st, implement, core.WorkOrderCmdTimeout); err != nil {
				panic(taskPollSeedError{err})
			}
			review := newOrder("review-1-seat-1", core.StageReview)
			if group == 6 {
				history(review, 3, 10, now.Add(time.Duration(10+index%20)*time.Minute))
			}
		case 8:
			spec := newOrder("spec-1", core.StageSpec)
			history(spec, 1, 4, now.Add(-2*time.Hour))
			submit(spec)
			spec.State = core.WorkOrderTimedOut
			if err := storetest.UpdateWorkOrder(ctx, st, spec, core.WorkOrderCmdTimeout); err != nil {
				panic(taskPollSeedError{err})
			}
		case 9:
			newOrder("implement-1", core.StageImplement)
		}
	}
	return nil
}

// taskPollMemoClock gives the server's response memo the clock.
func taskPollMemoClock(s *Server, clock func() time.Time) {
	s.readMemo = &readMemo{clock: clock}
}

// taskPollSlowClock is a memo clock that moves past the memo lifetime on each
// of its first eight readings and then holds still. A memo that builds again
// after every expired build reads several times before a build fits.
func taskPollSlowClock(now time.Time) func() time.Time {
	var mu sync.Mutex
	readings := 0
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		at := now.Add(time.Duration(min(readings, 8)) * (readMemoLifetime + time.Second))
		readings++
		return at
	}
}

// taskPollResponseDigest digests a response's status, body, and the paging
// and conditional headers a client reads.
func taskPollResponseDigest(w *httptest.ResponseRecorder) []byte {
	hash := sha256.New()
	fmt.Fprintf(hash, "%d\n", w.Code)
	for _, name := range []string{"Content-Type", "ETag", "Cache-Control", "Vary", "X-Conveyor-Total", "X-Conveyor-Limit", "X-Conveyor-Offset", "X-Conveyor-Cursor"} {
		fmt.Fprintf(hash, "%s: %s\n", name, w.Header().Get(name))
	}
	hash.Write(w.Body.Bytes())
	return hash.Sum(nil)
}

// taskPollConcurrentReproduction sends the three poll requests at once, each
// to a new server whose response memo reads the wall clock, as a client poll
// meets a daemon. It reports when each request started and ended relative
// to the first start, the poll's elapsed time, and the bytes, rows, and
// statements each read label received. The activity and next-action clocks
// stay at the fixture instant. The reproduction is reported, not bounded.
func taskPollConcurrentReproduction(t *testing.T, backend string, st taskPollStore, newServer func() *Server, paths []string) {
	t.Helper()
	st.ReadStats(0)
	type timing struct {
		Path      string  `json:"path"`
		Status    int     `json:"status"`
		StartMS   float64 `json:"start_ms"`
		EndMS     float64 `json:"end_ms"`
		ElapsedMS float64 `json:"elapsed_ms"`
	}
	timings := make([]timing, len(paths))
	started := time.Now()
	var wg sync.WaitGroup
	for i, path := range paths {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A request that has not answered within two minutes is cut
			// off and reported, so a rebuild loop cannot hang the test.
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			r := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer reader")
			w := httptest.NewRecorder()
			begin := time.Since(started)
			newServer().Handler().ServeHTTP(w, r)
			end := time.Since(started)
			timings[i] = timing{Path: path, Status: w.Code, StartMS: begin.Seconds() * 1000, EndMS: end.Seconds() * 1000, ElapsedMS: (end - begin).Seconds() * 1000}
		}()
	}
	wg.Wait()
	elapsed := time.Since(started)
	reads := st.ReadStats(64)
	report, err := json.Marshal(map[string]any{"backend": backend, "concurrent_real_clock": true, "elapsed_ms": elapsed.Seconds() * 1000, "requests": timings, "total": reads})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("task poll concurrent reproduction: %s", report)
	for _, timing := range timings {
		if timing.Status != http.StatusOK {
			t.Fatalf("%s answered %d in the concurrent reproduction", timing.Path, timing.Status)
		}
	}
}
