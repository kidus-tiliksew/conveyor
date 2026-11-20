package singlestore

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// markerShapeInput builds a fold input with pages tasks, a review task per
// task, and orders open review orders spread over the first five tasks
// (or every task of a smaller page).
func markerShapeInput(pages, orders int, keepAll bool) store.MarkerFoldInput {
	page := make([]string, pages)
	for i := range page {
		page[i] = fmt.Sprintf("task-%03d", i)
	}
	in := store.MarkerFoldInput{Workspace: "ws", Page: page, ReviewTasks: page, Now: time.Unix(1_800_000_000, 5), KeepAllClaims: keepAll}
	if orders > 0 {
		live := page[:min(5, pages)]
		in.LiveReviewTasks = live
		for i := range orders {
			taskID := live[i%len(live)]
			id := fmt.Sprintf("%s-review-%d", taskID, i)
			in.OpenReviewOrders = append(in.OpenReviewOrders, store.MarkerReviewOrder{TaskID: taskID, ID: id, JobID: id})
		}
	}
	return in
}

// markerShapeJobs is the first job list the lifecycle read of the input
// binds: the jobs of its open review orders, at most markerTaskChunk.
func markerShapeJobs(in store.MarkerFoldInput) []string {
	return markerRenewalJobBatches(in.OpenReviewOrders)[0]
}

// markerTestsNoPayload fails unless the statement names the stored payload
// only inside the compact payload projection: it tests no payload.
func markerTestsNoPayload(t *testing.T, name, query string) {
	t.Helper()
	outside := strings.ReplaceAll(query, markerCompact, "")
	for _, banned := range []string{"payload_json", "INSTR(", "REGEXP", "JSON_EXTRACT"} {
		if strings.Contains(outside, banned) {
			t.Fatalf("%s: holds %q outside the compact payload projection; the statement must test no payload", name, banned)
		}
	}
	if got := strings.Count(query, markerCompact); got != 1 {
		t.Fatalf("%s: projects the compact payload %d times, want once", name, got)
	}
}

// TestMarkerFoldQueryShape freezes the SingleStore narrow fold read as one
// statement text. Page size, open review orders, and KeepAllClaims change
// only bound values, never the text or the number of placeholders, so the
// engine compiles one plan for it (component-web-task-surfaces invariant
// 19). The statement binds one argument per question mark, selects every
// bounce, the forge kinds, and the review lifecycle and review-round kinds,
// and selects no work_order.updated event, claim, or renewal. It tests no
// payload: a text test on a stored payload builds the whole document for
// every row the engine lets reach it, which made this read take seconds per
// chunk on a workspace with a long history.
func TestMarkerFoldQueryShape(t *testing.T) {
	var want string
	wantArgs := -1
	for _, live := range []bool{false, true} {
		for _, keepAll := range []bool{false, true} {
			orderCounts := []int{0}
			if live {
				orderCounts = []int{1, 5, 250}
			}
			for _, pages := range []int{1, 41, 100} {
				for _, orders := range orderCounts {
					name := fmt.Sprintf("live=%t keepAll=%t pages=%d orders=%d", live, keepAll, pages, orders)
					query, args := markerFoldQuery(markerShapeInput(pages, orders, keepAll))
					if got := strings.Count(query, "?"); got != len(args) {
						t.Fatalf("%s: %d question marks for %d arguments", name, got, len(args))
					}
					if want == "" {
						want, wantArgs = query, len(args)
					}
					if query != want || len(args) != wantArgs {
						t.Fatalf("%s: statement text or argument count differs from the first input", name)
					}
				}
			}
		}
	}
	// The workspace, the page list, the review flag, and the review list.
	if wantArgs != 2*markerTaskChunk+2 {
		t.Fatalf("the narrow read binds %d arguments, want %d", wantArgs, 2*markerTaskChunk+2)
	}
	markerTestsNoPayload(t, "narrow read", want)
	for _, kind := range append([]string{"pipeline.bounced", "merge.failed", "review.completed", "work_order.released", "task.setup.changed"}, store.MarkerReviewRoundKinds...) {
		if !strings.Contains(want, "'"+kind+"'") {
			t.Fatalf("the narrow read does not select %s", kind)
		}
	}
	for _, kind := range store.MarkerForgePageKinds() {
		if !strings.Contains(want, "'"+kind+"'") {
			t.Fatalf("the narrow read does not select %s", kind)
		}
	}
	for _, absent := range []string{"'work_order.updated'", "'work_order.claimed'", "'work_order.lease_renewed'", "JSON_KEYS", "JSON_BUILD_OBJECT", "k_lower", "live_task", "JOIN", "UNION", "WITH "} {
		if strings.Contains(want, absent) {
			t.Fatalf("the narrow read holds %s", absent)
		}
	}
}

