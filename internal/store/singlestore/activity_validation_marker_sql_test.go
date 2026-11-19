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

// TestMarkerFoldQueryShape freezes the SingleStore narrow fold read as three
// statement texts: one without an open review order, where the claim
// reduction does not apply, and with one either reduced claims or every
// claim. Page size and the number of open review orders change only bound
// values, never the text or the number of placeholders, so the engine
// compiles at most three plans for it (component-web-task-surfaces
// invariant 19). Every statement binds one argument per question mark, names
// no snapshot body key outside the compact payload, selects the review-round
// kinds, and carries no per-key JSON proof or projection CASE. No statement
// selects a work_order.updated event. A statement tests a stored payload
// only in two places: the bounce source test, which follows a kind list that
// admits no claim, and the match of an open review order's ID against the
// claims of the bound tasks that have one. The wide reads own every other
// payload test (TestMarkerWideQueryShape).
func TestMarkerFoldQueryShape(t *testing.T) {
	texts := map[string]int{}
	for _, live := range []bool{false, true} {
		for _, keepAll := range []bool{false, true} {
			var want string
			wantArgs := -1
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
						t.Fatalf("%s: statement text or argument count differs from the first input of its variant", name)
					}
					outside := strings.ReplaceAll(query, markerCompact, "")
					if strings.Contains(outside, "governance_snapshot'") || strings.Contains(outside, "served_requirement_snapshot'") {
						t.Fatalf("%s: names a snapshot body key outside the compact payload", name)
					}
					for _, kind := range store.MarkerReviewRoundKinds {
						if !strings.Contains(query, "'"+kind+"'") {
							t.Fatalf("%s: does not select %s", name, kind)
						}
					}
					for _, retired := range []string{"JSON_KEYS", "JSON_BUILD_OBJECT", "k_lower", "live_task"} {
						if strings.Contains(query, retired) {
							t.Fatalf("%s: still holds %s", name, retired)
						}
					}
					if strings.Contains(query, "'work_order.updated'") {
						t.Fatalf("%s: the narrow read selects work_order.updated", name)
					}
					if !live && strings.Contains(query, "'work_order.claimed'") {
						t.Fatalf("%s: the narrow read selects claims without an open review order", name)
					}
					// The bounce source test follows a kind list without
					// claims; the open review order ID match follows a bound
					// list of the tasks that have such an order.
					bounce := strings.Index(query, `INSTR(e.payload_json,'"user-request-changes"')`)
					if bounce < 0 {
						t.Fatalf("%s: no bounce source test", name)
					}
					kinds := query[:bounce]
					kinds = kinds[strings.LastIndex(kinds, "e.kind IN ("):]
					kinds = kinds[:strings.Index(kinds, ")")]
					if strings.Contains(kinds, "work_order.claimed") || strings.Contains(kinds, "work_order.updated") || !strings.Contains(kinds, "'pipeline.bounced'") {
						t.Fatalf("%s: the kind list ahead of the bounce test is %s", name, kinds)
					}
					wantPayloadTests := 1
					if live && !keepAll {
						wantPayloadTests = 2
					}
					if got := strings.Count(query, "INSTR(e.payload_json"); got != wantPayloadTests {
						t.Fatalf("%s: %d stored-payload tests, want %d", name, got, wantPayloadTests)
					}
					if wantPayloadTests == 2 {
						named := query[strings.Index(query, "named_claims AS ("):]
						named = named[:strings.Index(named, "INSTR(e.payload_json")]
						if got := strings.Count(named, "?"); got < markerTaskChunk {
							t.Fatalf("%s: the open review order match binds no task list before its payload test", name)
						}
					}
				}
			}
			texts[want]++
			if live && !keepAll && !strings.Contains(want, "TABLE(JSON_TO_ARRAY(?))") {
				t.Fatal("the reduced open-review statement does not bind the open review orders as one JSON array")
			}
			if (keepAll || !live) && strings.Contains(want, "reducible") {
				t.Fatalf("the statement (live=%t keepAll=%t) still reduces claims", live, keepAll)
			}
		}
	}
	if len(texts) != 3 {
		t.Fatalf("the fold read produces %d statement texts, want 3", len(texts))
	}
	// The lease decoder runs once per statement, on renewals only.
	query, _ := markerFoldQuery(markerShapeInput(100, 5, false))
	if got := strings.Count(query, "AS lease_ok"); got != 1 {
		t.Fatalf("the open-review statement decodes leases %d times, want once", got)
	}
}

