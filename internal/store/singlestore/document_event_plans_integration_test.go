package singlestore

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"os"
	"reflect"
	"regexp"
	"slices"
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

	t.Run("prepared and legacy requirement pages", func(t *testing.T) {
		if !st.hotReadIndexReady("events_requirement_key_idx") {
			t.Skip("requirement index is not prepared")
		}
		for _, id := range []string{fixture.Requirement, "missing-" + core.NewTaskID()} {
			for _, q := range []store.DocumentEventQuery{{Limit: 50}, {Limit: 50, Offset: 50}, {Limit: 20, SnapshotID: 1}} {
				var pages [2]store.DocumentEventPage
				for i, prepared := range []bool{false, true} {
					query, args := documentEventPageQueryForLayout(ws, core.LineageRequirement, id, q, prepared)
					page := store.DocumentEventPage{Events: []core.Event{}, Limit: q.Limit, Offset: q.Offset}
					var err error
					pages[i], err = readDocumentEventPage(ctx, st.db, page, query, args...)
					if err != nil {
						t.Fatal(err)
					}
				}
				if !reflect.DeepEqual(pages[0], pages[1]) {
					t.Fatalf("id=%s query=%+v: legacy=%+v prepared=%+v", id, q, pages[0], pages[1])
				}
			}
		}
	})

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
		prepareDocumentPlanMeasurement(t, st, ctx)
		var version string
		var events int
		if err := st.db.QueryRowContext(ctx, `SELECT @@memsql_version`).Scan(&version); err != nil {
			t.Fatal(err)
		}
		if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events_columnstore WHERE workspace_id=?`, ws).Scan(&events); err != nil {
			t.Fatal(err)
		}
		var count, smallest, largest, sum int64
		var average float64
		if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*),MIN(LENGTH(payload_json)),AVG(LENGTH(payload_json)),MAX(LENGTH(payload_json)),SUM(LENGTH(payload_json)) FROM events_columnstore WHERE workspace_id=? AND task_id LIKE 'noise-%' AND kind='fixture.activity'`, ws).Scan(&count, &smallest, &average, &largest, &sum); err != nil {
			t.Fatal(err)
		}
		if count != int64(noise) || smallest < documentPlanPaddingBytes {
			t.Fatalf("noise events=%d smallest payload=%d bytes; want %d events of at least %d bytes", count, smallest, noise, documentPlanPaddingBytes)
		}
		t.Logf("measurement server=SingleStore %s workspace events=%d noise events=%d noise payload bytes min=%d avg=%.0f max=%d total=%d", version, events, count, smallest, average, largest, sum)
		checkDocumentEventPlans(t, st, ctx, ws, ids)
		for _, kind := range []core.LineageNodeType{core.LineageRequirement, core.LineageSystemDesign} {
			id := ids[kind]
			page, err := st.ListDocumentEventPage(ctx, kind, id, store.DocumentEventQuery{Limit: 50})
			if err != nil {
				t.Fatal(err)
			}
			if page.Total > 100 {
				t.Fatalf("%s has %d matching events; the measurement allows at most 100", kind, page.Total)
			}
			compareDocumentEventPages(t, st, ctx, ws, kind, id)
			for _, variant := range []struct {
				name  string
				build func(string, core.LineageNodeType, string, store.DocumentEventQuery) (string, []any)
			}{{"before", originalDocumentEventPageQuery}, {"after", documentEventPageQuery}} {
				query, args := variant.build(ws, kind, id, store.DocumentEventQuery{Limit: 50})
				durations := timeDocumentEventQuery(t, st, ctx, query, args, 2, 7)
				sorted := slices.Clone(durations)
				slices.Sort(sorted)
				t.Logf("measurement %s %s matching=%d warmup=2 runs=%d median=%s min=%s max=%s in run order=%v", kind, variant.name, page.Total, len(durations), sorted[len(sorted)/2], sorted[0], sorted[len(sorted)-1], durations)
				profile := profileDocumentEventQuery(t, st, ctx, query, args)
				t.Logf("measurement %s %s profile summary:\n%s", kind, variant.name, summarizeDocumentEventProfile(profile))
				t.Logf("measurement %s %s profile:\n%s", kind, variant.name, profile)
				if variant.name == "after" {
					if task, fetch := documentEventPlanProblems(kind, strings.Split(profile, "\n")); task != "" || fetch != "" {
						t.Fatalf("%s executed plan: task problem=%q page fetch problem=%q", kind, task, fetch)
					}
				}
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

func documentPlanTask(t testing.TB, st *Store, ctx context.Context, prefix string) string {
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

// documentPlanPaddingBytes is the least padding each noise event carries, so
// a fetch that reads every workspace payload costs what large events cost.
const documentPlanPaddingBytes = 4096

// documentPlanPadding returns documentPlanPaddingBytes hexadecimal characters
// that differ for each index, so column compression cannot fold them.
func documentPlanPadding(i int) string {
	random := rand.New(rand.NewPCG(uint64(i), 0x5eed))
	raw := make([]byte, documentPlanPaddingBytes/2)
	for j := range raw {
		raw[j] = byte(random.Uint32())
	}
	return hex.EncodeToString(raw)
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
				event := core.Event{TaskID: ids[i%tasks], Kind: "fixture.activity", At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Millisecond), Payload: core.JSONPayload(map[string]any{"index": i, "padding": documentPlanPadding(i)})}
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
	alias, filter, line string
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
// nearest ColumnStoreFilter line above it in a text plan or profile.
func documentEventScans(lines []string) []documentEventScan {
	var scans []documentEventScan
	for i, line := range lines {
		match := documentEventScanAlias.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		scan := documentEventScan{alias: match[1], line: line}
		// PROFILE output adds a per-partition statistics line, such as
		// [total_rows_in: ...], between an operator and its input.
		above := i - 1
		for above > 0 && strings.HasPrefix(strings.TrimLeft(lines[above], "| "), "[total_rows_in:") {
			above--
		}
		if above >= 0 && strings.Contains(lines[above], "ColumnStoreFilter [") {
			scan.filter = lines[above]
		}
		scans = append(scans, scan)
	}
	return scans
}

func (s documentEventScan) keyed(column, value string) bool {
	return regexp.MustCompile(regexp.QuoteMeta(s.alias+"."+column) + " = " + value + " (join )?index").MatchString(s.filter)
}

// taskMembership reports whether the scan reads events through the task IDs
// of matching proposals: a task_id or event_shard_key key lookup by a
// proposal's task_id.
func (s documentEventScan) taskMembership() bool {
	return s.keyed("event_shard_key", `\S+\.task_id`) || s.keyed("task_id", `\S+\.task_id`)
}

// pageLookup reports whether the scan reads selected rows by their stored
// shard key and ID through key lookups, as the page payload fetch must.
func (s documentEventScan) pageLookup() bool {
	return s.keyed("event_shard_key", `\S+\.event_shard_key`) && s.keyed("id", `\S+\.id`)
}

// filterScan reports whether the scan evaluates a payload membership
// predicate. The plan accepts these scans for events without a task and for
// System Design events, which no key covers.
func (s documentEventScan) filterScan() bool {
	return strings.Contains(s.filter, "JSON_EXTRACT_STRING(")
}

func (s documentEventScan) role() string {
	switch {
	case s.taskMembership():
		return "task membership by proposal task ID"
	case s.pageLookup():
		return "page fetch by shard key and ID"
	case s.filterScan():
		return "accepted payload filter scan"
	default:
		return "workspace read without a key lookup"
	}
}

// documentEventPlanProblems returns why a plan reads requirement task events
// beyond the matching tasks, and why its page fetch reads more than the
// selected rows. An empty string means that check passes.
func documentEventPlanProblems(kind core.LineageNodeType, lines []string) (task, fetch string) {
	var membership, lookup bool
	for _, scan := range documentEventScans(lines) {
		switch scan.role() {
		case "task membership by proposal task ID":
			membership = true
		case "page fetch by shard key and ID":
			lookup = true
		case "workspace read without a key lookup":
			if fetch == "" {
				fetch = "event scan " + scan.alias + " reads the workspace without a shard-key and ID lookup: " + strings.TrimSpace(scan.filter)
			}
		}
	}
	if fetch == "" && !lookup {
		fetch = "no event scan looks up the selected rows by shard key and ID"
	}
	if kind == core.LineageRequirement && !membership {
		task = "no event scan is bound to the task IDs of matching proposals"
	}
	return task, fetch
}

// originalMembershipWithPageLookup keeps the original combined membership
// predicate but fetches the page by shard key and ID. Only the task check
// can reject it.
func originalMembershipWithPageLookup(ws string, kind core.LineageNodeType, id string, q store.DocumentEventQuery) (string, []any) {
	query, args := originalDocumentEventPageQuery(ws, kind, id, q)
	head := query[:strings.Index(query, " ), selected AS")]
	return strings.Replace(head, "SELECT e.id,e.at FROM", "SELECT e.id,e.at,e.event_shard_key FROM", 1) + "\n" + documentEventPageSQL, args
}

// idOnlyPageFetch keeps the kind-specific membership but fetches the page by
// workspace and ID alone. Only the fetch check can reject it.
func idOnlyPageFetch(ws string, kind core.LineageNodeType, id string, q store.DocumentEventQuery) (string, []any) {
	query, args := documentEventPageQuery(ws, kind, id, q)
	return strings.Replace(query, "e.event_shard_key=p.event_shard_key AND ", "", 1), args
}

// checkDocumentEventPlans requires the kind-specific statements to pass both
// checks. It also requires the original statement and two variants to fail
// the checks they break, so each check is proven to reject its regression.
func checkDocumentEventPlans(t *testing.T, st *Store, ctx context.Context, ws string, ids map[core.LineageNodeType]string) {
	t.Helper()
	type variant struct {
		name                  string
		build                 func(string, core.LineageNodeType, string, store.DocumentEventQuery) (string, []any)
		taskFails, fetchFails bool
		requirementOnly       bool
	}
	variants := []variant{
		{name: "after", build: documentEventPageQuery},
		{name: "before", build: originalDocumentEventPageQuery, taskFails: true, fetchFails: true},
		{name: "original membership with page lookup", build: originalMembershipWithPageLookup, taskFails: true, requirementOnly: true},
		{name: "kind split with ID-only page fetch", build: idOnlyPageFetch, fetchFails: true},
	}
	if query, _ := idOnlyPageFetch(ws, core.LineageRequirement, "x", store.DocumentEventQuery{Limit: 1}); strings.Contains(query, "e.event_shard_key=p.event_shard_key") {
		t.Fatal("ID-only page fetch variant still joins by shard key")
	}
	for _, kind := range []core.LineageNodeType{core.LineageRequirement, core.LineageSystemDesign} {
		for _, v := range variants {
			if v.requirementOnly && kind != core.LineageRequirement {
				continue
			}
			for _, q := range []store.DocumentEventQuery{{Limit: 50}, {Limit: 3, Offset: 2, SnapshotID: 1 << 40}} {
				query, args := v.build(ws, kind, ids[kind], q)
				lines := explainDocumentEventQuery(t, st, ctx, query, args)
				plan := strings.Join(slices.DeleteFunc(slices.Clone(lines), func(line string) bool { return strings.TrimSpace(line) == "" }), "\n")
				var roles []string
				for _, scan := range documentEventScans(lines) {
					roles = append(roles, scan.alias+": "+scan.role())
				}
				t.Logf("%s %s query=%+v event scans=%v plan:\n%s", kind, v.name, q, roles, plan)
				task, fetch := documentEventPlanProblems(kind, lines)
				wantTask := v.taskFails && kind == core.LineageRequirement
				if (task != "") != wantTask {
					t.Fatalf("%s %s: task check problem=%q, want failure=%v\n%s", kind, v.name, task, wantTask, plan)
				}
				if (fetch != "") != v.fetchFails {
					t.Fatalf("%s %s: page fetch check problem=%q, want failure=%v\n%s", kind, v.name, fetch, v.fetchFails, plan)
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

var documentProfileAlias = regexp.MustCompile(`\] AS (\S+) `)

var documentProfileStat = regexp.MustCompile(`(actual_rows|exec_time|segments_scanned|segments_skipped|network_traffic): [0-9.,]+( KB|ms)?`)

// summarizeDocumentEventProfile lists each events scan with its role and
// statistics, and each data movement operator with its rows and bytes.
func summarizeDocumentEventProfile(profile string) string {
	lines := strings.Split(profile, "\n")
	var out []string
	for _, scan := range documentEventScans(lines) {
		out = append(out, fmt.Sprintf("scan %s (%s): %s", scan.alias, scan.role(), strings.Join(documentProfileStat.FindAllString(scan.line, -1), " ")))
	}
	for _, line := range lines {
		fields := strings.Fields(strings.TrimLeft(line, "| -"))
		if len(fields) == 0 || !slices.Contains([]string{"Repartition", "Broadcast", "Gather"}, fields[0]) {
			continue
		}
		name := fields[0]
		if alias := documentProfileAlias.FindStringSubmatch(line); alias != nil {
			name += " AS " + alias[1]
		}
		out = append(out, fmt.Sprintf("movement %s: %s", name, strings.Join(documentProfileStat.FindAllString(line, -1), " ")))
	}
	return strings.Join(out, "\n")
}

// prepareDocumentPlanMeasurement flushes the in-memory segment, runs the
// default segment merge, and
// collects statistics, so both statements run on the
// same settled layout.
func prepareDocumentPlanMeasurement(t *testing.T, st *Store, ctx context.Context) {
	t.Helper()
	for _, statement := range []string{
		"OPTIMIZE TABLE events_columnstore FLUSH",
		"OPTIMIZE TABLE events_columnstore",
		"ANALYZE TABLE events_columnstore",
		"ANALYZE TABLE task_context_proposals",
	} {
		started := time.Now()
		rows, err := st.db.QueryContext(ctx, statement)
		if err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
		for rows.Next() {
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
		t.Logf("measurement preparation %q took %s", statement, time.Since(started).Round(time.Millisecond))
	}
	var segments, rows int64
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(rows_count),0) FROM information_schema.columnar_segments WHERE database_name=DATABASE() AND table_name='events_columnstore' AND column_name='id'`).Scan(&segments, &rows); err != nil {
		t.Fatal(err)
	}
	t.Logf("measurement preparation events_columnstore segments=%d rows=%d", segments, rows)
}

// TestDocumentEventScanRoles classifies scans in EXPLAIN and PROFILE text
// without a database, including PROFILE's per-partition statistics line.
func TestDocumentEventScanRoles(t *testing.T) {
	plan := []string{
		"|   |   ColumnStoreFilter [(e.workspace_id = ? index) AND events_columnstore_44.event_shard_key = r0.task_id join index AND events_columnstore_44.workspace_id = r0.workspace_id join index] actual_rows: 80",
		"|   |    [total_rows_in: 61,440 | max:36,864 at partition_4]",
		"|   |   ColumnStoreScan db.events_columnstore AS events_columnstore_44, SORT KEY workspace_id (workspace_id, task_id, at, id) actual_rows: 61,440 segments_scanned: 2 segments_skipped: 10",
		"|       ColumnStoreFilter [JSON_EXTRACT_STRING(events_columnstore_42.payload_json,'requirement_id') = ? AND events_columnstore_42.workspace_id = ? index AND events_columnstore_42.task_id IS NULL index]",
		"|       ColumnStoreScan db.events_columnstore AS events_columnstore_42, SORT KEY workspace_id (workspace_id, task_id, at, id)",
		"        ColumnStoreFilter [events_columnstore_50.workspace_id = ? index AND events_columnstore_50.event_shard_key = r5.event_shard_key join index AND events_columnstore_50.id = r5.id join index] actual_rows: 50",
		"         [total_rows_in: 61,440 | max:36,864 at partition_4]",
		"        ColumnStoreScan db.events_columnstore AS events_columnstore_50, SORT KEY workspace_id (workspace_id, task_id, at, id) actual_rows: 61,440",
	}
	var roles []string
	for _, scan := range documentEventScans(plan) {
		roles = append(roles, scan.role())
	}
	want := []string{"task membership by proposal task ID", "accepted payload filter scan", "page fetch by shard key and ID"}
	if !reflect.DeepEqual(roles, want) {
		t.Fatalf("roles=%v want %v", roles, want)
	}
	if task, fetch := documentEventPlanProblems(core.LineageRequirement, plan); task != "" || fetch != "" {
		t.Fatalf("task=%q fetch=%q", task, fetch)
	}
	// An ID-only fetch and a scan without a proposal task bound both fail.
	unbounded := []string{
		"ColumnStoreFilter [JSON_EXTRACT_STRING(events_columnstore_31.payload_json,'requirement_id') = ? AND events_columnstore_31.workspace_id = ? index]",
		"ColumnStoreScan db.events_columnstore AS events_columnstore_31, SORT KEY workspace_id (workspace_id, task_id, at, id)",
		"ColumnStoreFilter [events_columnstore_38.workspace_id = ? index]",
		"ColumnStoreScan db.events_columnstore AS events_columnstore_38, SORT KEY workspace_id (workspace_id, task_id, at, id)",
	}
	if task, fetch := documentEventPlanProblems(core.LineageRequirement, unbounded); task == "" || fetch == "" {
		t.Fatalf("unbounded plan passed: task=%q fetch=%q", task, fetch)
	}
}
