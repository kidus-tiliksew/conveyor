package singlestore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// Preserve the pre-split statement as an independent pagination oracle. Its
// single OR predicate examines every event of the workspace.
const originalDocumentEventPageSQL = `WITH matched AS (
 SELECT e.id,e.at FROM events_columnstore e WHERE e.workspace_id=? AND (?=0 OR e.id<=?) AND (
 (?='requirement' AND ((e.task_id IS NULL AND JSON_EXTRACT_STRING(e.payload_json,'requirement_id')=?) OR EXISTS (
 SELECT 1 FROM task_context_proposals p WHERE p.workspace_id=e.workspace_id AND p.task_id=e.task_id AND p.target_kind='requirement' AND p.target_id=? AND p.state='confirmed')))
 OR (?='system_design' AND e.kind LIKE 'system_design.%' AND JSON_EXTRACT_STRING(e.payload_json,'document_id')=?))
 ), selected AS (SELECT id,at FROM matched ORDER BY at DESC,id DESC LIMIT ? OFFSET ?),
 totals AS (SELECT COUNT(*) AS total,COALESCE(MAX(id),0) AS snapshot FROM matched)
 SELECT totals.total,CASE WHEN ?>0 THEN ? ELSE totals.snapshot END,
 e.id,COALESCE(e.task_id,''),COALESCE(e.job_id,''),COALESCE(e.kind,''),COALESCE(e.actor_id,''),COALESCE(e.actor_role,''),e.payload_json,e.at
 FROM totals LEFT JOIN selected p ON 1=1 LEFT JOIN events_columnstore e ON e.workspace_id=? AND e.id=p.id ORDER BY e.at DESC,e.id DESC`

func originalDocumentEventPageQuery(ws string, kind core.LineageNodeType, id string, q store.DocumentEventQuery) (string, []any) {
	return originalDocumentEventPageSQL, []any{ws, q.SnapshotID, q.SnapshotID, string(kind), id, id, string(kind), id, q.Limit, q.Offset, q.SnapshotID, q.SnapshotID, ws}
}