// TestMarkerWideQueryShape freezes the two wide reads as one statement text
// each, whatever the number of tasks. A wide read is the only statement that
// tests or carries a work_order.updated payload or a claim payload of a task
// without an open review order, each 100 KB and more with its snapshots.
// SingleStore reads such a payload for every row that reaches the predicate
// naming it, so each wide read must select its rows by plain conjuncts: the
// workspace, a bound task list, one kind, and for claims the instant. It
// must hold no disjunction, no subquery, and no join, because each of those
// makes the engine read the payload of every row of the kind.
func TestMarkerWideQueryShape(t *testing.T) {
	since := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	for _, wide := range []struct {
		name, kind string
		binds      int
		build      func(tasks []string) (string, []any)
	}{
		{"submissions", "work_order.updated", markerTaskChunk + 1, func(tasks []string) (string, []any) { return markerSubmissionQuery("ws", tasks) }},
		{"request claims", "work_order.claimed", markerTaskChunk + 2, func(tasks []string) (string, []any) { return markerRequestClaimQuery("ws", tasks, since) }},
	} {
		var text string
		for _, size := range []int{1, 7, markerTaskChunk} {
			tasks := make([]string, size)
			for i := range tasks {
				tasks[i] = fmt.Sprintf("task-%03d", i)
			}
			query, args := wide.build(tasks)
			if text == "" {
				text = query
			} else if query != text {
				t.Fatalf("%s: %d tasks changed the statement text", wide.name, size)
			}
			if len(args) != wide.binds || strings.Count(query, "?") != wide.binds {
				t.Fatalf("%s: %d tasks bind %d arguments and %d placeholders, want %d", wide.name, size, len(args), strings.Count(query, "?"), wide.binds)
			}
			if got := args[1+size-1]; got != tasks[size-1] {
				t.Fatalf("%s: task list does not bind its last task: %v", wide.name, got)
			}
			if size < markerTaskChunk && args[markerTaskChunk] != tasks[0] {
				t.Fatalf("%s: task list is not padded with its first task", wide.name)
			}
		}
		where := text[strings.Index(text, "WHERE"):]
		for _, banned := range []string{" OR ", "SELECT", "JOIN", "UNION"} {
			if strings.Contains(where, banned) {
				t.Fatalf("%s: predicate holds %q; it must be plain conjuncts", wide.name, strings.TrimSpace(banned))
			}
		}
		for _, want := range []string{"e.workspace_id=?", "e.task_id IN (?", "e.kind='" + wide.kind + "'", `INSTR(e.payload_json,'"implement"')>0`} {
			if !strings.Contains(where, want) {
				t.Fatalf("%s: predicate does not hold %q", wide.name, want)
			}
		}
		// The bound task list precedes every payload test.
		if strings.Index(where, "e.task_id IN (?") > strings.Index(where, "INSTR(e.payload_json") {
			t.Fatalf("%s: a payload test precedes the bound task list", wide.name)
		}
		outside := strings.ReplaceAll(text, markerCompact, "")
		if strings.Contains(outside, "governance_snapshot'") || strings.Contains(outside, "served_requirement_snapshot'") {
			t.Fatalf("%s: names a snapshot body key outside the compact payload", wide.name)
		}
	}
	submissions, _ := markerSubmissionQuery("ws", []string{"task-000"})
	if !strings.Contains(submissions, `INSTR(e.payload_json,'"submitted"')>0`) {
		t.Fatal("the submission read does not test for the submitted state")
	}
	claims, args := markerRequestClaimQuery("ws", []string{"task-000"}, since)
	if !strings.Contains(claims, "e.at>=?") || args[len(args)-1] != since {
		t.Fatal("the request claim read does not bind its instant")
	}
}

// TestMarkerWideNeeds proves which wide rows a chunk reads. A task reads its
// submissions only when it has a merge failure. A task reads its claims only
// when a bounce decodes as a user change request, from the instant of its
// last such request; a bounce that only holds the source text elsewhere, and
// a task with an open review order, call for no claim read. The claim
// instant is the earliest of the listed tasks' last requests.
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
		row("b", "pipeline.bounced", user, 5),
		row("b", "pipeline.bounced", user, 9),
		row("b", "pipeline.bounced", `{"source":"review","feedback":"user-request-changes"}`, 30),
		row("c", "pipeline.bounced", `{"source":"review","feedback":"\"user-request-changes\""}`, 3),
		row("d", "pipeline.bounced", user, 7),
		row("d", "merge.failed", `not json`, 8),
		row("e", "pipeline.bounced", user, 4),
		row("f", "review.completed", `{}`, 6),
	})
	if got := strings.Join(needs.submissionTasks, ","); got != "a,d" {
		t.Fatalf("submission tasks = %s, want a,d", got)
	}
	if got := strings.Join(needs.claimTasks, ","); got != "b,d" {
		t.Fatalf("claim tasks = %s, want b,d", got)
	}
	if !needs.claimsSince.Equal(at(7)) {
		t.Fatalf("claims since %s, want %s", needs.claimsSince, at(7))
	}
	if needs := markerWideNeedsOf(in, nil); needs.submissionTasks != nil || needs.claimTasks != nil || !needs.claimsSince.IsZero() {
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
	_, args = markerFoldQuery(in)
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