// TestMarkerLifecycleQueryShape freezes the lifecycle read, which runs only
// for a chunk with an open review order, as two statement texts: reduced
// claims and every claim. The number of tasks with an open review order, the
// number of orders, and the number of their jobs change only bound values.
// The statement reads only claims and renewals, and selects them by bound
// lists on columns, so its payload tests meet only those rows: both the rows
// it reduces and the rows it matches against open review order IDs.
//
// Renewals are the one marker kind whose count follows how long a claim was
// held: on a production workspace one task held 16,000, all but a few dozen
// from verify claims. So the statement must select a renewal only when its
// job is in the bound list of the open review orders' jobs, a test on the
// job column inside marker_rows, before any payload test can meet the row.
// With the task list alone the read decoded every renewal of each task with
// an open review order and took 27 seconds for ten such tasks; a join to the
// open review orders in place of the bound list still built every renewal's
// payload first.
func TestMarkerLifecycleQueryShape(t *testing.T) {
	texts := map[string]bool{}
	for _, keepAll := range []bool{false, true} {
		var want string
		wantArgs := -1
		for _, pages := range []int{1, 41, 100} {
			for _, orders := range []int{1, 5, 250} {
				name := fmt.Sprintf("keepAll=%t pages=%d orders=%d", keepAll, pages, orders)
				in := markerShapeInput(pages, orders, keepAll)
				for batch, jobs := range append(markerRenewalJobBatches(in.OpenReviewOrders), nil) {
					for _, kinds := range []markerLifecycleKinds{markerClaimsAndRenewals, markerRenewalsOnly} {
						query, args := markerLifecycleQuery(in, jobs, kinds)
						if got := strings.Count(query, "?"); got != len(args) {
							t.Fatalf("%s batch %d: %d question marks for %d arguments", name, batch, got, len(args))
						}
						if want == "" {
							want, wantArgs = query, len(args)
						}
						if query != want || len(args) != wantArgs {
							t.Fatalf("%s batch %d: statement text or argument count differs from the first input of its variant", name, batch)
						}
					}
				}
			}
		}
		texts[want] = true
		name := fmt.Sprintf("keepAll=%t", keepAll)
		outside := strings.ReplaceAll(want, markerCompact, "")
		if strings.Contains(outside, "governance_snapshot'") || strings.Contains(outside, "served_requirement_snapshot'") {
			t.Fatalf("%s: names a snapshot body key outside the compact payload", name)
		}
		for _, absent := range []string{"'pipeline.bounced'", "'work_order.updated'", "'merge.failed'", "'review.completed'", "JSON_KEYS", "JSON_BUILD_OBJECT", "k_lower", "live_task"} {
			if strings.Contains(want, absent) {
				t.Fatalf("%s: holds %s", name, absent)
			}
		}
		// marker_rows is one selection by the bound task list, the two
		// lifecycle kinds, and for a renewal the bound job list: the
		// workspace, the tasks, the claim flag, the job flag, and the jobs.
		rows := want[strings.Index(want, "marker_rows AS (")+len("marker_rows AS ("):]
		rows = rows[:strings.Index(rows, "marker_clock AS")]
		if strings.Count(rows, "FROM events_columnstore e") != 1 || strings.Count(rows, "?") != 2*markerTaskChunk+3 ||
			!strings.Contains(rows, "e.kind IN ('work_order.claimed','work_order.lease_renewed')") || strings.Contains(strings.ReplaceAll(rows, markerCompact, ""), "payload_json") {
			t.Fatalf("%s: marker_rows is not one selection by task list, lifecycle kind, and job list: %s", name, rows)
		}
		where := rows[strings.Index(rows, "WHERE"):]
		for _, banned := range []string{"SELECT", "JOIN", "UNION", "INSTR(", "REGEXP", "JSON_"} {
			if strings.Contains(where, banned) {
				t.Fatalf("%s: the row predicate of marker_rows holds %q; it must test columns only:\n%s", name, banned, where)
			}
		}
		// The only disjunction admits the claims, when the execution reads
		// them, and of the renewals the ones on a bound job. A renewal has
		// no other way in.
		if flat := strings.Join(strings.Fields(where), " "); strings.Count(flat, " OR ") != 1 ||
			!strings.Contains(flat, "AND ((e.kind='work_order.claimed' AND ?=1) OR (e.kind='work_order.lease_renewed' AND ?=1 AND e.job_id IN (?") {
			t.Fatalf("%s: marker_rows does not select renewals by the bound job list alone:\n%s", name, where)
		}
		if strings.Contains(want, "JSON_TO_ARRAY(?))) o ON e.task_id=o.order_task AND e.job_id") {
			t.Fatalf("%s: renewals are selected by a join to the open review orders; the job list must be bound on the job column", name)
		}
		// The lease decoder runs once, on renewals only, and both arms read
		// marker_rows: with the renewal arm alone the statement took more
		// than 30 seconds to compile (TestMarkerFoldCompileIntegration).
		if got := strings.Count(want, "AS lease_ok"); got != 1 {
			t.Fatalf("%s: decodes leases %d times, want once", name, got)
		}
		if !strings.Contains(want, "FROM marker_rows WHERE kind='work_order.lease_renewed'") || !strings.Contains(want, "WHERE kind='work_order.claimed'") || strings.Count(want, "UNION ALL") != 1 {
			t.Fatalf("%s: the statement is not a renewal arm and a claim arm over marker_rows", name)
		}
		if keepAll {
			if strings.Contains(want, "reducible") || strings.Contains(want, "INSTR(e.payload_json") {
				t.Fatalf("%s: the every-claim statement still reduces or matches claims", name)
			}
			continue
		}
		if !strings.Contains(want, "TABLE(JSON_TO_ARRAY(?))") {
			t.Fatalf("%s: does not bind the open review orders as one JSON array", name)
		}
		// The one stored-payload test, the open review order ID match,
		// follows the bound task list in its own selection.
		if got := strings.Count(want, "INSTR(e.payload_json"); got != 1 {
			t.Fatalf("%s: %d stored-payload tests, want 1", name, got)
		}
		named := want[strings.Index(want, "named_claims AS ("):]
		named = named[:strings.Index(named, "INSTR(e.payload_json")]
		if !strings.Contains(named, "e.task_id IN (?") || strings.Count(named, "?") < markerTaskChunk+2 {
			t.Fatalf("%s: the open review order match binds no task list before its payload test", name)
		}
	}
	if len(texts) != 2 {
		t.Fatalf("the lifecycle read produces %d statement texts, want 2", len(texts))
	}
}

