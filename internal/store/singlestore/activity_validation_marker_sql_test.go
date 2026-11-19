package singlestore

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

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

// TestMarkerFoldQueryShape freezes the SingleStore fold read as four
// statement texts: without and with an open review order, each with reduced
// claims or every claim. Page size and the number of open review orders
// change only bound values, never the text or the number of placeholders,
// so the engine compiles at most four plans (component-web-task-surfaces
// invariant 19). Every statement binds one argument per question mark, names
// no snapshot body key outside the compact payload, selects the review-round
// kinds, and carries no per-key JSON proof or projection CASE.
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
					for _, retired := range []string{"JSON_KEYS", "JSON_BUILD_OBJECT", "k_lower"} {
						if strings.Contains(query, retired) {
							t.Fatalf("%s: still holds %s", name, retired)
						}
					}
				}
			}
			texts[want]++
			if live && !keepAll && !strings.Contains(want, "TABLE(JSON_TO_ARRAY(?))") {
				t.Fatal("the reduced open-review statement does not bind the open review orders as one JSON array")
			}
			if keepAll && strings.Contains(want, "reducible") {
				t.Fatalf("the every-claim statement (live=%t) still reduces claims", live)
			}
		}
	}
	if len(texts) != 4 {
		t.Fatalf("the fold read produces %d statement texts, want 4", len(texts))
	}
	// The lease decoder runs once per statement, on renewals only.
	query, _ := markerFoldQuery(markerShapeInput(100, 5, false))
	if got := strings.Count(query, "AS lease_ok"); got != 1 {
		t.Fatalf("the open-review statement decodes leases %d times, want once", got)
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