// TestDocumentEventQueryPlansIntegration compares the kind-specific statements
// with the original statement, checks the event access path of each branch,
// and, when CONVEYOR_DOCUMENT_EVENT_MEASUREMENT names a noise event count,
// times both statements on that many unrelated task events
// (component-persistence; component-document-corpus, Public surfaces).
func TestDocumentEventQueryPlansIntegration(t *testing.T) {
	noise := 0
	if raw := os.Getenv("CONVEYOR_DOCUMENT_EVENT_MEASUREMENT"); raw != "" && raw != "0" {
		var err error
		noise, err = strconv.Atoi(raw)
		if err != nil || noise < 200000 {
			t.Fatal("measurement needs at least 200000 noise events")
		}
	}
	st := integrationStore(t)
	ws := "document-plans-" + core.NewTaskID()
	ctx := documentPlanWorkspace(t, st, ws)
	fixture := storetest.SeedDocumentEventMembership(t, st, ctx)
	ids := map[core.LineageNodeType]string{core.LineageRequirement: fixture.Requirement, core.LineageSystemDesign: fixture.Design}
	seedDocumentPlanMembership(t, st, ctx, fixture)
	// The same document IDs in another workspace never join this membership.
	other := documentPlanWorkspace(t, st, "document-plans-other-"+core.NewTaskID())
	seedDocumentPlanOtherWorkspace(t, st, other, fixture)

	t.Run("results", func(t *testing.T) {
		for kind, id := range ids {
			compareDocumentEventPages(t, st, ctx, ws, kind, id)
			compareDocumentEventPages(t, st, ctx, ws, kind, "missing-"+core.NewTaskID())
		}
		pinned := map[core.LineageNodeType][]store.DocumentEventPage{}
		queries := map[core.LineageNodeType][]store.DocumentEventQuery{}
		for kind, id := range ids {
			first, err := st.ListDocumentEventPage(ctx, kind, id, store.DocumentEventQuery{Limit: 50})
			if err != nil || first.Total <= 50 {
				t.Fatalf("%s first page total=%d err=%v", kind, first.Total, err)
			}
			for _, q := range []store.DocumentEventQuery{{Limit: 50, SnapshotID: first.SnapshotID}, {Limit: 50, Offset: 50, SnapshotID: first.SnapshotID}, {Limit: 200, SnapshotID: first.SnapshotID}} {
				page, err := st.ListDocumentEventPage(ctx, kind, id, q)
				if err != nil {
					t.Fatal(err)
				}
				pinned[kind] = append(pinned[kind], page)
				queries[kind] = append(queries[kind], q)
			}
		}
		// Events appended after the first page join only unpinned pages.
		appendDocumentPlanEvents(t, st, ctx, fixture.ConfirmedTask, fixture, 5, time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC))
		for kind, id := range ids {
			for i, q := range queries[kind] {
				page, err := st.ListDocumentEventPage(ctx, kind, id, q)
				if err != nil || !reflect.DeepEqual(page, pinned[kind][i]) {
					t.Fatalf("%s pinned page %+v changed after append: %+v err=%v", kind, q, page, err)
				}
			}
			compareDocumentEventPages(t, st, ctx, ws, kind, id)
		}
	})

	t.Run("plans", func(t *testing.T) {
		checkDocumentEventPlans(t, st, ctx, ws, ids)
	})

	if noise == 0 {
		return
	}
	t.Run("measurement", func(t *testing.T) {
		seedDocumentPlanNoise(t, st, ctx, noise)
		for _, table := range []string{"events_columnstore", "task_context_proposals"} {
			if _, err := st.db.ExecContext(ctx, "ANALYZE TABLE "+table); err != nil {
				t.Fatal(err)
			}
		}
		var version string
		var events, matching int
		if err := st.db.QueryRowContext(ctx, `SELECT @@memsql_version`).Scan(&version); err != nil {
			t.Fatal(err)
		}
		if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events_columnstore WHERE workspace_id=?`, ws).Scan(&events); err != nil {
			t.Fatal(err)
		}
		t.Logf("measurement server=SingleStore %s workspace events=%d noise events=%d", version, events, noise)
		checkDocumentEventPlans(t, st, ctx, ws, ids)
		for _, kind := range []core.LineageNodeType{core.LineageRequirement, core.LineageSystemDesign} {
			id := ids[kind]
			page, err := st.ListDocumentEventPage(ctx, kind, id, store.DocumentEventQuery{Limit: 50})
			if err != nil {
				t.Fatal(err)
			}
			matching = page.Total
			if matching > 100 {
				t.Fatalf("%s has %d matching events; the measurement allows at most 100", kind, matching)
			}
			compareDocumentEventPages(t, st, ctx, ws, kind, id)
			for _, variant := range []struct {
				name  string
				build func(string, core.LineageNodeType, string, store.DocumentEventQuery) (string, []any)
			}{{"before", originalDocumentEventPageQuery}, {"after", documentEventPageQuery}} {
				query, args := variant.build(ws, kind, id, store.DocumentEventQuery{Limit: 50})
				durations := timeDocumentEventQuery(t, st, ctx, query, args, 2, 7)
				t.Logf("measurement %s %s matching=%d warmup=2 runs=%d median=%s min=%s max=%s all=%v", kind, variant.name, matching, len(durations), durations[len(durations)/2], durations[0], durations[len(durations)-1], durations)
				t.Logf("measurement %s %s profile:\n%s", kind, variant.name, profileDocumentEventQuery(t, st, ctx, query, args))
			}
		}
	})
}

func documentPlanWorkspace(t *testing.T, st *Store, ws string) context.Context {
	t.Helper()
	ctx := store.WithActor(store.WithWorkspace(t.Context(), ws), store.SystemActor())
	if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: ws, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	return ctx
}

func documentPlanTask(t *testing.T, st *Store, ctx context.Context, prefix string) string {
	t.Helper()
	ws, _ := store.WorkspaceFromContext(ctx)
	id := prefix + "-" + core.NewTaskID()
	if err := st.CreateTask(ctx, core.Task{ID: id, Workspace: ws, Title: id, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/" + id, State: core.TaskRunning, NextStage: core.StageImplement}); err != nil {
		t.Fatal(err)
	}
	return id
}

// appendDocumentPlanEvents adds matching requirement events on taskID and
// matching System Design consultations, with timestamps tied in threes.
func appendDocumentPlanEvents(t *testing.T, st *Store, ctx context.Context, taskID string, fixture storetest.DocumentEventMembershipFixture, n int, base time.Time) {
	t.Helper()
	for i := range n {
		at := base.Add(time.Duration(i/3) * time.Second)
		for _, event := range []core.Event{
			{TaskID: taskID, Kind: "fixture.activity", At: at, Payload: core.JSONPayload(map[string]any{"index": i})},
			{TaskID: taskID, Kind: "system_design.consulted", At: at, Payload: core.JSONPayload(map[string]any{"document_id": fixture.Design, "index": i})},
		} {
			if err := st.AppendEvent(ctx, event); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// seedDocumentPlanMembership adds enough matching events for multi-page
// queries, a second confirmed task, and System Design events on a task that
// serves no requirement.
func seedDocumentPlanMembership(t *testing.T, st *Store, ctx context.Context, fixture storetest.DocumentEventMembershipFixture) {
	t.Helper()
	second := documentPlanTask(t, st, ctx, "membership-second")
	if _, err := st.ProposeRequirementServes(ctx, second, fixture.Requirement, core.RequirementServesOperator, true); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC)
	appendDocumentPlanEvents(t, st, ctx, fixture.ConfirmedTask, fixture, 15, base)
	appendDocumentPlanEvents(t, st, ctx, second, fixture, 15, base)
	unrelated := documentPlanTask(t, st, ctx, "membership-design-only")
	for i := range 20 {
		if err := st.AppendEvent(ctx, core.Event{TaskID: unrelated, Kind: "system_design.consulted", At: base.Add(time.Duration(i/2) * time.Second), Payload: core.JSONPayload(map[string]any{"document_id": fixture.Design, "requirement_id": fixture.Requirement})}); err != nil {
			t.Fatal(err)
		}
	}
}

func seedDocumentPlanOtherWorkspace(t *testing.T, st *Store, ctx context.Context, fixture storetest.DocumentEventMembershipFixture) {
	t.Helper()
	ctx = store.WithActor(ctx, store.Actor{ID: "fixture", Role: core.ActorUser})
	if _, _, err := st.CreateRequirement(ctx, core.Requirement{ID: fixture.Requirement, Title: "Other " + fixture.Requirement}, core.RequirementVersion{Content: "# Other", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Stay apart."}}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ConfirmRequirementVersion(ctx, fixture.Requirement, 1); err != nil {
		t.Fatal(err)
	}
	task := documentPlanTask(t, st, ctx, "other-workspace")
	if _, err := st.ProposeRequirementServes(ctx, task, fixture.Requirement, core.RequirementServesOperator, true); err != nil {
		t.Fatal(err)
	}
	appendDocumentPlanEvents(t, st, ctx, task, fixture, 5, time.Date(2027, 1, 3, 0, 0, 0, 0, time.UTC))
}

// seedDocumentPlanNoise appends n events through the public append API on
// tasks that serve no document, so the events carry no document ID.
func seedDocumentPlanNoise(t *testing.T, st *Store, ctx context.Context, n int) {
	t.Helper()
	const tasks, workers = 200, 16
	ids := make([]string, tasks)
	for i := range ids {
		ids[i] = documentPlanTask(t, st, ctx, "noise")
	}
	started := time.Now()
	work := make(chan int)
	var wg sync.WaitGroup
	var once sync.Once
	var failure error
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				event := core.Event{TaskID: ids[i%tasks], Kind: "fixture.activity", At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Millisecond), Payload: core.JSONPayload(map[string]any{"index": i})}
				if err := st.AppendEvent(ctx, event); err != nil {
					once.Do(func() { failure = err })
				}
			}
		}()
	}
	for i := range n {
		work <- i
	}
	close(work)
	wg.Wait()
	if failure != nil {
		t.Fatal(failure)
	}
	t.Logf("measurement seeded %d noise events on %d tasks with %d writers in %s", n, tasks, workers, time.Since(started).Round(time.Second))
}

func compareDocumentEventPages(t *testing.T, st *Store, ctx context.Context, ws string, kind core.LineageNodeType, id string) {
	t.Helper()
	first, err := st.ListDocumentEventPage(ctx, kind, id, store.DocumentEventQuery{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	snapshots := []int64{0}
	if first.SnapshotID > 0 {
		snapshots = append(snapshots, first.SnapshotID, first.SnapshotID-1)
	}
	offsets := []int{0, 1, 2, 49, 50, 999}
	if first.Total > 0 {
		offsets = append(offsets, first.Total-1, first.Total, first.Total+1)
	}
	for _, snapshot := range snapshots {
		for _, limit := range []int{1, 50, 200} {
			for _, offset := range offsets {
				q := store.DocumentEventQuery{Limit: limit, Offset: offset, SnapshotID: snapshot}
				query, args := originalDocumentEventPageQuery(ws, kind, id, q)
				want, err := readDocumentEventPage(ctx, st.db, store.DocumentEventPage{Events: []core.Event{}, Limit: limit, Offset: offset}, query, args...)
				if err != nil {
					t.Fatal(err)
				}
				got, err := st.ListDocumentEventPage(ctx, kind, id, q)
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("%s %s query=%+v changed results:\ngot  %s\nwant %s\nerr=%v", kind, id, q, documentPageSummary(got), documentPageSummary(want), err)
				}
			}
		}
	}
}

func documentPageSummary(page store.DocumentEventPage) string {
	ids := make([]int64, 0, len(page.Events))
	for _, e := range page.Events {
		ids = append(ids, e.ID)
	}
	return fmt.Sprintf("total=%d snapshot=%d ids=%v", page.Total, page.SnapshotID, ids)
}

// documentEventScan is one events_columnstore scan of a statement's plan,
// with the column filter SingleStore applies to it.
type documentEventScan struct {
	alias, filter string
}

var documentEventScanAlias = regexp.MustCompile("ColumnStoreScan \\S+\\.events_columnstore AS (\\S+),")

func explainDocumentEventQuery(t *testing.T, st *Store, ctx context.Context, query string, args []any) []string {
	t.Helper()
	rows, err := st.db.QueryContext(ctx, "EXPLAIN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, strings.Split(line, "\n")...)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

// documentEventScans pairs each events_columnstore scan with the
// ColumnStoreFilter line directly above it in the text plan.
func documentEventScans(lines []string) []documentEventScan {
	var scans []documentEventScan
	for i, line := range lines {
		match := documentEventScanAlias.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		scan := documentEventScan{alias: match[1]}
		if i > 0 && strings.Contains(lines[i-1], "ColumnStoreFilter [") {
			scan.filter = lines[i-1]
		}
		scans = append(scans, scan)
	}
	return scans
}

// taskBound reports whether the scan reaches events through a task ID: a
// task_id or event_shard_key join-index lookup or an equality on either key.
func (s documentEventScan) taskBound() bool {
	for _, column := range []string{"task_id", "event_shard_key"} {
		if regexp.MustCompile(regexp.QuoteMeta(s.alias+"."+column) + " = [^ ]+ (join )?index").MatchString(s.filter) {
			return true
		}
	}
	return false
}

// taskless reports whether the scan is bounded to events without a task by
// the (workspace_id, task_id) sort key.
func (s documentEventScan) taskless() bool {
	return strings.Contains(s.filter, s.alias+".task_id IS NULL index")
}

// membership reports whether the scan evaluates a document membership
// predicate rather than fetching selected payloads.
func (s documentEventScan) membership() bool {
	return strings.Contains(s.filter, "JSON_EXTRACT_STRING(") || s.taskBound() || s.taskless()
}

// requirementPlanProblem returns why a requirement statement's plan examines
// events beyond the matching tasks, or "" when every membership scan is bound
// to events without a task or to the task IDs of matching proposals.
func requirementPlanProblem(lines []string) string {
	var task, taskless bool
	for _, scan := range documentEventScans(lines) {
		switch {
		case scan.taskBound():
			task = true
		case scan.taskless():
			taskless = true
		case scan.membership():
			return "membership scan " + scan.alias + " is bound only by workspace: " + strings.TrimSpace(scan.filter)
		}
		// A scan without a membership predicate fetches the selected payloads.
	}
	if !task {
		return "no events scan is bound by a task-ID key"
	}
	if !taskless {
		return "no events scan is bound to events without a task"
	}
	return ""
}

func checkDocumentEventPlans(t *testing.T, st *Store, ctx context.Context, ws string, ids map[core.LineageNodeType]string) {
	t.Helper()
	for _, kind := range []core.LineageNodeType{core.LineageRequirement, core.LineageSystemDesign} {
		for _, variant := range []struct {
			name  string
			build func(string, core.LineageNodeType, string, store.DocumentEventQuery) (string, []any)
		}{{"before", originalDocumentEventPageQuery}, {"after", documentEventPageQuery}} {
			for _, q := range []store.DocumentEventQuery{{Limit: 50}, {Limit: 3, Offset: 2, SnapshotID: 1 << 40}} {
				query, args := variant.build(ws, kind, ids[kind], q)
				lines := explainDocumentEventQuery(t, st, ctx, query, args)
				plan := strings.Join(slices.DeleteFunc(lines, func(line string) bool { return strings.TrimSpace(line) == "" }), "\n")
				t.Logf("%s %s query=%+v plan:\n%s", kind, variant.name, q, plan)
				if kind != core.LineageRequirement {
					continue
				}
				problem := requirementPlanProblem(lines)
				if variant.name == "after" && problem != "" {
					t.Fatalf("requirement statement examines events beyond the matching tasks: %s\n%s", problem, plan)
				}
				// The checker must reject the original statement's plan.
				if variant.name == "before" && problem == "" {
					t.Fatalf("plan check accepted the original statement:\n%s", plan)
				}
			}
		}
	}
}

func timeDocumentEventQuery(t *testing.T, st *Store, ctx context.Context, query string, args []any, warmup, runs int) []time.Duration {
	t.Helper()
	page := store.DocumentEventPage{Events: []core.Event{}}
	var durations []time.Duration
	for i := range warmup + runs {
		started := time.Now()
		if _, err := readDocumentEventPage(ctx, st.db, page, query, args...); err != nil {
			t.Fatal(err)
		}
		if i >= warmup {
			durations = append(durations, time.Since(started).Round(time.Microsecond))
		}
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	return durations
}

// profileDocumentEventQuery runs the statement under PROFILE and returns the
// executed plan with actual row counts and segment statistics.
func profileDocumentEventQuery(t *testing.T, st *Store, ctx context.Context, query string, args []any) string {
	t.Helper()
	conn, err := st.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	rows, err := conn.QueryContext(ctx, "PROFILE "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	var profile []string
	rows, err = conn.QueryContext(ctx, "SHOW PROFILE")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var line sql.NullString
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		profile = append(profile, line.String)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(profile, "\n")
}