// TestMarkerRenewalJobBatches proves which jobs the lifecycle read binds for
// renewals: the job of each open review order once, in order, in lists of at
// most markerTaskChunk jobs. An open review order without a job matches no
// renewal, so it gives no job. There is always one list, so the claims are
// read even when no open review order has a job.
func TestMarkerRenewalJobBatches(t *testing.T) {
	for name, orders := range map[string][]store.MarkerReviewOrder{
		"no open review order":     nil,
		"an order without a job":   {{TaskID: "a", ID: "a-review"}},
		"two orders without a job": {{TaskID: "a", ID: "a-review"}, {TaskID: "b", ID: "b-review"}},
	} {
		if got := markerRenewalJobBatches(orders); len(got) != 1 || len(got[0]) != 0 {
			t.Fatalf("%s gives %v, want one empty list", name, got)
		}
	}
	batches := markerRenewalJobBatches([]store.MarkerReviewOrder{
		{TaskID: "b", ID: "b-1", JobID: "b-job-1"},
		{TaskID: "a", ID: "a-1", JobID: "a-job-1"},
		{TaskID: "b", ID: "b-2", JobID: "b-job-2"},
		// A second order of one job, and an order without a job.
		{TaskID: "b", ID: "b-3", JobID: "b-job-1"},
		{TaskID: "c", ID: "c-1"},
	})
	if len(batches) != 1 || strings.Join(batches[0], ",") != "b-job-1,a-job-1,b-job-2" {
		t.Fatalf("batches = %v, want one of b-job-1,a-job-1,b-job-2", batches)
	}
	// Three seats per task over a full chunk of tasks: more jobs than one
	// statement binds.
	var orders []store.MarkerReviewOrder
	for task := range markerTaskChunk {
		for seat := range 3 {
			id := fmt.Sprintf("task-%03d-review-1-seat-%d", task, seat)
			orders = append(orders, store.MarkerReviewOrder{TaskID: fmt.Sprintf("task-%03d", task), ID: id, JobID: id})
		}
	}
	batches = markerRenewalJobBatches(orders)
	if len(batches) != 3 {
		t.Fatalf("%d jobs give %d lists, want 3", len(orders), len(batches))
	}
	seen := map[string]bool{}
	for index, batch := range batches {
		if len(batch) != markerTaskChunk {
			t.Fatalf("list %d holds %d jobs, want %d", index, len(batch), markerTaskChunk)
		}
		for _, job := range batch {
			if seen[job] {
				t.Fatalf("job %s is bound twice", job)
			}
			seen[job] = true
		}
	}
	if len(seen) != len(orders) {
		t.Fatalf("the lists bind %d jobs, want %d", len(seen), len(orders))
	}
	// One more job than a full list leaves a list of one.
	if batches = markerRenewalJobBatches(orders[:markerTaskChunk+1]); len(batches) != 2 || len(batches[1]) != 1 {
		t.Fatalf("%d jobs give lists %v, want a full list and a list of one", markerTaskChunk+1, batches)
	}
}

