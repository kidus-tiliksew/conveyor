package postgres

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestMarkerFoldQueryShape pins the PostgreSQL marker statements: the
// placeholders read $1 to $n in text order, one per argument, no snapshot body
// key is named outside the compact payload, the review-round kinds are
// selected, kept for review tasks, and projected, review claims are selected
// by job and never by an open review order ID, and the request claims of a
// task in review start at its request fence.
func TestMarkerFoldQueryShape(t *testing.T) {
	in := store.MarkerFoldInput{Workspace: "ws", Page: []string{"task"}, ReviewTasks: []string{"task"}, LiveReviewTasks: []string{"task"},
		OpenReviewOrders: []store.MarkerReviewOrder{{TaskID: "task", ID: "order-1", JobID: "job-1"}, {TaskID: "task", ID: `odd"id`, JobID: "job-2"}}, Now: time.Unix(10, 5)}
	placeholder := regexp.MustCompile(`\$([0-9]+)`)
	check := func(name, query string, args []any) {
		t.Helper()
		// The binder numbers every occurrence in text order, so the
		// placeholders must read $1, $2, ... $n with n the argument count.
		matches := placeholder.FindAllStringSubmatch(query, -1)
		if len(matches) != len(args) {
			t.Errorf("%s statement holds %d placeholders for %d arguments", name, len(matches), len(args))
		}
		for i, match := range matches {
			if n, _ := strconv.Atoi(match[1]); n != i+1 {
				t.Fatalf("%s statement's placeholder %d is $%d, want $%d", name, i+1, n, i+1)
			}
		}
		// The compact payload column names the snapshot keys only to delete them.
		outside := strings.ReplaceAll(query, markerCompact, "")
		if strings.Contains(outside, "governance_snapshot'") || strings.Contains(outside, "served_requirement_snapshot'") {
			t.Errorf("%s statement names a snapshot body key outside the compact payload", name)
		}
		// The review recovery folds read the review-round kinds of review
		// tasks, and each kind is projected to the keys its decoder reads.
		for _, kind := range store.MarkerReviewRoundKinds {
			if strings.Count(query, "'"+kind+"'") < 3 {
				t.Errorf("%s statement does not select, filter, and project %s", name, kind)
			}
		}
	}
	query, args := markerFoldQuery(in)
	check("open review fold", query, args)
	// A claim belongs to a review order only through its job
	// (store.MarkerReviewJobs): the statement matches no claim against an
	// open review order ID, binds no order ID, and selects a task's review
	// claims by the jobs of its open review orders.
	for _, absent := range []string{"unnest(", "order_quoted", "AND NOT (task_id=ANY("} {
		if strings.Contains(query, absent) {
			t.Fatalf("the open review fold holds %q; claims match review orders by job alone", absent)
		}
	}
	jobLists := 0
	for _, arg := range args {
		switch value := arg.(type) {
		case string:
			if value == `odd"id` || value == "order-1" {
				t.Fatalf("the open review fold binds the order ID %q", value)
			}
		case []string:
			if strings.Join(value, ",") == "job-1,job-2" {
				jobLists++
			}
			for _, id := range value {
				if id == `odd"id` || id == "order-1" {
					t.Fatalf("the open review fold binds the order ID %q", id)
				}
			}
		}
	}
	// One job list selects the review claims and one the review renewals.
	if jobLists != 2 {
		t.Fatalf("the open review fold binds the review job list %d times, want 2", jobLists)
	}
	// The request fold of a task in review reads its implement claims from
	// the request fence on, in the same statement.
	flat := strings.Join(strings.Fields(query), " ")
	for _, want := range []string{"request_bounces AS (", "request_fences AS (", "EXISTS (SELECT 1 FROM request_fences f WHERE f.task_id=folded_rows.task_id AND folded_rows.event_at>=f.fence)", "COALESCE(MAX(CASE WHEN"} {
		if !strings.Contains(flat, want) {
			t.Fatalf("the open review fold does not hold %q", want)
		}
	}
	plain := in
	plain.LiveReviewTasks, plain.OpenReviewOrders = nil, nil
	query, args = markerFoldQuery(plain)
	check("fold", query, args)
	if strings.Contains(query, "request_fences") {
		t.Fatal("a page without an open review order reads request fences")
	}
	full := in
	full.KeepAllClaims = true
	query, args = markerFoldQuery(full)
	check("keep-all-claims fold", query, args)
	if strings.Contains(query, "claim_horizon_rank>1") {
		t.Fatal("the keep-all-claims statement still reduces claims")
	}
}