// TestMarkerWideQueryShape freezes the two wide reads as one statement text
// each, whatever the number of tasks. A wide read is the only statement that
// carries a work_order.updated payload or a claim payload of a task without
// an open review order, each a whole work order. Neither tests a payload:
// the submission read selects by workspace, bound task list, and kind, and
// the request claim read by workspace, bound task list, kind, and one
// instant. Go applies the exact tests.
func TestMarkerWideQueryShape(t *testing.T) {
	tasksOf := func(size int) []string {
		tasks := make([]string, size)
		for i := range tasks {
			tasks[i] = fmt.Sprintf("task-%03d", i)
		}
		return tasks
	}
	since := func(i int) time.Time { return time.Date(2026, 11, 19, 12, i, 0, 0, time.UTC) }
	needsOf := func(size int) []markerClaimNeed {
		needs := make([]markerClaimNeed, size)
		for i, id := range tasksOf(size) {
			needs[i] = markerClaimNeed{taskID: id, since: since(i)}
		}
		return needs
	}

	var submissions string
	for _, size := range []int{1, 7, markerTaskChunk} {
		tasks := tasksOf(size)
		query, args := markerSubmissionQuery("ws", tasks)
		if submissions == "" {
			submissions = query
		} else if query != submissions {
			t.Fatalf("submissions: %d tasks changed the statement text", size)
		}
		if want := markerTaskChunk + 1; len(args) != want || strings.Count(query, "?") != want {
			t.Fatalf("submissions: %d tasks bind %d arguments and %d placeholders, want %d", size, len(args), strings.Count(query, "?"), want)
		}
		wantPad := tasks[0]
		if size == markerTaskChunk {
			wantPad = tasks[size-1]
		}
		if args[0] != "ws" || args[size] != tasks[size-1] || args[markerTaskChunk] != wantPad {
			t.Fatalf("submissions: %d tasks bind %v", size, args)
		}
	}
	markerTestsNoPayload(t, "submissions", submissions)
	where := submissions[strings.Index(submissions, "WHERE"):]
	for _, banned := range []string{" OR ", "SELECT", "JOIN", "UNION"} {
		if strings.Contains(where, banned) {
			t.Fatalf("submissions: predicate holds %q; it must be plain conjuncts", strings.TrimSpace(banned))
		}
	}
	for _, want := range []string{"e.workspace_id=?", "e.task_id IN (?", "e.kind='work_order.updated'"} {
		if !strings.Contains(where, want) {
			t.Fatalf("submissions: predicate does not hold %q", want)
		}
	}

	var claims string
	for _, size := range []int{1, 7, markerTaskChunk} {
		needs := needsOf(size)
		// The earliest instant is not the first task's.
		earliest := size / 2
		needs[earliest].since = since(0).Add(-time.Hour)
		query, args := markerRequestClaimQuery("ws", needs)
		if claims == "" {
			claims = query
		} else if query != claims {
			t.Fatalf("request claims: %d tasks changed the statement text", size)
		}
		// The workspace, the task list, and the one instant.
		if want := markerTaskChunk + 2; len(args) != want || strings.Count(query, "?") != want {
			t.Fatalf("request claims: %d tasks bind %d arguments and %d placeholders, want %d", size, len(args), strings.Count(query, "?"), want)
		}
		if args[0] != "ws" || args[size] != needs[size-1].taskID || args[len(args)-1] != needs[earliest].since {
			t.Fatalf("request claims: %d tasks bind %v, want the task list and the earliest instant %s", size, args, needs[earliest].since)
		}
	}
	markerTestsNoPayload(t, "request claims", claims)
	where = claims[strings.Index(claims, "WHERE"):]
	// Plain conjuncts only: a disjunction of one task and instant pair per
	// task took SingleStore more than 30 seconds to compile.
	for _, banned := range []string{" OR ", "SELECT", "JOIN", "UNION"} {
		if strings.Contains(where, banned) {
			t.Fatalf("request claims: predicate holds %q; it must be plain conjuncts", strings.TrimSpace(banned))
		}
	}
	for _, want := range []string{"e.workspace_id=?", "e.task_id IN (?", "e.kind='work_order.claimed'", "e.at>=?"} {
		if !strings.Contains(where, want) {
			t.Fatalf("request claims: predicate does not hold %q", want)
		}
	}
}

// TestMarkerWideNeeds proves which wide rows a chunk reads. A task reads its
// submissions only when it has a merge failure. A task reads its claims only
// when a bounce decodes as a user change request, from the instant of its
// own last such request; a bounce that only holds the source text elsewhere,
// and a task with an open review order, call for no claim read.
func TestMarkerWideNeeds(t *testing.T) {
	at := func(minute int) time.Time { return time.Date(2026, 11, 19, 12, minute, 0, 0, time.UTC) }
	row := func(task, kind, payload string, minute int) store.MarkerFoldRow {
		return store.MarkerFoldRow{Event: core.Event{TaskID: task, Kind: kind, Payload: json.RawMessage(payload), At: at(minute)}}
	}
	const user = `{"source":"user-request-changes"}`
	in := store.MarkerFoldInput{Workspace: "ws", Page: []string{"a", "b", "c", "d", "e", "f"}, LiveReviewTasks: []string{"e"}}
	needs := markerWideNeedsOf(in, []store.MarkerFoldRow{
		row("a", "merge.failed", `{}`, 1),
		row("a", "merge.confirmed", `{}`, 2),
		row("b", "pipeline.bounced", user, 9),
		row("b", "pipeline.bounced", user, 5),
		row("b", "pipeline.bounced", `{"source":"review","feedback":"user-request-changes"}`, 30),
		row("c", "pipeline.bounced", `{"source":"review","feedback":"\\"user-request-changes\\""}`, 3),
		row("d", "pipeline.bounced", user, 7),
		row("d", "merge.failed", `not json`, 8),
		row("e", "pipeline.bounced", user, 4),
		row("f", "review.completed", `{}`, 6),
	})
	if got := strings.Join(needs.submissionTasks, ","); got != "a,d" {
		t.Fatalf("submission tasks = %s, want a,d", got)
	}
	if len(needs.claims) != 2 || needs.claims[0] != (markerClaimNeed{taskID: "b", since: at(9)}) || needs.claims[1] != (markerClaimNeed{taskID: "d", since: at(7)}) {
		t.Fatalf("claim needs = %+v, want b from minute 9 and d from minute 7", needs.claims)
	}
	if needs := markerWideNeedsOf(in, nil); needs.submissionTasks != nil || needs.claims != nil {
		t.Fatalf("a chunk without narrow rows needs %+v, want nothing", needs)
	}
}

// TestMarkerFoldQueryBinds checks the padded lists and the bound order
// array: a page pads with its first task, an empty review list binds the
// empty flag and pads with the empty string, and the open review orders
// travel as one JSON value that marks an ID outside the safe alphabet.
func TestMarkerFoldQueryBinds(t *testing.T) {
	in := markerShapeInput(3, 0, false)
	in.ReviewTasks = nil
	_, args := markerFoldQuery(in)
	count := func(value any) int {
		n := 0
		for _, arg := range args {
			if arg == value {
				n++
			}
		}
		return n
	}
	if got := count("task-000"); got != markerTaskChunk-2 {
		t.Fatalf("the page binds its first task %d times, want %d", got, markerTaskChunk-2)
	}
	if got := count(""); got != markerTaskChunk {
		t.Fatalf("the empty review list binds %d empty strings, want %d", got, markerTaskChunk)
	}
	if count(int64(0)) != 1 || count(int64(1)) != 0 {
		t.Fatal("the empty review list does not bind its empty flag")
	}

	in = markerShapeInput(3, 2, false)
	in.OpenReviewOrders = append(in.OpenReviewOrders, store.MarkerReviewOrder{TaskID: "task-001", ID: `odd"id`, JobID: "job-odd"})
	_, args = markerLifecycleQuery(in, markerShapeJobs(in), markerClaimsAndRenewals)
	// The claim flag, the job flag, and the job list follow the task list.
	// The job list pads with its first job, so the padding matches no other
	// job's renewals.
	jobs := args[1+markerTaskChunk : 3+2*markerTaskChunk]
	if jobs[0] != int64(1) || jobs[1] != int64(1) || jobs[2] != "task-000-review-0" || jobs[3] != "task-001-review-1" || jobs[4] != "job-odd" || jobs[5] != "task-000-review-0" || jobs[1+markerTaskChunk] != "task-000-review-0" {
		t.Fatalf("the lifecycle read binds the claim flag, the job flag, and the job list as %v", jobs[:7])
	}
	// Without a job the list binds the empty flag and pads with the empty
	// string, so the statement selects no renewal. An execution for
	// renewals only binds the empty claim flag.
	_, noJobArgs := markerLifecycleQuery(in, nil, markerRenewalsOnly)
	if noJobs := noJobArgs[1+markerTaskChunk : 3+2*markerTaskChunk]; noJobs[0] != int64(0) || noJobs[1] != int64(0) || noJobs[2] != "" || noJobs[1+markerTaskChunk] != "" {
		t.Fatalf("an empty job list for renewals only binds %v, want two empty flags and empty strings", noJobs[:4])
	}
	var orders []struct {
		TaskID string `json:"task_id"`
		JobID  string `json:"job_id"`
		ID     string `json:"id"`
		Safe   int    `json:"safe"`
	}
	for _, arg := range args {
		if text, ok := arg.(string); ok && strings.HasPrefix(text, "[{") {
			if err := json.Unmarshal([]byte(text), &orders); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(orders) != 3 || orders[0].Safe != 1 || orders[2].Safe != 0 || orders[2].ID != `odd"id` {
		t.Fatalf("bound open review orders = %+v, want three with only the odd ID unsafe", orders)
	}
}

// TestMarkerPastLeaseFastPath pins the lexical claim fast path: it reads only
// plain ASCII text without a backslash, requires the lowercased text to hold
// the quoted lease key as often as the text itself, so a case-folded variant
// of the key keeps the claim, accepts only the exact UTC shape, and compares
// whole seconds strictly in binary order.
func TestMarkerPastLeaseFastPath(t *testing.T) {
	b := &markerBinder{}
	sql, args := b.finish(markerPastLease(b, b.bind("2026-11-19T12:00:00")))
	for _, want := range []string{
		"LENGTH(event_payload)=CHAR_LENGTH(event_payload)",
		"INSTR(event_payload,CHAR(92))=0",
		"LENGTH(REPLACE(CAST(LOWER(event_payload) AS BINARY)," + markerLeaseKey + ",''))=LENGTH(REPLACE(CAST(event_payload AS BINARY)," + markerLeaseKey + ",''))",
		markerUTCLeaseShape,
		"ASCII(SUBSTRING(JSON_EXTRACT_STRING(event_payload,'lease_expires_at'),11,1))=84",
		"CAST(LEFT(JSON_EXTRACT_STRING(event_payload,'lease_expires_at'),19) AS BINARY)<CAST(? AS BINARY)",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("fast path does not hold %q", want)
		}
	}
	if len(args) != 1 || strings.Count(sql, "?") != 1 {
		t.Fatalf("fast path binds %v", args)
	}
	if strings.Contains(markerUTCLeaseShape, "?") {
		t.Fatal("the lease shape holds a question mark, which the driver reads as a placeholder")
	}
}

// TestMarkerFoldFixturesMatchSource checks the pinned v0.72.0 statement
// against its source: the activity-marker read at 3062eb520 kept in the
// egress baseline emits the same text as tag v0.72.0. The pre-fix fixture
// was captured by running markerFoldQuery at 95133fd2 on markerCompileInput.
func TestMarkerFoldFixturesMatchSource(t *testing.T) {
	fixture := loadMarkerCompileFixture(t, "marker_fold_v0.72.0.json")
	for _, shape := range []struct {
		name string
		live bool
	}{{"no_live", false}, {"live", true}} {
		in := markerCompileInput(shape.live, false)
		query, args := legacyMarkerFoldQuery(in.Workspace, in.Page, in.ReviewTasks, in.LiveReviewTasks, in.Now)
		if query != fixture[shape.name].Query || len(args) != len(fixture[shape.name].Args) {
			t.Fatalf("the v0.72.0 %s fixture differs from its source", shape.name)
		}
	}
	prefix := loadMarkerCompileFixture(t, "marker_fold_95133fd2.json")
	for _, shape := range []string{"no_live", "live"} {
		if !strings.Contains(prefix[shape].Query, "JSON_KEYS(") {
			t.Fatalf("the pre-fix %s fixture is not the per-key statement", shape)
		}
	}
}

// TestMarkerProjectPayload proves the Go projection sends a kind's fold keys
// with their raw values only when a top-level key scan proves Go decodes
// exactly those values, and keeps the compact payload otherwise.
func TestMarkerProjectPayload(t *testing.T) {
	for _, tc := range []struct {
		name, kind, payload, want string
	}{
		{"projected", "pipeline.bounced", `{"source":"user-request-changes","feedback":"long text"}`, `{"source":"user-request-changes"}`},
		{"raw values and types kept", "merge.failed", `{"error":"say \"no\"","automatic":true,"head_sha":null,"x":1}`, `{"error":"say \"no\"","automatic":true,"head_sha":null}`},
		{"missing key omitted", "review.round_completed", `{"other":1}`, `{}`},
		{"case-variant key", "pipeline.bounced", `{"Source":"user-request-changes"}`, `{"Source":"user-request-changes"}`},
		{"exact and case-variant key", "pipeline.bounced", `{"source":"a","SOURCE":"b"}`, `{"source":"a","SOURCE":"b"}`},
		{"non-ASCII key", "pipeline.bounced", "{\"ſource\":\"user-request-changes\"}", "{\"ſource\":\"user-request-changes\"}"},
		{"duplicate key", "pipeline.bounced", `{"source":"a","source":"b"}`, `{"source":"a","source":"b"}`},
		{"escaped key decodes to the field", "pipeline.bounced", `{"\u0073ource":"x","feedback":"omit"}`, `{"source":"x"}`},
		{"not an object", "pipeline.bounced", `["source"]`, `["source"]`},
		{"trailing data", "pipeline.bounced", `{"source":"x"} {}`, `{"source":"x"} {}`},
		{"claims keep the compact payload", "work_order.claimed", `{"id":"o","stage":"implement","extra":1}`, `{"id":"o","stage":"implement","extra":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(markerProjectPayload(tc.kind, json.RawMessage(tc.payload))); got != tc.want {
				t.Fatalf("projection = %s, want %s", got, tc.want)
			}
		})
	}
	// Every projected kind has a key list, and claims have none.
	if markerProjectionKeys["work_order.claimed"] != nil {
		t.Fatal("claims must keep their whole compact payload")
	}
	for _, kind := range store.MarkerReviewRoundKinds {
		if _, ok := markerProjectionKeys[kind]; !ok {
			t.Fatalf("%s has no projection", kind)
		}
	}
}

// TestMarkerLatestSummariesQueryShape proves the latest-summary read of every
// chunk size is one statement text with one fixed-width list per task
// column, so SingleStore compiles one plan for it, and that it groups each
// table once and joins the groups to the task rows instead of carrying one
// arm per task.
func TestMarkerLatestSummariesQueryShape(t *testing.T) {
	var text string
	for _, size := range []int{1, 7, markerTaskChunk} {
		ids := make([]string, size)
		for i := range ids {
			ids[i] = fmt.Sprintf("task-%03d", i)
		}
		query, args := markerLatestSummariesQuery("ws", ids)
		if text == "" {
			text = query
		} else if query != text {
			t.Fatalf("chunk of %d tasks changed the statement text", size)
		}
		// Four event lists, one job list, one claim list, and one task list,
		// each with its workspace.
		if want := 7*markerTaskChunk + 5; len(args) != want || strings.Count(query, "?") != want {
			t.Fatalf("chunk of %d tasks binds %d arguments and %d placeholders, want %d", size, len(args), strings.Count(query, "?"), want)
		}
		for i, arg := range args {
			if s, ok := arg.(string); ok && s != "ws" && !strings.HasPrefix(s, "task-") {
				t.Fatalf("argument %d is %q", i, s)
			}
		}
	}
	if strings.Count(text, "LEFT JOIN") != 3 || strings.Count(text, "GROUP BY") != 2 || strings.Contains(text, "UNION") || strings.Contains(text, "ON 1=1") {
		t.Fatalf("latest-summary read is not three grouped reads joined to the task rows:\n%s", text)
	}
}
